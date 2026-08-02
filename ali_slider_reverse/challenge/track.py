"""触摸轨迹资产的读取、缩放与逐轮扰动。

``default_touch_track.json`` 不是测试记录，而是默认运行必需的**运行资产**：它是
一段已脱敏的真实人工滑动轨迹，只包含相对坐标与时间，不含 token、Cookie、session、
CertifyId 或签名密钥。

## 为什么只有一条轨迹，却不能每轮原样重放

真实人手的加速、抖动与停顿分布很难凭参数生成得像，所以形状仍然来自那条真实轨迹。
但"按距离等比缩放"是个**线性**变换：同一段距离每次都得到逐字节相同的 ``dt``
序列、相同的 y、相同的采样点数。跑几百轮之后，这条恒定的节奏本身就成了一个稳定
标识——和写死的设备指纹是同一类问题。

因此在缩放之后再叠一层扰动，覆盖真人每次滑动都会不同的几个维度：

```text
速度      全局 dt 缩放          同一个人两次滑动不会一样快
节奏      逐点 dt 抖动          采样间隔本来就不均匀
采样      随机丢少量 touchmove   浏览器的 touchmove 合帧是随机的
抖动      y 基线偏移 + 逐点噪声  手指不会走在一条直线上
压力      force 基线 + 逐点变化  接触压力随滑动变化
接触面    radiusX/Y             不同手指、不同角度不一样
```

**首尾两点不参与扰动**：首点必须是 ``x=0, dt=0``，末点的 x 必须精确等于本轮的
目标位移——差一两像素就直接导致 slidePos 对不上，整轮作废。
"""

from __future__ import annotations

import json
import math
import secrets
from pathlib import Path
from random import Random
from typing import Any

from .. import config
from ..errors import AliSliderError


SPEED_RANGE = (0.80, 1.28)
"""整条轨迹的时间缩放区间。"""

SAMPLE_JITTER_MS = 2
"""单个采样间隔的抖动上限（毫秒）。"""

DROP_RATIO_RANGE = (0.0, 0.10)
"""随机丢弃的 touchmove 比例区间。"""

FORCE_RANGE = (0.35, 0.85)
"""触点压力基线区间。"""

RADIUS_RANGE = (8.0, 26.0)
"""触点接触半径基线区间（CSS 像素）。"""

_MIN_EVENTS = 3


def _read_fixture(path: Path) -> tuple[list[dict[str, Any]], float]:
    """读取列式轨迹资产，返回 ``(采样列表, 源滑动距离)``。

    资产用 ``columns`` + ``track`` 的列式结构存储以节省体积，这里展开成对象。
    """

    try:
        fixture = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise AliSliderError(f"无法读取人工轨迹资产：{path}") from exc

    columns = fixture.get("columns")
    rows = fixture.get("track")
    if not isinstance(columns, list) or not isinstance(rows, list):
        raise AliSliderError("人工轨迹 fixture schema 无效")

    indexes = {str(name): index for index, name in enumerate(columns)}
    for name in ("type", "t", "x", "y"):
        if name not in indexes:
            raise AliSliderError(f"人工轨迹缺少列 {name}")

    samples = [
        {
            "type": str(row[indexes["type"]]),
            "t": float(row[indexes["t"]]),
            "x": float(row[indexes["x"]]),
            "y": float(row[indexes["y"]]),
        }
        for row in rows
    ]
    if len(samples) < _MIN_EVENTS:
        raise AliSliderError("人工轨迹至少需要三个采样")

    # 优先用资产自带的最终手柄位置；缺失时退回末采样的 x。
    source_distance = float(
        fixture.get("image", {}).get("finalSliderLeft", samples[-1]["x"])
    )
    return samples, source_distance


def _rescale(
    samples: list[dict[str, Any]],
    *,
    target_distance: float,
    source_distance: float,
) -> list[dict[str, Any]]:
    """把绝对时间的 ``t/x/y`` 采样转成相对 ``x/y/dt``，并缩放到目标距离。

    首尾两点会被钉死：首点 ``x=0, dt=0``，末点 ``x=target_distance``。缩放引入的
    浮点误差不能让手柄停在差一两像素的位置——那会直接导致 slidePos 对不上。
    """

    if not math.isfinite(target_distance) or target_distance < 0:
        raise ValueError("target_distance 不能为负数")
    if not math.isfinite(source_distance) or source_distance <= 0:
        raise ValueError("source_distance 必须为正数")

    output: list[dict[str, Any]] = []
    previous_time: float | None = None
    for index, sample in enumerate(samples):
        timestamp, x, y = sample["t"], sample["x"], sample["y"]
        if not all(math.isfinite(value) for value in (timestamp, x, y)):
            raise ValueError(f"samples[{index}] 含非有限数")
        dt = 0.0 if previous_time is None else timestamp - previous_time
        if dt < 0:
            raise ValueError("记录轨迹时间必须单调不减")
        output.append(
            {
                "x": x / source_distance * target_distance,
                "y": y,
                "dt": dt,
                "type": str(sample["type"]),
            }
        )
        previous_time = timestamp

    output[0]["x"] = 0.0
    output[0]["dt"] = 0.0
    output[-1]["x"] = float(target_distance)
    return output


def _thin_out(
    samples: list[dict[str, Any]],
    rng: Random,
) -> list[dict[str, Any]]:
    """随机丢弃少量 touchmove，被丢点的时长并入下一点。

    浏览器把连续的触摸位置合并成 touchmove 时本来就受帧率影响，采样点数每次不同
    是常态。丢点只在中段进行——首尾两点承载着起始位置与最终位移。
    """

    drop_ratio = rng.uniform(*DROP_RATIO_RANGE)
    movable = range(1, len(samples) - 1)
    droppable = max(0, len(samples) - _MIN_EVENTS)
    drop_count = min(droppable, int(len(samples) * drop_ratio))
    if drop_count <= 0:
        return samples

    dropped = set(rng.sample(list(movable), drop_count))
    kept: list[dict[str, Any]] = []
    carried = 0.0
    for index, sample in enumerate(samples):
        if index in dropped:
            carried += sample["dt"]
            continue
        sample["dt"] += carried
        carried = 0.0
        kept.append(sample)
    return kept


def _humanize(
    samples: list[dict[str, Any]],
    rng: Random,
    *,
    target_distance: float,
) -> list[dict[str, int | float | str]]:
    """在缩放后的轨迹上叠加逐轮扰动，并量化成桥要的字段。"""

    speed = rng.uniform(*SPEED_RANGE)
    y_offset = rng.randint(-4, 5)
    force_base = rng.uniform(*FORCE_RANGE)
    radius_base = rng.uniform(*RADIUS_RANGE)
    # 接触面的长短轴之比由手指角度决定，同一次滑动里基本不变。
    radius_ratio = rng.uniform(0.85, 1.15)

    last_index = len(samples) - 1
    target_x = round(target_distance)
    events: list[dict[str, int | float | str]] = []
    for index, sample in enumerate(samples):
        if index == 0:
            dt = 0
        else:
            jitter = rng.uniform(-SAMPLE_JITTER_MS, SAMPLE_JITTER_MS)
            dt = max(0, round(sample["dt"] * speed + jitter))

        if index == 0:
            x = 0
        elif index >= last_index - 1 or round(sample["x"]) >= target_x:
            # 尾段——手柄已经到位。动态 PE 的 slidePos 取自**最后一个
            # touchmove**，这里抖一个像素或把它抽稀掉，slidePos 就会与识别结果
            # 差 1px，整轮作废。真实轨迹的收尾本来也是 touchmove 与 touchend
            # 停在同一个 x 上。
            x = target_x
        else:
            # 中段允许 ±1px 的微小回退——真人手指本来就会抖；上界仍钉死在目标
            # 位移，避免抖动把手柄推过终点。
            x = min(target_x, round(sample["x"] + rng.randint(-1, 1)))

        events.append(
            {
                "x": x,
                "y": round(sample["y"]) + y_offset + rng.randint(-1, 1),
                "dt": dt,
                "type": sample["type"],
                "force": round(
                    min(1.0, max(0.0, force_base + rng.uniform(-0.06, 0.06))),
                    4,
                ),
                "radiusX": round(radius_base + rng.uniform(-2.0, 2.0), 3),
                "radiusY": round(
                    (radius_base + rng.uniform(-2.0, 2.0)) * radius_ratio, 3
                ),
            }
        )
    return events


def load_scaled_touch_track(
    *,
    target_distance: float,
    fixture_path: str | Path | None = None,
    rng: Random | None = None,
) -> list[dict[str, Any]]:
    """读取轨迹资产，缩放到本轮手柄距离，并叠加逐轮扰动。

    返回的每项形如
    ``{"x": int, "y": int, "dt": int, "type": str, "force": float,
    "radiusX": float, "radiusY": float}``，可直接交给
    :class:`~ali_slider_reverse.runtime.node_pe.PeRuntimeClient` 回放。

    传入固定种子的 :class:`Random` 可以复现同一条轨迹，用于测试与问题复盘。
    """

    generator = rng if rng is not None else secrets.SystemRandom()
    path = config.DEFAULT_TOUCH_TRACK if fixture_path is None else Path(fixture_path)
    samples, source_distance = _read_fixture(path)
    scaled = _rescale(
        samples,
        target_distance=target_distance,
        source_distance=source_distance,
    )
    thinned = _thin_out(scaled, generator)
    return _humanize(thinned, generator, target_distance=target_distance)


__all__ = ["load_scaled_touch_track"]
