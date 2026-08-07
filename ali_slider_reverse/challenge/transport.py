"""共享连接池与 TLS 预热：把握手挪出关键路径。

## 为什么需要

一轮挑战会访问 Init、Verify、可选 UploadLog、图片 CDN 与设备 RPC 主机。它们各自
独立握手，而 ``<prefix>-verify.captcha-open.aliyuncs.com`` 更是在
整条链路的**最后一步**才第一次被访问，那时候已经没有任何工作可以和它重叠。

设备链执行 Log1 并等待 DeviceConfig、构造首枚 token 的期间还有一小段空窗；
:func:`warm_connections` 把上述握手提前并行到这段时间。

预热只建立 TCP + TLS 连接并放回 urllib3 的连接池，**不发送任何 HTTP 字节**，因此
不产生任何协议侧副作用，也不会被服务端计入请求。

## 两条硬约束

* **走代理时不预热。** urllib3 的 ``CONNECT`` 隧道是在 ``urlopen`` 内部才建立的；
  直接对代理连接池调用 ``connect()`` 只会得到一个没有隧道的半成品连接，被后续
  请求取走反而出错。有代理就整体跳过，宁可不优化。
* **失败一律静默。** 预热纯属优化，任何异常都不能影响本轮挑战的成败。

## 为什么要共享 adapter

``requests.Session`` 不承诺跨线程共享，所以并发下载仍然每个 worker 一个 Session；
但真正持有热连接的是 adapter 里的 urllib3 ``PoolManager``，它本身是线程安全的。
把 **Session 按线程隔离、adapter 全轮共享**，既保留了原有的安全性质，又让预热出
来的连接能被所有出口复用。
"""

from __future__ import annotations

import threading
import time
from collections.abc import Iterator
from contextlib import contextmanager
from typing import Any

WARM_CONNECT_TIMEOUT_SECONDS = 5.0
"""单次预热握手的上限。

预热线程是 daemon，不会阻塞进程退出；这个上限只用来保证它们不会长期挂着。
"""


def build_pool_adapter(requests_module: Any, *, pool_size: int = 8) -> Any:
    """构造一轮挑战共享的 :class:`requests.adapters.HTTPAdapter`。

    ``max_retries`` 保持默认的 0：本项目所有重试语义都由上层显式控制，传输层
    静默重发会破坏"每个 CertifyId 只 Verify 一次"的约束。
    """

    return requests_module.adapters.HTTPAdapter(
        pool_connections=pool_size,
        pool_maxsize=pool_size,
    )


class SharedHttpAdapterPool:
    """按出网配置复用服务级 ``HTTPAdapter``。

    单轮挑战仍然各用自己的 :class:`requests.Session` 与 CookieJar；这里只把真正
    持有 TCP/TLS 热连接的 adapter 提升到服务生命周期。直连和每一套代理分别落在
    独立桶中，避免轮换代理时把连接取错出口。
    """

    def __init__(
        self,
        requests_module: Any | None = None,
        *,
        pool_size: int = 8,
        max_proxy_routes: int = 64,
    ) -> None:
        if (
            isinstance(pool_size, bool)
            or not isinstance(pool_size, int)
            or pool_size < 1
        ):
            raise ValueError("pool_size 必须是正整数")
        if (
            isinstance(max_proxy_routes, bool)
            or not isinstance(max_proxy_routes, int)
            or max_proxy_routes < 0
        ):
            raise ValueError("max_proxy_routes 必须是非负整数")
        if requests_module is None:
            import requests as requests_module  # type: ignore[no-redef]

        self._requests = requests_module
        self._pool_size = pool_size
        self._max_proxy_routes = max_proxy_routes
        self._lock = threading.Lock()
        self._adapters: dict[tuple[tuple[str, str], ...], Any] = {}
        self._closed = False

    @staticmethod
    def _configuration_key(
        proxies: dict[str, str] | None,
    ) -> tuple[tuple[str, str], ...]:
        """把直连/代理映射变成不含顺序差异的桶键。"""

        if not proxies:
            return ()
        return tuple(sorted((str(name), str(value)) for name, value in proxies.items()))

    def get(self, proxies: dict[str, str] | None = None) -> Any | None:
        """返回该出网配置的共享 adapter，超出代理桶上限时让调用方自建。"""

        key = self._configuration_key(proxies)
        with self._lock:
            if self._closed:
                raise RuntimeError("共享 HTTP adapter 池已关闭")
            adapter = self._adapters.get(key)
            if adapter is None:
                proxy_routes = sum(bool(route) for route in self._adapters)
                if key and proxy_routes >= self._max_proxy_routes:
                    return None
                adapter = build_pool_adapter(
                    self._requests,
                    pool_size=self._pool_size,
                )
                self._adapters[key] = adapter
            return adapter

    def close(self) -> None:
        """关闭池内全部直连/代理连接；重复调用安全。"""

        with self._lock:
            if self._closed:
                return
            self._closed = True
            adapters, self._adapters = tuple(self._adapters.values()), {}
        for adapter in adapters:
            try:
                adapter.close()
            except Exception:  # pragma: no cover - 关闭失败不影响进程退出。
                pass


@contextmanager
def pooled_session(
    requests_module: Any,
    adapter: Any | None,
    *,
    proxies: dict[str, str] | None = None,
) -> Iterator[Any]:
    """开出一个使用共享连接池的短生命周期 Session。

    退出时必须先把共享 adapter 摘掉再 ``close()``：``requests.Session.close()``
    会关闭它挂载的**全部** adapter，否则一次下载结束就把整轮预热好的连接连同
    连接池一起销毁了。
    """

    session = requests_module.Session()
    try:
        if adapter is not None:
            session.mount("https://", adapter)
        if proxies:
            session.proxies.update(proxies)
        yield session
    finally:
        if adapter is not None:
            session.adapters.pop("https://", None)
        session.close()


def _warm_one(adapter: Any, url: str, timeout: float) -> None:
    """对单个 URL 建立 TCP + TLS 连接并放回连接池。"""

    try:
        pool = adapter.poolmanager.connection_from_url(url)
        connection = pool._get_conn(timeout=timeout)
    except Exception:
        return

    try:
        # 池里已有热连接时直接放回，重复 connect() 会泄漏原有 socket。
        if getattr(connection, "sock", None) is None:
            connection.timeout = timeout
            connection.connect()
    except Exception:
        try:
            connection.close()
        except Exception:
            pass
        return

    try:
        pool._put_conn(connection)
    except Exception:
        pass


def warm_connections(
    adapter: Any | None,
    urls: tuple[str, ...],
    *,
    proxies: dict[str, str] | None = None,
    timeout: float = WARM_CONNECT_TIMEOUT_SECONDS,
    connections_per_url: int = 1,
    wait: bool = False,
) -> None:
    """并发预热若干主机的 TLS 连接。

    ``proxies`` 非空时整体跳过——见模块文档的第一条硬约束。

    默认仍立即返回；服务启动阶段可传 ``wait=True``，并按并发容量为每个主机准备
    多条连接，确保监听端口开放时首批请求也无需现场握手。
    """

    if adapter is None or proxies:
        return
    if (
        isinstance(connections_per_url, bool)
        or not isinstance(connections_per_url, int)
        or connections_per_url < 1
    ):
        raise ValueError("connections_per_url 必须是正整数")

    bounded_timeout = max(0.5, min(float(timeout), WARM_CONNECT_TIMEOUT_SECONDS))
    threads = tuple(
        threading.Thread(
            target=_warm_one,
            args=(adapter, url, bounded_timeout),
            name=f"ali-tls-warmup-{index + 1}",
            daemon=True,
        )
        for url in dict.fromkeys(urls)
        for index in range(connections_per_url)
    )
    for thread in threads:
        thread.start()
    if wait:
        deadline = time.monotonic() + bounded_timeout
        for thread in threads:
            thread.join(timeout=max(0.0, deadline - time.monotonic()))


__all__ = [
    "SharedHttpAdapterPool",
    "WARM_CONNECT_TIMEOUT_SECONDS",
    "build_pool_adapter",
    "pooled_session",
    "warm_connections",
]
