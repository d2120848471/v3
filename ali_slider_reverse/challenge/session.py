"""一轮挑战的编排：Init → 资源 → 识别 → PE → Verify → 可选业务。

## 控制流的硬约束

整条链路只有**一个** Verify 调用点，且每个 ``CertifyId`` 最多消耗一次：

```text
图片共识通过
  AND PE 的 schema / 坐标 / 时间通过
  AND FeiLin getter 与 session 通过
    → 发送一次 VerifyCaptchaV3
```

图片置信度不足、资源下载失败、schema 不符、时间异常或 getter 不一致，都必须在
网络 Verify **之前**停止。收到 F015 等失败码后也不能复用同一 ``CertifyId`` 去试
邻近坐标——那既不会成功，也是对服务端的滥用。

## 并发的边界

Init 之前拿不到本轮图片与 ``StaticPath``，无法安全预取。Init 之后能并行的只有
两类互相独立的工作：四项公开资源的下载，以及"只尝试一次的 UploadLog"与本地
OpenCV 识别。

UploadLog 是 best-effort 遥测，**不参与任何控制流**，公开 SDK 也不 await 它，
因此它只需要在本轮结束前被回收，不必挡住 PE。真正会挡住 PE 的只有图像置信度
这道闸。

还有一段容易被忽略的空窗：设备链（SDK 准备 → Node 启动 → Log1 → Log2）期间
Python 侧完全阻塞在读子进程输出上。本轮五个出网主机的 TLS 握手就在这里提前做掉
（见 :mod:`.transport`），其中 Verify 主机原本要等到整条链路的最后一步才第一次
握手。

所有 worker 都在本轮返回前回收，不留后台悬挂任务。单轮之内不并发多个挑战。多轮
可以同时进行，但每轮持有自己的客户端、FeiLin worker、临时目录与 ``CertifyId``，
轮与轮之间没有共享可变状态。
"""

from __future__ import annotations

import tempfile
import time
from concurrent.futures import Future, ThreadPoolExecutor
from contextlib import ExitStack
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any
from urllib.parse import urlsplit, urlunsplit

from .. import config
from ..errors import AliSliderError
from ..protocol.params import build_verify_captcha_param
from ..protocol.secrets import resolve_frontend_secrets
from ..protocol.signing import (
    compact_json,
    js_form_urlencode,
    rpc_v1_signature,
    utc_timestamp,
    uuid4_nonce,
)
from ..runtime.node_device import DeviceRuntimeClient, DeviceRuntimeResult
from ..runtime.node_pe import PeRuntimeClient
from ..runtime.vision import VisionResult, VisionWorker
from .assets import (
    AssetDownloader,
    ChallengeAssets,
    png_dimensions,
    require_relative_asset_path,
)
from .business import (
    BusinessRequestTemplate,
    BusinessResponse,
    submit_business,
)
from .track import load_scaled_touch_track
from .transport import build_pool_adapter, pooled_session, warm_connections


_SUPPORTED_PROXY_SCHEMES = frozenset(
    {"http", "https", "socks4", "socks5", "socks5h"}
)


# ==========================================================================
# 结果模型
# ==========================================================================


@dataclass(frozen=True, slots=True)
class CaptchaChallenge:
    """``InitCaptchaV3`` 返回的一轮挑战。"""

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
class VerifyBuild:
    """动态 PE 原生生成、并已通过全部校验的 Verify 载荷。"""

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

    device_getter_arguments: tuple[str, ...] = field(repr=False)
    feilin_interaction_events: tuple[dict[str, float | bool | str], ...] = field(
        repr=False
    )


@dataclass(frozen=True, slots=True)
class CaptchaVerifyResult:
    """``VerifyCaptchaV3`` 的结果。"""

    verify_code: str
    verify_result: bool
    security_token: str = field(repr=False)
    certify_id: str = field(repr=False)
    request_id: str = field(repr=False)
    raw: dict[str, Any] = field(repr=False)

    @property
    def succeeded(self) -> bool:
        """是否满足提交业务请求的硬门槛。"""

        return self.verify_code == "T001" and self.verify_result


@dataclass(frozen=True, slots=True)
class ChallengeOutcome:
    """一轮完整挑战的全部产出。"""

    device: DeviceRuntimeResult
    challenge: CaptchaChallenge
    vision: VisionResult
    build: VerifyBuild
    verify: CaptchaVerifyResult

    upload_log_succeeded: bool
    """best-effort 遥测是否送达。

    放在这一层而不是 :class:`VerifyBuild` 里：它与 PE 原生生成的 Verify 载荷毫无
    关系，只是本轮的一项记录，因此也不需要在 PE 开始前就有结果。
    """


# ==========================================================================
# 工具
# ==========================================================================


def normalize_proxies(proxy: str | None) -> dict[str, str] | None:
    """把单个代理串规范化为 requests 的 ``proxies`` 映射。

    整轮挑战的 Init/Verify/UploadLog、公开资源下载与 SDK 下载共用同一出口地址，
    避免同一 ``CertifyId`` 的请求出现在多个源 IP 上。``None`` 或空串表示直连。

    接受省略 scheme 的 ``host:port`` 写法，按 requests 的默认语义补 ``http://``。
    """

    if proxy is None:
        return None
    if not isinstance(proxy, str):
        raise ValueError("proxy 必须是字符串")
    value = proxy.strip()
    if not value:
        return None

    if "://" not in value:
        value = "http://" + value
    parsed = urlsplit(value)
    if parsed.scheme not in _SUPPORTED_PROXY_SCHEMES:
        raise ValueError(
            "proxy scheme 必须是 " + "/".join(sorted(_SUPPORTED_PROXY_SCHEMES))
        )
    if not parsed.hostname:
        raise ValueError("proxy 缺少主机名")
    if parsed.port is not None and not 1 <= parsed.port <= 65535:
        raise ValueError("proxy 端口必须位于 1..65535")
    return {"http": value, "https": value}


def _requests_module() -> Any:
    try:
        import requests
    except ImportError as exc:  # pragma: no cover - 由调用环境决定。
        raise AliSliderError("协议客户端需要 requests") from exc
    return requests


def _settled_flag(future: Future[bool], *, timeout: float) -> bool:
    """读取 best-effort 任务的结果，超时或异常一律记为失败。

    调用点必须选在"这个 worker 反正要被回收"之后——那时取结果不增加任何等待。
    """

    try:
        return bool(future.result(timeout=timeout))
    except Exception:  # pragma: no cover - 含超时；任务内部已吞掉全部异常。
        return False


# ==========================================================================
# 客户端
# ==========================================================================


class AliSliderClient:
    """按前端状态机执行一轮 Init → Verify → 可选业务提交。

    一个实例服务一轮挑战。常驻服务应当每轮新建并 :meth:`close`：``proxy``、
    ``SceneId`` 与 ``AaduaneId`` 逐轮不同，视觉 worker 也绑定单轮生命周期。
    """

    def __init__(
        self,
        *,
        scene_id: str = config.DEFAULT_SCENE_ID,
        prefix: str = config.DEFAULT_PREFIX,
        init_url: str | None = None,
        verify_url: str | None = None,
        upload_url: str = config.UPLOAD_URL,
        node_binary: str = config.DEFAULT_NODE_BINARY,
        vision_python: str | None = None,
        timeout: float = config.DEFAULT_TIMEOUT,
        referer: str = config.REFERER,
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

        referer_url = urlsplit(referer)
        if referer_url.scheme not in {"http", "https"} or not referer_url.netloc:
            raise ValueError("referer 必须是绝对 HTTP(S) URL")

        self.scene_id = scene_id
        self.prefix = prefix
        # Init 与 Verify 是同一实例的两个域名，默认由 prefix 推导以防错配。
        self.init_url = init_url or config.init_url(prefix)
        self.verify_url = verify_url or config.verify_url(prefix)
        self.upload_url = upload_url
        self.timeout = float(timeout)
        self.referer = referer
        self.origin = urlunsplit(
            (referer_url.scheme, referer_url.netloc, "", "", "")
        )
        self.proxies = dict(proxies) if proxies else None

        self._requests = _requests_module()
        # 本轮共享的连接池：Session 仍按线程隔离，但热连接集中在这个 adapter 上，
        # 预热出来的 TLS 连接才能被 Init/Verify/UploadLog 和四项资源下载共同复用。
        self._adapter = build_pool_adapter(self._requests)
        self._connections_warmed = False
        self.session = self._requests.Session()
        self.session.mount("https://", self._adapter)
        if self.proxies is not None:
            self.session.proxies.update(self.proxies)

        self.secrets = resolve_frontend_secrets()
        # 未显式覆盖时沿用公开前端恢复出的 RPC key id；不同验证码实例可以传入
        # 自己的 AaduaneId，但签名 secret 仍取自当前公开前端。
        self.rpc_key_id = rpc_key_id or self.secrets.main_rpc_key_id

        self.assets = AssetDownloader(
            requests_module=self._requests,
            timeout=self.timeout,
            proxies=self.proxies,
            referer=referer,
            adapter=self._adapter,
        )
        self.pe_runtime = PeRuntimeClient(
            node_binary=node_binary,
            prefix=prefix,
            region=config.DEFAULT_REGION,
            timeout=self.timeout,
        )
        self.vision = VisionWorker(
            python_executable=vision_python or config.DEFAULT_VISION_PYTHON,
            timeout=self.timeout,
        )

    # -- 生命周期 ---------------------------------------------------------

    def prewarm_vision(self) -> None:
        """提前启动视觉 worker，让 OpenCV 冷导入与网络往返重叠。

        建议在 Init 之前调用；图片就绪时 :meth:`solve_assets` 只需付热算法与一次
        本地 IPC 的成本。
        """

        self.vision.prewarm()

    def prewarm_connections(self) -> None:
        """提前建立本轮五个出网主机的 TLS 连接。

        建议在设备链开始前调用：那段时间 Python 只是阻塞在读 Node 输出上，正好
        用来把握手做掉。只建连接、不发 HTTP 字节，因此没有任何协议侧副作用。

        重复调用是安全的，每个客户端只预热一次；走代理时整体跳过（原因见
        :mod:`.transport`）。
        """

        if self._connections_warmed:
            return
        self._connections_warmed = True
        warm_connections(
            self._adapter,
            (
                self.init_url,
                self.verify_url,
                self.upload_url,
                config.IMAGE_BASE,
                config.PE_BASE,
            ),
            proxies=self.proxies,
            timeout=self.timeout,
        )

    def close(self) -> None:
        """回收视觉子进程、HTTP 会话与共享连接池。"""

        self.vision.close()
        try:
            self.session.close()
        except Exception:  # pragma: no cover - 关闭已失效会话的兜底。
            pass
        try:
            self._adapter.close()
        except Exception:  # pragma: no cover - 关闭已失效连接池的兜底。
            pass

    def __del__(self) -> None:  # pragma: no cover - 异常退出时的兜底回收。
        try:
            self.vision.close()
        except Exception:
            pass

    # -- RPC --------------------------------------------------------------

    def _rpc_params(
        self,
        action: str,
        extra: list[tuple[str, str]],
    ) -> dict[str, str]:
        """组装并签名一次 RPC 调用的参数。

        ``SignatureNonce`` 在业务参数之后、``Signature`` 之前加入——签名覆盖除
        自身以外的全部参数。
        """

        params: dict[str, str] = {
            "AaduaneId": self.rpc_key_id,
            "SignatureMethod": "HMAC-SHA1",
            "SignatureVersion": "1.0",
            "Format": "JSON",
            "Timestamp": utc_timestamp(),
            "Version": config.CAPTCHA_API_VERSION,
            "Action": action,
        }
        params.update(extra)
        params["SignatureNonce"] = uuid4_nonce()
        params["Signature"] = rpc_v1_signature(
            params, secret=self.secrets.main_rpc_key_secret
        )
        return params

    def _post_rpc(
        self,
        url: str,
        params: dict[str, str],
        *,
        session: Any | None = None,
        timeout: float | None = None,
    ) -> dict[str, Any]:
        """发送一次 RPC 并返回 JSON 对象。

        ``session`` 用于把 UploadLog 挪到独立会话上——它与 PE/Verify 并发在跑，
        不能和主 RPC 会话共用一个 ``requests.Session``。
        """

        try:
            response = (session or self.session).post(
                url,
                data=js_form_urlencode(params),
                headers={
                    **config.browser_headers(
                        referer=self.referer, origin=self.origin
                    ),
                    "Content-Type": (
                        "application/x-www-form-urlencoded; charset=UTF-8"
                    ),
                },
                timeout=self.timeout if timeout is None else timeout,
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

    # -- 各阶段 -----------------------------------------------------------

    def init_challenge(self, device_token: str) -> CaptchaChallenge:
        """发起 ``InitCaptchaV3``，取得本轮 CertifyId、图片与 StaticPath。"""

        init_started_ms = int(time.time() * 1000)
        payload = self._post_rpc(
            self.init_url,
            self._rpc_params(
                "InitCaptchaV3",
                [
                    ("SceneId", self.scene_id),
                    ("Language", "cn"),
                    ("Mode", "popup"),
                    ("DeviceToken", device_token),
                ],
            ),
        )
        init_finished_ms = int(time.time() * 1000)

        if payload.get("Success") is not True or payload.get("Code") != "Success":
            raise AliSliderError(f"InitCaptchaV3 失败：{payload.get('Code')}")
        certify_id = payload.get("CertifyId")
        if not isinstance(certify_id, str) or not certify_id:
            raise AliSliderError("InitCaptchaV3 缺少 CertifyId")

        return CaptchaChallenge(
            certify_id=certify_id,
            image_path=require_relative_asset_path(
                payload.get("Image"), label="Image"
            ),
            puzzle_image_path=require_relative_asset_path(
                payload.get("PuzzleImage"), label="PuzzleImage"
            ),
            static_path=require_relative_asset_path(
                payload.get("StaticPath"), label="StaticPath"
            ),
            captcha_type=str(payload.get("CaptchaType", "")),
            request_id=str(payload.get("RequestId", "")),
            raw=payload,
            init_started_ms=init_started_ms,
            init_finished_ms=init_finished_ms,
        )

    def download_assets(
        self,
        challenge: CaptchaChallenge,
        directory: str | Path,
    ) -> ChallengeAssets:
        """并发下载本轮的图片、动态 PE 与样式表。"""

        return self.assets.download(
            image_path=challenge.image_path,
            puzzle_image_path=challenge.puzzle_image_path,
            static_path=challenge.static_path,
            directory=directory,
        )

    def solve_assets(
        self,
        assets: ChallengeAssets,
        *,
        x_pos_override: int | None = None,
    ) -> VisionResult:
        """识别缺口位置并换算出手柄位移。"""

        return self.vision.solve(
            assets.background,
            assets.shadow,
            x_pos_override=x_pos_override,
        )

    @staticmethod
    def _load_log_entry(
        timing: tuple[int, int],
        *,
        message: str,
    ) -> dict[str, int | bool | str]:
        """构造一条资源加载遥测记录。"""

        if (
            not isinstance(timing, tuple)
            or len(timing) != 2
            or any(
                isinstance(value, bool) or not isinstance(value, int) or value < 1
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

        真实浏览器在动态 PE 的图片 ready 回调里记录 ``img``/``pImg``，再由
        ``config.log("rt", ...)`` 触发上传。纯协议端没有 DOM 图片解码阶段，因此
        在同批资源下载完成后构造等价的 ready 日志。CSS 会下载但不进 logInfo，
        与公开 SDK 一致。

        **任何失败都只返回 ``False``**：公开 SDK 不 await 这次上传，无论同步异常
        与否都立刻标记 logUploaded。遥测的构造、落盘或网络失败都不能改变本轮的
        PE/Verify 控制流，也不得把原异常内容带进日志。

        本方法在后台线程里与 PE 并发执行，因此走**独立 Session**（只共享线程安全
        的连接池），不碰主 RPC 会话。
        """

        try:
            timings = assets.load_timings
            if set(timings) != {"img", "pImg", "js"}:
                raise AliSliderError("资源加载计时字段不完整")

            ready_ms = max(timing[1] for timing in timings.values())
            init_host = urlsplit(self.init_url).hostname or ""
            prefix_label = self.prefix + "."
            sdk_host = (
                init_host[len(prefix_label) :]
                if init_host.startswith(prefix_label)
                else init_host
            )

            log_info = {
                "sId": self.scene_id,
                "pfx": self.prefix,
                "mInit": self._load_log_entry(
                    (challenge.init_started_ms, challenge.init_finished_ms),
                    message="INIT_SUCCESS",
                ),
                "hst": sdk_host,
                "cId": challenge.certify_id,
                "js": self._load_log_entry(
                    timings["js"], message="DYNAMICJS_LOADED"
                ),
                "pImg": self._load_log_entry(
                    timings["pImg"], message="IMAGE_LOADED"
                ),
                "img": self._load_log_entry(
                    timings["img"], message="IMAGE_LOADED"
                ),
                "rt": max(0, ready_ms - challenge.init_started_ms),
            }
            with pooled_session(
                self._requests, self._adapter, proxies=self.proxies
            ) as upload_session:
                self._post_rpc(
                    self.upload_url,
                    self._rpc_params(
                        "UploadLog", [("log", compact_json(log_info))]
                    ),
                    session=upload_session,
                    # 遥测不值得让本轮收尾时陪它等满主超时；上限压到 5 秒。
                    timeout=min(self.timeout, 5.0),
                )
        except Exception:
            return False
        return True

    def build_verify_data(
        self,
        challenge: CaptchaChallenge,
        assets: ChallengeAssets,
        vision: VisionResult,
        *,
        sdk_path: str | Path,
        device: DeviceRuntimeResult,
        init_begin_time: int,
        target_first_touch_age_ms: int,
        fixture_path: str | Path | None = None,
    ) -> VerifyBuild:
        """在隔离 Node VM 中让动态 PE 原生生成 data，并复核 getter 合同。

        除了 PE 桥自身的层层校验，这里额外确认两件事：PE 算出的 ``slidePos`` 与
        目标轨迹一致，以及 getter 恰好被调用一次、恰好收到一个非空字符串实参——
        后者是刷新 Verify DeviceToken 的唯一依据。
        """

        track = load_scaled_touch_track(
            target_distance=vision.slide_pos,
            fixture_path=fixture_path,
        )
        image_width, image_height = png_dimensions(
            assets.background, label="back.png"
        )
        puzzle_width, puzzle_height = png_dimensions(
            assets.shadow, label="shadow.png"
        )

        native = self.pe_runtime.build(
            sdk_path=sdk_path,
            pe_path=assets.pe_script,
            scene_id=self.scene_id,
            certify_id=challenge.certify_id,
            device_token=device.init_token,
            captcha_type=challenge.captcha_type or "PUZZLE",
            image=challenge.image_path,
            puzzle_image=challenge.puzzle_image_path,
            verify_arg_profile={
                "accessSec": device.verify_arg_key,
                "sessionIdSalt": device.verify_arg_plaintext,
            },
            device_config=device.device_config.as_dict(),
            dimensions={
                "imageWidth": image_width,
                "imageHeight": image_height,
                "puzzleWidth": puzzle_width,
                "puzzleHeight": puzzle_height,
                "renderedWidth": config.SLIDER_RENDERED_WIDTH,
                "handleWidth": config.SLIDER_HANDLE_WIDTH,
            },
            track=track,
            expected_x_pos=vision.x_pos,
            init_begin_time=init_begin_time,
            first_touch_age_ms=target_first_touch_age_ms,
        )

        if native.slide_pos != vision.slide_pos:
            raise AliSliderError(
                "动态 PE slidePos 与目标轨迹不一致："
                f"{native.slide_pos} != {vision.slide_pos}"
            )
        getter_argument = self._require_single_getter_argument(native)

        return VerifyBuild(
            data=native.data,
            x_pos=native.x_pos,
            slide_pos=native.slide_pos,
            track_start_time=native.track_start_time,
            verify_time=native.verify_time,
            track_event_count=native.track_event_count,
            native_mousemove_event_count=native.native_mousemove_event_count,
            post_getter_mousemove_event_count=(
                native.post_getter_mousemove_event_count
            ),
            target_first_touch_age_ms=native.target_first_touch_age_ms,
            first_touch_age_ms=native.first_touch_age_ms,
            touch_duration_ms=native.touch_duration_ms,
            last_touch_to_verify_ms=native.last_touch_to_verify_ms,
            post_interaction_delay_ms=native.post_interaction_delay_ms,
            device_getter_arguments=(getter_argument,),
            feilin_interaction_events=native.mousemove_events,
        )

    @staticmethod
    def _require_single_getter_argument(native: Any) -> str:
        """确认 PE 恰好调用一次 getter 且恰好传入一个非空字符串。"""

        if len(native.device_getter_calls) != 1:
            raise AliSliderError("动态 PE 的 FeiLin getter 调用次数异常")
        call = native.device_getter_calls[0]
        if call.argument_count != 1 or len(call.arguments) != 1:
            raise AliSliderError("动态 PE 的 FeiLin getter 参数数量异常")

        argument = call.arguments[0]
        if (
            argument.value_type != "string"
            or not isinstance(argument.value, str)
            or not argument.value
            or argument.length != len(argument.value)
        ):
            raise AliSliderError("动态 PE 的 FeiLin getter 参数形状异常")
        return argument.value

    def verify_challenge(
        self,
        challenge: CaptchaChallenge,
        verify_token: str,
        build: VerifyBuild,
    ) -> CaptchaVerifyResult:
        """发送本轮唯一的一次 ``VerifyCaptchaV3``。

        发送前最后一道闸：虚拟时钟快速回放可能生成"未来的 VerifyTime"，超出容差
        就停止，不消耗这次挑战。
        """

        if (
            build.verify_time
            > int(time.time() * 1000) + config.VERIFY_FUTURE_SKEW_LIMIT_MS
        ):
            raise AliSliderError(
                "VerifyTime 超出当前逻辑时钟新鲜度，已停止且未发送 Verify"
            )

        verify_param = build_verify_captcha_param(
            scene_id=self.scene_id,
            certify_id=challenge.certify_id,
            device_token=verify_token,
            data=build.data,
        )
        payload = self._post_rpc(
            self.verify_url,
            self._rpc_params(
                "VerifyCaptchaV3",
                [
                    ("SceneId", self.scene_id),
                    ("CertifyId", challenge.certify_id),
                    ("CaptchaVerifyParam", verify_param),
                ],
            ),
        )

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
        """在 T001 门禁通过后提交一次业务请求。"""

        if not verify.succeeded or not verify.security_token:
            raise AliSliderError("只有 T001 才能提交业务请求")
        return submit_business(
            self.session,
            template,
            certify_id=verify.certify_id,
            scene_id=self.scene_id,
            security_token=verify.security_token,
            app_bundle=app_bundle,
            timeout=self.timeout,
        )

    # -- 完整流程 ---------------------------------------------------------

    def run_captcha(
        self,
        device_runtime: DeviceRuntimeClient,
        *,
        artifacts_dir: str | Path | None = None,
        fixture_path: str | Path | None = None,
        x_pos_override: int | None = None,
        minimum_confidence: float = config.DEFAULT_MIN_CONFIDENCE,
    ) -> ChallengeOutcome:
        """执行一轮挑战；每个 CertifyId 严格只发一次 Verify。"""

        if not 0.0 <= minimum_confidence <= 1.0:
            raise ValueError("minimum_confidence 必须位于 0..1")

        # 设备链是本轮第一段长阻塞，且期间 Python 无事可做；五个出网主机的 TLS
        # 握手全部塞进这段空窗，其中 Verify 主机原本要等到最后一步才第一次握手。
        self.prewarm_connections()

        with ExitStack() as stack:
            # FeiLin VM 必须跨越整个 Init→PE→Verify 过程保持存活。
            device_session = stack.enter_context(
                device_runtime.challenge_session()
            )
            challenge = self.init_challenge(device_session.init_token)
            initial_device = device_session.initial_result

            directory: str | Path
            if artifacts_dir is None:
                directory = stack.enter_context(
                    tempfile.TemporaryDirectory(prefix="ali-slider-challenge-")
                )
            else:
                directory = artifacts_dir

            assets = self.download_assets(challenge, directory)

            # UploadLog 是 best-effort 遥测，不参与任何控制流，公开 SDK 也不 await
            # 它；因此只需在本轮返回前回收，不能让 PE 站在这里干等一次完整的网络
            # 往返。executor 注册在 ExitStack 上，任何路径退出都会 join，不留悬挂。
            executor = stack.enter_context(
                ThreadPoolExecutor(
                    max_workers=1, thread_name_prefix="ali-upload-log"
                )
            )
            upload_future = executor.submit(
                self.upload_initialization_log, challenge, assets
            )

            vision = self.solve_assets(assets, x_pos_override=x_pos_override)
            if vision.confidence < minimum_confidence:
                raise AliSliderError(
                    "缺口置信度不足，已停止且未发送 Verify："
                    f"{vision.confidence:.3f} < {minimum_confidence:.3f}"
                )

            # 首触年龄的门控发生在 PE slider ready 之后、真正 dispatch 之前，
            # 因此 Node 启动与 SDK/PE 初始化的耗时不会叠加到目标年龄上。
            build = self.build_verify_data(
                challenge,
                assets,
                vision,
                sdk_path=device_session.sdk_path,
                device=initial_device,
                init_begin_time=challenge.init_started_ms,
                target_first_touch_age_ms=(
                    device_session.target_first_touch_age_ms
                ),
                fixture_path=fixture_path,
            )

            device = device_session.complete_challenge(
                build.device_getter_arguments,
                build.feilin_interaction_events,
                # PE 事件时间是非负毫秒；用与前端 Math.round 一致的 half-up，
                # 而不是 Python 的银行家舍入。
                post_interaction_delay_ms=int(
                    build.post_interaction_delay_ms + 0.5
                ),
            )
            if device.getter_argument_count != len(
                build.device_getter_arguments
            ):
                raise AliSliderError(
                    "Verify DeviceToken 未按动态 PE getter 参数刷新"
                )

            verify = self.verify_challenge(
                challenge, device.verify_token, build
            )

            # 遥测 worker 注册在 ExitStack 上，本轮返回前无论如何都要 join，所以在
            # Verify 之后取结果不增加任何等待——只是把同一段 join 提前几行，换来
            # 精确的记录。上限与它自己的请求超时一致，异常情况下也不会挂死。
            upload_log_succeeded = _settled_flag(upload_future, timeout=5.0)

        return ChallengeOutcome(
            device=device,
            challenge=challenge,
            vision=vision,
            build=build,
            verify=verify,
            upload_log_succeeded=upload_log_succeeded,
        )


__all__ = [
    "AliSliderClient",
    "CaptchaChallenge",
    "CaptchaVerifyResult",
    "ChallengeOutcome",
    "VerifyBuild",
    "normalize_proxies",
]
