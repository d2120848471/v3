"""滑块的坐标换算：图片 ``xPos`` ↔ 手柄位移 ``slidePos``。

当前协议的手柄与拼图之间**不是线性关系**，而是一条二次曲线：

```text
xPos = s * (3*s + 65) / 845          正向：由手柄位移得到拼图横坐标
s    = (-65 + sqrt(65² + 12*845*xPos)) / 6    反向：由拼图横坐标反解手柄位移
```

其中 ``s`` 就是 ``slidePos``。协议要求 ``xPos`` 与 ``slidePos`` 两个值同时正确
且互相自洽，所以两个方向都要能算——识别给出 ``xPos``，轨迹需要 ``slidePos``。

取整必须复刻 JavaScript ``Math.round``（``floor(x + 0.5)``），不能用 Python 的
``round``：后者是银行家舍入，``round(0.5) == 0``，会在半整数处偏差 1px。
"""

from __future__ import annotations

import math


def pe_puzzle_x_from_slide_pos(slide_pos: float) -> float:
    """正向运动式：由手柄位移 ``s`` 得到拼图的 CSS 横坐标。"""

    if not math.isfinite(slide_pos) or slide_pos < 0:
        raise ValueError("slide_pos 不能为负数")
    return float(slide_pos) * (3.0 * float(slide_pos) + 65.0) / 845.0


def js_math_round(value: float) -> int:
    """复刻非负数上的 JavaScript ``Math.round``，即 ``floor(value + 0.5)``。"""

    if not math.isfinite(value) or value < 0:
        raise ValueError("value 不能为负数")
    return math.floor(value + 0.5)


def pe_slide_pos_from_puzzle_x(
    puzzle_x: float,
    *,
    rendered_width: float,
    handle_width: float,
    rounded: bool = True,
) -> float | int:
    """反解手柄位移，并限制在 DOM 可移动范围内。

    先取二次式的非负根，再 clamp 到 ``[0, rendered_width - handle_width]``，
    最后按 JavaScript 语义取整。clamp 在取整**之前**，与前端顺序一致。
    """

    if not math.isfinite(puzzle_x) or puzzle_x < 0:
        raise ValueError("puzzle_x 不能为负数")
    if not math.isfinite(rendered_width) or rendered_width <= 0:
        raise ValueError("rendered_width 必须为正数")
    if (
        not math.isfinite(handle_width)
        or handle_width < 0
        or handle_width > rendered_width
    ):
        raise ValueError("handle_width 必须位于 0..rendered_width")

    raw = (-65.0 + math.sqrt(65.0**2 + 12.0 * 845.0 * puzzle_x)) / 6.0
    clamped = min(max(raw, 0.0), rendered_width - handle_width)
    return js_math_round(clamped) if rounded else clamped


__all__ = [
    "js_math_round",
    "pe_puzzle_x_from_slide_pos",
    "pe_slide_pos_from_puzzle_x",
]
