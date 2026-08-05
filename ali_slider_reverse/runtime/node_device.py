"""FeiLin 设备链：跨 Init 保持同一个 Node VM，生成两阶段 DeviceToken。

## 为什么必须是同一个 VM

DeviceToken 不是一段静态指纹字符串，也不能在 Init 与 Verify 阶段各起一个互不
相关的环境。正确链路是：

```text
公开 SDK / FeiLin
  → Log1 → 解密 DeviceConfig → Log2 → Init DeviceToken
  → 保持 worker 与 session 存活
  → 回放本轮 PE getter 前已经发生的交互
  → 实际一参 getToken(...)
  → Log3 → Log2 → Verify DeviceToken
```

只替换 UA、伪造字段数量，或者为 Verify 重新启动第二个 VM，都会破坏跨阶段的
状态一致性。因此 :class:`DeviceRuntimeSession` 始终独占一个 VM 承载整轮
挑战，Python 侧只在两个阶段之间与它通信。标准路径一 VM 一 Node 进程；
快速池可让多个隔离 VM 共享一个 Node host，但不共享任何挑战状态。

## 职责边界

Python 负责下载与校验公开 ``AliyunCaptcha.js``、驱动 Node 桥、独立验证返回的
token 容器/session/时序；浏览器环境依赖较重的 Log1、DeviceConfig 解密与 FeiLin
采集过程全部由公开脚本自己在 Node 里完成。
"""

from __future__ import annotations

import hashlib
import json
import math
import os
import queue
import secrets
import subprocess
import tempfile
import threading
import time
from collections.abc import Iterator
from contextlib import ExitStack, contextmanager
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any
from urllib.parse import urlparse

from .. import config
from ..device_profile import DeviceProfile, encode_device_profile
from ..errors import DeviceRuntimeError
from ..protocol.device_token import (
    DeviceConfig,
    DeviceToken,
    aes_cbc_decrypt_base64,
    build_device_token,
    parse_device_token,
)
from ..protocol.secrets import resolve_frontend_secrets


# FeiLin 指纹明文里承载采集时钟的两个字段下标。
_FIELD_COLLECTOR_STARTED = 72
_FIELD_TOKEN_TIME = 74

_MAX_INTERACTION_EVENTS = 512
_MAX_INTERACTION_SPAN_MS = 60_000
_MAX_HOST_VM_COUNT = 8

_SDK_MINIMUM_BYTES = 50_000
"""公开 SDK 的最小合理体积；低于此值一定不是那个脚本。"""


@dataclass(frozen=True, slots=True)
class DeviceRuntimeResult:
    """Node 桥返回、并已由 Python 独立校验过的运行态。"""

    init_token: str = field(repr=False)
    """发给 ``InitCaptchaV3`` 的 DeviceToken。"""

    verify_token: str = field(repr=False)
    """PE 观测到 getter 参数后刷新出的、发给 ``VerifyCaptchaV3`` 的 DeviceToken。"""

    verify_arg_key: str = field(repr=False)
    verify_arg_plaintext: str = field(repr=False)
    """公开 SDK 传给动态 PE 的 ``tx(key, plaintext)`` 实参。"""

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


# ==========================================================================
# 桥输出解析
# ==========================================================================


def _bridge_error(stderr: str) -> str:
    """提取桥的结构化错误。

    直接把 stderr 塞进异常会带出整行混淆脚本甚至 token，所以只取桥自己输出的
    ``{"error": ...}`` 字段并截断。
    """

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


def _close_process_pipes(process: subprocess.Popen[str]) -> None:
    """子进程退出后显式关闭 Popen 管道，避免常驻服务泄漏 fd。"""

    for stream in (process.stdin, process.stdout, process.stderr):
        if stream is None or stream.closed:
            continue
        try:
            stream.close()
        except OSError:
            pass


def _parse_device_config(value: Any) -> DeviceConfig:
    """校验 Node VM 已解出的 DeviceConfig，不接受静默缺字段。"""

    if not isinstance(value, dict):
        raise DeviceRuntimeError("Node 设备桥缺少解密后的 DeviceConfig")
    for name in (
        "key",
        "sessionId",
        "version",
        "pluginElements",
        "pluginResource",
        "globalVariable",
        "timestamp",
        "ip",
    ):
        if not isinstance(value.get(name), str):
            raise DeviceRuntimeError(f"Node DeviceConfig.{name} 类型无效")
    if len(value["key"].encode("utf-8")) != 16:
        raise DeviceRuntimeError("Node DeviceConfig.key 不是 16 字节")
    if not value["sessionId"]:
        raise DeviceRuntimeError("Node DeviceConfig.sessionId 为空")

    switch = value.get("switch")
    if isinstance(switch, bool) or not isinstance(switch, int):
        raise DeviceRuntimeError("Node DeviceConfig.switch 类型无效")
    extra = value.get("extraSegments", [])
    if not isinstance(extra, list) or not all(
        isinstance(item, str) for item in extra
    ):
        raise DeviceRuntimeError("Node DeviceConfig.extraSegments 类型无效")

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
    """读取公开 SDK 传给动态 PE 的 ``tx(key, plaintext)`` 实参。

    公开 SDK 仍把这两个运行态字段提供给 PE，因此保持显式校验与传递；但当前 PE
    的黑盒差分已确认 ``arg`` 只随 CertifyId 变化——不要把旧版 ACCESS_SEC /
    SESSION_ID_SALT 的调用形态误写成当前 ``arg`` 的固定语义。
    """

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
            raise DeviceRuntimeError(f"Node Verify arg profile.{label} 无效")
    return access_sec, session_id_salt


def _fingerprint_fields(
    parsed: DeviceToken,
    *,
    device_config: DeviceConfig,
) -> tuple[str, ...]:
    """用本轮 DeviceConfig 的 key 解开指纹密文，返回 ``#`` 分段。

    解不开就说明 token 与 DeviceConfig 不是同一次采集，属于致命的会话错配。
    """

    try:
        plaintext = aes_cbc_decrypt_base64(
            parsed.fingerprint_cipher, key=device_config.key
        )
        text = plaintext.decode("utf-8")
    except (UnicodeDecodeError, ValueError) as exc:
        raise DeviceRuntimeError(
            "Verify DeviceToken 指纹密文无法按本次 DeviceConfig 解开"
        ) from exc
    return tuple(text.split("#"))


# ==========================================================================
# GatherCost 正常化
# ==========================================================================


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
    """用同一个采集耗时归一化同一 session 的两枚 token。

    FeiLin 把 ``GatherCost`` 单独放在 token 第四段，不参与指纹密文，所以可以在
    不动 session/cipher 的前提下重建容器与 MD5。

    真实页面的两枚 token 共享同一次采集耗时（约 226ms），但 Node 桥会在 Init 与
    Verify 阶段各测一次：空闲时两次都落到 0ms，并发抢 CPU 时两次都超过下限且互不
    相等。这是桥的测量方式使然，不代表 session 不一致——因此统一取**先发生的
    Init 耗时**，与真实页面里 Verify token 复用 Init 之前那次采集的行为一致。
    """

    init_cost = int(init_parsed.gather_cost)
    verify_cost = int(verify_parsed.gather_cost)
    valid_costs = [cost for cost in (init_cost, verify_cost) if cost >= lower]
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
    return init_token, init_parsed, verify_token, verify_parsed, changed


# ==========================================================================
# 桥返回值 → 已校验运行态
# ==========================================================================


def _build_result(
    payload: dict[str, Any],
    *,
    gather_cost_range: tuple[int, int],
    expected_getter_argument_count: int | None = None,
    expected_interaction_event_count: int | None = None,
) -> DeviceRuntimeResult:
    """把桥的原始输出转成经过完整校验的 :class:`DeviceRuntimeResult`。

    校验链：token 容器与 MD5 → GatherCost 正常化 → Init/Verify 同 session →
    session 与 DeviceConfig 一致 → 指纹可解密且 FeiLin 时钟不倒置 → getter 参数
    与交互事件数符合调用方预期 → Log1/Log2 请求链完整。任何一环不满足都直接
    失败，不做静默降级。
    """

    token = payload.get("deviceToken")
    if not isinstance(token, str) or not token:
        raise DeviceRuntimeError("Node 设备桥返回了空 Init token")
    verify_token = payload.get("verifyDeviceToken")
    if expected_getter_argument_count is None and verify_token is None:
        # Init 阶段还没有刷新过 token，两枚是同一个。
        verify_token = token
    if not isinstance(verify_token, str) or not verify_token:
        raise DeviceRuntimeError("Node 设备桥返回了空 Verify token")

    # salt 仍从公开前端密文运行时恢复，不把二次明文写进源码。
    salt = resolve_frontend_secrets().device_token_salt
    try:
        init_parsed = parse_device_token(token, salt=salt)
        verify_parsed = parse_device_token(verify_token, salt=salt)
    except ValueError as exc:
        raise DeviceRuntimeError(
            "Node 设备桥返回的 DeviceToken 容器校验失败"
        ) from exc

    lower, upper = gather_cost_range
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

    device_config = _parse_device_config(payload.get("deviceConfig"))
    if (
        init_parsed.session_id != device_config.session_id
        or verify_parsed.session_id != device_config.session_id
    ):
        raise DeviceRuntimeError(
            "DeviceToken session 与本轮 Log1 DeviceConfig 不一致"
        )

    verify_arg_key, verify_arg_plaintext = _parse_verify_arg_profile(
        payload.get("verifyArgProfile")
    )

    fields = _fingerprint_fields(verify_parsed, device_config=device_config)
    try:
        collector_started_ms = int(fields[_FIELD_COLLECTOR_STARTED], 10)
        token_time_ms = int(fields[_FIELD_TOKEN_TIME], 10)
    except (IndexError, ValueError) as exc:
        raise DeviceRuntimeError("DeviceToken 的 FeiLin 时钟字段无效") from exc
    if collector_started_ms <= 0 or token_time_ms < collector_started_ms:
        raise DeviceRuntimeError("DeviceToken 的 FeiLin 时序倒置")

    getter_argument_count = payload.get("getterArgumentCount")
    if getter_argument_count is not None and (
        isinstance(getter_argument_count, bool)
        or not isinstance(getter_argument_count, int)
        or getter_argument_count < 0
    ):
        raise DeviceRuntimeError("Node 设备桥返回了无效 getter 参数计数")
    if (
        expected_getter_argument_count is not None
        and getter_argument_count != expected_getter_argument_count
    ):
        raise DeviceRuntimeError("challenge-worker 未按动态 PE getter 合同刷新")

    interaction_event_count = payload.get("interactionEventCount")
    if interaction_event_count is not None and (
        isinstance(interaction_event_count, bool)
        or not isinstance(interaction_event_count, int)
        or not 1 <= interaction_event_count <= _MAX_INTERACTION_EVENTS
    ):
        raise DeviceRuntimeError("Node 设备桥返回了无效交互事件计数")
    if (
        expected_interaction_event_count is not None
        and interaction_event_count != expected_interaction_event_count
    ):
        raise DeviceRuntimeError("challenge-worker 未消费完整 PE mousemove 流")

    actions: list[str] = []
    requests_value = payload.get("requests")
    if isinstance(requests_value, list):
        actions = [
            item["action"]
            for item in requests_value
            if isinstance(item, dict) and isinstance(item.get("action"), str)
        ]
    request_count = payload.get("requestCount")
    if not isinstance(request_count, int) or request_count < 1:
        raise DeviceRuntimeError("Node 设备桥返回了无效请求计数")
    if expected_getter_argument_count is not None and not {
        "Log1",
        "Log2",
    }.issubset(actions):
        raise DeviceRuntimeError("challenge-worker 未完成 Log1/Log2 设备链")

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


def _normalize_interaction_events(
    events: tuple[dict[str, Any], ...],
) -> tuple[dict[str, float | bool | str], ...]:
    """校验并归一化要回放给 FeiLin 的 PE mousemove 流。

    这些事件必须是 PE VM 里真实产生过的：时间戳单调不减、坐标有界、总跨度不超过
    60 秒。回放伪造的事件会让 FeiLin 的采集与 PE 的 payload 对不上。
    """

    if not isinstance(events, tuple) or not 1 <= len(events) <= _MAX_INTERACTION_EVENTS:
        raise ValueError(
            f"interaction_events 必须包含 1..{_MAX_INTERACTION_EVENTS} 个 PE mousemove 事件"
        )

    normalized: list[dict[str, float | bool | str]] = []
    previous_time: float | None = None
    for index, event in enumerate(events):
        if (
            not isinstance(event, dict)
            or set(event) != {"type", "x", "y", "timeStamp", "isTrusted"}
            or event.get("type") != "mousemove"
            or event.get("isTrusted") is not True
        ):
            raise ValueError(f"interaction_events[{index}] 结构无效")

        x, y, timestamp = event["x"], event["y"], event["timeStamp"]
        if (
            any(
                isinstance(item, bool) or not isinstance(item, (int, float))
                for item in (x, y, timestamp)
            )
            or not all(math.isfinite(float(item)) for item in (x, y, timestamp))
            or abs(float(x)) > 10_000
            or abs(float(y)) > 10_000
            or not 0 <= float(timestamp) <= 180_000
            or (previous_time is not None and float(timestamp) < previous_time)
        ):
            raise ValueError(f"interaction_events[{index}] 数值无效")

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

    span = float(normalized[-1]["timeStamp"]) - float(normalized[0]["timeStamp"])
    if span > _MAX_INTERACTION_SPAN_MS:
        raise ValueError("interaction_events 总时长不能超过 60 秒")
    return tuple(normalized)


def _fetch_public_sdk(
    url: str,
    *,
    timeout: float,
    proxies: dict[str, str] | None = None,
) -> bytes:
    """下载公开 AliyunCaptcha.js 并做结构性检查。

    检查的是"看起来像不像那个 SDK"，不是固定版本锁定——验证码前端会正常热更新，
    锁死版本反而会让工具在下一次更新后直接失效。
    """

    parsed = urlparse(url)
    if parsed.scheme != "https" or parsed.hostname not in config.ALLOWED_SDK_HOSTS:
        raise DeviceRuntimeError("SDK URL 必须是允许的 alicdn HTTPS 地址")
    try:
        import requests
    except ImportError as exc:  # pragma: no cover - 由调用环境决定。
        raise DeviceRuntimeError("下载公开 SDK 需要 requests") from exc

    response = requests.get(url, timeout=timeout, proxies=proxies)
    response.raise_for_status()
    source = response.content
    if len(source) < _SDK_MINIMUM_BYTES or b"AliyunCaptchaConfig" not in source:
        raise DeviceRuntimeError("下载内容不像 AliyunCaptcha.js")
    return source


# ==========================================================================
# 公开 SDK 的本地缓存
#
# SDK 是公开静态资源，与本轮挑战无关，却在每轮最开头串行下载一次。缓存只按
# TTL 失效：条件请求同样要付一次完整往返，省不下多少，不如过期就重下。
# ==========================================================================


def _sdk_cache_path(url: str) -> Path | None:
    """返回该 URL 的缓存文件路径；无法确定缓存目录时返回 ``None``。"""

    try:
        base = os.environ.get("XDG_CACHE_HOME")
        root = Path(base) if base else Path.home() / ".cache"
        digest = hashlib.sha256(url.encode("utf-8")).hexdigest()[:16]
        return root / "ali-slider" / f"AliyunCaptcha-{digest}.js"
    except Exception:  # pragma: no cover - 取不到家目录时退回不缓存。
        return None


def _fresh_cached_sdk(url: str, *, ttl_seconds: float) -> Path | None:
    """命中且仍在有效期内的缓存文件，否则 ``None``。

    只做 stat 级检查：文件是写入前已通过结构校验的，重新读一遍 225KB 只为再确认
    一次并不划算；大小明显不对则视为未命中。
    """

    if ttl_seconds <= 0:
        return None
    path = _sdk_cache_path(url)
    if path is None:
        return None
    try:
        stat = path.stat()
    except OSError:
        return None
    if stat.st_size < _SDK_MINIMUM_BYTES:
        return None
    if time.time() - stat.st_mtime > ttl_seconds:
        return None
    return path


def _store_cached_sdk(url: str, source: bytes) -> Path | None:
    """把已校验的 SDK 原子写入缓存；不可写时返回 ``None`` 由调用方兜底。

    先写临时文件再 ``os.replace``：并发的另一轮可能正在读同一路径，绝不能让它
    读到写了一半的脚本。
    """

    path = _sdk_cache_path(url)
    if path is None:
        return None
    temporary = path.with_name(f"{path.name}.{os.getpid()}.tmp")
    try:
        path.parent.mkdir(parents=True, exist_ok=True)
        temporary.write_bytes(source)
        os.replace(temporary, path)
    except OSError:
        try:
            temporary.unlink(missing_ok=True)
        except OSError:  # pragma: no cover - 清理失败无需上报。
            pass
        return None
    return path


# ==========================================================================
# 客户端与会话
# ==========================================================================


def _require_positive_range(
    value: tuple[int, int],
    *,
    label: str,
    maximum: int | None = None,
) -> None:
    """校验闭区间参数：两端都是正整数且下界不大于上界。"""

    if (
        len(value) != 2
        or any(isinstance(item, bool) for item in value)
        or not all(isinstance(item, int) for item in value)
        or value[0] < 1
        or value[1] < value[0]
        or (maximum is not None and value[1] > maximum)
    ):
        bound = f"、上界不超过 {maximum}" if maximum is not None else ""
        raise ValueError(f"{label} 必须是正整数闭区间{bound}")


class DeviceRuntimeClient:
    """设备桥的配置载体；每轮挑战由它开出一个 :class:`DeviceRuntimeSession`。"""

    def __init__(
        self,
        *,
        node_binary: str = config.DEFAULT_NODE_BINARY,
        bridge_script: str | Path | None = None,
        sdk_path: str | Path | None = None,
        sdk_url: str = config.SDK_URL,
        prefix: str = config.DEFAULT_PREFIX,
        region: str = config.DEFAULT_REGION,
        timeout: float = config.DEFAULT_TIMEOUT,
        gather_cost_range: tuple[int, int] = config.DEFAULT_GATHER_COST_RANGE,
        first_touch_age_range: tuple[int, int] = (
            config.DEFAULT_FIRST_TOUCH_AGE_RANGE
        ),
        sdk_cache_ttl_seconds: float = config.SDK_CACHE_TTL_SECONDS,
        proxies: dict[str, str] | None = None,
        device_profile: DeviceProfile,
    ) -> None:
        if timeout <= 0:
            raise ValueError("timeout 必须为正数")
        if not isinstance(device_profile, DeviceProfile):
            raise ValueError("device_profile 必须是本轮生成的 DeviceProfile")
        if not prefix:
            raise ValueError("prefix 不能为空")
        if not region:
            raise ValueError("region 不能为空")
        if proxies is not None and not isinstance(proxies, dict):
            raise ValueError("proxies 必须是 requests 的映射或 None")
        _require_positive_range(gather_cost_range, label="gather_cost_range")
        _require_positive_range(
            first_touch_age_range,
            label="first_touch_age_range",
            maximum=120_000,
        )

        self.node_binary = node_binary
        self.bridge_script = (
            config.SDK_DEVICE_BRIDGE if bridge_script is None else Path(bridge_script)
        )
        self.sdk_path = None if sdk_path is None else Path(sdk_path)
        self.sdk_url = sdk_url
        self.prefix = prefix
        self.region = region
        self.timeout = float(timeout)
        self.gather_cost_range = gather_cost_range
        self.first_touch_age_range = first_touch_age_range
        self.sdk_cache_ttl_seconds = float(sdk_cache_ttl_seconds)
        self.proxies = dict(proxies) if proxies else None
        self.device_profile = device_profile

    @contextmanager
    def challenge_session(self) -> Iterator["DeviceRuntimeSession"]:
        """开启一轮挑战：准备好 SDK 文件，并保证退出时回收 Node 进程。"""

        with ExitStack() as stack:
            sdk_path = self._prepare_sdk(stack)
            yield stack.enter_context(DeviceRuntimeSession(self, sdk_path))

    def open_challenge_sessions(
        self, count: int
    ) -> tuple["DeviceRuntimeSession", ...]:
        """在一个 Node host 内打开 ``count`` 个隔离 VM 会话。

        返回的每个 session 都有独立 ``sessionId`` 和 VM；最后一个
        session 关闭时自动回收共享 Node 进程与 SDK 临时文件。单个 session
        可原位重建 Worker/VM，不影响同 host 的其他 session。
        """

        if (
            isinstance(count, bool)
            or not isinstance(count, int)
            or not 1 <= count <= _MAX_HOST_VM_COUNT
        ):
            raise ValueError(f"count 必须是 1..{_MAX_HOST_VM_COUNT} 的整数")

        stack = ExitStack()
        try:
            sdk_path = self._prepare_sdk(stack)
            host = _DeviceRuntimeHost(self, sdk_path, count, owner_stack=stack)
            return host.start()
        except Exception:
            stack.close()
            raise

    def _prepare_sdk(self, stack: ExitStack) -> Path:
        """定位本轮要用的公开 SDK 文件。

        优先级：显式指定的本地副本 → 仍在有效期内的缓存 → 现下载。下载后尽量写入
        缓存；缓存目录不可写时退回临时目录，临时目录的生命周期由调用方的 stack
        覆盖整个 session，因为 Node 进程会一直读它。
        """

        if self.sdk_path is not None:
            if not self.sdk_path.is_file():
                raise DeviceRuntimeError(f"SDK 文件不存在：{self.sdk_path}")
            return self.sdk_path

        cached = _fresh_cached_sdk(
            self.sdk_url, ttl_seconds=self.sdk_cache_ttl_seconds
        )
        if cached is not None:
            return cached

        source = _fetch_public_sdk(
            self.sdk_url,
            timeout=self.timeout,
            proxies=self.proxies,
        )
        if self.sdk_cache_ttl_seconds > 0:
            stored = _store_cached_sdk(self.sdk_url, source)
            if stored is not None:
                return stored

        directory = stack.enter_context(
            tempfile.TemporaryDirectory(prefix="ali-slider-sdk-")
        )
        path = Path(directory, "AliyunCaptcha.js")
        path.write_bytes(source)
        return path


class _DeviceRuntimeHost:
    """一个 Node 进程中持有多个隔离 FeiLin VM 槽位。"""

    def __init__(
        self,
        client: DeviceRuntimeClient,
        sdk_path: Path,
        count: int,
        *,
        owner_stack: ExitStack,
    ) -> None:
        self.client = client
        self.sdk_path = sdk_path
        self.count = count
        self.process: subprocess.Popen[str] | None = None
        self._queues: dict[
            int, queue.Queue[tuple[float, dict[str, Any]] | None]
        ] = {
            session_id: queue.Queue() for session_id in range(count)
        }
        self._stage_received_at: dict[tuple[int, str], float] = {}
        self._stdout_reader: threading.Thread | None = None
        self._write_lock = threading.Lock()
        self._state_lock = threading.Lock()
        self._open_sessions = set(range(count))
        self._closed = False
        self._owner_stack = owner_stack

    def start(self) -> tuple["DeviceRuntimeSession", ...]:
        """启动 host，等待全部 VM 完成 Init 后返回逻辑会话。"""

        if not self.client.bridge_script.is_file():
            raise DeviceRuntimeError(
                f"Node 设备桥不存在：{self.client.bridge_script}"
            )
        try:
            self.process = subprocess.Popen(
                [
                    self.client.node_binary,
                    str(self.client.bridge_script),
                    "--mode",
                    "challenge-host",
                    "--sdk",
                    str(self.sdk_path),
                    "--prefix",
                    self.client.prefix,
                    "--region",
                    self.client.region,
                    "--timeout-ms",
                    str(round(self.client.timeout * 1000)),
                    "--device-profile",
                    encode_device_profile(self.client.device_profile),
                    "--vm-count",
                    str(self.count),
                    "--persistent-host",
                ],
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
            self._start_stdout_reader()
            deadline = time.monotonic() + self.client.timeout + 5
            payloads = tuple(
                self._read_stage(session_id, "init", deadline=deadline)
                for session_id in range(self.count)
            )
            return tuple(
                DeviceRuntimeSession(
                    self.client,
                    self.sdk_path,
                    host=self,
                    session_id=session_id,
                    initial_payload=payload,
                    initialized_at=self._stage_received_at[(session_id, "init")],
                )
                for session_id, payload in enumerate(payloads)
            )
        except Exception:
            self.close()
            raise

    def _start_stdout_reader(self) -> None:
        process = self.process
        if process is None or process.stdout is None:
            raise DeviceRuntimeError("challenge-host 尚未启动")
        reader = threading.Thread(
            target=self._drain_stdout,
            name="ali-device-host-stdout",
            daemon=True,
        )
        self._stdout_reader = reader
        reader.start()

    def _drain_stdout(self) -> None:
        """将 host 输出按 ``sessionId`` 分流到各逻辑会话。"""

        process = self.process
        stream = None if process is None else process.stdout
        if stream is None:
            for lines in self._queues.values():
                lines.put(None)
            return
        try:
            for line in stream:
                try:
                    payload = json.loads(line)
                except json.JSONDecodeError:
                    continue
                if not isinstance(payload, dict):
                    continue
                session_id = payload.get("sessionId")
                lines = self._queues.get(session_id)
                if lines is not None:
                    lines.put((time.monotonic(), payload))
        finally:
            for lines in self._queues.values():
                lines.put(None)

    def _read_stage(
        self,
        session_id: int,
        expected: str,
        *,
        deadline: float | None = None,
    ) -> dict[str, Any]:
        process = self.process
        lines = self._queues[session_id]
        if process is None:
            raise DeviceRuntimeError("challenge-host 尚未启动")
        if deadline is None:
            deadline = time.monotonic() + self.client.timeout + 5

        while time.monotonic() < deadline:
            try:
                item = lines.get(timeout=max(0.0, deadline - time.monotonic()))
            except queue.Empty:
                break
            if item is None:
                break
            received_at, payload = item
            if payload.get("stage") == "error":
                detail = payload.get("error")
                if not isinstance(detail, str) or not detail:
                    detail = "Node 设备桥执行失败"
                raise DeviceRuntimeError(
                    f"challenge-host 会话 {session_id} 失败：{detail[:800]}"
                )
            if (
                payload.get("stage") == expected
                and isinstance(payload.get("deviceToken"), str)
            ):
                self._stage_received_at[(session_id, expected)] = received_at
                return payload

        detail = "未返回结构化错误"
        if process.stderr is not None and process.poll() is not None:
            detail = _bridge_error(process.stderr.read())
        raise DeviceRuntimeError(
            f"challenge-host 会话 {session_id} 未返回 {expected} 阶段："
            f"{detail}"
        )

    def send_completion(self, session_id: int, payload: dict[str, Any]) -> None:
        with self._state_lock:
            if self._closed or session_id not in self._open_sessions:
                raise DeviceRuntimeError("challenge-host 会话已关闭")
        self._write({"sessionId": session_id, **payload})

    def reset_session(self, session_id: int) -> tuple[dict[str, Any], float]:
        """原位重建一个已消费的 Worker/VM，并等待它重新完成 Init。"""

        with self._state_lock:
            if self._closed or session_id not in self._open_sessions:
                raise DeviceRuntimeError("challenge-host 会话已关闭")
        self._write({"sessionId": session_id, "reset": True})
        payload = self._read_stage(session_id, "init")
        return payload, self._stage_received_at[(session_id, "init")]

    def _write(self, payload: dict[str, Any]) -> None:
        process = self.process
        if process is None or process.stdin is None or process.poll() is not None:
            raise DeviceRuntimeError("challenge-host 已退出")
        try:
            with self._write_lock:
                process.stdin.write(
                    json.dumps(
                        payload,
                        ensure_ascii=False,
                        separators=(",", ":"),
                    )
                    + "\n"
                )
                process.stdin.flush()
        except (BrokenPipeError, OSError) as exc:
            raise DeviceRuntimeError("challenge-host 命令写入失败") from exc

    def release(self, session_id: int, *, completed: bool) -> None:
        """释放一个 VM 槽位；最后一个槽位负责回收 host。"""

        with self._state_lock:
            if self._closed or session_id not in self._open_sessions:
                return
            self._open_sessions.remove(session_id)
            last = not self._open_sessions
        if not completed:
            try:
                self._write({"sessionId": session_id, "cancel": True})
            except DeviceRuntimeError:
                pass
        if last:
            self.close()

    def close(self) -> None:
        """终止 host 并回收 SDK 所有者；可重复调用。"""

        with self._state_lock:
            if self._closed:
                return
            self._closed = True
            self._open_sessions.clear()
        process = self.process
        reader = self._stdout_reader
        if process is not None:
            if process.stdin is not None and not process.stdin.closed:
                try:
                    process.stdin.close()
                except (BrokenPipeError, OSError):
                    pass
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=2)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=2)
        if reader is not None and reader is not threading.current_thread():
            reader.join(timeout=0.5)
        if process is not None:
            _close_process_pipes(process)
        self._stdout_reader = None
        self.process = None
        self._owner_stack.close()


class DeviceRuntimeSession:
    """跨 Python Init 保持同一 FeiLin VM 的双阶段会话。

    两个阶段之间用行分隔的 JSON 通信：

    ```text
    __enter__            worker 启动 → Log1/Log2 → 输出 stage="init"
    (Python 侧执行 Init、下载资源、识别、跑 PE)
    complete_challenge   写入 getter 参数与 mousemove 流 → 输出 stage="verify"
    ```

    会话可独占 `challenge-worker` 进程，也可使用 `sessionId` 挂在
    共享 `challenge-host` 上；两种路径对上层暴露同一接口。
    """

    def __init__(
        self,
        client: DeviceRuntimeClient,
        sdk_path: Path,
        *,
        host: _DeviceRuntimeHost | None = None,
        session_id: int | None = None,
        initial_payload: dict[str, Any] | None = None,
        initialized_at: float | None = None,
    ) -> None:
        self.client = client
        self.sdk_path = sdk_path
        self._host = host
        self._session_id = session_id
        self.process: subprocess.Popen[str] | None = (
            None if host is None else host.process
        )
        self._stdout_lines: queue.Queue[str | None] | None = None
        self._stdout_reader: threading.Thread | None = None
        self._init_result: DeviceRuntimeResult | None = (
            None
            if initial_payload is None
            else _build_result(
                initial_payload,
                gather_cost_range=self.client.gather_cost_range,
            )
        )
        self._completed = False
        self.initialized_at = initialized_at
        self._target_first_touch_age_ms: int | None = None

    def __enter__(self) -> "DeviceRuntimeSession":
        if self._host is not None:
            if self._init_result is None or self._session_id is None:
                raise DeviceRuntimeError("challenge-host 会话未完成 Init")
            return self
        if not self.client.bridge_script.is_file():
            raise DeviceRuntimeError(
                f"Node 设备桥不存在：{self.client.bridge_script}"
            )
        try:
            self.process = subprocess.Popen(
                [
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
                    "--device-profile",
                    encode_device_profile(self.client.device_profile),
                ],
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
            self._start_stdout_reader()
            # 首阶段就完整校验容器、Log1 session 与 DeviceConfig，
            # 不合格时不必浪费一次真实的 Init 请求。
            self._init_result = _build_result(
                self._read_stage("init"),
                gather_cost_range=self.client.gather_cost_range,
            )
            self.initialized_at = time.monotonic()
        except Exception:
            self.close()
            raise
        return self

    def __exit__(self, exc_type: Any, exc: Any, traceback: Any) -> None:
        self.close()

    def _start_stdout_reader(self) -> None:
        """在独立线程中读取 Node stdout，兼容 Windows 子进程管道。"""

        process = self.process
        if process is None or process.stdout is None:
            raise DeviceRuntimeError("challenge-worker 尚未启动")

        lines: queue.Queue[str | None] = queue.Queue()
        self._stdout_lines = lines
        reader = threading.Thread(
            target=self._drain_stdout,
            args=(process, lines),
            name="ali-device-stdout",
            daemon=True,
        )
        self._stdout_reader = reader
        reader.start()

    @staticmethod
    def _drain_stdout(
        process: subprocess.Popen[str], lines: queue.Queue[str | None]
    ) -> None:
        """持续把子进程 stdout 行转交给主线程。"""

        stream = process.stdout
        if stream is None:
            lines.put(None)
            return
        try:
            for line in stream:
                lines.put(line)
        finally:
            # EOF 也要入队，让等待方从阻塞中醒来并返回结构化错误。
            lines.put(None)

    def _read_stage(self, expected: str) -> dict[str, Any]:
        """阻塞读取指定阶段的 JSON 输出，超时或进程退出即失败。"""

        if self._host is not None and self._session_id is not None:
            return self._host._read_stage(self._session_id, expected)
        process = self.process
        lines = self._stdout_lines
        if process is None or process.stdout is None or lines is None:
            raise DeviceRuntimeError("challenge-worker 尚未启动")

        deadline = time.monotonic() + self.client.timeout + 5
        while time.monotonic() < deadline:
            remaining = max(0.0, deadline - time.monotonic())
            try:
                line = lines.get(timeout=remaining)
            except queue.Empty:
                break
            if line is None:
                break
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

    @property
    def init_token(self) -> str:
        """发给 ``InitCaptchaV3`` 的 DeviceToken。"""

        return self.initial_result.init_token

    @property
    def initial_result(self) -> DeviceRuntimeResult:
        """Log1/Log2 首阶段已完整校验的运行态。

        动态 PE 初始化用的是页面在 Init 之前就持有的 DeviceConfig 与 Init token；
        最终 Verify token 要等 PE 观测到 getter 参数后，再在同一 VM 里刷新。
        """

        if self._init_result is None:
            raise DeviceRuntimeError("challenge-worker 尚未完成 Init 阶段")
        return self._init_result

    @property
    def target_first_touch_age_ms(self) -> int:
        """本轮首个 touch 相对 TrackStartTime 的目标逻辑年龄。

        每轮只抽样一次并缓存，保证 PE 构造与后续校验用的是同一个值。
        """

        if self._completed:
            raise DeviceRuntimeError("本轮 challenge-worker 已完成")
        if self._init_result is None:
            raise DeviceRuntimeError("challenge-worker 尚未完成 Init 阶段")
        if self._target_first_touch_age_ms is None:
            lower, upper = self.client.first_touch_age_range
            self._target_first_touch_age_ms = lower + secrets.randbelow(
                upper - lower + 1
            )
        return self._target_first_touch_age_ms

    @property
    def recyclable(self) -> bool:
        """此会话是否能在原 Node host 内换成一个全新的隔离 VM。"""

        return self._host is not None and self._session_id is not None

    def recycle(self) -> None:
        """重建共享 host 中的当前槽位，供设备池滚动补回容量。"""

        host = self._host
        session_id = self._session_id
        if host is None or session_id is None:
            raise DeviceRuntimeError("独占 challenge-worker 不支持原位重建")

        # reset 会终止旧 Worker；先清掉旧轮状态，确保后续任一步失败时 close()
        # 会按未完成会话发送 cancel，不会把新 Worker 留在共享 host 里。
        self._completed = False
        self._init_result = None
        self._target_first_touch_age_ms = None
        payload, initialized_at = host.reset_session(session_id)
        self._init_result = _build_result(
            payload,
            gather_cost_range=self.client.gather_cost_range,
        )
        self.initialized_at = initialized_at

    def complete_challenge(
        self,
        getter_arguments: tuple[str, ...],
        interaction_events: tuple[dict[str, Any], ...],
        *,
        post_interaction_delay_ms: int,
    ) -> DeviceRuntimeResult:
        """在同一 VM 内回放 PE mousemove 流，再调用实际的一参 ``getToken``。

        这是整条设备链的收口：把 PE 真实观测到的 getter 参数和交互前缀交回
        FeiLin，让它在**与 PE 相同的状态**下刷新出 Verify token。
        """

        if self._completed:
            raise DeviceRuntimeError("本轮 challenge-worker 已完成")
        if (
            not isinstance(getter_arguments, tuple)
            or len(getter_arguments) != 1
            or any(
                not isinstance(value, str) or not value or len(value) > 512
                for value in getter_arguments
            )
        ):
            raise ValueError(
                "getter_arguments 必须是含一个 1..512 字符字符串的 tuple"
            )
        normalized_events = _normalize_interaction_events(interaction_events)
        if (
            isinstance(post_interaction_delay_ms, bool)
            or not isinstance(post_interaction_delay_ms, int)
            or not 0 <= post_interaction_delay_ms <= 500
        ):
            raise ValueError("post_interaction_delay_ms 必须是 0..500 的整数")

        process = self.process
        if process is None or process.stdin is None or process.poll() is not None:
            raise DeviceRuntimeError("challenge-worker 已退出")
        completion_payload = {
            "complete": True,
            "getterArguments": list(getter_arguments),
            "interactionEvents": list(normalized_events),
            "postInteractionDelayMs": post_interaction_delay_ms,
        }
        try:
            if self._host is not None and self._session_id is not None:
                self._host.send_completion(
                    self._session_id,
                    completion_payload,
                )
            else:
                process.stdin.write(
                    json.dumps(
                        completion_payload,
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
        if self._host is None:
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
                raise DeviceRuntimeError("challenge-worker 失败：" + detail)
            if process.stderr is not None and process.stderr.read().strip():
                raise DeviceRuntimeError(
                    "challenge-worker 存在未处理的 FeiLin 采集异常"
                )

        self._completed = True
        result_payload = dict(payload)
        # 保留真正发给 InitCaptchaV3 的那枚 token（可能已正常化过 GatherCost）。
        result_payload["deviceToken"] = self.init_token
        return _build_result(
            result_payload,
            gather_cost_range=self.client.gather_cost_range,
            expected_getter_argument_count=len(getter_arguments),
            expected_interaction_event_count=len(normalized_events),
        )

    def close(self) -> None:
        """终止 Node 进程；已退出时是空操作。"""

        host = self._host
        if host is not None:
            session_id = self._session_id
            self._host = None
            self._session_id = None
            self.process = None
            if session_id is not None:
                host.release(session_id, completed=self._completed)
            return
        process = self.process
        if process is None:
            return
        reader = self._stdout_reader
        if process.stdin is not None and not process.stdin.closed:
            try:
                process.stdin.close()
            except (BrokenPipeError, OSError):
                pass
        if process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=2)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=2)
        if reader is not None and reader is not threading.current_thread():
            reader.join(timeout=0.5)
        _close_process_pipes(process)
        self._stdout_reader = None
        self._stdout_lines = None
        self.process = None


__all__ = [
    "DeviceRuntimeClient",
    "DeviceRuntimeResult",
    "DeviceRuntimeSession",
]
