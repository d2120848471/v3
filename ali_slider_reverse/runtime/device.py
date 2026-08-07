"""设备 RPC 与 DeviceToken 的纯 Python 服务兼容实现。

实现不下载 SDK/FeiLin JavaScript，也不创建浏览器补环境。当前已在线验证的兼容
顺序为：

``Log1 → DeviceConfig → Log2 → Log3 → Init → fingerprint/Log2 → Verify``。

这里的 111 段指纹、``cost=0`` 与 Log3 时点是服务端已接受的归一化协议选择，不
宣称和原生 FeiLin collector 的字段、耗时或异步上报生命周期逐字节一致。

静态 key 仍以公开脚本中的 AES 密文保存，只有
:func:`~ali_slider_reverse.protocol.secrets.resolve_frontend_secrets` 会在进程内
解开。模块不得记录请求 ``Data``、DeviceConfig、DeviceToken 或指纹明文。
"""

from __future__ import annotations

import base64
import hashlib
import math
import re
import secrets
import string
import time
from collections.abc import Iterator
from contextlib import contextmanager, suppress
from dataclasses import dataclass, field
from typing import Any

from .. import config
from ..device_profile import DeviceProfile
from ..errors import DeviceRuntimeError
from ..protocol.device_token import (
    DeviceConfig,
    DeviceToken,
    aes_cbc_decrypt_base64,
    aes_cbc_encrypt_base64,
    build_device_token,
    parse_device_token,
)
from ..protocol.secrets import FrontendSecrets, resolve_frontend_secrets
from ..protocol.signing import (
    compact_json,
    js_form_urlencode,
    rpc_v1_signature,
    uuid4_nonce,
)

_APP_KEY = "ab034ec0643f91399eb33e062dc7fae1"
_APP_NAME = "saf-captcha"
_APP_VERSION = "W20220202"
_FIELD0 = "W.10054"
_CAPTCHA_PLATFORM = "W.10001.c"
_DEVICE_API_VERSION = "2020-10-15"
_LOCATION_HREF = config.ORIGIN + "/package_product/pages/PaySubmit/index"
_MAX_INTERACTION_EVENTS = 512
_MAX_COMBAT_WAIT_SECONDS = 1.0


@dataclass(frozen=True, slots=True)
class DeviceRuntimeResult:
    """一轮设备会话中已经校验过的两枚 DeviceToken。"""

    init_token: str = field(repr=False)
    verify_token: str = field(repr=False)
    device_config: DeviceConfig = field(repr=False)
    init_parsed: DeviceToken = field(repr=False)
    verify_parsed: DeviceToken = field(repr=False)
    token_source: str
    request_count: int
    request_actions: tuple[str, ...]
    gather_cost_normalized: bool
    getter_argument_count: int | None = None
    fingerprint_field_count: int = 0
    collector_started_ms: int | None = None
    token_time_ms: int | None = None
    interaction_event_count: int | None = None


def _requests_module() -> Any:
    try:
        import requests
    except ImportError as exc:  # pragma: no cover - 安装环境决定。
        raise DeviceRuntimeError("设备协议需要 requests") from exc
    return requests


def _decode_utf8_base64(value: str, *, label: str) -> str:
    if not value:
        return ""
    try:
        return base64.b64decode(value, validate=True).decode("utf-8")
    except (ValueError, UnicodeDecodeError) as exc:
        raise DeviceRuntimeError(f"DeviceConfig.{label} Base64 无效") from exc


def parse_device_config(ciphertext: str, *, response_key: str) -> DeviceConfig:
    """解密并严格解析 Log1 返回的 DeviceConfig。"""

    try:
        plaintext = aes_cbc_decrypt_base64(ciphertext, key=response_key).decode(
            "utf-8"
        )
    except (ValueError, UnicodeDecodeError) as exc:
        raise DeviceRuntimeError("Log1 DeviceConfig 解密失败") from exc
    parts = plaintext.split("#")
    if len(parts) < 9:
        raise DeviceRuntimeError("DeviceConfig 分段不足")
    key = _decode_utf8_base64(parts[0], label="key")
    switch_text = _decode_utf8_base64(parts[1], label="switch")
    try:
        switch = int(switch_text, 10)
    except ValueError as exc:
        raise DeviceRuntimeError("DeviceConfig.switch 无效") from exc
    if len(key.encode("utf-8")) != 16 or not parts[2]:
        raise DeviceRuntimeError("DeviceConfig key/session 无效")
    return DeviceConfig(
        key=key,
        switch=switch,
        session_id=parts[2],
        version=parts[3],
        plugin_elements=_decode_utf8_base64(parts[4], label="pluginElements"),
        plugin_resource=_decode_utf8_base64(parts[5], label="pluginResource"),
        global_variable=_decode_utf8_base64(parts[6], label="globalVariable"),
        timestamp=parts[7],
        ip=parts[8],
        extra_segments=tuple(parts[9:]),
    )


def _chrome_version(profile: DeviceProfile) -> str:
    match = re.search(r"Chrome/([^ ]+)", profile.user_agent)
    return match.group(1) if match else profile.ua_full_version


def _android_major(profile: DeviceProfile) -> str:
    match = re.search(r"Android ([0-9]+)", profile.user_agent)
    return match.group(1) if match else profile.ua_platform_version.split(".")[0]


def _random_alnum(length: int) -> str:
    alphabet = string.digits + string.ascii_lowercase + string.ascii_uppercase
    return "".join(secrets.choice(alphabet) for _ in range(length))


def _session_probe(session_id: str) -> str:
    """从 FeiLin 会话 ID 尾部恢复 111 指纹的 index 21。"""

    suffix = session_id[-8:]
    key = "44d876b6"
    if len(suffix) != 8 or any(not 32 <= ord(char) <= 126 for char in suffix):
        raise DeviceRuntimeError("DeviceConfig.sessionId 尾部形状无效")
    stage1 = "".join(
        chr(32 + ((ord(left) - 32 + ord(right) - 32) % 95))
        for left, right in zip(suffix, key, strict=True)
    )
    raw = bytes(32 + ((ord(char) - 32 + 54) % 95) for char in stage1)
    return base64.b64encode(raw).decode("ascii")


def _seeded_stream(seed: str) -> Iterator[int]:
    """按 FNV-1a + xorshift32 生成画像所需的确定性字节流。"""

    state = 0x811C9DC5
    for character in seed:
        state = ((state ^ ord(character)) * 0x01000193) & 0xFFFFFFFF
    while True:
        state ^= (state << 13) & 0xFFFFFFFF
        state &= 0xFFFFFFFF
        state ^= state >> 17
        state &= 0xFFFFFFFF
        state ^= (state << 5) & 0xFFFFFFFF
        state &= 0xFFFFFFFF
        yield state


def _seeded_base64(seed: str, length: int) -> str:
    alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
    stream = _seeded_stream(seed)
    return "".join(alphabet[next(stream) % 64] for _ in range(length))


def _canvas_fingerprint(profile: DeviceProfile) -> str:
    length_stream = _seeded_stream(f"{profile.canvas_seed}:len")
    data_url = "data:image/png;base64," + _seeded_base64(
        f"{profile.canvas_seed}:canvas", 3400 + next(length_stream) % 420
    )
    material = compact_json(
        {"winding": True, "geometry": data_url, "text": data_url}
    )
    return hashlib.md5(material.encode("utf-8")).hexdigest()


def _gpu_fingerprint(profile: DeviceProfile) -> str:
    material = compact_json(
        {
            "fhgjjghdf": profile.gpu.unmasked_vendor,
            "xcvdfgfd": profile.gpu.unmasked_renderer,
        }
    )
    return hashlib.md5(material.encode("utf-8")).hexdigest()


def _brand_list(profile: DeviceProfile) -> str:
    return "[" + ",".join(brand for brand, _ in profile.ua_brands) + "]"


def _normalize_interaction_events(
    events: tuple[dict[str, Any], ...],
) -> tuple[dict[str, float | bool | str], ...]:
    if not isinstance(events, tuple) or not 1 <= len(events) <= _MAX_INTERACTION_EVENTS:
        raise ValueError(
            f"interaction_events 必须包含 1..{_MAX_INTERACTION_EVENTS} 个事件"
        )
    result: list[dict[str, float | bool | str]] = []
    previous = -1.0
    for index, event in enumerate(events):
        if not isinstance(event, dict) or event.get("type") != "mousemove":
            raise ValueError(f"interaction_events[{index}] 结构无效")
        x, y, timestamp = event.get("x"), event.get("y"), event.get("timeStamp")
        if (
            any(
                isinstance(value, bool) or not isinstance(value, (int, float))
                for value in (x, y, timestamp)
            )
            or not all(math.isfinite(float(value)) for value in (x, y, timestamp))
            or float(timestamp) < previous
            or event.get("isTrusted") is not True
        ):
            raise ValueError(f"interaction_events[{index}] 数值无效")
        result.append(
            {
                "type": "mousemove",
                "x": float(x),
                "y": float(y),
                "timeStamp": float(timestamp),
                "isTrusted": True,
            }
        )
        previous = float(timestamp)
    return tuple(result)


class DeviceRuntimeClient:
    """设备 RPC 的配置载体；每轮开出一个独立纯 Python 会话。"""

    def __init__(
        self,
        *,
        prefix: str = config.DEFAULT_PREFIX,
        region: str = config.DEFAULT_REGION,
        endpoint: str = config.DEVICE_ENDPOINT,
        timeout: float = config.DEFAULT_TIMEOUT,
        gather_cost_range: tuple[int, int] = config.DEFAULT_GATHER_COST_RANGE,
        first_touch_age_range: tuple[int, int] = config.DEFAULT_FIRST_TOUCH_AGE_RANGE,
        proxies: dict[str, str] | None = None,
        device_profile: DeviceProfile,
    ) -> None:
        if not isinstance(prefix, str) or not prefix:
            raise ValueError("prefix 不能为空")
        if not isinstance(region, str) or not region:
            raise ValueError("prefix 和 region 不能为空")
        if not isinstance(endpoint, str) or not endpoint:
            raise ValueError("endpoint 不能为空")
        if (
            isinstance(timeout, bool)
            or not isinstance(timeout, (int, float))
            or not math.isfinite(float(timeout))
            or timeout <= 0
        ):
            raise ValueError("timeout 必须为正数")
        if not isinstance(device_profile, DeviceProfile):
            raise ValueError("device_profile 必须是本轮生成的 DeviceProfile")
        for label, value in (
            ("gather_cost_range", gather_cost_range),
            ("first_touch_age_range", first_touch_age_range),
        ):
            if (
                not isinstance(value, tuple)
                or len(value) != 2
                or any(
                    isinstance(item, bool) or not isinstance(item, int)
                    for item in value
                )
                or value[0] < 1
                or value[1] < value[0]
            ):
                raise ValueError(f"{label} 必须是正整数闭区间")
        if proxies is not None and not isinstance(proxies, dict):
            raise ValueError("proxies 必须是 requests 映射或 None")
        self.prefix = prefix
        self.region = region
        self.endpoint = endpoint
        self.timeout = float(timeout)
        self.gather_cost_range = gather_cost_range
        self.first_touch_age_range = first_touch_age_range
        self.proxies = dict(proxies) if proxies else None
        self.device_profile = device_profile

    @contextmanager
    def challenge_session(self) -> Iterator[DeviceRuntimeSession]:
        with DeviceRuntimeSession(self) as session:
            yield session

    def open_challenge_sessions(self, count: int) -> tuple[DeviceRuntimeSession, ...]:
        if (
            isinstance(count, bool)
            or not isinstance(count, int)
            or not 1 <= count <= 64
        ):
            raise ValueError("count 必须是 1..64 的整数")
        return tuple(DeviceRuntimeSession(self) for _ in range(count))


class DeviceRuntimeSession:
    """跨 Init/Verify 保存 DeviceConfig 与兼容型 111 段指纹状态。"""

    def __init__(self, client: DeviceRuntimeClient) -> None:
        self.client = client
        self.initialized_at: float | None = None
        self._http: Any | None = None
        self._device_config: DeviceConfig | None = None
        self._init_result: DeviceRuntimeResult | None = None
        self._completed = False
        self._target_first_touch_age_ms: int | None = None
        self._collector_started_ms: int | None = None
        self._combat_started_ms: int | None = None
        self._gather_cost: int | None = None
        self._stable_fields: dict[int, str] = {}
        self._actions: list[str] = []

    def __enter__(self) -> DeviceRuntimeSession:
        if self._init_result is None:
            self._initialize()
        return self

    def __exit__(self, exc_type: Any, exc: Any, traceback: Any) -> None:
        self.close()

    def _new_http(self) -> Any:
        http = _requests_module().Session()
        if self.client.proxies:
            http.proxies.update(self.client.proxies)
        return http

    def _post_action(self, action: str, data: str) -> dict[str, Any]:
        secrets_value = resolve_frontend_secrets()
        params: dict[str, str] = {
            "AaduaneId": secrets_value.device_rpc_key_id,
            "Version": _DEVICE_API_VERSION,
            "SignatureMethod": "HMAC-SHA1",
            "SignatureVersion": "1.0",
            "Format": "JSON",
            "Action": action,
            "Data": data,
            "SignatureNonce": uuid4_nonce(),
        }
        params["Signature"] = rpc_v1_signature(
            params, secret=secrets_value.device_rpc_key_secret
        )
        http = self._http
        if http is None:
            raise DeviceRuntimeError("设备 HTTP 会话尚未初始化")
        try:
            response = http.post(
                self.client.endpoint,
                data=js_form_urlencode(params),
                headers={
                    **config.browser_headers(profile=self.client.device_profile),
                    "Content-Type": "application/x-www-form-urlencoded; charset=UTF-8",
                },
                timeout=self.client.timeout,
            )
            response.raise_for_status()
            payload = response.json()
        except Exception as exc:
            raise DeviceRuntimeError(f"设备 RPC {action} 请求失败") from exc
        if not isinstance(payload, dict) or str(payload.get("Code")) != "200":
            raise DeviceRuntimeError(f"设备 RPC {action} 返回失败")
        self._actions.append(action)
        return payload

    def _build_log1(self, secrets_value: FrontendSecrets) -> str:
        inner = aes_cbc_encrypt_base64(
            "#".join(
                (
                    _CAPTCHA_PLATFORM,
                    _APP_NAME,
                    "scene",
                    "captcha-front",
                    self.client.prefix,
                    self.client.region,
                )
            ),
            key=secrets_value.device_flag_key,
        )
        outer = f"{_APP_KEY}#W#{inner}#{_APP_VERSION}#CLOUD#"
        return aes_cbc_encrypt_base64(outer, key=secrets_value.device_request_key)

    def _initialize_stable_fields(self) -> None:
        profile = self.client.device_profile
        config_value = self._require_config()
        self._stable_fields = {
            21: _session_probe(config_value.session_id),
            32: _canvas_fingerprint(profile),
            73: _random_alnum(42),
            75: "mobile",
            78: _gpu_fingerprint(profile),
        }

    def _collector_timing(self, token_time_ms: int) -> str:
        """构造与 field72/74 自洽的 FeiLin 采集阶段时间线。"""

        config_value = self._require_config()
        collector = self._collector_started_ms
        combat = self._combat_started_ms
        if collector is None or combat is None:
            raise DeviceRuntimeError("FeiLin 采集时钟尚未初始化")
        try:
            config_time = int(config_value.timestamp, 10)
        except ValueError as exc:
            raise DeviceRuntimeError("DeviceConfig.timestamp 无效") from exc

        stage11 = max(0, config_time - collector)
        stage20 = stage11 + 1
        stage23 = combat - collector
        if stage23 <= stage20:
            raise DeviceRuntimeError("FeiLin combat 时钟早于配置阶段")
        stage30 = stage23 + 1
        stage40 = stage30 + 4
        elapsed = token_time_ms - collector
        if elapsed < stage40 + 3:
            raise DeviceRuntimeError("FeiLin token 时钟早于采集阶段")
        stage90 = elapsed - 3
        if self._init_result is None:
            stage93 = elapsed + 4
            stage94 = elapsed + 8
        else:
            stage93 = elapsed + 2
            stage94 = elapsed + 2
        return "|".join(
            (
                "10-0",
                f"11-{stage11}",
                f"20-{stage20}",
                f"23-{stage23}",
                f"30-{stage30}",
                f"40-{stage40}",
                f"90-{stage90}",
                f"91-{elapsed}",
                f"92-{elapsed}",
                f"93-{stage93}",
                f"94-{stage94}",
            )
        )

    def _fingerprint_fields(
        self, *, token_time_ms: int, getter_argument: str = ""
    ) -> tuple[str, ...]:
        config_value = self._require_config()
        profile = self.client.device_profile
        chrome_version = _chrome_version(profile)
        chrome_major = chrome_version.split(".", 1)[0]
        chrome_zeroed = f"{chrome_major}.0.0.0"
        fields = [""] * 111
        values: dict[int, str] = {
            0: _FIELD0,
            5: profile.platform,
            6: "Chrome",
            7: chrome_zeroed,
            22: str(profile.device_memory),
            34: str(profile.hardware_concurrency),
            36: profile.ua_platform,
            37: _android_major(profile),
            42: config_value.ip,
            44: str(profile.mobile).lower(),
            45: str(profile.mobile).lower(),
            47: f"{profile.screen.height}*{profile.screen.width}",
            49: str(profile.max_touch_points),
            53: _LOCATION_HREF,
            63: chrome_zeroed,
            64: profile.user_agent,
            67: _APP_NAME,
            68: str(config_value.switch),
            72: str(self._collector_started_ms or token_time_ms),
            74: str(token_time_ms),
            77: getter_argument,
            80: profile.app_version,
            85: str(config_value.switch),
            86: "0",
            87: config_value.timestamp,
            110: _brand_list(profile),
            **self._stable_fields,
        }
        # 43/71/74 是当前 FeiLin 每次 getToken 都刷新的字段。
        values[43] = self._collector_timing(token_time_ms)
        values[71] = _random_alnum(40)
        for index, value in values.items():
            if "#" in value:
                raise DeviceRuntimeError(f"fingerprint[{index}] 含非法分隔符")
            fields[index] = value
        return tuple(fields)

    def _build_token(
        self, *, getter_argument: str = ""
    ) -> tuple[str, DeviceToken, int, str]:
        now_ms = int(time.time() * 1000)
        fields = self._fingerprint_fields(
            token_time_ms=now_ms, getter_argument=getter_argument
        )
        plaintext = "#".join(fields)
        config_value = self._require_config()
        cipher = aes_cbc_encrypt_base64(plaintext, key=config_value.key)
        if self._gather_cost is None:
            lower, upper = self.client.gather_cost_range
            self._gather_cost = lower + secrets.randbelow(upper - lower + 1)
        salt = resolve_frontend_secrets().device_token_salt
        token = build_device_token(
            session_id=config_value.session_id,
            fingerprint_cipher=cipher,
            gather_cost=self._gather_cost,
            salt=salt,
        )
        return token, parse_device_token(token, salt=salt), now_ms, cipher

    def _build_log2(self, fingerprint_cipher: str, *, cost: int = 0) -> str:
        config_value = self._require_config()
        secrets_value = resolve_frontend_secrets()
        inner = "#".join(
            (
                config_value.session_id,
                fingerprint_cipher,
                aes_cbc_encrypt_base64(_APP_NAME, key=config_value.key),
                aes_cbc_encrypt_base64(_FIELD0, key=config_value.key),
                "",
                aes_cbc_encrypt_base64(
                    str(int(time.time() * 1000)), key=config_value.key
                ),
            )
        )
        outer = "#".join(
            (
                _APP_KEY,
                "W",
                aes_cbc_encrypt_base64(
                    f"{_FIELD0}#{_APP_NAME}", key=config_value.key
                ),
                _APP_VERSION,
                "CLOUD",
                str(cost),
                "501",
                base64.b64encode(inner.encode("utf-8")).decode("ascii"),
            )
        )
        return aes_cbc_encrypt_base64(outer, key=secrets_value.device_upload_key)

    def _log2_fingerprint_cipher(self, token_cipher: str) -> str:
        """从 DeviceToken 指纹派生兼容型 mode=501 Log2 的 111 段快照。

        兼容请求让 Log2 截止在阶段 92，DeviceToken 再包含阶段 93/94；其余
        110 段共享。该关系已获当前服务端接受，不代表原生 collector 的 settled
        请求字段数或生命周期。
        """

        config_value = self._require_config()
        try:
            plaintext = aes_cbc_decrypt_base64(
                token_cipher, key=config_value.key
            ).decode("utf-8")
        except (ValueError, UnicodeDecodeError) as exc:
            raise DeviceRuntimeError("Log2 指纹快照解密失败") from exc
        fields = plaintext.split("#")
        if len(fields) != 111:
            raise DeviceRuntimeError("Log2 指纹快照字段数异常")
        fields[43] = "|".join(
            part
            for part in fields[43].split("|")
            if not part.startswith(("93-", "94-"))
        )
        return aes_cbc_encrypt_base64("#".join(fields), key=config_value.key)

    def _build_log3(self) -> str:
        config_value = self._require_config()
        secrets_value = resolve_frontend_secrets()
        app_cipher = aes_cbc_encrypt_base64(_APP_NAME, key=config_value.key)
        field_cipher = aes_cbc_encrypt_base64(_FIELD0, key=config_value.key)
        marker_header = "#".join(
            (
                config_value.session_id,
                "",
                app_cipher,
                field_cipher,
                "",
                aes_cbc_encrypt_base64(
                    str(int(time.time() * 1000)), key=config_value.key
                ),
            )
        )
        event_json = compact_json(
            {
                "mousemove": [],
                "mouseclick": [],
                "keyup": [],
                "scrollTop": [],
                "scrollLeft": [],
                "pointerEvent": [],
                "clientType": "mobile",
                "startTime": self._combat_started_ms or int(time.time() * 1000),
                "timestamp": config_value.timestamp,
            }
        )
        combat_header = "#".join(
            (
                config_value.session_id,
                aes_cbc_encrypt_base64(event_json, key=config_value.key),
                app_cipher,
                field_cipher,
                "",
                aes_cbc_encrypt_base64(
                    str(int(time.time() * 1000)), key=config_value.key
                ),
            )
        )
        marker = base64.b64encode(marker_header.encode("utf-8")).decode("ascii")
        combat = base64.b64encode(combat_header.encode("utf-8")).decode("ascii")
        stage = f"511#{marker}-504#{combat}"
        outer = "#".join(
            (
                _APP_KEY,
                "W",
                aes_cbc_encrypt_base64(
                    f"{_FIELD0}#{_APP_NAME}", key=config_value.key
                ),
                _APP_VERSION,
                "CLOUD",
                "",
                base64.b64encode(stage.encode("utf-8")).decode("ascii"),
            )
        )
        return aes_cbc_encrypt_base64(outer, key=secrets_value.device_upload_key)

    def _require_config(self) -> DeviceConfig:
        if self._device_config is None:
            raise DeviceRuntimeError("设备会话尚未取得 DeviceConfig")
        return self._device_config

    def _initialize(self) -> None:
        self.close()
        self._init_result = None
        self._device_config = None
        self._target_first_touch_age_ms = None
        self._actions = []
        self._completed = False
        self._gather_cost = None
        self._collector_started_ms = int(time.time() * 1000)
        try:
            self._http = self._new_http()
            secrets_value = resolve_frontend_secrets()
            payload = self._post_action("Log1", self._build_log1(secrets_value))
            result_object = payload.get("ResultObject")
            ciphertext = (
                result_object.get("DeviceConfig")
                if isinstance(result_object, dict)
                else None
            )
            if not isinstance(ciphertext, str) or not ciphertext:
                raise DeviceRuntimeError("Log1 响应缺少 DeviceConfig")
            self._device_config = parse_device_config(
                ciphertext, response_key=secrets_value.device_response_key
            )
            try:
                config_time_ms = int(self._require_config().timestamp, 10)
            except ValueError as exc:
                raise DeviceRuntimeError("DeviceConfig.timestamp 无效") from exc
            combat_target_ms = config_time_ms + 90
            combat_wait_s = (
                combat_target_ms - int(time.time() * 1000)
            ) / 1000
            if combat_wait_s > min(self.client.timeout, _MAX_COMBAT_WAIT_SECONDS):
                raise DeviceRuntimeError("DeviceConfig.timestamp 超出允许时钟偏差")
            if combat_wait_s > 0:
                time.sleep(combat_wait_s)
            self._combat_started_ms = int(time.time() * 1000)
            self._initialize_stable_fields()
            # 按已通过在线验收的兼容时序生成首枚 token。
            time.sleep(0.044)
            token, parsed, token_time, cipher = self._build_token()
            log2_data = self._build_log2(self._log2_fingerprint_cipher(cipher))
            time.sleep(0.006)
            log3_data = self._build_log3()
            self._post_action("Log2", log2_data)
            self._post_action("Log3", log3_data)
            self._init_result = DeviceRuntimeResult(
                init_token=token,
                verify_token=token,
                device_config=self._require_config(),
                init_parsed=parsed,
                verify_parsed=parsed,
                token_source="python-compatible",
                request_count=len(self._actions),
                request_actions=tuple(self._actions),
                gather_cost_normalized=False,
                fingerprint_field_count=111,
                collector_started_ms=self._collector_started_ms,
                token_time_ms=token_time,
            )
            self.initialized_at = time.monotonic()
        except BaseException:
            self.close()
            self._device_config = None
            self._init_result = None
            raise

    @property
    def init_token(self) -> str:
        return self.initial_result.init_token

    @property
    def initial_result(self) -> DeviceRuntimeResult:
        if self._init_result is None:
            raise DeviceRuntimeError("设备会话尚未完成初始化")
        return self._init_result

    @property
    def target_first_touch_age_ms(self) -> int:
        if self._completed:
            raise DeviceRuntimeError("设备会话已经完成")
        if self._target_first_touch_age_ms is None:
            lower, upper = self.client.first_touch_age_range
            self._target_first_touch_age_ms = lower + secrets.randbelow(
                upper - lower + 1
            )
        return self._target_first_touch_age_ms

    @property
    def recyclable(self) -> bool:
        return self._http is not None

    def recycle(self) -> None:
        if self._http is None:
            raise DeviceRuntimeError("设备会话已经关闭")
        self._initialize()

    def complete_challenge(
        self,
        getter_arguments: tuple[str, ...],
        interaction_events: tuple[dict[str, Any], ...],
        *,
        post_interaction_delay_ms: int,
    ) -> DeviceRuntimeResult:
        if self._http is None or self._init_result is None:
            raise DeviceRuntimeError("设备会话尚未初始化或已经关闭")
        if self._completed:
            raise DeviceRuntimeError("设备会话已经完成")
        if (
            not isinstance(getter_arguments, tuple)
            or len(getter_arguments) != 1
            or not isinstance(getter_arguments[0], str)
            or not getter_arguments[0]
        ):
            raise ValueError("getter_arguments 必须包含一个非空字符串")
        events = _normalize_interaction_events(interaction_events)
        if (
            isinstance(post_interaction_delay_ms, bool)
            or not isinstance(post_interaction_delay_ms, int)
            or not 0 <= post_interaction_delay_ms <= 500
        ):
            raise ValueError("post_interaction_delay_ms 必须位于 0..500")

        # 纯 Python 轨迹层已验证事件形状与时间单调性；这里没有浏览器事件目标可
        # 回放。兼容设备链只把 getter 实参写入 field 77 并刷新 Verify token。
        token, parsed, token_time, token_cipher = self._build_token(
            getter_argument=getter_arguments[0]
        )
        # 完成阶段发一枚当前服务端接受的 mode=501 兼容 Log2；cost=0 是协议
        # 归一化值，不是对原生 collector 动态耗时的声明。
        log2_data = self._build_log2(
            self._log2_fingerprint_cipher(token_cipher), cost=0
        )
        self._post_action("Log2", log2_data)
        initial = self.initial_result
        self._completed = True
        return DeviceRuntimeResult(
            init_token=initial.init_token,
            verify_token=token,
            device_config=self._require_config(),
            init_parsed=initial.init_parsed,
            verify_parsed=parsed,
            token_source="python-compatible",
            request_count=len(self._actions),
            request_actions=tuple(self._actions),
            gather_cost_normalized=False,
            getter_argument_count=1,
            fingerprint_field_count=111,
            collector_started_ms=self._collector_started_ms,
            token_time_ms=token_time,
            interaction_event_count=len(events),
        )

    def close(self) -> None:
        http, self._http = self._http, None
        if http is not None:
            with suppress(Exception):  # pragma: no cover - 关闭兜底。
                http.close()


__all__ = [
    "DeviceRuntimeClient",
    "DeviceRuntimeResult",
    "DeviceRuntimeSession",
    "parse_device_config",
]
