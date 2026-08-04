"""设备会话预热池：把 Log1/Log2 挪到请求关键路径之外。

## 它省的是什么

一轮挑战的第一段是设备链——准备 SDK、启动 Node、跑完 Log1 与 Log2，实测约
460ms，期间 Python 只能阻塞等待。而 DeviceToken 是 ``InitCaptchaV3`` 的**入参**，
先于 ``CertifyId`` 存在、不与本轮挑战绑定，所以它完全可以提前备好。

实测把会话放置 10 秒与 30 秒后再跑完整一轮，都拿到 ``T001``；用池之后用户可见
耗时从约 1.4s 降到约 1.0s。默认的 :data:`MAX_AGE_SECONDS` 取在实测过的区间内侧。

## 标准模式与快速模式

预热是**投机**的：备好的会话如果没人来领，那次 Log1/Log2 就是白发的请求。这与
本项目"不产生非必要外部流量"的边界直接冲突，因此：

* 标准模式默认关闭，由部署方显式选择快速模式后才开启；
* 快速模式可以在服务启动时按并发容量一次性预热；其后仍然是**需求驱动**——只在
  会话被领用后补回容量，不做定时补货；
* **过期不重建**——备好的会话超龄就关掉丢弃，不会自动再起一个。流量因此随请求
  自然衰减到零，服务器空转时不会持续骚扰 FeiLin。

## 为什么一个池只绑定一套配置

``DeviceConfig`` 携带 ``ip`` 字段，而 Log1/Log2 在**预热那一刻**就已经发出去了。
如果拿一个直连预热出来的会话去服务走代理的一轮，指纹里的 IP 会和 Init/Verify 的
实际源 IP 对不上。因此池在第一次补货时固定完整配置签名（包括代理与画像）；不匹配
的请求直接走冷建，不会消费或挤掉已经备好的会话。

轮换代理的部署几乎命中不了池（每个代理都是新桶），此时开启它只会白发请求，不如
保持关闭。

## 与逐轮设备画像的冲突

同样的道理适用于设备画像：``DeviceToken`` 里的指纹是**预热那一刻**用当时那套画像
采集的，交给另一套画像的一轮就会让 HTTP 头和指纹对不上。画像默认逐轮重抽，因此
每一轮都是新桶——开启预热池只会白发 Log1/Log2，一次也命中不了。

标准模式继续逐轮生成画像并关闭预热；快速模式则显式固定本进程画像，让预热会话与
同一轮的 HTTP 头、FeiLin 和动态 PE 保持一致。
"""

from __future__ import annotations

import threading
import time
from collections.abc import Iterator
from contextlib import ExitStack, contextmanager
from dataclasses import dataclass, field
from typing import Any

from ..runtime.node_device import DeviceRuntimeClient, DeviceRuntimeSession


MAX_AGE_SECONDS = 20.0
"""预热会话的最大存活秒数。

实测 30 秒老化的 DeviceToken 仍能拿到 T001，这里留一半余量。超过 30 秒的边界
没有实测过，调大之前请先自己验。
"""


def configuration_key(client: DeviceRuntimeClient) -> str:
    """能影响 FeiLin 会话本身的全部配置的签名。

    代理与设备画像都必须进签名——理由见模块文档。
    """

    proxies = client.proxies or {}
    return "\x00".join(
        (
            client.node_binary,
            str(client.bridge_script),
            str(client.sdk_path),
            client.sdk_url,
            client.prefix,
            client.region,
            f"{client.timeout:g}",
            f"{client.gather_cost_range}",
            f"{client.first_touch_age_range}",
            client.device_profile.profile_id,
            *(f"{name}={proxies[name]}" for name in sorted(proxies)),
        )
    )


@dataclass(slots=True)
class _Entry:
    """一个已完成 Init 阶段、等着被领用的会话。"""

    session: DeviceRuntimeSession
    stack: ExitStack = field(repr=False)
    key: str = field(repr=False)
    created_at: float

    def expired(self, *, max_age_seconds: float) -> bool:
        return time.monotonic() - self.created_at > max_age_seconds

    def close(self) -> None:
        try:
            self.stack.close()
        except Exception:  # pragma: no cover - 回收失败不应影响调用方。
            pass


class DeviceSessionPool:
    """为一套固定配置持有有界数量的预热会话。"""

    def __init__(
        self,
        *,
        enabled: bool = False,
        max_age_seconds: float = MAX_AGE_SECONDS,
        capacity: int = 1,
    ) -> None:
        if max_age_seconds <= 0:
            raise ValueError("max_age_seconds 必须为正数")
        if isinstance(capacity, bool) or not isinstance(capacity, int) or capacity < 1:
            raise ValueError("capacity 必须是正整数")
        self.enabled = bool(enabled)
        self.max_age_seconds = float(max_age_seconds)
        self.capacity = capacity
        self._lock = threading.Lock()
        self._condition = threading.Condition(self._lock)
        self._entries: list[_Entry] = []
        self._filling = 0
        self._leased = 0
        self._key: str | None = None
        self._closed = False

    # -- 租借 -------------------------------------------------------------

    @contextmanager
    def lease(self, client: DeviceRuntimeClient) -> Iterator[Any]:
        """借出一轮挑战要用的设备运行时，未命中时原样退化为现建。"""

        key = configuration_key(client)
        tracked = self._begin_lease(key)
        entry = self._take(key, wait_seconds=client.timeout + 5.0)
        try:
            yield _LeasedRuntime(client, entry)
        finally:
            if entry is not None:
                entry.close()
            self._finish_lease(client, key, tracked=tracked)

    def _begin_lease(self, key: str) -> bool:
        """登记一条匹配请求，供补货逻辑识别仍在执行的同批请求。"""

        if not self.enabled:
            return False
        with self._lock:
            if self._closed:
                return False
            if self._key is None:
                self._key = key
            if key != self._key:
                return False
            self._leased += 1
            return True

    def _finish_lease(
        self,
        client: DeviceRuntimeClient,
        key: str,
        *,
        tracked: bool,
    ) -> None:
        """最后一条同批请求结束后再补货，避免 Node 进程争抢在途请求资源。"""

        if not tracked:
            return
        threads: tuple[threading.Thread, ...] = ()
        with self._lock:
            self._leased -= 1
            if self._leased == 0:
                # 在同一把锁内预留补货名额，避免最后一条请求退出与下一批请求
                # 进入之间的竞态让补货被静默跳过。
                threads = self._reserve_fill_locked(client, key)
        for thread in threads:
            thread.start()

    def _take(self, key: str, *, wait_seconds: float) -> _Entry | None:
        """取出新鲜会话；已有补货在途时等待它，避免再冷建一整批。"""

        deadline = time.monotonic() + max(0.0, wait_seconds)
        while True:
            with self._condition:
                if key != self._key or self._closed:
                    return None
                if self._entries:
                    entry = self._entries.pop()
                elif self._filling > 0:
                    remaining = deadline - time.monotonic()
                    if remaining <= 0:
                        return None
                    self._condition.wait(timeout=remaining)
                    continue
                else:
                    return None
            if entry.expired(max_age_seconds=self.max_age_seconds):
                entry.close()
                continue
            process = entry.session.process
            if process is None or process.poll() is not None:
                entry.close()
                continue
            return entry

    # -- 补货 -------------------------------------------------------------

    def prime(self, client: DeviceRuntimeClient) -> int:
        """并行补到容量并等待本批完成，返回当前可用会话数。"""

        key = configuration_key(client)
        threads = self._schedule_fill(client, key)
        deadline = time.monotonic() + client.timeout + 5.0
        for thread in threads:
            thread.join(timeout=max(0.0, deadline - time.monotonic()))
        with self._lock:
            return len(self._entries) if key == self._key else 0

    def _schedule_fill(
        self, client: DeviceRuntimeClient, key: str
    ) -> tuple[threading.Thread, ...]:
        """为绑定配置补足容量，返回本次新建的后台线程。"""

        with self._lock:
            threads = self._reserve_fill_locked(client, key)
        for thread in threads:
            thread.start()
        return threads

    def _reserve_fill_locked(
        self, client: DeviceRuntimeClient, key: str
    ) -> tuple[threading.Thread, ...]:
        """在持有 ``_lock`` 时原子预留补货名额并构造对应线程。"""

        if not self.enabled or self._closed:
            return ()
        if self._key is None:
            self._key = key
        if key != self._key:
            return ()
        # 同一批挑战还在执行时启动下一批 Node worker，会与 PE、视觉和网络
        # 收尾争抢 CPU/连接，既拖慢尾部请求，也会放大冷启动失败。等最后一条
        # lease 退出后由 _finish_lease 一次性补齐即可。
        if self._leased > 0:
            return ()
        missing = self.capacity - len(self._entries) - self._filling
        if missing <= 0:
            return ()
        self._filling += missing
        return tuple(
            threading.Thread(
                target=self._fill,
                args=(client, key),
                name=f"ali-device-prewarm-{index + 1}",
                daemon=True,
            )
            for index in range(missing)
        )

    def _fill(self, client: DeviceRuntimeClient, key: str) -> None:
        """在后台建好一枚会话；失败静默丢弃。"""

        stack = ExitStack()
        try:
            session = stack.enter_context(client.challenge_session())
        except Exception:
            stack.close()
            with self._condition:
                self._filling -= 1
                self._condition.notify_all()
            return

        entry = _Entry(
            session=session,
            stack=stack,
            key=key,
            created_at=time.monotonic(),
        )
        with self._condition:
            self._filling -= 1
            if self._closed or self._key != key or len(self._entries) >= self.capacity:
                stale: _Entry | None = entry
            else:
                self._entries.append(entry)
                stale = None
            self._condition.notify_all()
        if stale is not None:
            stale.close()

    # -- 生命周期 ---------------------------------------------------------

    def close(self) -> None:
        """关闭池并回收所有已经就绪的会话。"""

        with self._condition:
            self._closed = True
            entries, self._entries = self._entries, []
            self._condition.notify_all()
        for entry in entries:
            entry.close()


class _LeasedRuntime:
    """把"预热会话"与"现建会话"统一成 ``run_captcha`` 认识的同一个接口。"""

    __slots__ = ("_client", "_entry")

    def __init__(
        self,
        client: DeviceRuntimeClient,
        entry: _Entry | None,
    ) -> None:
        self._client = client
        self._entry = entry

    @contextmanager
    def challenge_session(self) -> Iterator[DeviceRuntimeSession]:
        if self._entry is None:
            with self._client.challenge_session() as session:
                yield session
            return
        # 预热会话的回收由 lease() 的 finally 负责，这里只借不关——否则本轮还
        # 没走完 Node 进程就没了。
        yield self._entry.session


__all__ = [
    "MAX_AGE_SECONDS",
    "DeviceSessionPool",
    "configuration_key",
]
