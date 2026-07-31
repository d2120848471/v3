"""阿里 V3 滑块的端到端纯协议客户端。

主流程由 Python 完成；当前 FeiLin 设备环境以及动态 PE 的真实 payload builder
通过受控 Node VM 执行。所有敏感会话材料只在内存中流转，不写入证据文件。
"""

from __future__ import annotations

import hashlib
import json
import os
import re
import struct
import subprocess
import sys
import tempfile
import time
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any
from urllib.parse import urlparse, urlsplit, urlunsplit

from .device_runtime import DeviceRuntimeClient, DeviceRuntimeResult
from .frontend_profile import resolve_frontend_secrets
from .image_solver import pe_slide_pos_from_puzzle_x
from .pe_runtime import PeRuntimeClient
from .protocol import (
    build_business_captcha_verify_param,
    build_business_signed_query,
    build_verify_captcha_param,
    compact_json,
    js_form_urlencode,
    rpc_v1_signature,
    utc_timestamp,
    uuid4_hex_nonce,
    uuid4_nonce,
)
from .track import rescale_recorded_touch_track


CAPTCHA_API_VERSION = "2023-03-05"
DEFAULT_SCENE_ID = "1ug4aptr"
DEFAULT_PREFIX = "fsgtmi"
DEFAULT_INIT_URL = "https://fsgtmi.captcha-open.aliyuncs.com/"
DEFAULT_VERIFY_URL = "https://fsgtmi-verify.captcha-open.aliyuncs.com/"
DEFAULT_UPLOAD_URL = "https://upload.captcha-open.aliyuncs.com/"
DEFAULT_IMAGE_BASE = "https://static-captcha.aliyuncs.com/"
DEFAULT_PE_BASE = "https://g.alicdn.com/captcha-frontend/dynamicJS/"
DEFAULT_ORIGIN = "http://localhost:38185"
DEFAULT_REFERER = DEFAULT_ORIGIN + "/"
DEFAULT_USER_AGENT = (
    "Mozilla/5.0 (iPhone; CPU iPhone OS 18_5 like Mac OS X) "
    "AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.5 "
    "Mobile/15E148 Safari/604.1"
)
DEFAULT_SEC_CH_UA = (
    '"Not;A=Brand";v="8", "Chromium";v="150", '
    '"Google Chrome";v="150"'
)
BUSINESS_SALT_SHA256 = (
    "f97a59184f21e111608dac2e6397f2cf07866638e5df657937e98f36279615d6"
)
_STATIC_PATH_RE = re.compile(r"^[A-Za-z0-9._/-]+$")
_PUBLIC_ASSET_PATH_RE = re.compile(r"^[A-Za-z0-9._/-]+$")
_PNG_SIGNATURE = b"\x89PNG\r\n\x1a\n"
_VERIFY_FUTURE_SKEW_LIMIT_MS = 2_000
_SUPPORTED_PROXY_SCHEMES = frozenset(
    {"http", "https", "socks4", "socks5", "socks5h"}
)


class AliSliderError(RuntimeError):
    """协议状态、响应或本地运行时不满足闭环要求。"""


@dataclass(frozen=True, slots=True)
class CaptchaChallenge:
    certify_id: str = field(repr=False)
    image_path: str
    puzzle_image_path: str
    static_path: str
    captcha_type: str
    request_id: str = field(repr=False)
    raw: dict[str, Any] = field(repr=False)
    init_started_ms: int
    init_finished_ms: int


@dataclass(frozen=True, slots=True)
class ChallengeAssets:
    background: Path
    shadow: Path
    pe_script: Path
    stylesheet: Path
    load_timings: dict[str, tuple[int, int]] = field(repr=False)
    stylesheet_loaded: bool


@dataclass(frozen=True, slots=True)
class VisionResult:
    x_pos: int
    slide_pos: int
    confidence: float
    candidates: tuple[dict[str, Any], ...]


@dataclass(frozen=True, slots=True)
class VerifyBuild:
    data: str = field(repr=False)
    x_pos: int = field(repr=False)
    slide_pos: int = field(repr=False)
    track_start_time: int = field(repr=False)
    verify_time: int = field(repr=False)
    track_event_count: int
    native_mousemove_event_count: int
    post_getter_mousemove_event_count: int
    target_first_touch_age_ms: int = field(repr=False)
    first_touch_age_ms: float = field(repr=False)
    touch_duration_ms: float = field(repr=False)
    last_touch_to_verify_ms: float = field(repr=False)
    post_interaction_delay_ms: float = field(repr=False)
    upload_log_succeeded: bool
    device_getter_arguments: tuple[str, ...] = field(repr=False)
    feilin_interaction_events: tuple[
        dict[str, float | bool | str],
        ...,
    ] = field(repr=False)


@dataclass(frozen=True, slots=True)
class CaptchaVerifyResult:
    verify_code: str
    verify_result: bool
    security_token: str = field(repr=False)
    certify_id: str = field(repr=False)
    request_id: str = field(repr=False)
    raw: dict[str, Any] = field(repr=False)

    @property
    def succeeded(self) -> bool:
        return self.verify_code == "T001" and self.verify_result


@dataclass(frozen=True, slots=True)
class BusinessRequestTemplate:
    base_url: str
    body: dict[str, Any] = field(repr=False)
    raw_token: str = field(repr=False)
    headers: dict[str, str] = field(repr=False)
    cookie: str = field(repr=False)


@dataclass(frozen=True, slots=True)
class BusinessResponse:
    status_code: int
    body: Any = field(repr=False)


def _requests_module() -> Any:
    try:
        import requests
    except ImportError as exc:  # pragma: no cover - 由调用环境决定。
        raise AliSliderError("协议客户端需要 requests") from exc
    return requests


def normalize_proxies(proxy: str | None) -> dict[str, str] | None:
    """把单个代理串规范化为 requests 的 ``proxies`` 映射。

    整轮挑战的 Init/Verify/UploadLog、公开资源下载与公开 SDK 下载共用同一出口
    地址，避免同一 ``CertifyId`` 的请求出现在多个源 IP 上。传入 ``None`` 或空串
    表示直连，此时返回 ``None``，调用方不设置任何代理。
    """

    if proxy is None:
        return None
    if not isinstance(proxy, str):
        raise ValueError("proxy 必须是字符串")
    value = proxy.strip()
    if not value:
        return None
    # 允许省略 scheme 的 host:port 写法，按 requests 的默认语义补 http://。
    if "://" not in value:
        value = "http://" + value
    parsed = urlsplit(value)
    if parsed.scheme not in _SUPPORTED_PROXY_SCHEMES:
        raise ValueError(
            "proxy scheme 必须是 "
            + "/".join(sorted(_SUPPORTED_PROXY_SCHEMES))
        )
    if not parsed.hostname:
        raise ValueError("proxy 缺少主机名")
    if parsed.port is not None and not 1 <= parsed.port <= 65535:
        raise ValueError("proxy 端口必须位于 1..65535")
    return {"http": value, "https": value}


def _require_relative_asset_path(value: Any, *, label: str) -> str:
    if not isinstance(value, str) or not value:
        raise AliSliderError(f"{label} 缺失")
    if (
        not _PUBLIC_ASSET_PATH_RE.fullmatch(value)
        or value.startswith("/")
        or ".." in value.split("/")
    ):
        raise AliSliderError(f"{label} 不是安全的相对资源路径")
    return value


def _png_dimensions(path: str | Path, *, label: str) -> tuple[int, int]:
    """只读 PNG IHDR，避免主 Python 环境额外依赖 Pillow/OpenCV。"""

    source = Path(path)
    try:
        with source.open("rb") as stream:
            header = stream.read(24)
    except OSError as exc:
        raise AliSliderError(f"无法读取 {label}") from exc
    if (
        len(header) != 24
        or header[:8] != _PNG_SIGNATURE
        or header[12:16] != b"IHDR"
    ):
        raise AliSliderError(f"{label} 不是有效 PNG")
    width, height = struct.unpack(">II", header[16:24])
    if width <= 0 or height <= 0 or width > 16_384 or height > 16_384:
        raise AliSliderError(f"{label} 尺寸异常")
    return width, height


def extract_business_sign_salt(bundle_path: str | Path) -> str:
    """从当前公开 app bundle 提取业务 SHA-512 尾串。

    当前版本用已离线复算的 SHA-256 锁定目标字面量，避免把相邻字符串误识别为
    签名盐，也避免把二次提取的明文常量复制进 Python 源码。
    """

    source = Path(bundle_path).read_text(encoding="utf-8")
    # 先按任意短字面量成对匹配，再做长度过滤；若直接把最小长度写进正则，
    # 相邻的短字符串会被错误地从前一个右引号跨到后一个左引号。
    for match in re.finditer(r"""(["'])([^"'\\\r\n]{0,256})\1""", source):
        candidate = match.group(2)
        if not 16 <= len(candidate) <= 64:
            continue
        digest = hashlib.sha256(candidate.encode("utf-8")).hexdigest()
        if digest == BUSINESS_SALT_SHA256:
            return candidate
    raise AliSliderError("未在当前 app bundle 找到已确认的业务签名尾串")


def load_business_template_from_capture(
    capture_path: str | Path,
) -> BusinessRequestTemplate:
    """从用户本地原始抓包读取一次 gopay 请求模板。"""

    document = json.loads(Path(capture_path).read_text(encoding="utf-8"))
    events = document.get("postSlider", {}).get("events", [])
    request_event: dict[str, Any] | None = None
    for event in events:
        if event.get("method") != "Network.requestWillBeSent":
            continue
        request = event.get("params", {}).get("request", {})
        if "/mall-toc/order/gopay" in str(request.get("url", "")):
            request_event = event
            break
    if request_event is None:
        raise AliSliderError("抓包中没有 gopay 请求")

    params = request_event["params"]
    request = params["request"]
    request_id = params.get("requestId")
    parsed_url = urlsplit(request["url"])
    if parsed_url.hostname not in {"localhost", "127.0.0.1"}:
        raise AliSliderError("业务模板不是本地 CTF 地址")
    base_url = urlunsplit(
        (parsed_url.scheme, parsed_url.netloc, parsed_url.path, "", "")
    )
    try:
        body = json.loads(request["postData"])
    except (KeyError, json.JSONDecodeError) as exc:
        raise AliSliderError("gopay 模板 body 不是 JSON") from exc
    if not isinstance(body, dict):
        raise AliSliderError("gopay 模板 body 顶层必须是 object")

    raw_headers = request.get("headers", {})
    if not isinstance(raw_headers, dict):
        raw_headers = {}
    normalized_headers = {
        str(key).lower(): str(value)
        for key, value in raw_headers.items()
    }
    authorization = normalized_headers.get("authorization", "")
    if not authorization.startswith("Bearer "):
        raise AliSliderError("gopay 模板缺少 Bearer Authorization")
    raw_token = authorization[7:]

    cookie = ""
    extra_headers: dict[str, str] = {}
    for event in events:
        if event.get("method") != "Network.requestWillBeSentExtraInfo":
            continue
        event_params = event.get("params", {})
        if event_params.get("requestId") != request_id:
            continue
        values = event_params.get("headers", {})
        if isinstance(values, dict):
            extra_headers = {
                str(key).lower(): str(value)
                for key, value in values.items()
            }
            cookie = extra_headers.get("cookie", "")
        break
    merged_headers = dict(extra_headers)
    merged_headers.update(normalized_headers)
    canonical_names = {
        "accept": "Accept",
        "accept-language": "Accept-Language",
        "authorization": "Authorization",
        "clientid": "clientid",
        "content-type": "Content-Type",
        "origin": "Origin",
        "referer": "Referer",
        "user-agent": "User-Agent",
    }
    headers = {
        canonical: merged_headers[key]
        for key, canonical in canonical_names.items()
        if key in merged_headers
    }
    return BusinessRequestTemplate(
        base_url=base_url,
        body=body,
        raw_token=raw_token,
        headers=headers,
        cookie=cookie,
    )


class AliSliderClient:
    """按前端状态机执行一轮 Init → Verify → 可选业务提交。"""

    def __init__(
        self,
        *,
        scene_id: str = DEFAULT_SCENE_ID,
        prefix: str = DEFAULT_PREFIX,
        init_url: str = DEFAULT_INIT_URL,
        verify_url: str = DEFAULT_VERIFY_URL,
        upload_url: str = DEFAULT_UPLOAD_URL,
        image_base: str = DEFAULT_IMAGE_BASE,
        pe_base: str = DEFAULT_PE_BASE,
        node_binary: str = "node",
        vision_python: str | None = None,
        timeout: float = 20.0,
        user_agent: str = DEFAULT_USER_AGENT,
        referer: str = DEFAULT_REFERER,
        proxies: dict[str, str] | None = None,
        rpc_key_id: str | None = None,
    ) -> None:
        if not scene_id or not prefix:
            raise ValueError("scene_id 和 prefix 不能为空")
        if timeout <= 0:
            raise ValueError("timeout 必须为正数")
        if proxies is not None and not isinstance(proxies, dict):
            raise ValueError("proxies 必须是 requests 的映射或 None")
        if rpc_key_id is not None and not isinstance(rpc_key_id, str):
            raise ValueError("rpc_key_id 必须是字符串或 None")
        self.scene_id = scene_id
        self.prefix = prefix
        self.init_url = init_url
        self.verify_url = verify_url
        self.upload_url = upload_url
        self.image_base = image_base
        self.pe_base = pe_base
        self.node_binary = node_binary
        self.vision_python = vision_python or sys.executable
        self.timeout = float(timeout)
        self.user_agent = user_agent
        self.referer = referer
        referer_url = urlsplit(referer)
        if (
            referer_url.scheme not in {"http", "https"}
            or not referer_url.netloc
        ):
            raise ValueError("referer 必须是绝对 HTTP(S) URL")
        self.origin = urlunsplit(
            (referer_url.scheme, referer_url.netloc, "", "", "")
        )
        self._requests = _requests_module()
        self.proxies = dict(proxies) if proxies else None
        self.session = self._requests.Session()
        if self.proxies is not None:
            self.session.proxies.update(self.proxies)
        self.secrets = resolve_frontend_secrets()
        # 未显式覆盖时沿用公开前端密文恢复出的 RPC key id；不同验证码实例可以
        # 由调用方传入自己的 AaduaneId，但签名 secret 仍取自当前公开前端。
        self.rpc_key_id = rpc_key_id or self.secrets.main_rpc_key_id
        self.pe_runtime = PeRuntimeClient(
            node_binary=node_binary,
            prefix=prefix,
            region="cn",
            timeout=timeout,
        )
        self._vision_process: subprocess.Popen[str] | None = None

    @staticmethod
    def _vision_environment() -> dict[str, str]:
        package_root = Path(__file__).resolve().parent.parent
        environment = os.environ.copy()
        prior_pythonpath = environment.get("PYTHONPATH")
        environment["PYTHONPATH"] = (
            str(package_root)
            if not prior_pythonpath
            else str(package_root) + os.pathsep + prior_pythonpath
        )
        return environment

    def _prewarm_vision(self) -> None:
        """异步预载视觉解释器；图片就绪后由 :meth:`solve_assets` 消费。"""

        if (
            self._vision_process is not None
            and self._vision_process.poll() is None
        ):
            return
        self._close_vision()
        try:
            self._vision_process = subprocess.Popen(
                [
                    self.vision_python,
                    "-m",
                    "ali_slider_reverse.vision_bridge",
                    "--worker",
                ],
                stdin=subprocess.PIPE,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
                bufsize=1,
                env=self._vision_environment(),
            )
        except FileNotFoundError as exc:
            raise AliSliderError("OpenCV vision bridge 不可用") from exc

    def _close_vision(self) -> None:
        """回收尚未被识别请求消费的预热子进程。"""

        process = self._vision_process
        self._vision_process = None
        if process is None:
            return
        if process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=1.0)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
        for stream in (process.stdin, process.stdout, process.stderr):
            if stream is not None:
                stream.close()

    def close(self) -> None:
        """显式回收视觉子进程与 HTTP 会话。

        CLI 进程退出即可回收，但常驻 API 服务每轮都会新建一个客户端；异常路径下
        预热的 vision worker 不会被 :meth:`solve_assets` 消费，必须显式关闭。
        """

        self._close_vision()
        try:
            self.session.close()
        except Exception:  # pragma: no cover - 关闭已失效会话的兜底。
            pass

    def __del__(self) -> None:  # pragma: no cover - 异常退出时的兜底回收。
        try:
            self._close_vision()
        except Exception:
            pass

    def _browser_headers(
        self,
        *,
        destination: str = "empty",
        include_origin: bool = True,
        mode: str = "cors",
    ) -> dict[str, str]:
        headers = {
            "Accept": "*/*",
            "Accept-Language": "zh-CN,zh;q=0.9",
            "Referer": self.referer,
            "Sec-CH-UA": DEFAULT_SEC_CH_UA,
            "Sec-CH-UA-Mobile": "?1",
            "Sec-CH-UA-Platform": '"iOS"',
            "Sec-Fetch-Dest": destination,
            "Sec-Fetch-Mode": mode,
            "Sec-Fetch-Site": "cross-site",
            "Priority": "u=1, i",
            "User-Agent": self.user_agent,
        }
        if include_origin:
            headers["Origin"] = self.origin
        return headers

    def _rpc_params(
        self,
        action: str,
        extra: list[tuple[str, str]],
    ) -> dict[str, str]:
        params: dict[str, str] = {
            "AaduaneId": self.rpc_key_id,
            "SignatureMethod": "HMAC-SHA1",
            "SignatureVersion": "1.0",
            "Format": "JSON",
            "Timestamp": utc_timestamp(),
            "Version": CAPTCHA_API_VERSION,
            "Action": action,
        }
        for key, value in extra:
            params[key] = value
        params["SignatureNonce"] = uuid4_nonce()
        params["Signature"] = rpc_v1_signature(
            params,
            secret=self.secrets.main_rpc_key_secret,
        )
        return params

    def _post_rpc(
        self,
        url: str,
        params: dict[str, str],
    ) -> dict[str, Any]:
        try:
            response = self.session.post(
                url,
                data=js_form_urlencode(params),
                headers={
                    **self._browser_headers(),
                    "Content-Type":
                        "application/x-www-form-urlencoded; charset=UTF-8",
                },
                timeout=self.timeout,
            )
            response.raise_for_status()
        except Exception as exc:
            raise AliSliderError("验证码 RPC 请求失败") from exc
        try:
            payload = response.json()
        except ValueError as exc:
            raise AliSliderError("验证码 RPC 响应不是 JSON") from exc
        if not isinstance(payload, dict):
            raise AliSliderError("验证码 RPC 响应顶层不是 object")
        return payload

    def init_challenge(self, device_token: str) -> CaptchaChallenge:
        init_started_ms = int(time.time() * 1000)
        params = self._rpc_params(
            "InitCaptchaV3",
            [
                ("SceneId", self.scene_id),
                ("Language", "cn"),
                ("Mode", "popup"),
                ("DeviceToken", device_token),
            ],
        )
        payload = self._post_rpc(self.init_url, params)
        init_finished_ms = int(time.time() * 1000)
        if payload.get("Success") is not True or payload.get("Code") != "Success":
            raise AliSliderError(
                f"InitCaptchaV3 失败：{payload.get('Code')}"
            )
        static_path = _require_relative_asset_path(
            payload.get("StaticPath"),
            label="StaticPath",
        )
        if not _STATIC_PATH_RE.fullmatch(static_path):
            raise AliSliderError("StaticPath 字符集不合法")
        certify_id = payload.get("CertifyId")
        if not isinstance(certify_id, str) or not certify_id:
            raise AliSliderError("InitCaptchaV3 缺少 CertifyId")
        return CaptchaChallenge(
            certify_id=certify_id,
            image_path=_require_relative_asset_path(
                payload.get("Image"), label="Image"
            ),
            puzzle_image_path=_require_relative_asset_path(
                payload.get("PuzzleImage"), label="PuzzleImage"
            ),
            static_path=static_path,
            captcha_type=str(payload.get("CaptchaType", "")),
            request_id=str(payload.get("RequestId", "")),
            raw=payload,
            init_started_ms=init_started_ms,
            init_finished_ms=init_finished_ms,
        )

    def _download(
        self,
        url: str,
        destination: Path,
    ) -> tuple[int, int]:
        parsed = urlparse(url)
        if parsed.scheme != "https" or parsed.hostname not in {
            "static-captcha.aliyuncs.com",
            "g.alicdn.com",
        }:
            raise AliSliderError(f"拒绝下载非白名单资源：{url}")
        fetch_destination = {
            ".css": "style",
            ".js": "script",
        }.get(destination.suffix, "image")
        started_ms = int(time.time() * 1000)
        try:
            # requests.Session 不承诺跨线程共享；公开静态资源也不依赖
            # Init/Verify RPC 的 cookie，因此每个并发 worker 独占短会话。
            with self._requests.Session() as session:
                if self.proxies is not None:
                    session.proxies.update(self.proxies)
                response = session.get(
                    url,
                    headers=self._browser_headers(
                        destination=fetch_destination,
                        include_origin=False,
                        mode="no-cors",
                    ),
                    timeout=self.timeout,
                )
                response.raise_for_status()
                content = response.content
        except Exception as exc:
            raise AliSliderError("公开验证码资源下载失败") from exc
        try:
            destination.write_bytes(content)
        except OSError as exc:
            raise AliSliderError("公开验证码资源落盘失败") from exc
        return started_ms, int(time.time() * 1000)

    def download_assets(
        self,
        challenge: CaptchaChallenge,
        directory: str | Path,
    ) -> ChallengeAssets:
        target = Path(directory)
        target.mkdir(parents=True, exist_ok=True)
        background = target / "back.png"
        shadow = target / "shadow.png"
        pe_script = target / "pe.js"
        stylesheet = target / "main.css"
        static_version = challenge.static_path.split("/", 1)[0]
        if (
            not static_version
            or not _PUBLIC_ASSET_PATH_RE.fullmatch(static_version)
        ):
            raise AliSliderError("StaticPath 版本段不合法")
        css_path = static_version + "/main.css"

        downloads = {
            "img": (
                self.image_base + challenge.image_path,
                background,
            ),
            "pImg": (
                self.image_base + challenge.puzzle_image_path,
                shadow,
            ),
            "js": (
                self.pe_base + challenge.static_path + ".js",
                pe_script,
            ),
            "css": (
                self.pe_base + css_path,
                stylesheet,
            ),
        }
        # 四项公开资源互不依赖。退出 executor context 会在成功和异常路径
        # 等待全部 worker，避免临时目录回收后仍有后台写入。
        with ThreadPoolExecutor(
            max_workers=len(downloads),
            thread_name_prefix="ali-public-asset",
        ) as executor:
            futures = {
                name: executor.submit(self._download, url, destination)
                for name, (url, destination) in downloads.items()
            }

        # 固定必需资源的异常观察顺序，避免调度完成顺序改变主错误。
        background_timing = futures["img"].result()
        shadow_timing = futures["pImg"].result()
        pe_timing = futures["js"].result()
        try:
            futures["css"].result()
        except AliSliderError:
            # 公开 SDK 的 CSS loader 独立于 JS/图片 ready Promise；失败只
            # 上报 networkError，不阻断动态 PE 初始化或后续 Verify。
            stylesheet_loaded = False
        else:
            stylesheet_loaded = True
        return ChallengeAssets(
            background=background,
            shadow=shadow,
            pe_script=pe_script,
            stylesheet=stylesheet,
            load_timings={
                "img": background_timing,
                "pImg": shadow_timing,
                "js": pe_timing,
            },
            stylesheet_loaded=stylesheet_loaded,
        )

    @staticmethod
    def _successful_load_log(
        timing: tuple[int, int],
        *,
        message: str,
    ) -> dict[str, int | bool | str]:
        if (
            not isinstance(timing, tuple)
            or len(timing) != 2
            or any(
                isinstance(value, bool)
                or not isinstance(value, int)
                or value < 1
                for value in timing
            )
            or timing[1] < timing[0]
        ):
            raise AliSliderError("资源加载计时无效")
        return {
            "t": timing[1],
            "s": True,
            "msg": message,
            "rt": timing[1] - timing[0],
        }

    def upload_initialization_log(
        self,
        challenge: CaptchaChallenge,
        assets: ChallengeAssets,
    ) -> bool:
        """按公开 SDK 的一次性 best-effort 语义发送 ``UploadLog``。

        真实浏览器在动态 PE 的图片 ready 回调中记录 ``img/pImg``，再由
        ``config.log("rt", ...)`` 触发上传。纯协议端没有 DOM 图片解码阶段，
        因此在同一批公开资源全部下载完成后构造等价 ready 日志；CSS 会下载，
        但与公开 SDK 一样不进入 logInfo，也不参与 ready 门控。
        """

        try:
            timings = assets.load_timings
            required = {"img", "pImg", "js"}
            if set(timings) != required:
                raise AliSliderError("资源加载计时字段不完整")
            init_timing = (
                challenge.init_started_ms,
                challenge.init_finished_ms,
            )
            m_init = self._successful_load_log(
                init_timing,
                message="INIT_SUCCESS",
            )
            js_log = self._successful_load_log(
                timings["js"],
                message="DYNAMICJS_LOADED",
            )
            puzzle_log = self._successful_load_log(
                timings["pImg"],
                message="IMAGE_LOADED",
            )
            image_log = self._successful_load_log(
                timings["img"],
                message="IMAGE_LOADED",
            )
            ready_ms = max(
                timings["js"][1],
                timings["pImg"][1],
                timings["img"][1],
            )
            init_host = urlsplit(self.init_url).hostname or ""
            prefix_label = self.prefix + "."
            sdk_host = (
                init_host[len(prefix_label):]
                if init_host.startswith(prefix_label)
                else init_host
            )
            log_info = {
                "sId": self.scene_id,
                "pfx": self.prefix,
                "mInit": m_init,
                "hst": sdk_host,
                "cId": challenge.certify_id,
                "js": js_log,
                "pImg": puzzle_log,
                "img": image_log,
                "rt": max(
                    0,
                    ready_ms - challenge.init_started_ms,
                ),
            }
            params = self._rpc_params(
                "UploadLog",
                [("log", compact_json(log_info))],
            )
            self._post_rpc(self.upload_url, params)
        except Exception:
            # 公开 SDK 不 await UploadLog，且无论同步异常与否都立刻标记
            # logUploaded；遥测的构造、落盘或网络失败均不能改变本轮
            # PE/Verify 控制流，且不得把原异常内容带入日志。
            return False
        return True

    def solve_assets(
        self,
        assets: ChallengeAssets,
        *,
        x_pos_override: int | None = None,
        slider_width: int = 300,
        handle_width: int = 40,
    ) -> VisionResult:
        if x_pos_override is None:
            process = self._vision_process
            self._vision_process = None
            try:
                if process is None:
                    completed = subprocess.run(
                        [
                            self.vision_python,
                            "-m",
                            "ali_slider_reverse.vision_bridge",
                            "--background",
                            str(assets.background),
                            "--shadow",
                            str(assets.shadow),
                        ],
                        capture_output=True,
                        text=True,
                        check=True,
                        timeout=self.timeout,
                        env=self._vision_environment(),
                    )
                else:
                    request = json.dumps(
                        {
                            "background": str(assets.background),
                            "shadow": str(assets.shadow),
                        },
                        ensure_ascii=False,
                        separators=(",", ":"),
                    ) + "\n"
                    try:
                        stdout, stderr = process.communicate(
                            request,
                            timeout=self.timeout,
                        )
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.communicate()
                        raise
                    completed = subprocess.CompletedProcess(
                        args=process.args,
                        returncode=process.returncode,
                        stdout=stdout,
                        stderr=stderr,
                    )
                    completed.check_returncode()
            except (FileNotFoundError, subprocess.TimeoutExpired) as exc:
                raise AliSliderError("OpenCV vision bridge 不可用") from exc
            except subprocess.CalledProcessError as exc:
                detail = exc.stderr.strip().splitlines()[-1:]
                raise AliSliderError(
                    "OpenCV 缺口求解失败："
                    + (detail[0][:500] if detail else "未知错误")
                ) from exc
            try:
                output = json.loads(completed.stdout.splitlines()[-1])
                x_pos = int(output["xPos"])
                confidence = float(output["confidence"])
                candidates = tuple(output.get("candidates", []))
            except (IndexError, KeyError, TypeError, ValueError) as exc:
                raise AliSliderError("vision bridge 输出无效") from exc
        else:
            if isinstance(x_pos_override, bool) or x_pos_override < 0:
                raise ValueError("x_pos_override 必须是非负整数")
            x_pos = int(x_pos_override)
            confidence = 1.0
            candidates = ()

        slide_pos = int(
            pe_slide_pos_from_puzzle_x(
                x_pos,
                rendered_width=slider_width,
                handle_width=handle_width,
                rounded=True,
            )
        )
        return VisionResult(
            x_pos=x_pos,
            slide_pos=slide_pos,
            confidence=confidence,
            candidates=candidates,
        )

    @staticmethod
    def _recorded_samples(
        fixture_path: str | Path,
    ) -> tuple[list[dict[str, float | str]], float]:
        fixture = json.loads(Path(fixture_path).read_text(encoding="utf-8"))
        columns = fixture.get("columns")
        rows = fixture.get("track")
        if not isinstance(columns, list) or not isinstance(rows, list):
            raise AliSliderError("人工轨迹 fixture schema 无效")
        indexes = {str(name): index for index, name in enumerate(columns)}
        for field in ("type", "t", "x", "y"):
            if field not in indexes:
                raise AliSliderError(f"人工轨迹缺少列 {field}")
        samples = [
            {
                "type": str(row[indexes["type"]]),
                "t": float(row[indexes["t"]]),
                "x": float(row[indexes["x"]]),
                "y": float(row[indexes["y"]]),
            }
            for row in rows
        ]
        source_distance = float(
            fixture.get("image", {}).get(
                "finalSliderLeft",
                samples[-1]["x"],
            )
        )
        return samples, source_distance

    def build_verify_data(
        self,
        challenge: CaptchaChallenge,
        assets: ChallengeAssets,
        vision: VisionResult,
        *,
        sdk_path: str | Path,
        verify_arg_key: str,
        verify_arg_plaintext: str,
        device_token: str,
        device_config: dict[str, Any],
        init_begin_time: int,
        target_first_touch_age_ms: int,
        upload_log_succeeded: bool,
        fixture_path: str | Path | None = None,
    ) -> VerifyBuild:
        if not isinstance(upload_log_succeeded, bool):
            raise ValueError("upload_log_succeeded 必须是 bool")
        path = (
            Path(__file__).with_name("default_touch_track.json")
            if fixture_path is None
            else Path(fixture_path)
        )
        recorded, source_distance = self._recorded_samples(path)
        relative = rescale_recorded_touch_track(
            recorded,
            target_distance=vision.slide_pos,
            source_distance=source_distance,
        )
        for event, source in zip(relative, recorded, strict=True):
            event["type"] = str(source["type"])
        image_width, image_height = _png_dimensions(
            assets.background,
            label="back.png",
        )
        puzzle_width, puzzle_height = _png_dimensions(
            assets.shadow,
            label="shadow.png",
        )
        native = self.pe_runtime.build(
            sdk_path=sdk_path,
            pe_path=assets.pe_script,
            scene_id=self.scene_id,
            certify_id=challenge.certify_id,
            device_token=device_token,
            captcha_type=challenge.captcha_type or "PUZZLE",
            image=challenge.image_path,
            puzzle_image=challenge.puzzle_image_path,
            verify_arg_profile={
                "accessSec": verify_arg_key,
                "sessionIdSalt": verify_arg_plaintext,
            },
            device_config=device_config,
            dimensions={
                "imageWidth": image_width,
                "imageHeight": image_height,
                "puzzleWidth": puzzle_width,
                "puzzleHeight": puzzle_height,
                "renderedWidth": 300,
                "handleWidth": 40,
            },
            track=relative,
            expected_x_pos=vision.x_pos,
            init_begin_time=init_begin_time,
            first_touch_age_ms=target_first_touch_age_ms,
        )
        if native.slide_pos != vision.slide_pos:
            raise AliSliderError(
                "动态 PE slidePos 与目标轨迹不一致："
                f"{native.slide_pos} != {vision.slide_pos}"
            )
        if len(native.device_getter_calls) != 1:
            raise AliSliderError(
                "动态 PE 的 FeiLin getter 调用次数异常"
            )
        getter_call = native.device_getter_calls[0]
        if (
            getter_call.argument_count != 1
            or len(getter_call.arguments) != 1
        ):
            raise AliSliderError(
                "动态 PE 的 FeiLin getter 参数数量异常"
            )
        getter_argument = getter_call.arguments[0]
        if (
            getter_argument.value_type != "string"
            or not isinstance(getter_argument.value, str)
            or not getter_argument.value
            or getter_argument.length != len(getter_argument.value)
        ):
            raise AliSliderError(
                "动态 PE 的 FeiLin getter 参数形状异常"
            )
        return VerifyBuild(
            data=native.data,
            x_pos=native.x_pos,
            slide_pos=native.slide_pos,
            track_start_time=native.track_start_time,
            verify_time=native.verify_time,
            track_event_count=native.track_event_count,
            native_mousemove_event_count=(
                native.native_mousemove_event_count
            ),
            post_getter_mousemove_event_count=(
                native.post_getter_mousemove_event_count
            ),
            target_first_touch_age_ms=(
                native.target_first_touch_age_ms
            ),
            first_touch_age_ms=native.first_touch_age_ms,
            touch_duration_ms=native.touch_duration_ms,
            last_touch_to_verify_ms=native.last_touch_to_verify_ms,
            post_interaction_delay_ms=(
                native.post_interaction_delay_ms
            ),
            upload_log_succeeded=upload_log_succeeded,
            device_getter_arguments=(getter_argument.value,),
            feilin_interaction_events=native.mousemove_events,
        )

    def verify_challenge(
        self,
        challenge: CaptchaChallenge,
        verify_token: str,
        build: VerifyBuild,
    ) -> CaptchaVerifyResult:
        if (
            build.verify_time
            > int(time.time() * 1000) + _VERIFY_FUTURE_SKEW_LIMIT_MS
        ):
            raise AliSliderError(
                "VerifyTime 超出当前逻辑时钟新鲜度，"
                "已停止且未发送 Verify"
            )
        verify_param = build_verify_captcha_param(
            scene_id=self.scene_id,
            certify_id=challenge.certify_id,
            device_token=verify_token,
            data=build.data,
        )
        params = self._rpc_params(
            "VerifyCaptchaV3",
            [
                ("SceneId", self.scene_id),
                ("CertifyId", challenge.certify_id),
                ("CaptchaVerifyParam", verify_param),
            ],
        )
        payload = self._post_rpc(self.verify_url, params)
        result = payload.get("Result")
        if not isinstance(result, dict):
            result = {}
        return CaptchaVerifyResult(
            verify_code=str(result.get("VerifyCode", payload.get("Code", ""))),
            verify_result=result.get("VerifyResult") is True,
            security_token=str(result.get("securityToken", "")),
            certify_id=str(result.get("certifyId", challenge.certify_id)),
            request_id=str(payload.get("RequestId", "")),
            raw=payload,
        )

    def submit_business(
        self,
        template: BusinessRequestTemplate,
        verify: CaptchaVerifyResult,
        *,
        app_bundle: str | Path,
    ) -> BusinessResponse:
        if not verify.succeeded or not verify.security_token:
            raise AliSliderError("只有 T001 才能提交业务请求")
        body = dict(template.body)
        body["captchaVerifyParam"] = build_business_captcha_verify_param(
            certify_id=verify.certify_id,
            scene_id=self.scene_id,
            security_token=verify.security_token,
            is_sign=True,
        )
        body_text = compact_json(body)
        lgtime = int(time.time() * 1000)
        _, query = build_business_signed_query(
            body_text=body_text,
            raw_token=template.raw_token,
            salt=extract_business_sign_salt(app_bundle),
            lgtime=lgtime,
            lgnonce=uuid4_hex_nonce(),
        )
        headers = dict(template.headers)
        headers["Content-Type"] = "application/json"
        if template.cookie:
            headers["Cookie"] = template.cookie
        try:
            response = self.session.post(
                template.base_url + "?" + query,
                data=body_text.encode("utf-8"),
                headers=headers,
                timeout=self.timeout,
            )
        except Exception as exc:
            raise AliSliderError("本地业务请求失败") from exc
        try:
            response_body: Any = response.json()
        except ValueError:
            response_body = response.text
        return BusinessResponse(
            status_code=response.status_code,
            body=response_body,
        )

    def run_captcha(
        self,
        device_runtime: DeviceRuntimeClient,
        *,
        artifacts_dir: str | Path | None = None,
        fixture_path: str | Path | None = None,
        x_pos_override: int | None = None,
        minimum_confidence: float = 0.45,
    ) -> tuple[
        DeviceRuntimeResult,
        CaptchaChallenge,
        VisionResult,
        VerifyBuild,
        CaptchaVerifyResult,
    ]:
        """执行一轮挑战；每个 CertifyId 严格只发一次 Verify。"""

        if not 0.0 <= minimum_confidence <= 1.0:
            raise ValueError("minimum_confidence 必须位于 0..1")

        with device_runtime.challenge_session() as device_session:
            # Init 使用 Log1/Log2 产生的首 token；Node VM 保持存活，直到
            # 原生 PE 轨迹构造完成并观测到实际 getter 参数后再刷新 token。
            init_begin_time = int(time.time() * 1000)
            challenge = self.init_challenge(device_session.init_token)
            init_begin_time = int(
                getattr(
                    challenge,
                    "init_started_ms",
                    init_begin_time,
                )
            )
            initial_device = device_session.initial_result
            target_first_touch_age_ms = (
                device_session.target_first_touch_age_ms
            )

            def execute(
                directory: str | Path,
            ) -> tuple[
                DeviceRuntimeResult,
                VisionResult,
                VerifyBuild,
                CaptchaVerifyResult,
            ]:
                assets = self.download_assets(challenge, directory)
                # 公开 SDK 不 await UploadLog；这里让 best-effort 遥测与本地
                # 图像识别重叠，但在 PE/Verify 使用主 RPC session 前完成回收。
                with ThreadPoolExecutor(
                    max_workers=1,
                    thread_name_prefix="ali-upload-log",
                ) as executor:
                    upload_future = executor.submit(
                        self.upload_initialization_log,
                        challenge,
                        assets,
                    )
                    vision = self.solve_assets(
                        assets,
                        x_pos_override=x_pos_override,
                    )
                    if vision.confidence < minimum_confidence:
                        raise AliSliderError(
                            "缺口置信度不足，已停止且未发送 Verify："
                            f"{vision.confidence:.3f} "
                            f"< {minimum_confidence:.3f}"
                        )
                upload_log_succeeded = upload_future.result()
                # 首 touch 的门控发生在 PE slider ready 后、真正 dispatch 前；
                # Node 启动与 SDK/PE 初始化耗时不会叠加到目标年龄。
                build = self.build_verify_data(
                    challenge,
                    assets,
                    vision,
                    sdk_path=device_session.sdk_path,
                    verify_arg_key=initial_device.verify_arg_key,
                    verify_arg_plaintext=(
                        initial_device.verify_arg_plaintext
                    ),
                    device_token=initial_device.init_token,
                    device_config=initial_device.device_config.as_dict(),
                    init_begin_time=init_begin_time,
                    target_first_touch_age_ms=(
                        target_first_touch_age_ms
                    ),
                    upload_log_succeeded=upload_log_succeeded,
                    fixture_path=fixture_path,
                )
                device = device_session.complete_challenge(
                    build.device_getter_arguments,
                    build.feilin_interaction_events,
                    # PE 事件时间为非负毫秒；使用与前端 Math.round
                    # 一致的 half-up，而不是 Python 的 bankers round。
                    post_interaction_delay_ms=int(
                        build.post_interaction_delay_ms + 0.5
                    ),
                )
                if (
                    device.getter_argument_count
                    != len(build.device_getter_arguments)
                ):
                    raise AliSliderError(
                        "Verify DeviceToken 未按动态 PE getter 参数刷新"
                    )
                verify = self.verify_challenge(
                    challenge,
                    device.verify_token,
                    build,
                )
                return device, vision, build, verify

            if artifacts_dir is None:
                with tempfile.TemporaryDirectory(
                    prefix="ali-slider-challenge-"
                ) as directory:
                    device, vision, build, verify = execute(directory)
            else:
                device, vision, build, verify = execute(artifacts_dir)

        return device, challenge, vision, build, verify


__all__ = [
    "AliSliderClient",
    "AliSliderError",
    "BusinessRequestTemplate",
    "BusinessResponse",
    "CaptchaChallenge",
    "CaptchaVerifyResult",
    "ChallengeAssets",
    "VerifyBuild",
    "VisionResult",
    "extract_business_sign_salt",
    "load_business_template_from_capture",
]
