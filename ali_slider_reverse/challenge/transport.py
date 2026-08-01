"""共享连接池与 TLS 预热：把握手挪出关键路径。

## 为什么需要

一轮挑战要依次访问五个不同主机——Init、Verify、UploadLog、图片 CDN、动态 PE
CDN。它们各自独立握手，而 ``<prefix>-verify.captcha-open.aliyuncs.com`` 更是在
整条链路的**最后一步**才第一次被访问，那时候已经没有任何工作可以和它重叠。

与此同时，设备链（SDK 准备 → Node 启动 → Log1 → Log2）期间 Python 侧完全阻塞在
读取子进程输出上，是一段几百毫秒的纯空窗。:func:`warm_connections` 就是把上述
握手提前挪进这段空窗。

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
) -> None:
    """并发预热若干主机的 TLS 连接；立即返回，不阻塞调用方。

    ``proxies`` 非空时整体跳过——见模块文档的第一条硬约束。
    """

    if adapter is None or proxies:
        return

    bounded_timeout = max(0.5, min(float(timeout), WARM_CONNECT_TIMEOUT_SECONDS))
    for url in dict.fromkeys(urls):
        thread = threading.Thread(
            target=_warm_one,
            args=(adapter, url, bounded_timeout),
            name="ali-tls-warmup",
            daemon=True,
        )
        thread.start()


__all__ = [
    "WARM_CONNECT_TIMEOUT_SECONDS",
    "build_pool_adapter",
    "pooled_session",
    "warm_connections",
]
