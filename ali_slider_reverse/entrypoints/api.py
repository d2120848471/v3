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

每轮挑战使用独立的客户端、FeiLin worker、临时目录与 ``CertifyId``，只共享线程
安全的底层连接池与 best-effort 执行器；单次 Verify 的边界仍然逐轮成立。
"""

from __future__ import annotations

import argparse
import json
import secrets
import select
import socket
import sys
import threading
import time
import webbrowser
from collections import deque
from collections.abc import Callable
from concurrent.futures import Executor, ThreadPoolExecutor
from dataclasses import dataclass
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any
from urllib.parse import parse_qs, urlsplit

from .. import __version__, config
from ..challenge.device_pool import DeviceSessionPool, MAX_VMS_PER_NODE
from ..challenge.session import normalize_proxies
from ..challenge.transport import SharedHttpAdapterPool, warm_connections
from ..device_profile import DeviceProfile, generate_device_profile
from ..errors import AliSliderError, ApiRequestError
from .options import (
    RuntimeSettings,
    add_confidence_argument,
    add_runtime_arguments,
    add_server_arguments,
    effective_server_concurrency,
    fast_mode_enabled,
)

_SCENE_ID_MAX_LENGTH = 64
_RPC_KEY_ID_MAX_LENGTH = 128
_PREFIX_MAX_LENGTH = 32
_DOCS_PATH = "/docs"
_OPENAPI_PATH = "/openapi.json"
_OUTPUT_LOCK = threading.Lock()

_STEP_LABELS = {
    "setup": "准备运行环境",
    "devicePoolWait": "等待预热会话",
    "deviceSession": "取得设备会话",
    "init": "InitCaptchaV3",
    "downloadAssets": "下载挑战资源",
    "vision": "识别缺口",
    "buildVerifyData": "生成 Verify 数据",
    "completeDevice": "刷新设备 Token",
    "verify": "VerifyCaptchaV3",
    "uploadLog": "读取 UploadLog",
    "clientCleanup": "回收本轮资源",
    "total": "本轮总耗时",
}


_DOCS_HTML = """<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <title>AliSlider API 接口文档</title>
  <style>
    :root { color-scheme: light; font-family: Inter, "Segoe UI", sans-serif; }
    body { margin: 0; background: #f6f8fb; color: #172033; }
    main { max-width: 960px; margin: 36px auto; padding: 0 20px 48px; }
    h1 { margin-bottom: 6px; }
    .muted { color: #64748b; }
    .card { background: #fff; border: 1px solid #dbe2ea; border-radius: 12px;
      box-shadow: 0 8px 30px rgba(15, 23, 42, .06); margin-top: 20px;
      padding: 22px; }
    .route { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
    .method { background: #16a34a; color: #fff; border-radius: 5px;
      font-weight: 700; padding: 5px 10px; }
    code, pre { font-family: "Cascadia Code", Consolas, monospace; }
    form { display: grid; grid-template-columns: 1fr 1fr; gap: 14px;
      margin-top: 20px; }
    label { display: grid; gap: 6px; font-size: 14px; font-weight: 600; }
    input { border: 1px solid #cbd5e1; border-radius: 7px; padding: 10px;
      font: inherit; }
    .wide { grid-column: 1 / -1; }
    button { background: #2563eb; border: 0; border-radius: 7px; color: white;
      cursor: pointer; font-weight: 700; padding: 11px 18px; width: fit-content; }
    button:disabled { cursor: wait; opacity: .65; }
    pre { background: #0f172a; border-radius: 8px; color: #e2e8f0;
      min-height: 110px; overflow: auto; padding: 16px; white-space: pre-wrap; }
    .links a { margin-right: 18px; }
    @media (max-width: 680px) { form { grid-template-columns: 1fr; }
      .wide { grid-column: auto; } }
  </style>
</head>
<body>
<main>
  <h1>AliSlider API</h1>
  <div class="muted">本地 HTTP 接口文档·快速模式使用共享连接池与常驻设备会话。</div>
  <div class="card">
    <div class="route"><span class="method">POST</span>
      <strong><code>/api/slider</code></strong>
      <span class="muted">执行一轮滑块验证</span>
    </div>
    <form id="request-form">
      <label>SceneId<input name="SceneId" value="__DEFAULT_SCENE_ID__"></label>
      <label>prefix（可选）<input name="prefix" placeholder="使用服务默认值"></label>
      <label>AaduaneId（可选）<input name="AaduaneId"></label>
      <label>proxy（可选）<input name="proxy"
        placeholder="http://127.0.0.1:7890"></label>
      <button type="submit" class="wide">发送请求</button>
    </form>
    <h3>完整响应</h3>
    <div class="muted">timingsMs 给出每个步骤耗时；慢机器调用端读超时建议至少 90 秒。</div>
    <pre id="response">点击“发送请求”后，这里会显示 HTTP 状态码和完整 JSON 返回。</pre>
  </div>
  <div class="card links">
    <strong>其他地址</strong><br><br>
    <a href="/health" target="_blank">健康检查</a>
    <a href="/openapi.json" target="_blank">OpenAPI JSON</a>
  </div>
</main>
<script>
const form = document.getElementById("request-form");
const output = document.getElementById("response");
form.addEventListener("submit", async (event) => {
  event.preventDefault();
  const button = form.querySelector("button");
  const body = {};
  for (const [key, value] of new FormData(form).entries()) {
    if (String(value).trim()) body[key] = String(value).trim();
  }
  button.disabled = true;
  button.textContent = "请求中…";
  output.textContent = "正在执行挑战…";
  try {
    const response = await fetch("/api/slider", {
      method: "POST",
      headers: {"Content-Type": "application/json"},
      body: JSON.stringify(body)
    });
    const text = await response.text();
    let rendered = text;
    try { rendered = JSON.stringify(JSON.parse(text), null, 2); } catch (_) {}
    output.textContent = `HTTP ${response.status}\n\n${rendered}`;
  } catch (error) {
    output.textContent = `请求失败\n\n${error}`;
  } finally {
    button.disabled = false;
    button.textContent = "发送请求";
  }
});
</script>
</body>
</html>
""".replace("__DEFAULT_SCENE_ID__", config.DEFAULT_SCENE_ID)


def _openapi_document() -> dict[str, Any]:
    """返回文档页和其他工具可直接读取的最小 OpenAPI 合同。"""

    optional_text = {"type": "string", "nullable": True}
    return {
        "openapi": "3.0.3",
        "info": {"title": "AliSlider API", "version": __version__},
        "paths": {
            config.API_SOLVE_PATH: {
                "post": {
                    "summary": "执行一轮滑块验证",
                    "requestBody": {
                        "required": False,
                        "content": {
                            "application/json": {
                                "schema": {
                                    "type": "object",
                                    "properties": {
                                        "SceneId": {
                                            "type": "string",
                                            "default": config.DEFAULT_SCENE_ID,
                                        },
                                        "prefix": optional_text,
                                        "AaduaneId": optional_text,
                                        "proxy": optional_text,
                                    },
                                }
                            }
                        },
                    },
                    "responses": {
                        "200": {
                            "description": "完整验证结果（含分步骤耗时）",
                            "content": {
                                "application/json": {
                                    "schema": {
                                        "type": "object",
                                        "properties": {
                                            "elapsedMs": {
                                                "type": "integer",
                                                "minimum": 0,
                                            },
                                            "timingsMs": {
                                                "type": "object",
                                                "additionalProperties": {
                                                    "type": "integer",
                                                    "minimum": 0,
                                                },
                                            },
                                            "traceId": {"type": "string"},
                                        },
                                    }
                                }
                            },
                        },
                        "400": {"description": "请求参数无效"},
                        "429": {
                            "description": "超出并发上限；按返回值退避",
                            "headers": {
                                "Retry-After": {
                                    "description": "建议等待秒数",
                                    "schema": {
                                        "type": "integer",
                                        "minimum": 1,
                                    },
                                }
                            },
                        },
                        "500": {"description": "协议或运行错误"},
                    },
                }
            },
            config.API_HEALTH_PATH: {
                "get": {
                    "summary": "健康检查",
                    "responses": {"200": {"description": "ready"}},
                }
            },
        },
    }


def _startup_warm_urls() -> tuple[str, ...]:
    """返回快速模式需要预建连接的主机；关闭的遥测不建连。"""

    urls = [
        config.init_url(),
        config.verify_url(),
        config.IMAGE_BASE,
        config.PE_BASE,
    ]
    if config.UPLOAD_LOG_ENABLED:
        urls.append(config.UPLOAD_URL)
    return tuple(urls)


def _print_full_result(
    method: str, path: str, status: int, body: dict[str, Any]
) -> None:
    """把每次接口请求的完整 JSON 返回打到服务窗口。"""

    if path == config.API_HEALTH_PATH:
        return
    stamp = time.strftime("%Y-%m-%d %H:%M:%S")
    rendered = json.dumps(body, ensure_ascii=False, separators=(",", ":"))
    with _OUTPUT_LOCK:
        print(
            f"[{stamp}] {method} {path} {status}\n{rendered}",
            file=sys.stderr,
        )


def _print_step_result(trace_id: str, name: str, elapsed_ms: int) -> None:
    """按 traceId 原子打印阶段耗时，避免 5 线程日志互相串行。"""

    stamp = time.strftime("%Y-%m-%d %H:%M:%S")
    label = _STEP_LABELS.get(name, name)
    with _OUTPUT_LOCK:
        print(
            f"[{stamp}] [{trace_id}] {label}: {elapsed_ms} ms",
            file=sys.stderr,
        )


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
    *,
    device_profile: DeviceProfile | None = None,
    transport_pool: SharedHttpAdapterPool | None = None,
    upload_executor: Executor | None = None,
    timing_hook: Callable[[str, int], None] | None = None,
    cancel_check: Callable[[], None] | None = None,
) -> dict[str, Any]:
    """执行一轮完整挑战并返回响应体。

    每次调用都新建客户端与设备运行时：``proxy``、``SceneId`` 与 ``AaduaneId``
    逐轮不同，而 FeiLin worker 本身就绑定单轮 session，不适合跨请求复用。

    ``pool`` 只改变 FeiLin 会话是"现建"还是"请求前已备好"，不改变本轮只用一个
    会话、一个 ``CertifyId`` 的语义；未开启时行为与之前完全一致。
    """

    timings_ms: dict[str, int] = {}
    overall_started = time.monotonic()

    def report_step(name: str, elapsed_ms: int) -> None:
        timings_ms[name] = max(0, int(elapsed_ms))
        if timing_hook is not None:
            try:
                timing_hook(name, timings_ms[name])
            except Exception:
                # 展示回调不参与协议控制流。
                pass

    client: Any | None = None
    try:
        if cancel_check is not None:
            cancel_check()
        setup_started = time.monotonic()
        try:
            # 标准模式逐轮生成；快速模式由服务入口传入本进程固定画像。无论哪种
            # 模式，同一轮 HTTP 头、FeiLin 指纹与动态 PE 环境共用同一个对象。
            device_profile = device_profile or generate_device_profile()
            client = settings.build_client(
                scene_id=request.scene_id,
                prefix=request.prefix,
                proxies=request.proxies,
                rpc_key_id=request.rpc_key_id,
                device_profile=device_profile,
                adapter=(
                    transport_pool.get(request.proxies)
                    if transport_pool is not None
                    else None
                ),
                upload_executor=upload_executor,
            )
            device_runtime = settings.build_device_runtime(
                prefix=request.prefix,
                proxies=request.proxies,
                device_profile=device_profile,
            )
            # 视觉解释器的冷导入与 DeviceToken、Init、资源下载重叠；五个出网
            # 主机的 TLS 握手与设备链重叠。
            client.prewarm_vision()
            client.prewarm_connections()
        finally:
            report_step(
                "setup", int((time.monotonic() - setup_started) * 1000)
            )

        if cancel_check is not None:
            cancel_check()
        if pool is None:
            report_step("devicePoolWait", 0)
            outcome = client.run_captcha(
                device_runtime,
                minimum_confidence=settings.minimum_confidence,
                timing_hook=report_step,
                cancel_check=cancel_check,
            )
        else:
            pool_wait_started = time.monotonic()
            pool_wait_reported = False
            try:
                with pool.lease(
                    device_runtime, cancel_check=cancel_check
                ) as leased:
                    report_step(
                        "devicePoolWait",
                        int(
                            (time.monotonic() - pool_wait_started) * 1000
                        ),
                    )
                    pool_wait_reported = True
                    if cancel_check is not None:
                        cancel_check()
                    outcome = client.run_captcha(
                        leased,
                        minimum_confidence=settings.minimum_confidence,
                        timing_hook=report_step,
                        cancel_check=cancel_check,
                    )
            finally:
                if not pool_wait_reported:
                    report_step(
                        "devicePoolWait",
                        int(
                            (time.monotonic() - pool_wait_started) * 1000
                        ),
                    )
    finally:
        cleanup_started = time.monotonic()
        try:
            if client is not None:
                client.close()
        finally:
            report_step(
                "clientCleanup",
                int((time.monotonic() - cleanup_started) * 1000),
            )
            report_step(
                "total", int((time.monotonic() - overall_started) * 1000)
            )

    verify = outcome.verify
    return {
        "ok": verify.succeeded,
        "securityToken": verify.security_token,
        "VerifyCode": verify.verify_code,
        "VerifyResult": verify.verify_result,
        "certifyId": verify.certify_id,
        "sceneId": request.scene_id,
        "proxied": request.proxies is not None,
        "elapsedMs": timings_ms["total"],
        "timingsMs": timings_ms,
    }


class _ChallengeSlots:
    """有界并发闸门，并根据近期耗时给 429 生成可执行的退避信息。"""

    def __init__(self, limit: int) -> None:
        if isinstance(limit, bool) or not isinstance(limit, int) or limit < 0:
            raise ValueError("并发上限必须是非负整数")
        self.limit = limit
        self._semaphore = (
            threading.BoundedSemaphore(limit) if limit > 0 else None
        )
        self._lock = threading.Lock()
        self._active_started: dict[int, float] = {}
        self._durations_ms: deque[int] = deque(maxlen=32)

    def acquire(self, blocking: bool = True) -> bool:
        if self._semaphore is not None and not self._semaphore.acquire(
            blocking=blocking
        ):
            return False
        with self._lock:
            self._active_started[threading.get_ident()] = time.monotonic()
        return True

    def release(self, *, elapsed_ms: int | None = None) -> None:
        with self._lock:
            self._active_started.pop(threading.get_ident(), None)
            if elapsed_ms is not None:
                self._durations_ms.append(max(0, int(elapsed_ms)))
        if self._semaphore is not None:
            self._semaphore.release()

    def overload_info(self, *, fallback_ms: int) -> dict[str, int]:
        """返回活动数、下一次重试时间和调用端总超时建议。"""

        now = time.monotonic()
        fallback_ms = max(1_000, int(fallback_ms))
        with self._lock:
            active_started = tuple(self._active_started.values())
            durations = sorted(self._durations_ms)

        if durations:
            # 样本少时最大值比虚假的精确分位数更稳；最多只保留最近 32 轮。
            baseline_ms = durations[-1]
        else:
            baseline_ms = fallback_ms
        remaining = [
            max(1_000, baseline_ms - int((now - started) * 1000))
            for started in active_started
        ]
        retry_after_ms = min(remaining, default=baseline_ms)
        recommended_timeout_ms = max(
            fallback_ms * 2,
            int(baseline_ms * 1.5),
            30_000,
        )
        return {
            "activeChallenges": len(active_started),
            "maxConcurrency": self.limit,
            "retryAfterMs": retry_after_ms,
            "recommendedClientTimeoutMs": recommended_timeout_ms,
        }


class _Unlimited(_ChallengeSlots):
    """不限并发时仍记录活动请求，保持处理逻辑一致。"""

    def __init__(self) -> None:
        super().__init__(0)


class _ClientDisconnected(Exception):
    """HTTP 调用端已放弃等待；本轮在下一个安全阶段边界停止。"""


def _connection_closed(connection: Any) -> bool:
    """非阻塞检测明文 HTTP socket 是否收到 FIN/RST。"""

    try:
        readable, _, _ = select.select((connection,), (), (), 0)
        if not readable:
            return False
        return connection.recv(1, socket.MSG_PEEK) == b""
    except (BlockingIOError, InterruptedError):
        return False
    except OSError:
        return True


class SliderApiHandler(BaseHTTPRequestHandler):
    """提供滑块接口、健康检查和自包含的本地接口文档。"""

    protocol_version = "HTTP/1.1"
    server_version = "AliSliderApi"
    sys_version = ""

    settings: RuntimeSettings = RuntimeSettings()
    slots: Any = _Unlimited()
    device_pool: DeviceSessionPool | None = None
    transport_pool: SharedHttpAdapterPool | None = None
    upload_executor: Executor | None = None
    device_profile: DeviceProfile | None = None

    result_hook: Callable[[str, str, int, dict[str, Any]], None] | None = None
    """可选的宿主回调，每次响应发出后以 ``(method, path, status, body)`` 调用。

    给桌面启动器一类的宿主一个观察本轮结果的口子。回调内的异常一律吞掉：展示
    逻辑绝不能影响已经发出的协议响应。

    赋值请用**可调用对象**或 ``staticmethod``：这是类属性，直接挂一个普通函数会
    被描述符协议当成方法绑定，第一个实参会变成 handler 自己。
    """

    progress_hook: Callable[[str, str, int], None] | None = None
    """可选阶段回调，参数为 ``(trace_id, 阶段名, 耗时毫秒)``。"""

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
        if parts.path == "/":
            self._send_redirect(_DOCS_PATH)
            return
        if parts.path == _DOCS_PATH:
            self._send_bytes(
                200,
                _DOCS_HTML.encode("utf-8"),
                content_type="text/html; charset=utf-8",
            )
            return
        if parts.path == _OPENAPI_PATH:
            self._send_json(200, _openapi_document())
            return
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

        trace_id = secrets.token_hex(6)

        # 保留有界背压，避免调用端 15 秒超时后继续发包把慢机器压得更慢。429
        # 附带动态退避时间；调用端必须等待，不能立即循环重发。
        if not self.slots.acquire(blocking=False):
            fallback_ms = int(self.settings.timeout * 1000)
            overload_info = (
                self.slots.overload_info(fallback_ms=fallback_ms)
                if hasattr(self.slots, "overload_info")
                else {
                    "activeChallenges": 0,
                    "maxConcurrency": 0,
                    "retryAfterMs": fallback_ms,
                    "recommendedClientTimeoutMs": fallback_ms * 2,
                }
            )
            retry_after_seconds = max(
                1, (overload_info["retryAfterMs"] + 999) // 1000
            )
            self._send_json(
                429,
                {
                    "ok": False,
                    "errorType": "TooManyChallenges",
                    "error": (
                        "并发已满，旧请求仍在服务端运行；请按 retryAfterMs "
                        "退避，不要在 15 秒超时后立即重发"
                    ),
                    "traceId": trace_id,
                    **overload_info,
                },
                headers={
                    "Retry-After": str(retry_after_seconds),
                    "X-Trace-ID": trace_id,
                },
            )
            return

        request_started = time.monotonic()
        timings_ms: dict[str, int] = {}

        def report_step(name: str, elapsed_ms: int) -> None:
            timings_ms[name] = elapsed_ms
            hook = self.progress_hook
            if hook is not None:
                try:
                    hook(trace_id, name, elapsed_ms)
                except Exception:
                    pass

        def ensure_client_connected() -> None:
            if _connection_closed(self.connection):
                raise _ClientDisconnected

        try:
            body = solve_once(
                request,
                self.settings,
                self.device_pool,
                device_profile=self.device_profile,
                transport_pool=self.transport_pool,
                upload_executor=self.upload_executor,
                timing_hook=report_step,
                cancel_check=ensure_client_connected,
            )
            body["traceId"] = trace_id
        except _ClientDisconnected:
            # 客户端已经放弃结果，不再发送错误响应；run_captcha 已在安全阶段边界
            # 停止，finally 会立即释放并发槽。
            return
        except (AliSliderError, ValueError) as exc:
            self._send_error(
                500,
                exc,
                extra={"traceId": trace_id, "timingsMs": timings_ms},
                headers={"X-Trace-ID": trace_id},
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
                    "traceId": trace_id,
                    "timingsMs": timings_ms,
                },
                headers={"X-Trace-ID": trace_id},
            )
            return
        finally:
            elapsed_ms = int((time.monotonic() - request_started) * 1000)
            try:
                self.slots.release(elapsed_ms=elapsed_ms)
            except TypeError:  # 兼容自定义的标准 Semaphore。
                self.slots.release()

        # 验证码未通过也是一次完整、正常的协议往返，用 200 承载 ok=false。
        self._send_json(
            200, body, headers={"X-Trace-ID": trace_id}
        )

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

    def _send_json(
        self,
        status: int,
        body: dict[str, Any],
        *,
        headers: dict[str, str] | None = None,
    ) -> None:
        payload = json.dumps(
            body, ensure_ascii=False, separators=(",", ":")
        ).encode("utf-8")
        self._send_bytes(
            status,
            payload,
            content_type="application/json; charset=utf-8",
            headers=headers,
        )

        hook = self.result_hook
        if hook is not None:
            try:
                hook(self.command, urlsplit(self.path).path, status, body)
            except Exception:  # pragma: no cover - 展示失败不影响协议响应。
                pass

    def _send_bytes(
        self,
        status: int,
        payload: bytes,
        *,
        content_type: str,
        headers: dict[str, str] | None = None,
    ) -> None:
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(payload)))
        self.send_header("Cache-Control", "no-store")
        for name, value in (headers or {}).items():
            self.send_header(name, value)
        self.end_headers()
        self.wfile.write(payload)

    def _send_redirect(self, location: str) -> None:
        self.send_response(302)
        self.send_header("Location", location)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def _send_error(
        self,
        status: int,
        exc: Exception,
        *,
        extra: dict[str, Any] | None = None,
        headers: dict[str, str] | None = None,
    ) -> None:
        self._send_json(
            status,
            {
                "ok": False,
                "errorType": type(exc).__name__,
                "error": str(exc),
                **(extra or {}),
            },
            headers=headers,
        )

    def log_message(self, format: str, *args: Any) -> None:
        """请求结果由 ``result_hook`` 连同完整 JSON 统一输出。"""

        return None


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="python -m ali_slider_reverse.entrypoints.api",
        description="阿里 V3 滑块纯协议 HTTP 接口",
    )
    add_server_arguments(parser)
    add_runtime_arguments(parser)
    add_confidence_argument(parser)
    parser.add_argument(
        "--open-docs",
        action="store_true",
        help="启动后用默认浏览器打开本地接口文档",
    )
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    if args.max_concurrency < 0:
        raise SystemExit("--max-concurrency 必须 >= 0")
    if not 1 <= args.vms_per_node <= MAX_VMS_PER_NODE:
        raise SystemExit(
            f"--vms-per-node 必须位于 1..{MAX_VMS_PER_NODE}"
        )
    if not 0.0 <= args.min_confidence <= 1.0:
        raise SystemExit("--min-confidence 必须位于 0..1")

    settings = RuntimeSettings.from_args(args)
    fast_mode = fast_mode_enabled(args)
    concurrency = effective_server_concurrency(args)
    fixed_profile = generate_device_profile() if fast_mode else None

    SliderApiHandler.settings = settings
    SliderApiHandler.slots = _ChallengeSlots(concurrency)
    SliderApiHandler.device_profile = fixed_profile
    SliderApiHandler.result_hook = staticmethod(_print_full_result)
    SliderApiHandler.progress_hook = staticmethod(_print_step_result)
    transport_pool = (
        SharedHttpAdapterPool(
            pool_size=max(concurrency, 8),
            max_proxy_routes=max(concurrency * 4, 8),
        )
        if fast_mode
        else None
    )
    SliderApiHandler.transport_pool = transport_pool
    upload_executor = (
        ThreadPoolExecutor(
            max_workers=max(concurrency, 1),
            thread_name_prefix="ali-upload-log-service",
        )
        if fast_mode and config.UPLOAD_LOG_ENABLED
        else None
    )
    SliderApiHandler.upload_executor = upload_executor
    device_pool = (
        DeviceSessionPool(
            enabled=True,
            capacity=max(concurrency, 1),
            vms_per_node=args.vms_per_node,
        )
        if fast_mode
        else None
    )
    SliderApiHandler.device_pool = device_pool

    server = ThreadingHTTPServer((args.host, args.port), SliderApiHandler)
    server.daemon_threads = True
    host, port = server.server_address[:2]
    prewarmed = 0
    if device_pool is not None and fixed_profile is not None:
        prewarmed = device_pool.prime(
            settings.build_device_runtime(
                prefix=config.DEFAULT_PREFIX,
                proxies=None,
                device_profile=fixed_profile,
            )
        )
    if transport_pool is not None:
        # 设备池就绪后再按并发容量建立 TCP/TLS（不发 HTTP 字节），避免握手过早完成
        # 后在漫长的 Node prime 期间闲置失效；等开始监听时首批请求也能直接复用。
        warm_connections(
            transport_pool.get(None),
            _startup_warm_urls(),
            timeout=settings.timeout,
            connections_per_url=max(concurrency, 1),
            wait=True,
        )
    limit_label = (
        f"并发上限 {concurrency}"
        if concurrency > 0
        else "不限并发"
    )
    node_hosts = (
        max(concurrency, 1) + args.vms_per_node - 1
    ) // args.vms_per_node
    mode_label = (
        f"快速模式，预热 {prewarmed}/{max(concurrency, 1)} VM，"
        f"{node_hosts} 个 Node host（每个最多 {args.vms_per_node} VM）"
        if fast_mode
        else "标准模式"
    )
    display_host = "127.0.0.1" if host in {"0.0.0.0", "::"} else host
    base_url = f"http://{display_host}:{port}"
    docs_url = base_url + _DOCS_PATH
    print(
        f"滑块 API 已启动（{limit_label}，{mode_label}）\n"
        f"接口文档：{docs_url}\n"
        f"请求地址：{base_url}{config.API_SOLVE_PATH}\n"
        "接口请求的完整 JSON 返回会输出在本窗口；Ctrl+C 停止服务。",
        file=sys.stderr,
    )
    if args.open_docs:
        try:
            webbrowser.open(docs_url, new=2)
        except Exception:
            # 服务本身已就绪；服务器没有图形桌面时仍可复制上方链接。
            pass
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print("正在停止……", file=sys.stderr)
    finally:
        server.server_close()
        if device_pool is not None:
            device_pool.close()
        if upload_executor is not None:
            upload_executor.shutdown(wait=True)
        if transport_pool is not None:
            transport_pool.close()
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
