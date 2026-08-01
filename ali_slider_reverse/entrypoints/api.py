"""HTTP 接口入口。

把原本只能由 ``cli run`` 触发的一轮挑战包装成接口调用。实现只用标准库
``http.server``，不引入 Web 框架依赖。

## 参数边界

Node、视觉解释器、超时等运行环境由**启动命令**固定；逐轮变化的只有四个请求参数：

```text
SceneId     可选，验证码场景 ID
proxy       可选，本轮全部出网走该代理；省略或空串则整轮直连
AaduaneId   可选，覆盖 RPC key id
prefix      可选，验证码实例域名前缀，同时决定 Init/Verify 域名
```

``proxy`` 覆盖本轮**全部三个网络出口**（SDK 下载、Init/Verify/日志、图片与动态
PE），避免同一 ``CertifyId`` 的请求出现在多个源 IP 上。

## 状态码语义

```text
400  入参不合法，不消耗挑战
429  超出并发上限
500  协议或运行错误
200  一次正常往返；验证码未通过时 ok=false，由 VerifyCode 说明原因
```

每轮挑战使用独立的客户端、FeiLin worker、临时目录与 ``CertifyId``，不共享任何
可变状态；单次 Verify 的边界仍然逐轮成立。
"""

from __future__ import annotations

import argparse
import json
import sys
import threading
import time
from collections.abc import Callable
from contextlib import closing
from dataclasses import dataclass
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any
from urllib.parse import parse_qs, urlsplit

from .. import config
from ..challenge.device_pool import DeviceSessionPool
from ..challenge.session import normalize_proxies
from ..errors import AliSliderError, ApiRequestError
from .options import (
    RuntimeSettings,
    add_confidence_argument,
    add_runtime_arguments,
    add_server_arguments,
)


_SCENE_ID_MAX_LENGTH = 64
_RPC_KEY_ID_MAX_LENGTH = 128
_PREFIX_MAX_LENGTH = 32


@dataclass(frozen=True, slots=True)
class SolveRequest:
    """单次接口调用解析出的挑战参数。"""

    scene_id: str
    proxies: dict[str, str] | None
    rpc_key_id: str | None
    prefix: str


def _optional_text(value: Any, *, label: str, max_length: int) -> str | None:
    """接受字符串或缺省；空串一律按"未传"处理，交由默认值兜底。"""

    if value is None:
        return None
    if not isinstance(value, str):
        raise ApiRequestError(f"{label} 必须是字符串")
    text = value.strip()
    if not text:
        return None
    if len(text) > max_length:
        raise ApiRequestError(f"{label} 超过 {max_length} 字符")
    return text


def parse_solve_request(payload: dict[str, Any]) -> SolveRequest:
    """把接口 JSON 或查询参数映射为一轮挑战的配置。

    ``SceneId`` 与 ``AaduaneId`` 同时兼容首字母小写写法，便于不同调用方接入。
    """

    if not isinstance(payload, dict):
        raise ApiRequestError("请求体必须是 JSON object")

    scene_id = _optional_text(
        payload.get("SceneId", payload.get("sceneId")),
        label="SceneId",
        max_length=_SCENE_ID_MAX_LENGTH,
    )
    rpc_key_id = _optional_text(
        payload.get("AaduaneId", payload.get("aaduaneId")),
        label="AaduaneId",
        max_length=_RPC_KEY_ID_MAX_LENGTH,
    )
    prefix = _optional_text(
        payload.get("prefix", payload.get("Prefix")),
        label="prefix",
        max_length=_PREFIX_MAX_LENGTH,
    )
    if prefix is not None and not prefix.isalnum():
        raise ApiRequestError("prefix 只能是字母数字")

    proxy = payload.get("proxy", payload.get("Proxy"))
    if proxy is not None and not isinstance(proxy, str):
        raise ApiRequestError("proxy 必须是字符串")
    try:
        proxies = normalize_proxies(proxy)
    except ValueError as exc:
        raise ApiRequestError(str(exc)) from exc

    return SolveRequest(
        scene_id=scene_id or config.DEFAULT_SCENE_ID,
        proxies=proxies,
        rpc_key_id=rpc_key_id,
        prefix=prefix or config.DEFAULT_PREFIX,
    )


def solve_once(
    request: SolveRequest,
    settings: RuntimeSettings,
    pool: DeviceSessionPool | None = None,
) -> dict[str, Any]:
    """执行一轮完整挑战并返回响应体。

    每次调用都新建客户端与设备运行时：``proxy``、``SceneId`` 与 ``AaduaneId``
    逐轮不同，而 FeiLin worker 本身就绑定单轮 session，不适合跨请求复用。

    ``pool`` 只改变 FeiLin 会话是"现建"还是"上一轮之后就备好的"，不改变本轮
    只用一个会话、一个 ``CertifyId`` 的语义；未开启时行为与之前完全一致。
    """

    client = settings.build_client(
        scene_id=request.scene_id,
        prefix=request.prefix,
        proxies=request.proxies,
        rpc_key_id=request.rpc_key_id,
    )
    device_runtime = settings.build_device_runtime(
        prefix=request.prefix, proxies=request.proxies
    )
    with closing(client):
        # 视觉解释器的冷导入与 DeviceToken、Init、资源下载重叠；五个出网主机的
        # TLS 握手与设备链重叠。
        client.prewarm_vision()
        client.prewarm_connections()
        started = time.monotonic()
        if pool is None:
            outcome = client.run_captcha(
                device_runtime, minimum_confidence=settings.minimum_confidence
            )
        else:
            with pool.lease(device_runtime) as leased:
                outcome = client.run_captcha(
                    leased, minimum_confidence=settings.minimum_confidence
                )
        elapsed_ms = int((time.monotonic() - started) * 1000)

    verify = outcome.verify
    return {
        "ok": verify.succeeded,
        "securityToken": verify.security_token,
        "VerifyCode": verify.verify_code,
        "VerifyResult": verify.verify_result,
        "certifyId": verify.certify_id,
        "sceneId": request.scene_id,
        "proxied": request.proxies is not None,
        "elapsedMs": elapsed_ms,
    }


class _Unlimited:
    """不限并发时的槽位占位，让处理逻辑无需分支判断。"""

    def acquire(self, blocking: bool = True) -> bool:
        return True

    def release(self) -> None:
        return None


class SliderApiHandler(BaseHTTPRequestHandler):
    """把 ``/api/slider`` 映射到一轮挑战；其余路径一律 404。"""

    protocol_version = "HTTP/1.1"
    server_version = "AliSliderApi"
    sys_version = ""

    settings: RuntimeSettings = RuntimeSettings()
    slots: Any = _Unlimited()
    device_pool: DeviceSessionPool | None = None

    result_hook: Callable[[str, str, int, dict[str, Any]], None] | None = None
    """可选的宿主回调，每次响应发出后以 ``(method, path, status, body)`` 调用。

    给桌面启动器一类的宿主一个观察本轮结果的口子。回调内的异常一律吞掉：展示
    逻辑绝不能影响已经发出的协议响应。

    赋值请用**可调用对象**或 ``staticmethod``：这是类属性，直接挂一个普通函数会
    被描述符协议当成方法绑定，第一个实参会变成 handler 自己。
    """

    def do_POST(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler 约定。
        if urlsplit(self.path).path != config.API_SOLVE_PATH:
            self._send_json(404, {"ok": False, "error": "未知路径"})
            return
        try:
            payload = self._read_json_body()
        except ApiRequestError as exc:
            self._send_error(400, exc)
            return
        self._handle_solve(payload)

    def do_GET(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler 约定。
        parts = urlsplit(self.path)
        if parts.path == config.API_HEALTH_PATH:
            self._send_json(200, {"ok": True, "status": "ready"})
            return
        if parts.path != config.API_SOLVE_PATH:
            self._send_json(404, {"ok": False, "error": "未知路径"})
            return
        # GET 只是 POST 的便捷等价物，便于 curl 快速验证。
        self._handle_solve(
            {
                key: values[-1]
                for key, values in parse_qs(
                    parts.query, keep_blank_values=True
                ).items()
            }
        )

    def _handle_solve(self, payload: dict[str, Any]) -> None:
        try:
            request = parse_solve_request(payload)
        except ApiRequestError as exc:
            self._send_error(400, exc)
            return

        # 默认不限并发；仅在显式设置上限时拒绝超出的请求，避免排队让调用方一起
        # 超时。每轮挑战本身仍是互相独立的 CertifyId，不共享会话或 worker。
        if not self.slots.acquire(blocking=False):
            self._send_json(
                429,
                {
                    "ok": False,
                    "errorType": "TooManyChallenges",
                    "error": "已达并发挑战上限，请稍后重试",
                },
            )
            return

        try:
            body = solve_once(request, self.settings, self.device_pool)
        except (AliSliderError, ValueError) as exc:
            self._send_error(500, exc)
            return
        except Exception:
            # 与 CLI 一致：不把未预期异常的内容回给调用方。
            self._send_json(
                500,
                {
                    "ok": False,
                    "errorType": "UnhandledProtocolError",
                    "error": "未处理的协议运行错误",
                },
            )
            return
        finally:
            self.slots.release()

        # 验证码未通过也是一次完整、正常的协议往返，用 200 承载 ok=false。
        self._send_json(200, body)

    def _read_json_body(self) -> dict[str, Any]:
        try:
            length = int(self.headers.get("Content-Length", "0"))
        except ValueError as exc:
            raise ApiRequestError("Content-Length 无效") from exc
        if length < 0 or length > config.API_MAX_BODY_BYTES:
            raise ApiRequestError(
                f"请求体必须位于 0..{config.API_MAX_BODY_BYTES} 字节"
            )
        if length == 0:
            return {}

        try:
            payload = json.loads(self.rfile.read(length).decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise ApiRequestError("请求体不是合法 JSON") from exc
        if not isinstance(payload, dict):
            raise ApiRequestError("请求体必须是 JSON object")
        return payload

    def _send_json(self, status: int, body: dict[str, Any]) -> None:
        payload = json.dumps(
            body, ensure_ascii=False, separators=(",", ":")
        ).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

        hook = self.result_hook
        if hook is not None:
            try:
                hook(self.command, urlsplit(self.path).path, status, body)
            except Exception:  # pragma: no cover - 展示失败不影响协议响应。
                pass

    def _send_error(self, status: int, exc: Exception) -> None:
        self._send_json(
            status,
            {
                "ok": False,
                "errorType": type(exc).__name__,
                "error": str(exc),
            },
        )

    def log_message(self, format: str, *args: Any) -> None:
        """只记录方法、路径与状态码。

        GET 的 query 可能带有 ``proxy`` 的用户名/口令，默认实现会把整条
        requestline 写进 stderr，因此这里改为只输出剥掉 query 的路径。
        """

        status = args[1] if len(args) > 1 else "-"
        print(
            f"{self.log_date_time_string()} {self.command} "
            f"{urlsplit(self.path).path} {status}",
            file=sys.stderr,
        )


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="python -m ali_slider_reverse.entrypoints.api",
        description="阿里 V3 滑块纯协议 HTTP 接口",
    )
    add_server_arguments(parser)
    add_runtime_arguments(parser)
    add_confidence_argument(parser)
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    if args.max_concurrency < 0:
        raise SystemExit("--max-concurrency 必须 >= 0")
    if not 0.0 <= args.min_confidence <= 1.0:
        raise SystemExit("--min-confidence 必须位于 0..1")

    SliderApiHandler.settings = RuntimeSettings.from_args(args)
    SliderApiHandler.slots = (
        threading.BoundedSemaphore(args.max_concurrency)
        if args.max_concurrency > 0
        else _Unlimited()
    )
    device_pool = DeviceSessionPool(enabled=args.prewarm_device_session)
    SliderApiHandler.device_pool = device_pool

    server = ThreadingHTTPServer((args.host, args.port), SliderApiHandler)
    server.daemon_threads = True
    host, port = server.server_address[:2]
    limit_label = (
        f"并发上限 {args.max_concurrency}"
        if args.max_concurrency > 0
        else "不限并发"
    )
    prewarm_label = (
        "预热设备会话" if args.prewarm_device_session else "不预热设备会话"
    )
    print(
        f"滑块接口已启动：http://{host}:{port}{config.API_SOLVE_PATH}"
        f"（{limit_label}，{prewarm_label}）",
        file=sys.stderr,
    )
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print("正在停止……", file=sys.stderr)
    finally:
        server.server_close()
        device_pool.close()
    return 0


__all__ = [
    "SliderApiHandler",
    "SolveRequest",
    "build_parser",
    "main",
    "parse_solve_request",
    "solve_once",
]


if __name__ == "__main__":
    raise SystemExit(main())
