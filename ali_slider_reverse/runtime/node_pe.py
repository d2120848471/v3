"""动态 PE 桥：让本轮公开脚本自己生成 Verify ``data``。

## 为什么不由 Python 构造 data

每轮 Init 都会返回新的 ``StaticPath``，对应一份新的 ``pe.*.js``。把某个旧分片的
混淆字段、前缀或 payload 分片硬编码成长期协议，下一次前端更新就会失效。因此这里
的做法是：下载本轮的动态 PE，放进隔离且禁网的 Node ``vm`` 中，喂给它最小可用的
DOM、事件与时钟接口，回放触摸轨迹，截获 ``captchaVerifyCallback``，保留 PE **原生
生成**的完整 ``data``。

Python 一侧不猜测任何密文，只做独立复核——把 data 离线解开，逐项比对 schema、
字段顺序、坐标、时间与 getter 时序。

## 三个时序合同

当前 PE 的关键观察是这三个数**互不相等**，混淆它们是最容易出错的地方：

```text
输入 touch 事件                        86
getter 调用前已形成的 native mm         86
最终 data.TrackList.mm                 87   ← 多出 getter 之后的 RAF 尾项
```

所以：Verify 的 ``data`` 必须保留序列化时存在的全部 87 条 mm；FeiLin getter 只能
看到 getter 调用前的 86 条**连续前缀**；不能把 getter 之后的 RAF 尾项提前回放给
FeiLin。``mm.timeStamp`` 与 getter 的 ``observedAtMs`` 必须来自同一个 PE VM 的
``performance.now()`` 时间域。

## 虚拟时钟

轨迹的逻辑时长（首触年龄、每个 dt、VerifyTime）与真实墙钟是两个时间域。PE 使用
显式、单调、离散推进的逻辑时钟：每个事件之后仍跨一个真实的 ``setTimeout(0)``
macrotask，让 dispatch 期间安排的 RAF/host callback 在下一事件前跑完，但不会按
逻辑 ``dt`` 真实 sleep。所以子进程超时只需覆盖真实执行与 IO，不随轨迹时长扩张。
"""

from __future__ import annotations

import json
import math
import subprocess
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from .. import config
from ..errors import DataCodecError, PeRuntimeError
from ..protocol.data_codec import (
    DATA_PAYLOAD_FIELDS,
    TRACK_LIST_FIELDS,
    unpack_data,
)
from ..vision.geometry import pe_puzzle_x_from_slide_pos


# --------------------------------------------------------------------------
# 校验容差
#
# 逻辑时钟是离散推进的，PE 内部的事件调度、RAF 回调与序列化都会引入几毫秒的
# 抖动，因此这些是"允许的偏差"，不是"可以随便调大的开关"——放宽它们等于放宽
# 对 PE 真实行为的验证强度。
# --------------------------------------------------------------------------

FIRST_TOUCH_TOLERANCE_MS = 250
"""实际首触年龄相对目标值的允许偏差。"""

TOUCH_DURATION_TOLERANCE_MS = 300
"""实际轨迹时长相对输入 dt 总和的允许偏差。"""

POST_TOUCH_VERIFY_MIN_MS = 0
POST_TOUCH_VERIFY_MAX_MS = 500
"""末次 touch 到 VerifyTime、以及末次 mm 到 getter 之间的合理区间。"""

X_POS_QUANTIZATION_TOLERANCE_PX = 1
"""PE 原生 xPos 与图像识别值之间的量化容差。"""

_MAX_TRACK_EVENTS = 512
_MAX_TRACK_DURATION_MS = 60_000
_MAX_EVENT_TIME_MS = 180_000


@dataclass(frozen=True, slots=True)
class PeGetterArgument:
    """PE 调用 FeiLin getter 时传入的单个实参。"""

    value_type: str
    length: int | None
    value: str | None = field(repr=False)
    equals_certify_id: bool
    equals_scene_id: bool


@dataclass(frozen=True, slots=True)
class PeGetterCall:
    """一次被截获的 FeiLin getter 调用。"""

    owner: str
    observed_at_ms: float = field(repr=False)
    """调用发生时刻，取自 PE VM 的 ``performance.now()`` 时间域。"""

    argument_count: int
    arguments: tuple[PeGetterArgument, ...]


@dataclass(frozen=True, slots=True)
class PeRuntimeResult:
    """PE 原生生成、并已由 Python 独立复核过的 data 及其元数据。"""

    data: str = field(repr=False)
    track_event_count: int
    native_device_token_matched: bool
    payload_keys: tuple[str, ...]
    track_keys: tuple[str, ...]
    x_pos: int = field(repr=False)
    slide_pos: int = field(repr=False)
    track_start_time: int = field(repr=False)
    verify_time: int = field(repr=False)
    target_first_touch_age_ms: int = field(repr=False)
    first_touch_age_ms: float = field(repr=False)
    touch_duration_ms: float = field(repr=False)
    last_touch_to_verify_ms: float = field(repr=False)
    post_interaction_delay_ms: float = field(repr=False)
    dispatch_lateness_ms: float = field(repr=False)
    device_getter_calls: tuple[PeGetterCall, ...]

    native_mousemove_event_count: int
    """data 中的全部 mm 条数（含 getter 之后的 RAF 尾项）。"""

    post_getter_mousemove_event_count: int
    """getter 之后才产生的 mm 条数。"""

    mousemove_events: tuple[dict[str, float | bool | str], ...] = field(repr=False)
    """getter 调用前的 mm 连续前缀，用于回放给 FeiLin。"""


# ==========================================================================
# 输入构造与校验
# ==========================================================================


def _require_text(value: Any, *, label: str, maximum: int) -> None:
    if not isinstance(value, str) or not 1 <= len(value) <= maximum:
        raise ValueError(f"{label} 必须是 1..{maximum} 字符字符串")


@dataclass(frozen=True, slots=True)
class _BuildRequest:
    """一次 data 构造的全部输入，构造时即完成校验。

    把校验集中在这里，可以在启动 Node 之前就否掉不合法的输入——子进程一旦跑起来
    就会消耗真实的挑战上下文。
    """

    sdk_path: Path
    pe_path: Path
    scene_id: str
    certify_id: str
    device_token: str
    captcha_type: str
    image: str
    puzzle_image: str
    verify_arg_profile: dict[str, Any]
    device_config: dict[str, Any]
    dimensions: dict[str, Any]
    track: list[dict[str, Any]]
    expected_x_pos: int | None
    init_begin_time: int | None
    first_touch_age_ms: int

    total_track_duration_ms: int = field(init=False)

    def __post_init__(self) -> None:
        if not self.sdk_path.is_file():
            raise ValueError("SDK 路径必须指向普通文件")
        if not self.pe_path.is_file():
            raise ValueError("PE 路径必须指向普通文件")

        for label, value, maximum in (
            ("scene_id", self.scene_id, 128),
            ("certify_id", self.certify_id, 128),
            ("device_token", self.device_token, 65_536),
            ("captcha_type", self.captcha_type, 64),
            ("image", self.image, 4096),
            ("puzzle_image", self.puzzle_image, 4096),
        ):
            _require_text(value, label=label, maximum=maximum)

        if not isinstance(self.verify_arg_profile, dict):
            raise ValueError("verify_arg_profile 必须是 object")
        for name in ("accessSec", "sessionIdSalt"):
            value = self.verify_arg_profile.get(name)
            if not isinstance(value, str) or not 1 <= len(value) <= 128:
                raise ValueError(f"verify_arg_profile.{name} 无效")
        if not isinstance(self.device_config, dict):
            raise ValueError("device_config 必须是 object")

        self._validate_dimensions()
        object.__setattr__(
            self, "total_track_duration_ms", self._validate_track()
        )
        self._validate_bounds()

    def _validate_dimensions(self) -> None:
        if not isinstance(self.dimensions, dict):
            raise ValueError("dimensions 必须是 object")
        for name in (
            "imageWidth",
            "imageHeight",
            "puzzleWidth",
            "puzzleHeight",
            "renderedWidth",
            "handleWidth",
        ):
            value = self.dimensions.get(name)
            if isinstance(value, bool) or not isinstance(value, int) or value < 1:
                raise ValueError(f"dimensions.{name} 必须是正整数")
        if self.dimensions["handleWidth"] > self.dimensions["renderedWidth"]:
            raise ValueError("dimensions.handleWidth 不能大于 renderedWidth")

    def _validate_track(self) -> int:
        """校验轨迹采样并返回 dt 总和。"""

        if not isinstance(self.track, list) or not 3 <= len(self.track) <= _MAX_TRACK_EVENTS:
            raise ValueError(f"track 必须包含 3..{_MAX_TRACK_EVENTS} 个采样")

        total = 0
        for index, sample in enumerate(self.track):
            if not isinstance(sample, dict):
                raise ValueError(f"track[{index}] 必须是 object")
            x, y, dt = sample.get("x"), sample.get("y"), sample.get("dt")
            if (
                isinstance(x, bool)
                or not isinstance(x, (int, float))
                or isinstance(y, bool)
                or not isinstance(y, (int, float))
                or not math.isfinite(x)
                or not math.isfinite(y)
                or isinstance(dt, bool)
                or not isinstance(dt, int)
                or dt < 0
                or (index == 0 and dt != 0)
            ):
                raise ValueError(f"track[{index}] 的 x/y/dt 无效")
            event_type = sample.get("type")
            if event_type is not None and event_type not in {
                "touchstart",
                "touchmove",
                "touchend",
            }:
                raise ValueError(f"track[{index}].type 无效")
            total += dt

        if total > _MAX_TRACK_DURATION_MS:
            raise ValueError("track 总时长不能超过 60 秒")
        return total

    def _validate_bounds(self) -> None:
        maximum_slide_pos = (
            self.dimensions["renderedWidth"] - self.dimensions["handleWidth"]
        )
        maximum_puzzle_x = pe_puzzle_x_from_slide_pos(maximum_slide_pos)
        if self.expected_x_pos is not None and (
            isinstance(self.expected_x_pos, bool)
            or not isinstance(self.expected_x_pos, int)
            or not 0 <= self.expected_x_pos <= maximum_puzzle_x
        ):
            raise ValueError("expected_x_pos 必须位于当前 PE 可达整数坐标域")
        if self.init_begin_time is not None and (
            isinstance(self.init_begin_time, bool)
            or not isinstance(self.init_begin_time, int)
            or self.init_begin_time < 1
        ):
            raise ValueError("init_begin_time 必须是正整数 epoch 毫秒")
        if (
            isinstance(self.first_touch_age_ms, bool)
            or not isinstance(self.first_touch_age_ms, int)
            or not 1 <= self.first_touch_age_ms <= 120_000
        ):
            raise ValueError("first_touch_age_ms 必须是 1..120000 的整数")

    def to_bridge_payload(self) -> dict[str, Any]:
        """序列化成 Node 桥的 stdin JSON。"""

        return {
            "sceneId": self.scene_id,
            "certifyId": self.certify_id,
            "deviceToken": self.device_token,
            "captchaType": self.captcha_type,
            "image": self.image,
            "puzzleImage": self.puzzle_image,
            "verifyArgProfile": self.verify_arg_profile,
            "deviceConfig": self.device_config,
            "dimensions": self.dimensions,
            "track": self.track,
            "expectedXPos": self.expected_x_pos,
            "initBeginTime": self.init_begin_time,
            "firstTouchAgeMs": self.first_touch_age_ms,
        }


# ==========================================================================
# 桥输出解析
# ==========================================================================


def _parse_output(stdout: str) -> dict[str, Any]:
    """从 stdout 末尾取出桥的 JSON 结果。"""

    for line in reversed(stdout.splitlines()):
        try:
            candidate = json.loads(line)
        except json.JSONDecodeError:
            continue
        if isinstance(candidate, dict):
            return candidate
    raise PeRuntimeError("Node PE bridge 未返回 JSON 结果")


def _bridge_error(stderr: str | None) -> str:
    """提取桥的结构化错误，避免把整行混淆脚本写进异常。"""

    for line in reversed((stderr or "").splitlines()):
        try:
            candidate = json.loads(line)
        except json.JSONDecodeError:
            continue
        message = candidate.get("error") if isinstance(candidate, dict) else None
        if isinstance(message, str) and message:
            return message[:800]
    return "未返回结构化错误"


def _string_tuple(value: Any, label: str) -> tuple[str, ...]:
    if (
        not isinstance(value, list)
        or not value
        or any(not isinstance(item, str) or not item for item in value)
    ):
        raise PeRuntimeError(f"Node PE bridge {label} 无效")
    return tuple(value)


def _integer_value(value: Any, label: str) -> int:
    """接受整数或其十进制字符串形式。"""

    if isinstance(value, bool):
        raise PeRuntimeError(f"Node PE bridge {label} 无效")
    if isinstance(value, int):
        return value
    if isinstance(value, str) and value.isdigit():
        return int(value, 10)
    raise PeRuntimeError(f"Node PE bridge {label} 无效")


def _number_value(value: Any, label: str) -> float:
    if (
        isinstance(value, bool)
        or not isinstance(value, (int, float))
        or not math.isfinite(value)
    ):
        raise PeRuntimeError(f"Node PE bridge {label} 无效")
    return float(value)


def _getter_calls(
    value: Any,
    *,
    scene_id: str,
    certify_id: str,
) -> tuple[PeGetterCall, ...]:
    """解析并严格校验被截获的 getter 调用。

    ``equalsCertifyId``/``equalsSceneId`` 由桥自行判断，这里重新算一遍做交叉
    验证：桥说"这个参数等于 CertifyId"时，Python 必须能独立确认。
    """

    if not isinstance(value, list):
        raise PeRuntimeError("Node PE bridge getter 调用元数据无效")

    calls: list[PeGetterCall] = []
    for item in value:
        if not isinstance(item, dict):
            raise PeRuntimeError("Node PE bridge getter 调用项无效")
        owner = item.get("owner")
        observed_at = item.get("observedAtMs")
        count = item.get("argumentCount")
        arguments = item.get("arguments")
        if (
            owner not in {"z_um", "um"}
            or isinstance(observed_at, bool)
            or not isinstance(observed_at, (int, float))
            or not math.isfinite(float(observed_at))
            or not 0 <= float(observed_at) <= _MAX_EVENT_TIME_MS
            or isinstance(count, bool)
            or not isinstance(count, int)
            or count < 0
            or not isinstance(arguments, list)
            or len(arguments) != count
        ):
            raise PeRuntimeError("Node PE bridge getter 调用结构无效")

        parsed: list[PeGetterArgument] = []
        for argument in arguments:
            if not isinstance(argument, dict):
                raise PeRuntimeError("Node PE bridge getter 参数元数据无效")
            value_type = argument.get("type")
            length = argument.get("length")
            argument_value = argument.get("value")
            equals_certify_id = argument.get("equalsCertifyId")
            equals_scene_id = argument.get("equalsSceneId")
            if (
                not isinstance(value_type, str)
                or not value_type
                or (
                    length is not None
                    and (
                        isinstance(length, bool)
                        or not isinstance(length, int)
                        or length < 0
                    )
                )
                or not isinstance(equals_certify_id, bool)
                or not isinstance(equals_scene_id, bool)
                or (
                    argument_value is not None
                    and (
                        not isinstance(argument_value, str)
                        or len(argument_value) > 512
                    )
                )
                or (
                    isinstance(argument_value, str)
                    and length != len(argument_value)
                )
                or equals_certify_id != (argument_value == certify_id)
                or equals_scene_id != (argument_value == scene_id)
            ):
                raise PeRuntimeError("Node PE bridge getter 参数形状无效")
            parsed.append(
                PeGetterArgument(
                    value_type=value_type,
                    length=length,
                    value=argument_value,
                    equals_certify_id=equals_certify_id,
                    equals_scene_id=equals_scene_id,
                )
            )

        calls.append(
            PeGetterCall(
                owner=owner,
                observed_at_ms=float(observed_at),
                argument_count=count,
                arguments=tuple(parsed),
            )
        )
    return tuple(calls)


def _event_times(value: Any, label: str) -> tuple[float, ...]:
    """从压平的事件串中取出各事件的时间列。

    格式为 ``x,y,time,isTrusted`` 以 ``|`` 连接。
    """

    if not isinstance(value, str) or not value:
        raise PeRuntimeError(f"Node PE bridge {label} 无效")

    times: list[float] = []
    for record in value.split("|"):
        fields = record.split(",")
        if len(fields) != 4:
            raise PeRuntimeError(f"Node PE bridge {label} 格式异常")
        try:
            event_time = float(fields[2])
        except ValueError as exc:
            raise PeRuntimeError(f"Node PE bridge {label} 时间无效") from exc
        if not math.isfinite(event_time) or event_time < 0:
            raise PeRuntimeError(f"Node PE bridge {label} 时间无效")
        times.append(event_time)
    return tuple(times)


def _mousemove_events(
    value: Any,
    label: str,
) -> tuple[dict[str, float | bool | str], ...]:
    """把压平的 mm 串还原成事件对象，并校验取值范围与单调性。"""

    if not isinstance(value, str) or not value:
        raise PeRuntimeError(f"Node PE bridge {label} 无效")

    events: list[dict[str, float | bool | str]] = []
    previous_time: float | None = None
    for record in value.split("|"):
        fields = record.split(",")
        if len(fields) != 4:
            raise PeRuntimeError(f"Node PE bridge {label} 格式异常")
        try:
            x, y, timestamp, trusted = (float(item) for item in fields)
        except ValueError as exc:
            raise PeRuntimeError(f"Node PE bridge {label} 数值无效") from exc
        if (
            not all(
                math.isfinite(item) for item in (x, y, timestamp, trusted)
            )
            or abs(x) > 10_000
            or abs(y) > 10_000
            or not 0 <= timestamp <= _MAX_EVENT_TIME_MS
            or trusted != 1
            or (previous_time is not None and timestamp < previous_time)
        ):
            raise PeRuntimeError(f"Node PE bridge {label} 数值异常")
        events.append(
            {
                "type": "mousemove",
                "x": x,
                "y": y,
                "timeStamp": timestamp,
                "isTrusted": True,
            }
        )
        previous_time = timestamp

    span = events[-1]["timeStamp"] - events[0]["timeStamp"]
    if not 1 <= len(events) <= _MAX_TRACK_EVENTS or span > _MAX_TRACK_DURATION_MS:
        raise PeRuntimeError(f"Node PE bridge {label} 事件范围异常")
    return tuple(events)


# ==========================================================================
# 客户端
# ==========================================================================


class PeRuntimeClient:
    """在隔离 Node VM 中运行动态 PE，并独立复核它返回的 data。"""

    def __init__(
        self,
        *,
        node_binary: str = config.DEFAULT_NODE_BINARY,
        bridge_script: str | Path | None = None,
        prefix: str = config.DEFAULT_PREFIX,
        region: str = config.DEFAULT_REGION,
        timeout: float = config.DEFAULT_TIMEOUT,
    ) -> None:
        for label, value in (
            ("node_binary", node_binary),
            ("prefix", prefix),
            ("region", region),
        ):
            if not isinstance(value, str) or not value:
                raise ValueError(f"{label} 必须是非空字符串")
        if (
            isinstance(timeout, bool)
            or not isinstance(timeout, (int, float))
            or not math.isfinite(timeout)
            or timeout < 1
        ):
            raise ValueError("timeout 必须是不少于 1 秒的有限数值")

        self.node_binary = node_binary
        self.bridge_script = (
            config.PE_DATA_BRIDGE if bridge_script is None else Path(bridge_script)
        )
        self.prefix = prefix
        self.region = region
        self.timeout = float(timeout)

    def build(
        self,
        *,
        sdk_path: str | Path,
        pe_path: str | Path,
        scene_id: str,
        certify_id: str,
        device_token: str,
        captcha_type: str,
        image: str,
        puzzle_image: str,
        verify_arg_profile: dict[str, Any],
        device_config: dict[str, Any],
        dimensions: dict[str, Any],
        track: list[dict[str, Any]],
        expected_x_pos: int | None = None,
        init_begin_time: int | None = None,
        first_touch_age_ms: int | None = None,
    ) -> PeRuntimeResult:
        """让当前动态 PE 原生生成 data，并逐层复核后返回。

        流程：校验输入 → 跑 Node 桥 → 校验桥自报的元数据 → 离线解包 data 并与
        元数据交叉比对 → 校验 touch/getter/Verify 三处时序。任何一步不符都抛
        :class:`~ali_slider_reverse.errors.PeRuntimeError`，绝不带着可疑的 data
        继续走到 Verify。
        """

        if not self.bridge_script.is_file():
            raise PeRuntimeError("Node PE bridge 文件不存在")

        request = _BuildRequest(
            sdk_path=Path(sdk_path),
            pe_path=Path(pe_path),
            scene_id=scene_id,
            certify_id=certify_id,
            device_token=device_token,
            captcha_type=captcha_type,
            image=image,
            puzzle_image=puzzle_image,
            verify_arg_profile=verify_arg_profile,
            device_config=device_config,
            dimensions=dimensions,
            track=track,
            expected_x_pos=expected_x_pos,
            init_begin_time=init_begin_time,
            first_touch_age_ms=first_touch_age_ms,
        )

        output = self._invoke_bridge(request)
        data, matched, getter_calls = self._verify_session_fields(output, request)
        meta = self._verify_meta(output, request)
        decoded_track = self._verify_decoded_data(data, meta, request)
        timing = self._verify_timing(
            decoded_track, meta, getter_calls, request
        )

        return PeRuntimeResult(
            data=data,
            track_event_count=len(request.track),
            native_device_token_matched=matched,
            payload_keys=meta["payload_keys"],
            track_keys=meta["track_keys"],
            x_pos=meta["x_pos"],
            slide_pos=meta["slide_pos"],
            track_start_time=meta["track_start_time"],
            verify_time=meta["verify_time"],
            target_first_touch_age_ms=meta["target_first_touch_age"],
            first_touch_age_ms=timing["first_touch_age"],
            touch_duration_ms=timing["touch_duration"],
            last_touch_to_verify_ms=timing["last_touch_to_verify"],
            post_interaction_delay_ms=timing["post_interaction_delay"],
            dispatch_lateness_ms=meta["dispatch_lateness"],
            device_getter_calls=getter_calls,
            native_mousemove_event_count=timing["native_mm_count"],
            post_getter_mousemove_event_count=timing["post_getter_mm_count"],
            mousemove_events=timing["pre_getter_events"],
        )

    def _invoke_bridge(self, request: _BuildRequest) -> dict[str, Any]:
        """启动 Node 子进程并取回 JSON 输出。

        逻辑轨迹时长不影响真实耗时（虚拟时钟不 sleep），所以超时只按配置值给，
        外层再留 5 秒余量用于进程启停。
        """

        operation_timeout_ms = round(self.timeout * 1000)
        command = [
            self.node_binary,
            str(self.bridge_script),
            "--sdk",
            str(request.sdk_path),
            "--pe",
            str(request.pe_path),
            "--prefix",
            self.prefix,
            "--region",
            self.region,
            "--timeout-ms",
            str(operation_timeout_ms),
        ]
        try:
            completed = subprocess.run(
                command,
                input=json.dumps(
                    request.to_bridge_payload(),
                    ensure_ascii=False,
                    separators=(",", ":"),
                ),
                capture_output=True,
                text=True,
                check=True,
                timeout=operation_timeout_ms / 1000 + 5,
            )
        except FileNotFoundError as exc:
            raise PeRuntimeError("找不到 Node 运行时") from exc
        except subprocess.TimeoutExpired as exc:
            raise PeRuntimeError("Node PE bridge 执行超时") from exc
        except subprocess.CalledProcessError as exc:
            raise PeRuntimeError(
                "Node PE bridge 失败：" + _bridge_error(exc.stderr)
            ) from exc
        return _parse_output(completed.stdout)

    @staticmethod
    def _verify_session_fields(
        output: dict[str, Any],
        request: _BuildRequest,
    ) -> tuple[str, bool, tuple[PeGetterCall, ...]]:
        """确认桥回传的会话字段与本轮输入完全一致。"""

        verify_param = output.get("verifyParam")
        if not isinstance(verify_param, dict):
            raise PeRuntimeError("Node PE bridge 缺少 verifyParam")
        if (
            verify_param.get("sceneId") != request.scene_id
            or verify_param.get("certifyId") != request.certify_id
            or verify_param.get("deviceToken") != request.device_token
        ):
            raise PeRuntimeError("Node PE bridge 返回的会话字段不匹配")

        data = verify_param.get("data")
        if not isinstance(data, str) or not data:
            raise PeRuntimeError("Node PE bridge 返回了空 data")

        count = output.get("trackEventCount")
        if (
            isinstance(count, bool)
            or not isinstance(count, int)
            or count != len(request.track)
        ):
            raise PeRuntimeError("Node PE bridge 返回了无效轨迹事件计数")

        matched = output.get("nativeDeviceTokenMatched")
        if not isinstance(matched, bool):
            raise PeRuntimeError("Node PE bridge 返回了无效 token 匹配标志")

        getter_calls = _getter_calls(
            output.get("deviceGetterCalls"),
            scene_id=request.scene_id,
            certify_id=request.certify_id,
        )
        return data, matched, getter_calls

    @staticmethod
    def _verify_meta(
        output: dict[str, Any],
        request: _BuildRequest,
    ) -> dict[str, Any]:
        """校验桥自报的 payload 元数据：字段顺序、坐标与首触年龄。"""

        meta = output.get("payloadMeta")
        if not isinstance(meta, dict):
            raise PeRuntimeError("Node PE bridge 缺少 payloadMeta")

        payload_keys = _string_tuple(meta.get("keys"), "payload keys")
        if payload_keys != DATA_PAYLOAD_FIELDS:
            raise PeRuntimeError("Node PE bridge payload 字段顺序异常")
        track_keys = _string_tuple(meta.get("trackKeys"), "track keys")
        if track_keys != TRACK_LIST_FIELDS:
            raise PeRuntimeError("Node PE bridge TrackList 字段顺序异常")

        values = {
            "payload_keys": payload_keys,
            "track_keys": track_keys,
            "x_pos": _integer_value(meta.get("xPos"), "xPos"),
            "slide_pos": _integer_value(meta.get("slidePos"), "slidePos"),
            "track_start_time": _integer_value(
                meta.get("trackStartTime"), "TrackStartTime"
            ),
            "verify_time": _integer_value(meta.get("verifyTime"), "VerifyTime"),
            "target_first_touch_age": _integer_value(
                meta.get("targetFirstTouchAgeMs"), "targetFirstTouchAgeMs"
            ),
            "actual_dispatch_age": _number_value(
                meta.get("actualFirstTouchAgeMs"), "actualFirstTouchAgeMs"
            ),
            "dispatch_lateness": _number_value(
                meta.get("dispatchLatenessMs"), "dispatchLatenessMs"
            ),
        }

        if (
            values["x_pos"] < 0
            or values["slide_pos"] < 0
            or values["track_start_time"] <= 0
            or values["verify_time"] < values["track_start_time"]
            or values["target_first_touch_age"] != request.first_touch_age_ms
            or abs(values["dispatch_lateness"]) > FIRST_TOUCH_TOLERANCE_MS
        ):
            raise PeRuntimeError("Node PE bridge payload 数值异常")
        if (
            request.expected_x_pos is not None
            and abs(values["x_pos"] - float(request.expected_x_pos))
            > X_POS_QUANTIZATION_TOLERANCE_PX
        ):
            raise PeRuntimeError("Node PE bridge xPos 超出图片/PE 量化容差")
        return values

    @staticmethod
    def _verify_decoded_data(
        data: str,
        meta: dict[str, Any],
        request: _BuildRequest,
    ) -> dict[str, Any]:
        """离线解开 data，并与桥自报的元数据逐项交叉比对。

        这一步是整条链路的独立复核点：桥说了什么不算数，data 里真正编码进去的
        才算数。返回解包后的 ``TrackList`` 供时序校验使用。
        """

        try:
            payload = unpack_data(data).payload
        except (DataCodecError, ValueError) as exc:
            raise PeRuntimeError("Node PE bridge data 无法按当前协议解包") from exc

        if tuple(payload) != DATA_PAYLOAD_FIELDS:
            raise PeRuntimeError("Node PE bridge data 顶层字段顺序异常")
        track_list = payload.get("TrackList")
        if not isinstance(track_list, dict):
            raise PeRuntimeError("Node PE bridge data TrackList 类型异常")
        if tuple(track_list) != TRACK_LIST_FIELDS:
            raise PeRuntimeError("Node PE bridge data TrackList 字段顺序异常")

        track_start = payload["TrackStartTime"]
        verify_time = payload["VerifyTime"]
        x_pos = payload["xPos"]
        slide_pos = payload["slidePos"]
        arg = payload["arg"]
        # 用 type(...) is int 而不是 isinstance：JSON 里的时间必须是整数，
        # 浮点或 bool 都说明 PE 的序列化路径出了问题。
        if (
            type(track_start) is not int
            or type(verify_time) is not int
            or not isinstance(x_pos, str)
            or not x_pos
            or not isinstance(slide_pos, str)
            or not slide_pos
            or not isinstance(arg, str)
            or not arg
        ):
            raise PeRuntimeError("Node PE bridge data 顶层字段类型异常")

        track_list_start = track_list["startTime"]
        if (
            type(track_list_start) is not int
            or track_list_start != track_start
            or any(
                not isinstance(track_list[name], str)
                for name in TRACK_LIST_FIELDS
                if name != "startTime"
            )
        ):
            raise PeRuntimeError(
                "Node PE bridge data TrackList 字段类型/时间异常"
            )

        if (
            track_start != meta["track_start_time"]
            or verify_time != meta["verify_time"]
            or x_pos != str(meta["x_pos"])
            or slide_pos != str(meta["slide_pos"])
        ):
            raise PeRuntimeError("Node PE bridge data 明文元数据不一致")
        return track_list

    @staticmethod
    def _verify_timing(
        track_list: dict[str, Any],
        meta: dict[str, Any],
        getter_calls: tuple[PeGetterCall, ...],
        request: _BuildRequest,
    ) -> dict[str, Any]:
        """校验 touch、getter 与 Verify 三处时序，并切出 getter 前的 mm 前缀。

        这里落实"86 → 86 → 87"合同：data 里的 mm 总数可以多于输入事件数（尾部
        RAF），但 **getter 之前**的 mm 条数必须与输入轨迹严格相等——多一条就说明
        回放给 FeiLin 的状态与 PE 当时看到的不一致。
        """

        touch_start_times = _event_times(track_list.get("tc"), "TrackList.tc")
        touch_end_times = _event_times(track_list.get("te"), "TrackList.te")
        if len(touch_start_times) != 1 or len(touch_end_times) != 1:
            raise PeRuntimeError("Node PE bridge touch 首尾事件数异常")
        if len(getter_calls) != 1:
            raise PeRuntimeError("Node PE bridge getter 调用次数异常")

        native_events = _mousemove_events(track_list.get("mm"), "TrackList.mm")
        getter_observed_at = getter_calls[0].observed_at_ms
        pre_getter_events = tuple(
            event
            for event in native_events
            if float(event["timeStamp"]) <= getter_observed_at
        )
        if len(pre_getter_events) != len(request.track):
            raise PeRuntimeError("Node PE bridge getter 前原生 mousemove 数量异常")

        first_touch_age = touch_start_times[0]
        last_touch_age = touch_end_times[0]
        touch_duration = last_touch_age - first_touch_age
        last_touch_to_verify = meta["verify_time"] - (
            meta["track_start_time"] + last_touch_age
        )
        post_interaction_delay = getter_observed_at - float(
            pre_getter_events[-1]["timeStamp"]
        )

        if (
            abs(first_touch_age - request.first_touch_age_ms)
            > FIRST_TOUCH_TOLERANCE_MS
            or abs(first_touch_age - meta["actual_dispatch_age"]) > 25
            or abs(touch_duration - request.total_track_duration_ms)
            > TOUCH_DURATION_TOLERANCE_MS
            or not (
                POST_TOUCH_VERIFY_MIN_MS
                <= last_touch_to_verify
                <= POST_TOUCH_VERIFY_MAX_MS
            )
            or not (
                POST_TOUCH_VERIFY_MIN_MS
                <= post_interaction_delay
                <= POST_TOUCH_VERIFY_MAX_MS
            )
        ):
            raise PeRuntimeError("Node PE bridge data 的实际 touch/Verify 时序异常")

        return {
            "first_touch_age": first_touch_age,
            "touch_duration": touch_duration,
            "last_touch_to_verify": last_touch_to_verify,
            "post_interaction_delay": post_interaction_delay,
            "native_mm_count": len(native_events),
            "post_getter_mm_count": len(native_events) - len(pre_getter_events),
            "pre_getter_events": pre_getter_events,
        }


__all__ = [
    "FIRST_TOUCH_TOLERANCE_MS",
    "PeGetterArgument",
    "PeGetterCall",
    "PeRuntimeClient",
    "PeRuntimeResult",
    "X_POS_QUANTIZATION_TOLERANCE_PX",
]
