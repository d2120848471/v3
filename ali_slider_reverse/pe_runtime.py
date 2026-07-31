"""通过受控 Node VM 调用当前公开动态 PE 生成 Verify ``data``。"""

from __future__ import annotations

import json
import math
import subprocess
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from .data_codec import (
    DATA_PAYLOAD_FIELDS,
    TRACK_LIST_FIELDS,
    DataCodecError,
    unpack_data,
)
from .image_solver import pe_puzzle_x_from_slide_pos


FIRST_TOUCH_TOLERANCE_MS = 250
TOUCH_DURATION_TOLERANCE_MS = 300
POST_TOUCH_VERIFY_MIN_MS = 0
POST_TOUCH_VERIFY_MAX_MS = 500
X_POS_QUANTIZATION_TOLERANCE_PX = 1


class PeRuntimeError(RuntimeError):
    """动态 PE 桥无法生成或验证当前分片的 data。"""


@dataclass(frozen=True, slots=True)
class PeGetterArgument:
    value_type: str
    length: int | None
    value: str | None = field(repr=False)
    equals_certify_id: bool
    equals_scene_id: bool


@dataclass(frozen=True, slots=True)
class PeGetterCall:
    owner: str
    observed_at_ms: float = field(repr=False)
    argument_count: int
    arguments: tuple[PeGetterArgument, ...]


@dataclass(frozen=True, slots=True)
class PeRuntimeResult:
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
    post_getter_mousemove_event_count: int
    mousemove_events: tuple[dict[str, float | bool | str], ...] = field(
        repr=False
    )


class PeRuntimeClient:
    def __init__(
        self,
        *,
        node_binary: str = "node",
        bridge_script: str | Path | None = None,
        prefix: str = "fsgtmi",
        region: str = "cn",
        timeout: float = 20.0,
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
            Path(__file__).with_name("pe_data_bridge.mjs")
            if bridge_script is None
            else Path(bridge_script)
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
        sdk_file = Path(sdk_path)
        pe_file = Path(pe_path)
        if not self.bridge_script.is_file():
            raise PeRuntimeError("Node PE bridge 文件不存在")
        if not sdk_file.is_file():
            raise ValueError("SDK 路径必须指向普通文件")
        if not pe_file.is_file():
            raise ValueError("PE 路径必须指向普通文件")
        for label, value, maximum in (
            ("scene_id", scene_id, 128),
            ("certify_id", certify_id, 128),
            ("device_token", device_token, 65_536),
            ("captcha_type", captcha_type, 64),
            ("image", image, 4096),
            ("puzzle_image", puzzle_image, 4096),
        ):
            if not isinstance(value, str) or not 1 <= len(value) <= maximum:
                raise ValueError(f"{label} 必须是 1..{maximum} 字符字符串")
        if not isinstance(verify_arg_profile, dict):
            raise ValueError("verify_arg_profile 必须是 object")
        for field in ("accessSec", "sessionIdSalt"):
            value = verify_arg_profile.get(field)
            if not isinstance(value, str) or not 1 <= len(value) <= 128:
                raise ValueError(f"verify_arg_profile.{field} 无效")
        if not isinstance(device_config, dict):
            raise ValueError("device_config 必须是 object")
        if not isinstance(dimensions, dict):
            raise ValueError("dimensions 必须是 object")
        for field in (
            "imageWidth",
            "imageHeight",
            "puzzleWidth",
            "puzzleHeight",
            "renderedWidth",
            "handleWidth",
        ):
            value = dimensions.get(field)
            if isinstance(value, bool) or not isinstance(value, int) or value < 1:
                raise ValueError(f"dimensions.{field} 必须是正整数")
        if dimensions["handleWidth"] > dimensions["renderedWidth"]:
            raise ValueError(
                "dimensions.handleWidth 不能大于 renderedWidth"
            )
        if not isinstance(track, list) or not 3 <= len(track) <= 512:
            raise ValueError("track 必须包含 3..512 个采样")
        total_duration = 0
        for index, sample in enumerate(track):
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
            total_duration += dt
        if total_duration > 60_000:
            raise ValueError("track 总时长不能超过 60 秒")
        maximum_slide_pos = (
            dimensions["renderedWidth"] - dimensions["handleWidth"]
        )
        maximum_puzzle_x = pe_puzzle_x_from_slide_pos(
            maximum_slide_pos
        )
        if expected_x_pos is not None and (
            isinstance(expected_x_pos, bool)
            or not isinstance(expected_x_pos, int)
            or not 0 <= expected_x_pos <= maximum_puzzle_x
        ):
            raise ValueError(
                "expected_x_pos 必须位于当前 PE 可达整数坐标域"
            )
        if (
            init_begin_time is not None
            and (
                isinstance(init_begin_time, bool)
                or not isinstance(init_begin_time, int)
                or init_begin_time < 1
            )
        ):
            raise ValueError("init_begin_time 必须是正整数 epoch 毫秒")
        if (
            isinstance(first_touch_age_ms, bool)
            or not isinstance(first_touch_age_ms, int)
            or not 1 <= first_touch_age_ms <= 120_000
        ):
            raise ValueError(
                "first_touch_age_ms 必须是 1..120000 的整数"
            )

        payload = {
            "sceneId": scene_id,
            "certifyId": certify_id,
            "deviceToken": device_token,
            "captchaType": captcha_type,
            "image": image,
            "puzzleImage": puzzle_image,
            "verifyArgProfile": verify_arg_profile,
            "deviceConfig": device_config,
            "dimensions": dimensions,
            "track": track,
            "expectedXPos": expected_x_pos,
            "initBeginTime": init_begin_time,
            "firstTouchAgeMs": first_touch_age_ms,
        }
        # 虚拟时钟会离散推进 first-touch 与 track dt；子进程 timeout 只保护
        # 真实 SDK/PE 执行和宿主 I/O，不再按逻辑轨迹时长扩张。
        operation_timeout_ms = round(self.timeout * 1000)
        command = [
            self.node_binary, str(self.bridge_script), "--sdk", str(sdk_file),
            "--pe", str(pe_file), "--prefix", self.prefix, "--region",
            self.region, "--timeout-ms", str(operation_timeout_ms),
        ]
        try:
            completed = subprocess.run(
                command,
                input=json.dumps(
                    payload,
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
                "Node PE bridge 失败：" + self._bridge_error(exc.stderr)
            ) from exc

        payload = self._parse_output(completed.stdout)
        verify_param = payload.get("verifyParam")
        if not isinstance(verify_param, dict):
            raise PeRuntimeError("Node PE bridge 缺少 verifyParam")
        if (
            verify_param.get("sceneId") != scene_id
            or verify_param.get("certifyId") != certify_id
            or verify_param.get("deviceToken") != device_token
        ):
            raise PeRuntimeError("Node PE bridge 返回的会话字段不匹配")
        data = verify_param.get("data")
        if not isinstance(data, str) or not data:
            raise PeRuntimeError("Node PE bridge 返回了空 data")
        count = payload.get("trackEventCount")
        if (
            isinstance(count, bool)
            or not isinstance(count, int)
            or count != len(track)
        ):
            raise PeRuntimeError("Node PE bridge 返回了无效轨迹事件计数")
        matched = payload.get("nativeDeviceTokenMatched")
        if not isinstance(matched, bool):
            raise PeRuntimeError("Node PE bridge 返回了无效 token 匹配标志")
        device_getter_calls = self._getter_calls(
            payload.get("deviceGetterCalls"),
            scene_id=scene_id,
            certify_id=certify_id,
        )
        meta = payload.get("payloadMeta")
        if not isinstance(meta, dict):
            raise PeRuntimeError("Node PE bridge 缺少 payloadMeta")
        payload_keys = self._string_tuple(meta.get("keys"), "payload keys")
        if payload_keys != DATA_PAYLOAD_FIELDS:
            raise PeRuntimeError("Node PE bridge payload 字段顺序异常")
        track_keys = self._string_tuple(
            meta.get("trackKeys"),
            "track keys",
        )
        if track_keys != TRACK_LIST_FIELDS:
            raise PeRuntimeError("Node PE bridge TrackList 字段顺序异常")
        x_pos = self._integer_value(meta.get("xPos"), "xPos")
        slide_pos = self._integer_value(
            meta.get("slidePos"),
            "slidePos",
        )
        track_start_time = self._integer_value(
            meta.get("trackStartTime"),
            "TrackStartTime",
        )
        verify_time = self._integer_value(
            meta.get("verifyTime"),
            "VerifyTime",
        )
        target_first_touch_age = self._integer_value(
            meta.get("targetFirstTouchAgeMs"),
            "targetFirstTouchAgeMs",
        )
        actual_dispatch_age = self._number_value(
            meta.get("actualFirstTouchAgeMs"),
            "actualFirstTouchAgeMs",
        )
        dispatch_lateness = self._number_value(
            meta.get("dispatchLatenessMs"),
            "dispatchLatenessMs",
        )
        if (
            x_pos < 0
            or slide_pos < 0
            or track_start_time <= 0
            or verify_time < track_start_time
            or target_first_touch_age != first_touch_age_ms
            or abs(dispatch_lateness) > FIRST_TOUCH_TOLERANCE_MS
        ):
            raise PeRuntimeError("Node PE bridge payload 数值异常")
        if (
            expected_x_pos is not None
            and abs(x_pos - float(expected_x_pos))
            > X_POS_QUANTIZATION_TOLERANCE_PX
        ):
            raise PeRuntimeError(
                "Node PE bridge xPos 超出图片/PE 量化容差"
            )

        try:
            decoded_payload = unpack_data(data).payload
        except (DataCodecError, ValueError) as exc:
            raise PeRuntimeError(
                "Node PE bridge data 无法按当前协议解包"
            ) from exc
        if tuple(decoded_payload) != DATA_PAYLOAD_FIELDS:
            raise PeRuntimeError("Node PE bridge data 顶层字段顺序异常")
        decoded_track = decoded_payload.get("TrackList")
        if (
            not isinstance(decoded_track, dict)
        ):
            raise PeRuntimeError("Node PE bridge data TrackList 类型异常")
        if tuple(decoded_track) != TRACK_LIST_FIELDS:
            raise PeRuntimeError(
                "Node PE bridge data TrackList 字段顺序异常"
            )
        decoded_track_start = decoded_payload["TrackStartTime"]
        decoded_verify_time = decoded_payload["VerifyTime"]
        decoded_x_pos = decoded_payload["xPos"]
        decoded_slide_pos = decoded_payload["slidePos"]
        decoded_arg = decoded_payload["arg"]
        if (
            type(decoded_track_start) is not int
            or type(decoded_verify_time) is not int
            or not isinstance(decoded_x_pos, str)
            or not decoded_x_pos
            or not isinstance(decoded_slide_pos, str)
            or not decoded_slide_pos
            or not isinstance(decoded_arg, str)
            or not decoded_arg
        ):
            raise PeRuntimeError(
                "Node PE bridge data 顶层字段类型异常"
            )
        decoded_track_list_start = decoded_track["startTime"]
        if (
            type(decoded_track_list_start) is not int
            or decoded_track_list_start != decoded_track_start
            or any(
                not isinstance(decoded_track[field], str)
                for field in TRACK_LIST_FIELDS
                if field != "startTime"
            )
        ):
            raise PeRuntimeError(
                "Node PE bridge data TrackList 字段类型/时间异常"
            )
        if (
            decoded_track_start != track_start_time
            or decoded_verify_time != verify_time
            or decoded_x_pos != str(x_pos)
            or decoded_slide_pos != str(slide_pos)
            or tuple(decoded_payload) != payload_keys
            or tuple(decoded_track) != track_keys
        ):
            raise PeRuntimeError("Node PE bridge data 明文元数据不一致")

        touch_start_times = self._event_times(
            decoded_track.get("tc"),
            "TrackList.tc",
        )
        touch_end_times = self._event_times(
            decoded_track.get("te"),
            "TrackList.te",
        )
        native_mousemove_events = self._mousemove_events(
            decoded_track.get("mm"),
            "TrackList.mm",
        )
        if len(touch_start_times) != 1 or len(touch_end_times) != 1:
            raise PeRuntimeError("Node PE bridge touch 首尾事件数异常")
        first_touch_age = touch_start_times[0]
        last_touch_age = touch_end_times[0]
        touch_duration = last_touch_age - first_touch_age
        last_touch_to_verify = (
            verify_time - (track_start_time + last_touch_age)
        )
        if len(device_getter_calls) != 1:
            raise PeRuntimeError(
                "Node PE bridge getter 调用次数异常"
            )
        getter_observed_at = device_getter_calls[0].observed_at_ms
        mousemove_events = tuple(
            event
            for event in native_mousemove_events
            if float(event["timeStamp"]) <= getter_observed_at
        )
        if len(mousemove_events) != count:
            raise PeRuntimeError(
                "Node PE bridge getter 前原生 mousemove 数量异常"
            )
        post_getter_mousemove_event_count = (
            len(native_mousemove_events) - len(mousemove_events)
        )
        last_mousemove_age = float(mousemove_events[-1]["timeStamp"])
        post_interaction_delay = (
            getter_observed_at - last_mousemove_age
        )
        if (
            abs(first_touch_age - first_touch_age_ms)
            > FIRST_TOUCH_TOLERANCE_MS
            or abs(first_touch_age - actual_dispatch_age) > 25
            or abs(touch_duration - total_duration)
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
            raise PeRuntimeError(
                "Node PE bridge data 的实际 touch/Verify 时序异常"
            )
        return PeRuntimeResult(
            data=data,
            track_event_count=count,
            native_device_token_matched=matched,
            payload_keys=payload_keys,
            track_keys=track_keys,
            x_pos=x_pos,
            slide_pos=slide_pos,
            track_start_time=track_start_time,
            verify_time=verify_time,
            target_first_touch_age_ms=target_first_touch_age,
            first_touch_age_ms=first_touch_age,
            touch_duration_ms=touch_duration,
            last_touch_to_verify_ms=last_touch_to_verify,
            post_interaction_delay_ms=post_interaction_delay,
            dispatch_lateness_ms=dispatch_lateness,
            device_getter_calls=device_getter_calls,
            native_mousemove_event_count=len(
                native_mousemove_events
            ),
            post_getter_mousemove_event_count=(
                post_getter_mousemove_event_count
            ),
            mousemove_events=mousemove_events,
        )

    @staticmethod
    def _getter_calls(
        value: Any,
        *,
        scene_id: str,
        certify_id: str,
    ) -> tuple[PeGetterCall, ...]:
        if not isinstance(value, list):
            raise PeRuntimeError("Node PE bridge getter 调用元数据无效")
        calls: list[PeGetterCall] = []
        for item in value:
            if not isinstance(item, dict):
                raise PeRuntimeError(
                    "Node PE bridge getter 调用项无效"
                )
            owner = item.get("owner")
            observed_at = item.get("observedAtMs")
            count = item.get("argumentCount")
            arguments = item.get("arguments")
            if (
                owner not in {"z_um", "um"}
                or isinstance(observed_at, bool)
                or not isinstance(observed_at, (int, float))
                or not math.isfinite(float(observed_at))
                or not 0 <= float(observed_at) <= 180_000
                or isinstance(count, bool)
                or not isinstance(count, int)
                or count < 0
                or not isinstance(arguments, list)
                or len(arguments) != count
            ):
                raise PeRuntimeError(
                    "Node PE bridge getter 调用结构无效"
                )
            parsed_arguments: list[PeGetterArgument] = []
            for argument in arguments:
                if not isinstance(argument, dict):
                    raise PeRuntimeError(
                        "Node PE bridge getter 参数元数据无效"
                    )
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
                    or equals_certify_id
                    != (argument_value == certify_id)
                    or equals_scene_id
                    != (argument_value == scene_id)
                ):
                    raise PeRuntimeError(
                        "Node PE bridge getter 参数形状无效"
                    )
                parsed_arguments.append(
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
                    arguments=tuple(parsed_arguments),
                )
            )
        return tuple(calls)

    @staticmethod
    def _string_tuple(value: Any, label: str) -> tuple[str, ...]:
        if (
            not isinstance(value, list)
            or not value
            or any(not isinstance(item, str) or not item for item in value)
        ):
            raise PeRuntimeError(f"Node PE bridge {label} 无效")
        return tuple(value)

    @staticmethod
    def _integer_value(value: Any, label: str) -> int:
        if isinstance(value, bool):
            raise PeRuntimeError(f"Node PE bridge {label} 无效")
        if isinstance(value, int):
            return value
        if isinstance(value, str) and value.isdigit():
            return int(value, 10)
        raise PeRuntimeError(f"Node PE bridge {label} 无效")

    @staticmethod
    def _number_value(value: Any, label: str) -> float:
        if (
            isinstance(value, bool)
            or not isinstance(value, (int, float))
            or not math.isfinite(value)
        ):
            raise PeRuntimeError(f"Node PE bridge {label} 无效")
        return float(value)

    @staticmethod
    def _event_times(value: Any, label: str) -> tuple[float, ...]:
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
                raise PeRuntimeError(
                    f"Node PE bridge {label} 时间无效"
                ) from exc
            if not math.isfinite(event_time) or event_time < 0:
                raise PeRuntimeError(
                    f"Node PE bridge {label} 时间无效"
                )
            times.append(event_time)
        return tuple(times)

    @staticmethod
    def _mousemove_events(
        value: Any,
        label: str,
    ) -> tuple[dict[str, float | bool | str], ...]:
        if not isinstance(value, str) or not value:
            raise PeRuntimeError(f"Node PE bridge {label} 无效")
        events: list[dict[str, float | bool | str]] = []
        previous_time: float | None = None
        for record in value.split("|"):
            fields = record.split(",")
            if len(fields) != 4:
                raise PeRuntimeError(
                    f"Node PE bridge {label} 格式异常"
                )
            try:
                x = float(fields[0])
                y = float(fields[1])
                timestamp = float(fields[2])
                trusted = float(fields[3])
            except ValueError as exc:
                raise PeRuntimeError(
                    f"Node PE bridge {label} 数值无效"
                ) from exc
            if (
                not all(
                    math.isfinite(item)
                    for item in (x, y, timestamp, trusted)
                )
                or abs(x) > 10_000
                or abs(y) > 10_000
                or not 0 <= timestamp <= 180_000
                or trusted != 1
                or (
                    previous_time is not None
                    and timestamp < previous_time
                )
            ):
                raise PeRuntimeError(
                    f"Node PE bridge {label} 数值异常"
                )
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
        if (
            not 1 <= len(events) <= 512
            or (
                events[-1]["timeStamp"]
                - events[0]["timeStamp"]
            ) > 60_000
        ):
            raise PeRuntimeError(
                f"Node PE bridge {label} 事件范围异常"
            )
        return tuple(events)

    @staticmethod
    def _parse_output(stdout: str) -> dict[str, Any]:
        for line in reversed(stdout.splitlines()):
            try:
                candidate = json.loads(line)
            except json.JSONDecodeError:
                continue
            if isinstance(candidate, dict):
                return candidate
        raise PeRuntimeError("Node PE bridge 未返回 JSON 结果")

    @staticmethod
    def _bridge_error(stderr: str | None) -> str:
        for line in reversed((stderr or "").splitlines()):
            try:
                candidate = json.loads(line)
            except json.JSONDecodeError:
                continue
            message = candidate.get("error") if isinstance(candidate, dict) else None
            if isinstance(message, str) and message:
                return message[:800]
        return "未返回结构化错误"
