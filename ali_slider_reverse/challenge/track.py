"""触摸轨迹资产的读取与缩放。

``default_touch_track.json`` 不是测试记录，而是默认运行必需的**运行资产**：它是
一段已脱敏的真实人工滑动轨迹，只包含相对坐标与时间，不含 token、Cookie、session、
CertifyId 或签名密钥。

轨迹本身不做合成——真实人手的加速、抖动与停顿分布很难凭参数生成得像。这里的做法
是把一条真实轨迹按本轮的目标距离等比缩放，保留原有的时间节奏。
"""

from __future__ import annotations

import json
import math
from pathlib import Path
from typing import Any

from .. import config
from ..errors import AliSliderError


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
    if len(samples) < 3:
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
) -> list[dict[str, int]]:
    """把绝对时间的 ``t/x/y`` 采样转成相对 ``x/y/dt``，并缩放到目标距离。

    首尾两点会被钉死：首点 ``x=0, dt=0``，末点 ``x=target_distance``。缩放引入的
    浮点误差不能让手柄停在差一两像素的位置——那会直接导致 slidePos 对不上。
    """

    if not math.isfinite(target_distance) or target_distance < 0:
        raise ValueError("target_distance 不能为负数")
    if not math.isfinite(source_distance) or source_distance <= 0:
        raise ValueError("source_distance 必须为正数")

    output: list[dict[str, int]] = []
    previous_time: float | None = None
    for index, sample in enumerate(samples):
        timestamp, x, y = sample["t"], sample["x"], sample["y"]
        if not all(math.isfinite(value) for value in (timestamp, x, y)):
            raise ValueError(f"samples[{index}] 含非有限数")
        dt = 0 if previous_time is None else round(timestamp - previous_time)
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


def load_scaled_touch_track(
    *,
    target_distance: float,
    fixture_path: str | Path | None = None,
) -> list[dict[str, Any]]:
    """读取轨迹资产，缩放到本轮手柄距离，返回带事件类型的相对采样。

    返回的每项形如 ``{"x": int, "y": int, "dt": int, "type": str}``，可直接交给
    :class:`~ali_slider_reverse.runtime.node_pe.PeRuntimeClient` 回放。
    """

    path = config.DEFAULT_TOUCH_TRACK if fixture_path is None else Path(fixture_path)
    samples, source_distance = _read_fixture(path)
    scaled = _rescale(
        samples,
        target_distance=target_distance,
        source_distance=source_distance,
    )
    for event, source in zip(scaled, samples, strict=True):
        event["type"] = str(source["type"])
    return scaled


__all__ = ["load_scaled_touch_track"]
