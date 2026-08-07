"""当前 Aliyun V3 Puzzle PE 的纯 Python 计算实现。

受控差分覆盖了本机保存的 31 个 ``3.29.0/pe.*`` 分片：TrackList 字段顺序、
坐标运动式、RAF 尾事件、data key 与随机前缀均一致。``StaticPath`` 只用于选择
``arg`` 的公开分片 key；未知分片回落为同形随机值，因为当前服务端不校验该字段，
但其余 data 仍逐层自校验。
"""

from __future__ import annotations

import base64
import math
import secrets
import time
from dataclasses import dataclass, field
from typing import Any

from .. import config
from ..device_profile import DeviceProfile
from ..errors import PeRuntimeError
from ..protocol.data_codec import (
    DATA_PAYLOAD_FIELDS,
    TRACK_LIST_FIELDS,
    pack_data,
    transform64,
    unpack_data,
)
from ..vision.geometry import js_math_round, pe_puzzle_x_from_slide_pos

_ORIGIN_X = 94
_ORIGIN_Y = 548
_FRESHNESS_MARGIN_MS = 750
_MAX_TRACK_EVENTS = 512
_MAX_TRACK_DURATION_MS = 60_000

# 当前样本中 arg 的 16 字节 key。data 层 key 与此表无关，全部由 data_codec
# 的 PE091_DATA_KEY 处理。
_ARG_KEYS: dict[str, str] = {
    "3.29.0/pe.075.f195b27e016eecc3": "0kgx07m96ptqvk0c",
    "3.29.0/pe.068.4270dea37d68dd55": "om8wsc61jxel4dza",
    "3.29.0/pe.096.af431a090d6756cf": "2szbggnqfxa3rr37",
    "3.29.0/pe.094.f10b48a2d9910e6c": "8srw808jd9nvcnpf",
    "3.29.0/pe.061.66e362095c40df65": "ckg2kd9zn3zpct19",
    "3.29.0/pe.078.1ec68fc4b6bb14b7": "jogfs7z1zyf1e9go",
    "3.29.0/pe.088.ba6d7f87b920b698": "8iadi5j9z4kjf2cq",
    "3.29.0/pe.067.4c6dfbace0e490ad": "h3kdq43rgqryzcik",
    "3.29.0/pe.087.0603cddfaf6ed853": "m1i4acijv5u4km6r",
    "3.29.0/pe.072.64e9154e3053635f": "wmt6ga3rxqi4m8ei",
    "3.29.0/pe.086.5cd1be4bee85aff9": "34chpeu1rpq0gu7o",
    "3.29.0/pe.054.0622378da6a707ca": "nasmu37k6ajq78et",
    "3.29.0/pe.071.dc130f88dee78b04": "wwp31xzwdszbk3vw",
    "3.29.0/pe.062.40231f5d7f8351a3": "0lncwrzc9lwba9fq",
    "3.29.0/pe.077.b1b6f0c91d158f85": "jltdjjn2hw5ccco8",
    "3.29.0/pe.089.0db63cec7a44dbe2": "zl3j41v4shnf0iqd",
    "3.29.0/pe.080.eaa50e5b7a73b0c6": "tkasxtbj5agi48hs",
    "3.29.0/pe.066.d9f02d3dd6d9f5f6": "n2j4v2qqb6rz3djl",
    "3.29.0/pe.069.bcf01ca00f5bd260": "l0wo7rcn2ebvqg9e",
    "3.29.0/pe.091.00665af58b020d81": "0kd8i0mclivjow32",
    "3.29.0/pe.098.f37c3b7710aa77a9": "xgarfd2mxn3ne5qu",
}


@dataclass(frozen=True, slots=True)
class PeGetterArgument:
    value_type: str
    length: int | None
    value: str | None = field(repr=False)
    equals_certify_id: bool
    equals_scene_id: bool


@dataclass(frozen=True, slots=True)
class PeGetterPlan:
    """由 PE 协议公式导出的设备 token 刷新计划，不是浏览器运行时观测。"""

    owner: str
    derived_at_ms: float = field(repr=False)
    argument_count: int
    arguments: tuple[PeGetterArgument, ...]


@dataclass(frozen=True, slots=True)
class PeRuntimeResult:
    data: str = field(repr=False)
    track_event_count: int
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
    device_getter_plans: tuple[PeGetterPlan, ...]
    data_mousemove_event_count: int
    post_getter_data_event_count: int
    mousemove_events: tuple[dict[str, float | bool | str], ...] = field(
        repr=False
    )


def _event_type(sample: dict[str, Any], index: int, count: int) -> str:
    value = sample.get("type")
    if value is None:
        return (
            "touchstart"
            if index == 0
            else "touchend" if index == count - 1 else "touchmove"
        )
    if value not in {"touchstart", "touchmove", "touchend"}:
        raise ValueError(f"track[{index}].type 无效")
    return str(value)


def _validate_track(track: list[dict[str, Any]]) -> tuple[list[dict[str, Any]], int]:
    if not isinstance(track, list) or not 3 <= len(track) <= _MAX_TRACK_EVENTS:
        raise ValueError(f"track 必须包含 3..{_MAX_TRACK_EVENTS} 个采样")
    normalized: list[dict[str, Any]] = []
    duration = 0
    for index, sample in enumerate(track):
        if not isinstance(sample, dict):
            raise ValueError(f"track[{index}] 必须是 object")
        x, y, dt = sample.get("x"), sample.get("y"), sample.get("dt")
        if (
            isinstance(x, bool)
            or not isinstance(x, (int, float))
            or isinstance(y, bool)
            or not isinstance(y, (int, float))
            or not math.isfinite(float(x))
            or not math.isfinite(float(y))
            or isinstance(dt, bool)
            or not isinstance(dt, int)
            or dt < 0
            or (index == 0 and dt != 0)
        ):
            raise ValueError(f"track[{index}] 的 x/y/dt 无效")
        duration += dt
        normalized.append(
            {
                "x": float(x),
                "y": float(y),
                "dt": dt,
                "type": _event_type(sample, index, len(track)),
            }
        )
    if (
        normalized[0]["type"] != "touchstart"
        or normalized[-1]["type"] != "touchend"
        or any(item["type"] != "touchmove" for item in normalized[1:-1])
        or duration > _MAX_TRACK_DURATION_MS
    ):
        raise ValueError("track 必须是 touchstart、touchmove*、touchend")
    return normalized, duration


def build_pe_arg(certify_id: str, static_path: str | None) -> str:
    """构造 payload ``arg``；已知分片逐字节等价，未知分片保持同形。"""

    if not isinstance(certify_id, str) or not certify_id:
        raise ValueError("certify_id 不能为空")
    key = _ARG_KEYS.get(str(static_path or "").removesuffix(".js"))
    source = certify_id.encode("utf-8")
    transformed = (
        transform64(source, key)
        if key is not None
        else secrets.token_bytes(len(source))
    )
    return base64.b64encode(transformed).decode("ascii")


def _event_record(x: int, y: int, timestamp: int) -> str:
    return f"{x},{y},{timestamp},1"


class PeRuntimeClient:
    """不启动子进程、不加载 JavaScript 的 PE 计算器。"""

    def __init__(
        self,
        *,
        prefix: str = config.DEFAULT_PREFIX,
        region: str = config.DEFAULT_REGION,
        timeout: float = config.DEFAULT_TIMEOUT,
        device_profile: DeviceProfile,
    ) -> None:
        if not prefix or not region:
            raise ValueError("prefix 和 region 不能为空")
        if (
            isinstance(timeout, bool)
            or not isinstance(timeout, (int, float))
            or not math.isfinite(float(timeout))
            or timeout <= 0
        ):
            raise ValueError("timeout 必须为正数")
        if not isinstance(device_profile, DeviceProfile):
            raise ValueError("device_profile 必须是本轮生成的 DeviceProfile")
        self.prefix = prefix
        self.region = region
        self.timeout = float(timeout)
        self.device_profile = device_profile

    def build(
        self,
        *,
        scene_id: str,
        certify_id: str,
        dimensions: dict[str, Any],
        track: list[dict[str, Any]],
        static_path: str | None = None,
        expected_x_pos: int | None = None,
        init_begin_time: int | None = None,
        first_touch_age_ms: int | None = None,
    ) -> PeRuntimeResult:
        """生成并立即反向解包校验一轮 Verify data。"""

        if not isinstance(scene_id, str) or not scene_id:
            raise ValueError("scene_id 不能为空")
        if not isinstance(certify_id, str) or not certify_id:
            raise ValueError("certify_id 不能为空")
        if not isinstance(dimensions, dict):
            raise ValueError("dimensions 必须是 object")
        rendered_width = dimensions.get("renderedWidth", 300)
        handle_width = dimensions.get("handleWidth", 40)
        if any(
            isinstance(value, bool) or not isinstance(value, int)
            for value in (rendered_width, handle_width)
        ):
            raise ValueError("renderedWidth/handleWidth 必须是整数")
        if rendered_width <= 0 or not 0 < handle_width <= rendered_width:
            raise ValueError("滑轨尺寸无效")

        normalized, duration = _validate_track(track)
        first_age = (
            first_touch_age_ms
            if first_touch_age_ms is not None
            else config.DEFAULT_FIRST_TOUCH_AGE_RANGE[0]
        )
        if (
            isinstance(first_age, bool)
            or not isinstance(first_age, int)
            or first_age < 1
        ):
            raise ValueError("first_touch_age_ms 必须是正整数")

        now_ms = int(time.time() * 1000)
        requested_start = now_ms if init_begin_time is None else init_begin_time
        if isinstance(requested_start, bool) or not isinstance(requested_start, int):
            raise ValueError("init_begin_time 必须是 epoch 毫秒整数")
        latest_start = now_ms - first_age - duration - _FRESHNESS_MARGIN_MS
        track_start = min(requested_start, latest_start)
        if track_start < 1:
            raise PeRuntimeError("无法建立有效的 PE 逻辑时钟")

        elapsed = first_age
        records: list[tuple[str, str]] = []
        mousemove_events: list[dict[str, float | bool | str]] = []
        last_move_x = 0.0
        for sample in normalized:
            elapsed += int(sample["dt"])
            x = js_math_round(_ORIGIN_X + float(sample["x"]))
            y_value = _ORIGIN_Y + float(sample["y"])
            # 轨迹 y 在当前资产中始终为正绝对坐标；显式 half-up 保留 JS 语义。
            y = math.floor(y_value + 0.5)
            record = _event_record(x, y, elapsed)
            records.append((str(sample["type"]), record))
            mousemove_events.append(
                {
                    "type": "mousemove",
                    "x": float(x),
                    "y": float(y),
                    "timeStamp": float(elapsed),
                    "isTrusted": True,
                }
            )
            if sample["type"] == "touchmove":
                last_move_x = float(sample["x"])

        maximum_slide = rendered_width - handle_width
        slide_pos = js_math_round(min(max(last_move_x, 0.0), maximum_slide))
        x_pos = js_math_round(pe_puzzle_x_from_slide_pos(slide_pos))
        if expected_x_pos is not None and (
            isinstance(expected_x_pos, bool) or not isinstance(expected_x_pos, int)
        ):
            raise ValueError("expected_x_pos 必须是整数或 None")
        if expected_x_pos is not None and abs(x_pos - expected_x_pos) > 1:
            raise PeRuntimeError(
                f"纯 Python PE xPos 与识别结果不一致：{x_pos} != {expected_x_pos}"
            )

        tc = "|".join(value for kind, value in records if kind == "touchstart")
        tmv = "|".join(value for kind, value in records if kind == "touchmove")
        te = "|".join(value for kind, value in records if kind == "touchend")
        last_x, last_y = (int(item) for item in records[-1][1].split(",")[:2])
        raf_tail = _event_record(
            js_math_round(last_x * 1.15),
            js_math_round(last_y * 1.15),
            elapsed + 16,
        )
        mm = "|".join([*(value for _, value in records), raf_tail])
        screen = self.device_profile.screen
        si = ",".join(
            str(value)
            for value in (
                screen.inner_width,
                screen.width,
                screen.inner_height,
                screen.inner_width,
                screen.inner_height,
                screen.outer_height,
                screen.height,
                "120.00000000000082",
                screen.outer_width,
            )
        )
        track_list = {
            "mc": "",
            "tc": tc,
            "mu": "",
            "te": te,
            "mp": "",
            "tmv": tmv,
            "mm": mm,
            "ks": "",
            "fi": "",
            "startTime": track_start,
            "si": si,
        }
        verify_time = track_start + elapsed
        payload = {
            "TrackList": track_list,
            "TrackStartTime": track_start,
            "VerifyTime": verify_time,
            "xPos": str(x_pos),
            "slidePos": str(slide_pos),
            "arg": build_pe_arg(certify_id, static_path),
        }
        data = pack_data(payload)
        decoded = unpack_data(data).payload
        if decoded != payload:
            raise PeRuntimeError("纯 Python PE data 正向/反向校验不一致")

        argument = PeGetterArgument(
            value_type="string",
            length=len(certify_id),
            value=certify_id,
            equals_certify_id=True,
            equals_scene_id=certify_id == scene_id,
        )
        getter = PeGetterPlan(
            owner="python-compatible.getToken",
            derived_at_ms=float(elapsed),
            argument_count=1,
            arguments=(argument,),
        )
        return PeRuntimeResult(
            data=data,
            track_event_count=len(normalized),
            payload_keys=DATA_PAYLOAD_FIELDS,
            track_keys=TRACK_LIST_FIELDS,
            x_pos=x_pos,
            slide_pos=slide_pos,
            track_start_time=track_start,
            verify_time=verify_time,
            target_first_touch_age_ms=first_age,
            first_touch_age_ms=float(first_age),
            touch_duration_ms=float(duration),
            last_touch_to_verify_ms=0.0,
            post_interaction_delay_ms=0.0,
            dispatch_lateness_ms=0.0,
            device_getter_plans=(getter,),
            data_mousemove_event_count=len(records) + 1,
            post_getter_data_event_count=1,
            mousemove_events=tuple(mousemove_events),
        )


__all__ = [
    "PeGetterArgument",
    "PeGetterPlan",
    "PeRuntimeClient",
    "PeRuntimeResult",
    "build_pe_arg",
]
