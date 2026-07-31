"""阿里 V3 滑块的 HTTP 接口入口。

把原本只能用 ``cli run`` 触发的一轮挑战包装成接口调用，便于外部系统按场景测试。
请求参数只有三项：``SceneId``、``proxy`` 与 ``AaduaneId``；传了 ``proxy`` 时本轮
Init/Verify/UploadLog、公开图片与动态 PE 下载以及公开 SDK 下载全部走该代理，不传
则整轮直连。Node、视觉解释器、超时等运行环境参数由服务启动命令决定，不经接口传入。

实现只用标准库 ``http.server``，不引入 Web 框架依赖。每轮挑战仍严格遵守
``README`` 的单次 Verify 边界：一个 ``CertifyId`` 最多发送一次 ``VerifyCaptchaV3``，
失败不重试、不换坐标重试。
"""

from __future__ import annotations

import argparse
import json
import sys
import threading
import time
from dataclasses import dataclass
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any
from urllib.parse import parse_qs, urlsplit

from .client import (
    DEFAULT_PREFIX,
    DEFAULT_SCENE_ID,
    AliSliderClient,
    AliSliderError,
    normalize_proxies,
)
from .device_runtime import (
    DEFAULT_FIRST_TOUCH_AGE_RANGE,
    DeviceRuntimeClient,
    DeviceRuntimeError,
)
from .pe_runtime import PeRuntimeError


DEFAULT_HOST = "127.0.0.1"
DEFAULT_PORT = 8000
SOLVE_PATH = "/api/slider"
HEALTH_PATH = "/health"
# 一轮挑战包含 Node VM、四路公网下载与 OpenCV 识别，请求体不需要很大。
_MAX_BODY_BYTES = 64 * 1024
_SCENE_ID_MAX_LENGTH = 64
_RPC_KEY_ID_MAX_LENGTH = 128
_PREFIX_MAX_LENGTH = 32


class ApiRequestError(ValueError):
    """接口入参不合法；对应 HTTP 400，不会消耗任何挑战。"""


@dataclass(frozen=True, slots=True)
class RuntimeSettings:
    """服务启动时固定的运行环境，与单次请求无关。"""

    node_binary: str = "node"
    vision_python: str = "/opt/homebrew/bin/python3"
    sdk_js: str | None = None
    timeout: float = 25.0
    minimum_confidence: float = 0.45
    gather_cost_range: tuple[int, int] = (180, 260)
    first_touch_age_range: tuple[int, int] = DEFAULT_FIRST_TOUCH_AGE_RANGE


@dataclass(frozen=True, slots=True)
class SolveRequest:
    """单次接口调用解析出的挑战参数。"""

    scene_id: str
    proxies: dict[str, str] | None
    rpc_key_id: str | None
    prefix: str


def _require_optional_text(
    value: Any,
    *,
    label: str,
    max_length: int,
) -> str | None:
    """接受字符串或缺省；空串一律按“未传”处理，交由默认值兜底。"""

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
    """把接口 JSON/查询参数映射为一轮挑战的配置。

    ``SceneId`` 与 ``AaduaneId`` 同时兼容首字母小写写法，便于不同调用方接入。
    """

    if not isinstance(payload, dict):
        raise ApiRequestError("请求体必须是 JSON object")

    scene_id = _require_optional_text(
        payload.get("SceneId", payload.get("sceneId")),
        label="SceneId",
        max_length=_SCENE_ID_MAX_LENGTH,
    )
    rpc_key_id = _require_optional_text(
        payload.get("AaduaneId", payload.get("aaduaneId")),
        label="AaduaneId",
        max_length=_RPC_KEY_ID_MAX_LENGTH,
    )
    prefix = _require_optional_text(
        payload.get("prefix", payload.get("Prefix")),
        label="prefix",
        max_length=_PREFIX_MAX_LENGTH,
    )
    proxy = payload.get("proxy", payload.get("Proxy"))
    if proxy is not None and not isinstance(proxy, str):
        raise ApiRequestError("proxy 必须是字符串")
    try:
        proxies = normalize_proxies(proxy)
    except ValueError as exc:
        raise ApiRequestError(str(exc)) from exc

    if prefix is not None and not prefix.isalnum():
        raise ApiRequestError("prefix 只能是字母数字")

    return SolveRequest(
        scene_id=scene_id or DEFAULT_SCENE_ID,
        proxies=proxies,
        rpc_key_id=rpc_key_id,
        prefix=prefix or DEFAULT_PREFIX,
    )


def solve_once(
    request: SolveRequest,
    settings: RuntimeSettings,
) -> dict[str, Any]:
    """执行一轮完整挑战并返回响应体。

    每次调用都新建客户端与设备运行时：``proxy``、``SceneId`` 和 ``AaduaneId``
    逐轮不同，且 FeiLin worker 本身就绑定单轮 session，不适合跨请求复用。
    """

    # 同一验证码实例的 Init/Verify 域名由 prefix 决定；覆盖 prefix 时必须同步，
    # 否则会用 A 实例的域名发送 B 实例的 SceneId。
    init_url = f"https://{request.prefix}.captcha-open.aliyuncs.com/"
    verify_url = f"https://{request.prefix}-verify.captcha-open.aliyuncs.com/"

    client = AliSliderClient(
        scene_id=request.scene_id,
        prefix=request.prefix,
        init_url=init_url,
        verify_url=verify_url,
        node_binary=settings.node_binary,
        vision_python=settings.vision_python,
        timeout=settings.timeout,
        proxies=request.proxies,
        rpc_key_id=request.rpc_key_id,
    )
    try:
        # 视觉解释器的冷导入与 DeviceToken、Init、资源下载重叠。
        client._prewarm_vision()
        runtime = DeviceRuntimeClient(
            node_binary=settings.node_binary,
            sdk_path=settings.sdk_js,
            prefix=request.prefix,
            region="cn",
            timeout=settings.timeout,
            gather_cost_range=settings.gather_cost_range,
            first_touch_age_range=settings.first_touch_age_range,
            proxies=request.proxies,
        )
        started = time.monotonic()
        _, _, _, _, verify = client.run_captcha(
            runtime,
            minimum_confidence=settings.minimum_confidence,
        )
        elapsed_ms = int((time.monotonic() - started) * 1000)
    finally:
        client.close()

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

    def _send_json(self, status: int, body: dict[str, Any]) -> None:
        payload = json.dumps(
            body,
            ensure_ascii=False,
            separators=(",", ":"),
        ).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def _read_json_body(self) -> dict[str, Any]:
        try:
            length = int(self.headers.get("Content-Length", "0"))
        except ValueError as exc:
            raise ApiRequestError("Content-Length 无效") from exc
        if length < 0 or length > _MAX_BODY_BYTES:
            raise ApiRequestError(
                f"请求体必须位于 0..{_MAX_BODY_BYTES} 字节"
            )
        if length == 0:
            return {}
        raw = self.rfile.read(length)
        try:
            payload = json.loads(raw.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise ApiRequestError("请求体不是合法 JSON") from exc
        if not isinstance(payload, dict):
            raise ApiRequestError("请求体必须是 JSON object")
        return payload

    @staticmethod
    def _query_payload(query: str) -> dict[str, Any]:
        return {
            key: values[-1]
            for key, values in parse_qs(query, keep_blank_values=True).items()
        }

    def _handle_solve(self, payload: dict[str, Any]) -> None:
        try:
            request = parse_solve_request(payload)
        except ApiRequestError as exc:
            self._send_json(
                400,
                {
                    "ok": False,
                    "errorType": "ApiRequestError",
                    "error": str(exc),
                },
            )
            return

        # 默认不限制并发；仅在显式设置上限时拒绝超出的请求，避免排队让调用方
        # 一起超时。每轮挑战本身仍是互相独立的 CertifyId，不共享会话或 worker。
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
            body = solve_once(request, self.settings)
        except (
            AliSliderError,
            DeviceRuntimeError,
            PeRuntimeError,
            ValueError,
        ) as exc:
            self._send_json(
                500,
                {
                    "ok": False,
                    "errorType": type(exc).__name__,
                    "error": str(exc),
                },
            )
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

    def do_POST(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler 约定。
        path = urlsplit(self.path).path
        if path != SOLVE_PATH:
            self._send_json(404, {"ok": False, "error": "未知路径"})
            return
        try:
            payload = self._read_json_body()
        except ApiRequestError as exc:
            self._send_json(
                400,
                {
                    "ok": False,
                    "errorType": "ApiRequestError",
                    "error": str(exc),
                },
            )
            return
        self._handle_solve(payload)

    def do_GET(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler 约定。
        parts = urlsplit(self.path)
        if parts.path == HEALTH_PATH:
            self._send_json(200, {"ok": True, "status": "ready"})
            return
        if parts.path != SOLVE_PATH:
            self._send_json(404, {"ok": False, "error": "未知路径"})
            return
        # GET 只是 POST 的便捷等价物，便于 curl 快速验证。
        self._handle_solve(self._query_payload(parts.query))

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
        prog="python -m ali_slider_reverse.api",
        description="阿里 V3 滑块纯协议 HTTP 接口",
    )
    parser.add_argument("--host", default=DEFAULT_HOST, help="监听地址")
    parser.add_argument(
        "--port",
        type=int,
        default=DEFAULT_PORT,
        help=f"监听端口（默认 {DEFAULT_PORT}）",
    )
    parser.add_argument(
        "--max-concurrency",
        type=int,
        default=0,
        help="同时进行的挑战数上限（默认 0 表示不限制）",
    )
    parser.add_argument(
        "--node",
        default="node",
        help="Node 可执行文件（默认 node）",
    )
    parser.add_argument(
        "--vision-python",
        default="/opt/homebrew/bin/python3",
        help="带 OpenCV/NumPy 的 Python（默认 /opt/homebrew/bin/python3）",
    )
    parser.add_argument(
        "--sdk-js",
        help="可选：本地公开 AliyunCaptcha.js；省略则从官方 CDN 下载",
    )
    parser.add_argument(
        "--timeout",
        type=float,
        default=25.0,
        help="单步网络/子进程超时秒数（默认 25）",
    )
    parser.add_argument(
        "--min-confidence",
        type=float,
        default=0.45,
        help="低于此图像置信度时停止且不发 Verify（默认 0.45）",
    )
    parser.add_argument(
        "--gather-cost-min",
        type=int,
        default=180,
        help="Node 过快时 GatherCost 正常化下界（默认 180）",
    )
    parser.add_argument(
        "--gather-cost-max",
        type=int,
        default=260,
        help="Node 过快时 GatherCost 正常化上界（默认 260）",
    )
    parser.add_argument(
        "--first-touch-age-min",
        type=int,
        default=DEFAULT_FIRST_TOUCH_AGE_RANGE[0],
        help=(
            "首 touch 相对 TrackStartTime 的下界毫秒"
            f"（默认 {DEFAULT_FIRST_TOUCH_AGE_RANGE[0]}）"
        ),
    )
    parser.add_argument(
        "--first-touch-age-max",
        type=int,
        default=DEFAULT_FIRST_TOUCH_AGE_RANGE[1],
        help=(
            "首 touch 相对 TrackStartTime 的上界毫秒"
            f"（默认 {DEFAULT_FIRST_TOUCH_AGE_RANGE[1]}）"
        ),
    )
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    if args.max_concurrency < 0:
        raise SystemExit("--max-concurrency 必须 >= 0")
    if not 0.0 <= args.min_confidence <= 1.0:
        raise SystemExit("--min-confidence 必须位于 0..1")

    SliderApiHandler.settings = RuntimeSettings(
        node_binary=args.node,
        vision_python=args.vision_python,
        sdk_js=args.sdk_js,
        timeout=args.timeout,
        minimum_confidence=args.min_confidence,
        gather_cost_range=(args.gather_cost_min, args.gather_cost_max),
        first_touch_age_range=(
            args.first_touch_age_min,
            args.first_touch_age_max,
        ),
    )
    SliderApiHandler.slots = (
        threading.BoundedSemaphore(args.max_concurrency)
        if args.max_concurrency > 0
        else _Unlimited()
    )

    server = ThreadingHTTPServer((args.host, args.port), SliderApiHandler)
    server.daemon_threads = True
    host, port = server.server_address[:2]
    limit_label = (
        f"并发上限 {args.max_concurrency}"
        if args.max_concurrency > 0
        else "不限并发"
    )
    print(
        f"滑块接口已启动：http://{host}:{port}{SOLVE_PATH}（{limit_label}）",
        file=sys.stderr,
    )
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print("正在停止……", file=sys.stderr)
    finally:
        server.server_close()
    return 0


__all__ = [
    "ApiRequestError",
    "HEALTH_PATH",
    "RuntimeSettings",
    "SOLVE_PATH",
    "SliderApiHandler",
    "SolveRequest",
    "build_parser",
    "main",
    "parse_solve_request",
    "solve_once",
]


if __name__ == "__main__":
    raise SystemExit(main())
