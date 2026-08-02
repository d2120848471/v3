"""设备会话预热池：把 Log1/Log2 挪到上一轮的响应之后。

## 它省的是什么

一轮挑战的第一段是设备链——准备 SDK、启动 Node、跑完 Log1 与 Log2，实测约
460ms，期间 Python 只能阻塞等待。而 DeviceToken 是 ``InitCaptchaV3`` 的**入参**，
先于 ``CertifyId`` 存在、不与本轮挑战绑定，所以它完全可以提前备好。

实测把会话放置 10 秒与 30 秒后再跑完整一轮，都拿到 ``T001``；用池之后用户可见
耗时从约 1.4s 降到约 1.0s。默认的 :data:`MAX_AGE_SECONDS` 取在实测过的区间内侧。

## 为什么默认关闭

预热是**投机**的：备好的会话如果没人来领，那次 Log1/Log2 就是白发的请求。这与
本项目"不产生非必要外部流量"的边界直接冲突，因此：

* 默认关闭，由部署方显式开启；
* **需求驱动**——只在刚服务完一轮之后为同一配置补一个，不做定时补货；
* **过期不重建**——备好的会话超龄就关掉丢弃，不会自动再起一个。流量因此随请求
  自然衰减到零，服务器空转时不会持续骚扰 FeiLin。

## 为什么必须按配置分桶

``DeviceConfig`` 携带 ``ip`` 字段，而 Log1/Log2 在**预热那一刻**就已经发出去了。
如果拿一个直连预热出来的会话去服务走代理的一轮，指纹里的 IP 会和 Init/Verify 的
实际源 IP 对不上。因此租借前必须确认配置签名完全一致——包括代理——不匹配就直接
丢弃重建，绝不将就。

轮换代理的部署几乎命中不了池（每个代理都是新桶），此时开启它只会白发请求，不如
保持关闭。

## 与逐轮设备画像的冲突

同样的道理适用于设备画像：``DeviceToken`` 里的指纹是**预热那一刻**用当时那套画像
采集的，交给另一套画像的一轮就会让 HTTP 头和指纹对不上。画像默认逐轮重抽，因此
每一轮都是新桶——开启预热池只会白发 Log1/Log2，一次也命中不了。

**当前默认配置下，这个开关应当保持关闭。** 只有当部署方显式固定画像（比如自己
持有一个 :class:`DeviceProfile` 复用若干轮）时，它才重新有意义。
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
    """最多持有一个预热会话的单槽池。

    只留一个槽是刻意的：槽越多，投机出去的 FeiLin 会话就越多。一个槽已经足够把
    设备链从"下一轮的关键路径"挪到"上一轮的响应之后"。
    """

    def __init__(
        self,
        *,
        enabled: bool = False,
        max_age_seconds: float = MAX_AGE_SECONDS,
    ) -> None:
        if max_age_seconds <= 0:
            raise ValueError("max_age_seconds 必须为正数")
        self.enabled = bool(enabled)
        self.max_age_seconds = float(max_age_seconds)
        self._lock = threading.Lock()
        self._entry: _Entry | None = None
        self._filling: set[str] = set()
        self._closed = False

    # -- 租借 -------------------------------------------------------------

    @contextmanager
    def lease(self, client: DeviceRuntimeClient) -> Iterator[Any]:
        """借出一轮挑战要用的设备运行时。

        返回的对象只提供 ``challenge_session()``，与 :class:`DeviceRuntimeClient`
        在 ``run_captcha`` 眼里完全等价——命中池时它交出预热好的会话，未命中时
        原样退化为现建。

        无论命中与否，本轮结束后都会为同一配置补一个（仅在池已开启时）。
        """

        key = configuration_key(client)
        entry = self._take(key)
        try:
            yield _LeasedRuntime(client, entry)
        finally:
            if entry is not None:
                entry.close()
            self._schedule_fill(client, key)

    def _take(self, key: str) -> _Entry | None:
        """取出匹配且仍新鲜的槽内会话；不匹配或超龄一律关掉丢弃。"""

        with self._lock:
            entry = self._entry
            self._entry = None
        if entry is None:
            return None
        if entry.key != key or entry.expired(
            max_age_seconds=self.max_age_seconds
        ):
            entry.close()
            return None
        # 进程可能已经异常退出，交出去之前确认它还活着。
        process = entry.session.process
        if process is None or process.poll() is not None:
            entry.close()
            return None
        return entry

    # -- 补货 -------------------------------------------------------------

    def _schedule_fill(self, client: DeviceRuntimeClient, key: str) -> None:
        """本轮服务完之后，为同一配置在后台补一个。

        只在这里补——没有定时器，没有"过期自动重建"，因此投机流量随请求自然
        衰减到零。
        """

        if not self.enabled or self._closed:
            return
        with self._lock:
            if self._entry is not None or key in self._filling:
                return
            self._filling.add(key)
        threading.Thread(
            target=self._fill,
            args=(client, key),
            name="ali-device-prewarm",
            daemon=True,
        ).start()

    def _fill(self, client: DeviceRuntimeClient, key: str) -> None:
        """在后台建好一个会话并放进槽里；失败静默丢弃。"""

        stack = ExitStack()
        try:
            session = stack.enter_context(client.challenge_session())
        except Exception:
            stack.close()
            with self._lock:
                self._filling.discard(key)
            return

        entry = _Entry(
            session=session,
            stack=stack,
            key=key,
            created_at=time.monotonic(),
        )
        with self._lock:
            self._filling.discard(key)
            # 关池或已有槽位时不占坑，直接回收，避免悬挂 Node 进程。
            if self._closed or self._entry is not None:
                stale: _Entry | None = entry
            else:
                self._entry, stale = entry, None
        if stale is not None:
            stale.close()

    # -- 生命周期 ---------------------------------------------------------

    def close(self) -> None:
        """关闭池并回收槽内会话。"""

        with self._lock:
            self._closed = True
            entry, self._entry = self._entry, None
        if entry is not None:
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
