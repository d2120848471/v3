"""可复现的滑块轨迹、真实触摸事件归一化和 SDK 轨迹压平工具。"""

from __future__ import annotations

import math
import random
from collections.abc import Iterable, Mapping, Sequence
from dataclasses import dataclass
from typing import Any


SDK_TRACK_FIELDS: dict[str, tuple[str, ...]] = {
    # 字段顺序来自 pe.091 运行态的 ``t_`` 映射；顺序会直接影响最终 data。
    "fi": ("time", "type", "isTrusted"),
    "ks": ("time", "key", "isTrusted"),
    "mc": ("x", "y", "time", "button", "isTrusted"),
    "mm": ("x", "y", "time", "isTrusted"),
    "mp": ("x", "y", "time", "isTrusted"),
    "mu": ("x", "y", "time", "button", "isTrusted"),
    "tc": ("x", "y", "time", "isTrusted"),
    "te": ("x", "y", "time", "isTrusted"),
    "tmv": ("x", "y", "time", "isTrusted"),
    "si": (
        "innerWidth",
        "screenWidth",
        "innerHeight",
        "clientWidth",
        "clientWidthHeight",
        "outerHeight",
        "screenHeight",
        "fps",
        "outerWidth",
    ),
}

# 成功样本中 nV(rr) 的稳定输出顺序。显式列出可避免调用方 dict 构造顺序
# 意外改变 JSON.stringify 前的对象顺序。
SDK_TRACK_OUTPUT_ORDER = (
    "fi",
    "ks",
    "mc",
    "mm",
    "mp",
    "mu",
    "si",
    "startTime",
    "tc",
    "te",
    "tmv",
)


@dataclass(frozen=True, slots=True)
class TrackEvent:
    """相对轨迹事件；``dt`` 是相对上一事件经过的毫秒数。"""

    x: int
    y: int
    dt: int

    def as_dict(self) -> dict[str, int]:
        return {"x": self.x, "y": self.y, "dt": self.dt}


@dataclass(frozen=True, slots=True)
class TrackConfig:
    """缓出轨迹参数。

    ``steps`` 表示移动区间数量，最终会输出 ``steps + 1`` 个事件（含起点）。
    """

    duration_ms: int = 900
    steps: int = 36
    easing_power: float = 3.0
    vertical_jitter: float = 1.5
    horizontal_jitter: float = 0.35
    time_jitter: float = 0.20
    overshoot: float = 0.0
    overshoot_fraction: float = 0.84

    def validate(self) -> None:
        if self.duration_ms <= 0:
            raise ValueError("duration_ms 必须为正数")
        if self.steps < 2:
            raise ValueError("steps 至少为 2")
        if self.duration_ms < self.steps:
            raise ValueError("duration_ms 至少应等于 steps，保证每步 dt >= 1ms")
        if self.easing_power <= 0:
            raise ValueError("easing_power 必须为正数")
        if self.vertical_jitter < 0 or self.horizontal_jitter < 0:
            raise ValueError("轨迹抖动幅度不能为负数")
        if not 0 <= self.time_jitter < 1:
            raise ValueError("time_jitter 必须位于 [0, 1)")
        if self.overshoot < 0:
            raise ValueError("overshoot 不能为负数")
        if not 0.5 <= self.overshoot_fraction < 1:
            raise ValueError("overshoot_fraction 必须位于 [0.5, 1)")


def _integer_durations(
    *,
    total_ms: int,
    steps: int,
    jitter: float,
    rng: random.Random,
) -> list[int]:
    """生成正整数时间片，并保持总和严格等于 duration_ms。"""

    weights = [max(0.05, 1.0 + rng.uniform(-jitter, jitter)) for _ in range(steps)]
    remaining = total_ms - steps
    if remaining == 0:
        return [1] * steps

    scaled = [weight / sum(weights) * remaining for weight in weights]
    floors = [math.floor(value) for value in scaled]
    durations = [1 + value for value in floors]
    left = remaining - sum(floors)

    # 最大余数法可避免普通 round 造成总时长漂移。
    order = sorted(
        range(steps),
        key=lambda index: scaled[index] - floors[index],
        reverse=True,
    )
    for index in order[:left]:
        durations[index] += 1
    return durations


def _ease_out(progress: float, power: float) -> float:
    return 1.0 - (1.0 - progress) ** power


def generate_human_track(
    distance: float,
    *,
    seed: int | str | bytes | bytearray | None = None,
    start_x: float = 0.0,
    start_y: float = 0.0,
    config: TrackConfig | None = None,
) -> list[dict[str, int]]:
    """生成缓出、可配置且可由 ``seed`` 完整复现的 ``x/y/dt`` 轨迹。

    默认 x 单调向目标移动；设置 ``overshoot`` 后会先略微越过，再回收到目标。
    首事件 ``dt=0``，后续 ``dt`` 总和严格等于 ``duration_ms``。
    """

    if distance < 0:
        raise ValueError("distance 不能为负数")
    cfg = TrackConfig() if config is None else config
    cfg.validate()
    rng = random.Random(seed)
    durations = _integer_durations(
        total_ms=cfg.duration_ms,
        steps=cfg.steps,
        jitter=cfg.time_jitter,
        rng=rng,
    )

    target = start_x + distance
    overshoot_target = target + cfg.overshoot
    result = [TrackEvent(round(start_x), round(start_y), 0)]
    previous_x = float(start_x)
    current_y = float(start_y)

    for index in range(1, cfg.steps + 1):
        progress = index / cfg.steps
        if cfg.overshoot > 0 and progress < cfg.overshoot_fraction:
            local = progress / cfg.overshoot_fraction
            ideal_x = start_x + (overshoot_target - start_x) * _ease_out(
                local, cfg.easing_power
            )
            monotonic_forward = True
        elif cfg.overshoot > 0:
            local = (progress - cfg.overshoot_fraction) / (
                1.0 - cfg.overshoot_fraction
            )
            # 回收段采用平滑三次曲线，末点严格落在真实目标。
            smooth = local * local * (3.0 - 2.0 * local)
            ideal_x = overshoot_target + (target - overshoot_target) * smooth
            monotonic_forward = False
        else:
            ideal_x = start_x + distance * _ease_out(progress, cfg.easing_power)
            monotonic_forward = True

        if index == cfg.steps:
            x = target
            y = start_y
        else:
            # 越接近两端抖动越小，避免起点/终点产生突兀偏移。
            envelope = math.sin(math.pi * progress)
            x = ideal_x + rng.gauss(0.0, cfg.horizontal_jitter * envelope)
            if monotonic_forward:
                x = max(previous_x, min(x, overshoot_target))
            else:
                x = min(previous_x, max(x, target))

            # y 使用带回中心阻尼的随机游走，比每点独立随机更连续。
            current_y += rng.gauss(0.0, cfg.vertical_jitter * 0.45)
            current_y += (start_y - current_y) * 0.28
            bound = max(cfg.vertical_jitter, 0.0)
            y = max(start_y - bound, min(current_y, start_y + bound))

        rounded_x = round(x)
        if cfg.overshoot == 0:
            rounded_x = max(result[-1].x, min(rounded_x, round(target)))
        rounded_y = round(y)
        result.append(TrackEvent(rounded_x, rounded_y, durations[index - 1]))
        previous_x = x
        current_y = y

    # 核心协议只暴露普通 dict，方便直接紧凑 JSON 序列化或交给 Node/JS。
    return [event.as_dict() for event in result]


_X_KEYS = ("clientX", "pageX", "screenX", "x")
_Y_KEYS = ("clientY", "pageY", "screenY", "y")
_TIME_KEYS = ("timeStamp", "timestamp", "time", "t")


def _first_present(mapping: Mapping[str, Any], keys: Sequence[str]) -> Any:
    for key in keys:
        if key in mapping:
            return mapping[key]
    return None


def _touch_point(event: Mapping[str, Any]) -> Mapping[str, Any]:
    """兼容浏览器 TouchEvent 的 touches/changedTouches 和扁平采样对象。"""

    for key in ("touches", "changedTouches", "targetTouches"):
        points = event.get(key)
        if isinstance(points, Sequence) and not isinstance(points, (str, bytes)) and points:
            point = points[0]
            if isinstance(point, Mapping):
                return point
    return event


def normalize_touch_events(
    events: Iterable[Mapping[str, Any]],
    *,
    relative: bool = True,
    timestamp_unit: str = "ms",
) -> list[dict[str, int]]:
    """把真实触摸采样归一化为 ``x/y/dt``。

    支持扁平的 ``clientX/clientY/timeStamp``，也支持原始 TouchEvent 风格的
    ``touches``/``changedTouches``。``timestamp_unit`` 可为 ``"ms"`` 或 ``"s"``。
    若某事件没有绝对时间戳但含 ``dt``，则把该 ``dt`` 作为相对上一事件的毫秒数。
    """

    if timestamp_unit not in {"ms", "s"}:
        raise ValueError("timestamp_unit 只能是 'ms' 或 's'")
    scale = 1000.0 if timestamp_unit == "s" else 1.0
    raw_events = list(events)
    if not raw_events:
        return []

    output: list[dict[str, int]] = []
    origin_x: float | None = None
    origin_y: float | None = None
    previous_time: float | None = None
    synthetic_time = 0.0

    for index, event in enumerate(raw_events):
        if not isinstance(event, Mapping):
            raise TypeError(f"events[{index}] 必须是 mapping")
        point = _touch_point(event)
        x_value = _first_present(point, _X_KEYS)
        y_value = _first_present(point, _Y_KEYS)
        if x_value is None or y_value is None:
            raise ValueError(f"events[{index}] 缺少可识别的 x/y 坐标")
        try:
            x = float(x_value)
            y = float(y_value)
        except (TypeError, ValueError) as exc:
            raise ValueError(f"events[{index}] 的 x/y 必须是数值") from exc
        if not math.isfinite(x) or not math.isfinite(y):
            raise ValueError(f"events[{index}] 的 x/y 必须是有限数")

        time_value = _first_present(event, _TIME_KEYS)
        if time_value is not None:
            try:
                current_time = float(time_value) * scale
            except (TypeError, ValueError) as exc:
                raise ValueError(f"events[{index}] 的时间戳必须是数值") from exc
            if not math.isfinite(current_time):
                raise ValueError(f"events[{index}] 的时间戳必须是有限数")
            dt = 0 if previous_time is None else round(current_time - previous_time)
            synthetic_time = current_time
        elif "dt" in event:
            try:
                dt = round(float(event["dt"]))
            except (TypeError, ValueError) as exc:
                raise ValueError(f"events[{index}].dt 必须是数值") from exc
            synthetic_time += dt
            current_time = synthetic_time
        else:
            raise ValueError(f"events[{index}] 缺少时间戳或 dt")
        if dt < 0:
            raise ValueError(f"events[{index}] 的时间不能早于上一事件")

        if origin_x is None:
            origin_x, origin_y = x, y
        normalized_x = x - origin_x if relative else x
        normalized_y = y - origin_y if relative else y
        output.append({"x": round(normalized_x), "y": round(normalized_y), "dt": dt})
        previous_time = current_time

    return output


def _js_track_scalar(value: Any) -> str:
    """复刻 pe.091 轨迹压平时的 ``String(value || "-1")``。

    这里刻意保留 JavaScript 的“假值回退”行为：缺字段、``None``、空字符串、
    ``False`` 和数值零都会变成 ``-1``。真实坐标/时间通常非零，但固定向量需要
    精确复刻这一处容易忽略的边界语义。
    """

    if not value:
        return "-1"
    if value is True:
        return "true"
    if isinstance(value, str):
        return value
    if isinstance(value, int):
        return str(value)
    if isinstance(value, float):
        if not math.isfinite(value):
            raise ValueError("SDK 轨迹字段不能包含 NaN/Infinity")
        return str(int(value)) if value.is_integer() else str(value)
    raise TypeError(f"SDK 轨迹字段不支持 {type(value).__name__}")


def _flatten_sdk_record(
    record: Mapping[str, Any],
    fields: Sequence[str],
    *,
    label: str,
) -> str:
    if not isinstance(record, Mapping):
        raise TypeError(f"{label} 必须是 mapping")
    return ",".join(_js_track_scalar(record.get(field)) for field in fields)


def serialize_sdk_track_list(
    track: Mapping[str, Any],
    *,
    include_empty: bool = True,
) -> dict[str, str | int | float]:
    """复刻动态脚本 ``nV(track)`` 的轨迹压平结果。

    事件数组先按各自字段表用逗号连接，再以 ``|`` 连接事件；``si`` 是单个屏幕
    信息对象；``startTime`` 保留 epoch 毫秒数。返回对象顺序固定为成功样本中的
    pe.091 顺序，适合直接放入后续 ``TrackList``。
    """

    if not isinstance(track, Mapping):
        raise TypeError("track 必须是 mapping")

    unknown = [key for key in track if key not in SDK_TRACK_OUTPUT_ORDER]
    if unknown:
        raise ValueError(f"track 包含未知字段：{unknown}")

    output: dict[str, str | int | float] = {}
    for key in SDK_TRACK_OUTPUT_ORDER:
        if key == "startTime":
            if key not in track:
                if include_empty:
                    continue
                raise ValueError("track 缺少 startTime")
            value = track[key]
            if isinstance(value, bool) or not isinstance(value, (int, float)):
                raise TypeError("track.startTime 必须是数值")
            if not math.isfinite(float(value)) or value < 0:
                raise ValueError("track.startTime 必须是非负有限数")
            output[key] = value
            continue

        fields = SDK_TRACK_FIELDS[key]
        value = track.get(key)
        if key == "si":
            if value is None:
                if include_empty:
                    continue
                raise ValueError("track 缺少 si")
            output[key] = _flatten_sdk_record(value, fields, label="track.si")
            continue

        if value is None:
            events: list[Any] = []
        elif isinstance(value, Sequence) and not isinstance(value, (str, bytes)):
            events = list(value)
        else:
            raise TypeError(f"track.{key} 必须是事件序列")

        if events or include_empty:
            output[key] = "|".join(
                _flatten_sdk_record(event, fields, label=f"track.{key}[{index}]")
                for index, event in enumerate(events)
            )

    return output


def build_sdk_touch_tracker(
    events: Iterable[Mapping[str, Any]],
    *,
    start_time_ms: int,
    performance_start_ms: int = 20_000,
    origin_x: int = 94,
    origin_y: int = 548,
    screen_info: Mapping[str, Any] | None = None,
    include_mouse_tail: bool = True,
) -> dict[str, Any]:
    """把相对 ``x/y/dt`` 采样构造成当前 SDK tracker。

    第一项进入 ``tc``、中间项进入 ``tmv``、末项进入 ``te``；所有采样同时进入
    ``mm``，字段顺序再由 :func:`serialize_sdk_track_list` 精确压平。事件 ``time``
    使用页面 performance 时间域，``startTime`` 则使用 epoch 毫秒，两者与成功
    抓包的时间域边界一致。
    """

    if isinstance(start_time_ms, bool) or not isinstance(start_time_ms, int):
        raise TypeError("start_time_ms 必须是整数")
    if start_time_ms < 0:
        raise ValueError("start_time_ms 不能为负数")
    if performance_start_ms < 0:
        raise ValueError("performance_start_ms 不能为负数")

    samples = list(events)
    if len(samples) < 3:
        raise ValueError("至少需要起点、移动点和终点三个采样")
    normalized: list[dict[str, int]] = []
    elapsed = 0
    for index, sample in enumerate(samples):
        if not isinstance(sample, Mapping):
            raise TypeError(f"events[{index}] 必须是 mapping")
        try:
            x = round(float(sample["x"]))
            y = round(float(sample["y"]))
            dt = round(float(sample["dt"]))
        except (KeyError, TypeError, ValueError) as exc:
            raise ValueError(
                f"events[{index}] 必须包含数值 x/y/dt"
            ) from exc
        if dt < 0 or (index == 0 and dt != 0):
            raise ValueError("首事件 dt 必须为 0，后续 dt 不能为负数")
        elapsed += dt
        normalized.append(
            {
                "x": origin_x + x,
                "y": origin_y + y,
                "time": performance_start_ms + elapsed,
                "isTrusted": 1,
            }
        )

    default_screen = {
        "innerWidth": 430,
        "screenWidth": 430,
        "innerHeight": 932,
        "clientWidth": 430,
        "clientWidthHeight": 932,
        "outerHeight": 932,
        "screenHeight": 932,
        "fps": 122.69938650306749,
        "outerWidth": 430,
    }
    effective_screen = (
        default_screen if screen_info is None else dict(screen_info)
    )
    # Chrome 移动设备模式会在 touch 流旁产生一条几乎重合的 mousemove 流：
    # 起点早约 1ms，释放点晚约 1ms，并在释放后补一个鼠标位置。该关系来自成功包，
    # 不能把 ``mm`` 简单写成与 touch 数组同一对象和同一时间。
    mouse_moves = [dict(event) for event in normalized]
    mouse_moves[0]["time"] = max(1, mouse_moves[0]["time"] - 1)
    mouse_moves[-1]["time"] += 1
    if include_mouse_tail:
        screen_width = int(effective_screen.get("screenWidth", 430))
        mouse_moves.append(
            {
                "x": min(screen_width - 1, mouse_moves[-1]["x"] + 47),
                "y": mouse_moves[-1]["y"] + 83,
                "time": mouse_moves[-1]["time"] + 16,
                "isTrusted": 1,
            }
        )

    tracker: dict[str, Any] = {
        "fi": [],
        "ks": [],
        "mc": [],
        "mm": mouse_moves,
        "mp": [],
        "mu": [],
        "si": effective_screen,
        "startTime": start_time_ms,
        "tc": [normalized[0]],
        "te": [normalized[-1]],
        "tmv": normalized[1:-1],
    }
    return tracker


def rescale_recorded_touch_track(
    samples: Iterable[Mapping[str, Any]],
    *,
    target_distance: float,
    source_distance: float | None = None,
) -> list[dict[str, int]]:
    """把已授权记录的 ``t/x/y`` 人工轨迹缩放到本次手柄距离。"""

    records = list(samples)
    if len(records) < 3:
        raise ValueError("记录轨迹至少需要三个采样")
    if not math.isfinite(target_distance) or target_distance < 0:
        raise ValueError("target_distance 不能为负数")
    if source_distance is None:
        try:
            source_distance = float(records[-1]["x"])
        except (KeyError, TypeError, ValueError) as exc:
            raise ValueError("无法从末采样推导 source_distance") from exc
    if not math.isfinite(source_distance) or source_distance <= 0:
        raise ValueError("source_distance 必须为正数")

    output: list[dict[str, int]] = []
    previous_time: float | None = None
    for index, record in enumerate(records):
        try:
            timestamp = float(record["t"])
            x = float(record["x"])
            y = float(record["y"])
        except (KeyError, TypeError, ValueError) as exc:
            raise ValueError(
                f"samples[{index}] 必须包含数值 t/x/y"
            ) from exc
        if not all(math.isfinite(value) for value in (timestamp, x, y)):
            raise ValueError(f"samples[{index}] 含非有限数")
        dt = (
            0
            if previous_time is None
            else round(timestamp - previous_time)
        )
        if dt < 0:
            raise ValueError("记录轨迹时间必须单调不减")
        output.append(
            {
                "x": round(x / source_distance * target_distance),
                "y": round(y),
                "dt": dt,
            }
        )
        previous_time = timestamp
    output[0]["x"] = 0
    output[0]["dt"] = 0
    output[-1]["x"] = round(target_distance)
    return output


# 语义化别名：调用者可以明确表达“从触摸事件构造轨迹”。
track_from_touch_events = normalize_touch_events


__all__ = [
    "SDK_TRACK_FIELDS",
    "SDK_TRACK_OUTPUT_ORDER",
    "TrackConfig",
    "TrackEvent",
    "generate_human_track",
    "build_sdk_touch_tracker",
    "normalize_touch_events",
    "rescale_recorded_touch_track",
    "serialize_sdk_track_list",
    "track_from_touch_events",
]
