"""通过公开 SDK + FeiLin 生成当前会话的 DeviceToken。

Python 负责下载/校验公开 ``AliyunCaptcha.js``、调用 Node 桥并验证返回 token；
Node 只承担浏览器环境依赖较重的 Log1、DeviceConfig 与 FeiLin 收集过程。
"""

from __future__ import annotations

import json
import math
import select
import secrets
import subprocess
import tempfile
import time
from collections.abc import Iterator
from contextlib import contextmanager
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any
from urllib.parse import urlparse

from .device import (
    DeviceConfig,
    DeviceToken,
    aes_cbc_decrypt_base64,
    build_device_token,
    parse_device_token,
)
from .frontend_profile import resolve_frontend_secrets


DEFAULT_SDK_URL = (
    "https://o.alicdn.com/captcha-frontend/aliyunCaptcha/AliyunCaptcha.js"
)
DEFAULT_FIRST_TOUCH_AGE_RANGE = (650, 850)
_ALLOWED_SDK_HOSTS = frozenset({"o.alicdn.com", "g.alicdn.com"})


class DeviceRuntimeError(RuntimeError):
    """设备运行桥无法安全、完整地产生 token。"""


@dataclass(frozen=True, slots=True)
class DeviceRuntimeResult:
    """Node 桥的最小、已校验结果。"""

    init_token: str = field(repr=False)
    verify_token: str = field(repr=False)
    verify_arg_key: str = field(repr=False)
    verify_arg_plaintext: str = field(repr=False)
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


def _normalize_fast_gather_cost(
    token: str,
    parsed: DeviceToken,
    *,
    salt: str,
    lower: int,
    upper: int,
) -> tuple[str, DeviceToken, bool]:
    """修正 Node 补环境过快导致的零耗时。

    FeiLin 把 ``GatherCost`` 独立放在 token 第四段，不参与指纹密文；当前真实页面
    采集约为 226ms，而同一公开脚本在轻量 Node DOM 中会落到 0ms。这里只在原值低于
    下限时按当前正常区间重建容器及其 MD5，不修改 FeiLin 产生的 session/cipher。
    """

    try:
        cost = int(parsed.gather_cost)
    except ValueError as exc:  # pragma: no cover - parse_device_token 已保证十进制。
        raise DeviceRuntimeError("DeviceToken GatherCost 无效") from exc
    if cost >= lower:
        return token, parsed, False
    normalized_cost = lower + secrets.randbelow(upper - lower + 1)
    rebuilt = build_device_token(
        session_id=parsed.session_id,
        fingerprint_cipher=parsed.fingerprint_cipher,
        gather_cost=normalized_cost,
        salt=salt,
    )
    return rebuilt, parse_device_token(rebuilt, salt=salt), True


def _normalize_gather_cost_pair(
    init_token: str,
    init_parsed: DeviceToken,
    verify_token: str,
    verify_parsed: DeviceToken,
    *,
    salt: str,
    lower: int,
    upper: int,
) -> tuple[str, DeviceToken, str, DeviceToken, bool]:
    """以同一个采集耗时归一化同一 session 的两枚 token。

    页面中的 Init/Verify token 来自同一次 FeiLin collector；成功抓包也表明两者
    的 ``GatherCost`` 保持一致。Node 轻量 DOM 会让原始耗时落到 0，但不能因此
    给同一 session 的两枚 token 分别生成两个随机耗时。
    """

    init_cost = int(init_parsed.gather_cost)
    verify_cost = int(verify_parsed.gather_cost)
    valid_costs = [
        cost
        for cost in (init_cost, verify_cost)
        if cost >= lower
    ]
    # Node 桥在 Init 与 Verify 阶段各自测一次采集耗时：空闲时两次都落到 0，被
    # 下面的随机值统一；并发挑战抢占 CPU 时两次都会超过下限且互不相等。这是
    # 桥的测量方式使然，不代表 FeiLin session 不一致，因此统一取先发生的 Init
    # 耗时——真实页面里 Verify token 复用的也正是 Init 之前那次采集的结果。
    target_cost = (
        valid_costs[0]
        if valid_costs
        else lower + secrets.randbelow(upper - lower + 1)
    )

    changed = False
    if init_cost != target_cost:
        init_token = build_device_token(
            session_id=init_parsed.session_id,
            fingerprint_cipher=init_parsed.fingerprint_cipher,
            gather_cost=target_cost,
            salt=salt,
        )
        init_parsed = parse_device_token(init_token, salt=salt)
        changed = True
    if verify_cost != target_cost:
        verify_token = build_device_token(
            session_id=verify_parsed.session_id,
            fingerprint_cipher=verify_parsed.fingerprint_cipher,
            gather_cost=target_cost,
            salt=salt,
        )
        verify_parsed = parse_device_token(verify_token, salt=salt)
        changed = True
    return (
        init_token,
        init_parsed,
        verify_token,
        verify_parsed,
        changed,
    )


def _parse_bridge_output(stdout: str) -> dict[str, Any]:
    """从可能夹有公开 SDK console 输出的 stdout 末尾取 JSON。"""

    for line in reversed(stdout.splitlines()):
        candidate = line.strip()
        if not candidate.startswith("{"):
            continue
        try:
            payload = json.loads(candidate)
        except json.JSONDecodeError:
            continue
        if isinstance(payload, dict) and "deviceToken" in payload:
            return payload
    raise DeviceRuntimeError("Node 设备桥没有返回 DeviceToken JSON")


def _bridge_error(stderr: str) -> str:
    """提取桥的结构化错误，避免把整行混淆脚本或 token 写入异常。"""

    for line in reversed(stderr.splitlines()):
        candidate = line.strip()
        if not candidate.startswith("{"):
            continue
        try:
            payload = json.loads(candidate)
        except json.JSONDecodeError:
            continue
        message = payload.get("error") if isinstance(payload, dict) else None
        if isinstance(message, str) and message:
            return message[:800]
    return "未返回结构化错误"


def _parse_device_config_object(value: Any) -> DeviceConfig:
    """校验 Node VM 已解出的 DeviceConfig，不接受静默缺字段。"""

    if not isinstance(value, dict):
        raise DeviceRuntimeError("Node 设备桥缺少解密后的 DeviceConfig")
    text_fields = (
        "key",
        "sessionId",
        "version",
        "pluginElements",
        "pluginResource",
        "globalVariable",
        "timestamp",
        "ip",
    )
    for field in text_fields:
        if not isinstance(value.get(field), str):
            raise DeviceRuntimeError(
                f"Node DeviceConfig.{field} 类型无效"
            )
    if len(value["key"].encode("utf-8")) != 16:
        raise DeviceRuntimeError("Node DeviceConfig.key 不是 16 字节")
    if not value["sessionId"]:
        raise DeviceRuntimeError("Node DeviceConfig.sessionId 为空")
    switch = value.get("switch")
    if isinstance(switch, bool) or not isinstance(switch, int):
        raise DeviceRuntimeError("Node DeviceConfig.switch 类型无效")
    extra = value.get("extraSegments", [])
    if (
        not isinstance(extra, list)
        or not all(isinstance(item, str) for item in extra)
    ):
        raise DeviceRuntimeError(
            "Node DeviceConfig.extraSegments 类型无效"
        )
    return DeviceConfig(
        key=value["key"],
        switch=switch,
        session_id=value["sessionId"],
        version=value["version"],
        plugin_elements=value["pluginElements"],
        plugin_resource=value["pluginResource"],
        global_variable=value["globalVariable"],
        timestamp=value["timestamp"],
        ip=value["ip"],
        extra_segments=tuple(extra),
    )


def _parse_verify_arg_profile(value: Any) -> tuple[str, str]:
    """读取当前公开 SDK 传给动态 PE 的 ``tx(key, plaintext)`` 实参。"""

    if not isinstance(value, dict):
        raise DeviceRuntimeError("Node 设备桥缺少 Verify arg profile")
    access_sec = value.get("accessSec")
    session_id_salt = value.get("sessionIdSalt")
    for label, item in (
        ("accessSec", access_sec),
        ("sessionIdSalt", session_id_salt),
    ):
        if (
            not isinstance(item, str)
            or not 1 <= len(item) <= 128
            or not item.isascii()
            or not item.isprintable()
        ):
            raise DeviceRuntimeError(
                f"Node Verify arg profile.{label} 无效"
            )
    # 公开 SDK 仍把这两个运行态字段提供给动态 PE，因此保持显式校验与传递。
    # 但当前 PE 黑盒差分已确认 arg 只随 CertifyId 变化；不得把旧版
    # ACCESS_SEC / SESSION_ID_SALT 调用形态误写成当前 arg 的固定语义。
    return access_sec, session_id_salt


def _fingerprint_fields(
    parsed: DeviceToken,
    *,
    device_config: DeviceConfig,
) -> tuple[str, ...]:
    try:
        plaintext = aes_cbc_decrypt_base64(
            parsed.fingerprint_cipher,
            key=device_config.key,
        )
        text = plaintext.decode("utf-8")
    except (UnicodeDecodeError, ValueError) as exc:
        raise DeviceRuntimeError(
            "Verify DeviceToken 指纹密文无法按本次 DeviceConfig 解开"
        ) from exc
    return tuple(text.split("#"))


def _normalize_interaction_events(
    value: tuple[dict[str, Any], ...],
) -> tuple[dict[str, float | bool | str], ...]:
    if not isinstance(value, tuple) or not 1 <= len(value) <= 512:
        raise ValueError(
            "interaction_events 必须包含 1..512 个 PE mousemove 事件"
        )
    normalized: list[dict[str, float | bool | str]] = []
    previous_time: float | None = None
    for index, event in enumerate(value):
        if (
            not isinstance(event, dict)
            or set(event) != {
                "type",
                "x",
                "y",
                "timeStamp",
                "isTrusted",
            }
            or event.get("type") != "mousemove"
            or event.get("isTrusted") is not True
        ):
            raise ValueError(
                f"interaction_events[{index}] 结构无效"
            )
        x = event.get("x")
        y = event.get("y")
        timestamp = event.get("timeStamp")
        if (
            isinstance(x, bool)
            or not isinstance(x, (int, float))
            or isinstance(y, bool)
            or not isinstance(y, (int, float))
            or isinstance(timestamp, bool)
            or not isinstance(timestamp, (int, float))
            or not all(
                math.isfinite(float(item))
                for item in (x, y, timestamp)
            )
            or abs(float(x)) > 10_000
            or abs(float(y)) > 10_000
            or not 0 <= float(timestamp) <= 180_000
            or (
                previous_time is not None
                and float(timestamp) < previous_time
            )
        ):
            raise ValueError(
                f"interaction_events[{index}] 数值无效"
            )
        normalized.append(
            {
                "type": "mousemove",
                "x": float(x),
                "y": float(y),
                "timeStamp": float(timestamp),
                "isTrusted": True,
            }
        )
        previous_time = float(timestamp)
    if (
        float(normalized[-1]["timeStamp"])
        - float(normalized[0]["timeStamp"])
        > 60_000
    ):
        raise ValueError("interaction_events 总时长不能超过 60 秒")
    return tuple(normalized)


def _download_public_sdk(
    url: str,
    destination: Path,
    *,
    timeout: float,
    proxies: dict[str, str] | None = None,
) -> None:
    parsed = urlparse(url)
    if parsed.scheme != "https" or parsed.hostname not in _ALLOWED_SDK_HOSTS:
        raise DeviceRuntimeError("SDK URL 必须是允许的 alicdn HTTPS 地址")
    try:
        import requests
    except ImportError as exc:  # pragma: no cover - 由调用环境决定。
        raise DeviceRuntimeError("下载公开 SDK 需要 requests") from exc

    response = requests.get(url, timeout=timeout, proxies=proxies)
    response.raise_for_status()
    source = response.content
    # 这是结构校验而不是固定版本锁定；验证码前端可能正常热更新。
    if len(source) < 50_000 or b"AliyunCaptchaConfig" not in source:
        raise DeviceRuntimeError("下载内容不像 AliyunCaptcha.js")
    destination.write_bytes(source)


class DeviceRuntimeClient:
    """生成并校验当前 FeiLin DeviceToken。"""

    def __init__(
        self,
        *,
        node_binary: str = "node",
        bridge_script: str | Path | None = None,
        sdk_path: str | Path | None = None,
        sdk_url: str = DEFAULT_SDK_URL,
        prefix: str = "fsgtmi",
        region: str = "cn",
        timeout: float = 20.0,
        gather_cost_range: tuple[int, int] = (180, 260),
        first_touch_age_range: tuple[int, int] = (
            DEFAULT_FIRST_TOUCH_AGE_RANGE
        ),
        proxies: dict[str, str] | None = None,
    ) -> None:
        if timeout <= 0:
            raise ValueError("timeout 必须为正数")
        if not prefix:
            raise ValueError("prefix 不能为空")
        if not region:
            raise ValueError("region 不能为空")
        if proxies is not None and not isinstance(proxies, dict):
            raise ValueError("proxies 必须是 requests 的映射或 None")
        if (
            len(gather_cost_range) != 2
            or isinstance(gather_cost_range[0], bool)
            or isinstance(gather_cost_range[1], bool)
            or not isinstance(gather_cost_range[0], int)
            or not isinstance(gather_cost_range[1], int)
            or gather_cost_range[0] < 1
            or gather_cost_range[1] < gather_cost_range[0]
        ):
            raise ValueError("gather_cost_range 必须是正整数闭区间")
        if (
            len(first_touch_age_range) != 2
            or isinstance(first_touch_age_range[0], bool)
            or isinstance(first_touch_age_range[1], bool)
            or not isinstance(first_touch_age_range[0], int)
            or not isinstance(first_touch_age_range[1], int)
            or first_touch_age_range[0] < 1
            or first_touch_age_range[1] < first_touch_age_range[0]
            or first_touch_age_range[1] > 120_000
        ):
            raise ValueError(
                "first_touch_age_range 必须是 1..120000 "
                "毫秒的正整数闭区间"
            )
        self.node_binary = node_binary
        self.bridge_script = (
            Path(__file__).with_name("sdk_device_bridge.mjs")
            if bridge_script is None
            else Path(bridge_script)
        )
        self.sdk_path = None if sdk_path is None else Path(sdk_path)
        self.sdk_url = sdk_url
        self.prefix = prefix
        self.region = region
        self.timeout = float(timeout)
        self.gather_cost_range = gather_cost_range
        self.first_touch_age_range = first_touch_age_range
        self.proxies = dict(proxies) if proxies else None

    def _result_from_payload(
        self,
        payload: dict[str, Any],
        *,
        expected_getter_argument_count: int | None = None,
        expected_interaction_event_count: int | None = None,
    ) -> DeviceRuntimeResult:
        token = payload.get("deviceToken")
        if not isinstance(token, str) or not token:
            raise DeviceRuntimeError("Node 设备桥返回了空 Init token")
        verify_token = payload.get("verifyDeviceToken")
        if expected_getter_argument_count is None and verify_token is None:
            verify_token = token
        if not isinstance(verify_token, str) or not verify_token:
            raise DeviceRuntimeError("Node 设备桥返回了空 Verify token")

        # salt 仍从公开前端密文运行时恢复，不把二次明文写进源代码。
        salt = resolve_frontend_secrets().device_token_salt
        try:
            init_parsed = parse_device_token(token, salt=salt)
            verify_parsed = parse_device_token(
                verify_token,
                salt=salt,
            )
        except ValueError as exc:
            raise DeviceRuntimeError(
                "Node 设备桥返回的 DeviceToken 容器校验失败"
            ) from exc
        lower, upper = self.gather_cost_range
        (
            token,
            init_parsed,
            verify_token,
            verify_parsed,
            gather_cost_normalized,
        ) = _normalize_gather_cost_pair(
            token,
            init_parsed,
            verify_token,
            verify_parsed,
            salt=salt,
            lower=lower,
            upper=upper,
        )
        if init_parsed.session_id != verify_parsed.session_id:
            raise DeviceRuntimeError("Init/Verify DeviceToken 会话不一致")

        device_config = _parse_device_config_object(
            payload.get("deviceConfig")
        )
        if (
            init_parsed.session_id != device_config.session_id
            or verify_parsed.session_id != device_config.session_id
        ):
            raise DeviceRuntimeError(
                "DeviceToken session 与本轮 Log1 DeviceConfig 不一致"
            )
        verify_arg_key, verify_arg_plaintext = (
            _parse_verify_arg_profile(payload.get("verifyArgProfile"))
        )

        fields = _fingerprint_fields(
            verify_parsed,
            device_config=device_config,
        )
        try:
            collector_started_ms = int(fields[72], 10)
            token_time_ms = int(fields[74], 10)
        except (IndexError, ValueError) as exc:
            raise DeviceRuntimeError(
                "DeviceToken 的 FeiLin 时钟字段无效"
            ) from exc
        if (
            collector_started_ms <= 0
            or token_time_ms < collector_started_ms
        ):
            raise DeviceRuntimeError(
                "DeviceToken 的 FeiLin 时序倒置"
            )
        getter_argument_count = payload.get("getterArgumentCount")
        if getter_argument_count is not None and (
            isinstance(getter_argument_count, bool)
            or not isinstance(getter_argument_count, int)
            or getter_argument_count < 0
        ):
            raise DeviceRuntimeError(
                "Node 设备桥返回了无效 getter 参数计数"
            )
        if (
            expected_getter_argument_count is not None
            and getter_argument_count
            != expected_getter_argument_count
        ):
            raise DeviceRuntimeError(
                "challenge-worker 未按动态 PE getter 合同刷新"
            )
        interaction_event_count = payload.get("interactionEventCount")
        if interaction_event_count is not None and (
            isinstance(interaction_event_count, bool)
            or not isinstance(interaction_event_count, int)
            or interaction_event_count < 1
            or interaction_event_count > 512
        ):
            raise DeviceRuntimeError(
                "Node 设备桥返回了无效交互事件计数"
            )
        if (
            expected_interaction_event_count is not None
            and interaction_event_count
            != expected_interaction_event_count
        ):
            raise DeviceRuntimeError(
                "challenge-worker 未消费完整 PE mousemove 流"
            )

        requests_value = payload.get("requests")
        actions: list[str] = []
        if isinstance(requests_value, list):
            for item in requests_value:
                if isinstance(item, dict) and isinstance(
                    item.get("action"), str
                ):
                    actions.append(item["action"])
        request_count = payload.get("requestCount")
        if not isinstance(request_count, int) or request_count < 1:
            raise DeviceRuntimeError("Node 设备桥返回了无效请求计数")
        if (
            expected_getter_argument_count is not None
            and not {"Log1", "Log2"}.issubset(actions)
        ):
            raise DeviceRuntimeError(
                "challenge-worker 未完成 Log1/Log2 设备链"
            )

        return DeviceRuntimeResult(
            init_token=token,
            verify_token=verify_token,
            verify_arg_key=verify_arg_key,
            verify_arg_plaintext=verify_arg_plaintext,
            device_config=device_config,
            init_parsed=init_parsed,
            verify_parsed=verify_parsed,
            token_source=str(payload.get("tokenSource", "")),
            request_count=request_count,
            request_actions=tuple(actions),
            gather_cost_normalized=gather_cost_normalized,
            getter_argument_count=getter_argument_count,
            fingerprint_field_count=len(fields),
            collector_started_ms=collector_started_ms,
            token_time_ms=token_time_ms,
            interaction_event_count=interaction_event_count,
        )

    def _run_bridge(self, sdk_path: Path) -> DeviceRuntimeResult:
        if not self.bridge_script.is_file():
            raise DeviceRuntimeError(
                f"Node 设备桥不存在：{self.bridge_script}"
            )
        command = [
            self.node_binary,
            str(self.bridge_script),
            "--mode",
            "live-token",
            "--sdk",
            str(sdk_path),
            "--prefix",
            self.prefix,
            "--region",
            self.region,
            "--timeout-ms",
            str(round(self.timeout * 1000)),
        ]
        try:
            completed = subprocess.run(
                command,
                capture_output=True,
                text=True,
                check=True,
                timeout=self.timeout + 5,
            )
        except FileNotFoundError as exc:
            raise DeviceRuntimeError(
                f"找不到 Node：{self.node_binary}"
            ) from exc
        except subprocess.TimeoutExpired as exc:
            raise DeviceRuntimeError("Node 设备桥执行超时") from exc
        except subprocess.CalledProcessError as exc:
            raise DeviceRuntimeError(
                "Node 设备桥失败：" + _bridge_error(exc.stderr)
            ) from exc

        return self._result_from_payload(
            _parse_bridge_output(completed.stdout)
        )

    def generate(self) -> DeviceRuntimeResult:
        """下载（或使用指定）SDK，执行一次正常 Log1 并返回 token。"""

        if self.sdk_path is not None:
            if not self.sdk_path.is_file():
                raise DeviceRuntimeError(f"SDK 文件不存在：{self.sdk_path}")
            return self._run_bridge(self.sdk_path)

        with tempfile.TemporaryDirectory(prefix="ali-slider-sdk-") as directory:
            sdk_path = Path(directory, "AliyunCaptcha.js")
            _download_public_sdk(
                self.sdk_url,
                sdk_path,
                timeout=self.timeout,
                proxies=self.proxies,
            )
            return self._run_bridge(sdk_path)

    @contextmanager
    def challenge_session(
        self,
    ) -> Iterator["DeviceRuntimeSession"]:
        """保持一个 Node/FeiLin VM 跨越 Init，随后生成绑定 token。"""

        if self.sdk_path is not None:
            if not self.sdk_path.is_file():
                raise DeviceRuntimeError(
                    f"SDK 文件不存在：{self.sdk_path}"
                )
            with DeviceRuntimeSession(self, self.sdk_path) as session:
                yield session
            return

        with tempfile.TemporaryDirectory(
            prefix="ali-slider-sdk-"
        ) as directory:
            sdk_path = Path(directory, "AliyunCaptcha.js")
            _download_public_sdk(
                self.sdk_url,
                sdk_path,
                timeout=self.timeout,
                proxies=self.proxies,
            )
            with DeviceRuntimeSession(self, sdk_path) as session:
                yield session


class DeviceRuntimeSession:
    """跨 Python Init 保持同一 FeiLin 会话的双阶段 Node worker。"""

    def __init__(
        self,
        client: DeviceRuntimeClient,
        sdk_path: Path,
    ) -> None:
        self.client = client
        self.sdk_path = sdk_path
        self.process: subprocess.Popen[str] | None = None
        self._init_payload: dict[str, Any] | None = None
        self._init_result: DeviceRuntimeResult | None = None
        self._completed = False
        self._target_first_touch_age_ms: int | None = None

    def _command(self) -> list[str]:
        return [
            self.client.node_binary,
            str(self.client.bridge_script),
            "--mode",
            "challenge-worker",
            "--sdk",
            str(self.sdk_path),
            "--prefix",
            self.client.prefix,
            "--region",
            self.client.region,
            "--timeout-ms",
            str(round(self.client.timeout * 1000)),
        ]

    def _read_stage(self, expected: str) -> dict[str, Any]:
        process = self.process
        if process is None or process.stdout is None:
            raise DeviceRuntimeError("challenge-worker 尚未启动")
        deadline = time.monotonic() + self.client.timeout + 5
        while time.monotonic() < deadline:
            remaining = max(0.0, deadline - time.monotonic())
            readable, _, _ = select.select(
                [process.stdout],
                [],
                [],
                remaining,
            )
            if not readable:
                break
            line = process.stdout.readline()
            if not line:
                if process.poll() is not None:
                    break
                continue
            try:
                payload = json.loads(line)
            except json.JSONDecodeError:
                continue
            if (
                isinstance(payload, dict)
                and payload.get("stage") == expected
                and isinstance(payload.get("deviceToken"), str)
            ):
                return payload

        detail = "未返回结构化错误"
        if process.stderr is not None and process.poll() is not None:
            detail = _bridge_error(process.stderr.read())
        raise DeviceRuntimeError(
            f"challenge-worker 未返回 {expected} 阶段：{detail}"
        )

    def __enter__(self) -> "DeviceRuntimeSession":
        if not self.client.bridge_script.is_file():
            raise DeviceRuntimeError(
                f"Node 设备桥不存在：{self.client.bridge_script}"
            )
        try:
            self.process = subprocess.Popen(
                self._command(),
                stdin=subprocess.PIPE,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
                bufsize=1,
            )
        except FileNotFoundError as exc:
            raise DeviceRuntimeError(
                f"找不到 Node：{self.client.node_binary}"
            ) from exc
        try:
            self._init_payload = self._read_stage("init")
            # 首阶段也立即校验容器、Log1 session 与 DeviceConfig。
            self._init_result = self.client._result_from_payload(
                self._init_payload
            )
        except Exception:
            self.close()
            raise
        return self

    @property
    def init_token(self) -> str:
        if self._init_result is None:
            raise DeviceRuntimeError("challenge-worker 尚未完成 Init 阶段")
        return self._init_result.init_token

    @property
    def initial_result(self) -> DeviceRuntimeResult:
        """返回 Log1/Log2 首阶段已经完整校验的运行态。

        动态 PE 初始化使用页面在 Init 前已持有的 DeviceConfig 与 Init token；
        最终 Verify token 要等 PE 观测 getter 参数后再在同一 FeiLin VM 刷新。
        """

        if self._init_result is None:
            raise DeviceRuntimeError("challenge-worker 尚未完成 Init 阶段")
        return self._init_result

    @property
    def target_first_touch_age_ms(self) -> int:
        """为本轮抽样一次首 touch 相对 TrackStartTime 的目标年龄。"""

        if self._completed:
            raise DeviceRuntimeError("本轮 challenge-worker 已完成")
        if self._init_result is None:
            raise DeviceRuntimeError("challenge-worker 尚未完成 Init 阶段")
        if self._target_first_touch_age_ms is None:
            lower, upper = self.client.first_touch_age_range
            self._target_first_touch_age_ms = (
                lower + secrets.randbelow(upper - lower + 1)
            )
        return self._target_first_touch_age_ms

    def complete_challenge(
        self,
        getter_arguments: tuple[str, ...],
        interaction_events: tuple[dict[str, Any], ...],
        *,
        post_interaction_delay_ms: int,
    ) -> DeviceRuntimeResult:
        """在同一 VM 回放 PE mousemove 流后调用实际一参 ``getToken``。"""

        if self._completed:
            raise DeviceRuntimeError("本轮 challenge-worker 已完成")
        if (
            not isinstance(getter_arguments, tuple)
            or len(getter_arguments) != 1
            or any(
                not isinstance(value, str)
                or not value
                or len(value) > 512
                for value in getter_arguments
            )
        ):
            raise ValueError(
                "getter_arguments 必须是含一个 1..512 字符字符串的 tuple"
            )
        normalized_events = _normalize_interaction_events(
            interaction_events
        )
        if (
            isinstance(post_interaction_delay_ms, bool)
            or not isinstance(post_interaction_delay_ms, int)
            or not 0 <= post_interaction_delay_ms <= 500
        ):
            raise ValueError(
                "post_interaction_delay_ms 必须是 0..500 的整数"
            )
        process = self.process
        if (
            process is None
            or process.stdin is None
            or process.poll() is not None
        ):
            raise DeviceRuntimeError("challenge-worker 已退出")
        try:
            process.stdin.write(
                json.dumps(
                    {
                        "complete": True,
                        "getterArguments": list(getter_arguments),
                        "interactionEvents": list(normalized_events),
                        "postInteractionDelayMs": (
                            post_interaction_delay_ms
                        ),
                    },
                    ensure_ascii=False,
                    separators=(",", ":"),
                )
            )
            process.stdin.close()
        except (BrokenPipeError, OSError) as exc:
            raise DeviceRuntimeError(
                "无法把挑战完成信号交给 challenge-worker"
            ) from exc

        payload = self._read_stage("verify")
        try:
            return_code = process.wait(timeout=self.client.timeout + 5)
        except subprocess.TimeoutExpired as exc:
            raise DeviceRuntimeError(
                "challenge-worker 完成后未正常退出"
            ) from exc
        if return_code != 0:
            detail = (
                _bridge_error(process.stderr.read())
                if process.stderr is not None
                else "未返回结构化错误"
            )
            raise DeviceRuntimeError(
                "challenge-worker 失败：" + detail
            )
        if process.stderr is not None and process.stderr.read().strip():
            raise DeviceRuntimeError(
                "challenge-worker 存在未处理的 FeiLin 采集异常"
            )
        self._completed = True
        result_payload = dict(payload)
        # 保留真正发给 InitCaptchaV3 的（可能已正常化 GatherCost）token。
        result_payload["deviceToken"] = self.init_token
        return self.client._result_from_payload(
            result_payload,
            expected_getter_argument_count=len(getter_arguments),
            expected_interaction_event_count=len(normalized_events),
        )

    def close(self) -> None:
        process = self.process
        if process is None:
            return
        if process.stdin is not None and not process.stdin.closed:
            process.stdin.close()
        if process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=2)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=2)
        self.process = None

    def __exit__(
        self,
        exc_type: Any,
        exc: Any,
        traceback: Any,
    ) -> None:
        self.close()


__all__ = [
    "DEFAULT_FIRST_TOUCH_AGE_RANGE",
    "DEFAULT_SDK_URL",
    "DeviceRuntimeClient",
    "DeviceRuntimeError",
    "DeviceRuntimeResult",
    "DeviceRuntimeSession",
]
