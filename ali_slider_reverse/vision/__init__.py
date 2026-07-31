"""图像层：缺口定位与坐标换算。

```text
geometry.py    xPos ↔ slidePos 的二次运动式与 JS 取整语义（纯标准库）
gap_solver.py  基于 OpenCV 的多路缺口检测与 Chamfer 仲裁
```

``geometry`` 只依赖标准库，主协议进程直接使用；``gap_solver`` 需要
opencv-python 与 numpy，但对它们是**惰性导入**——只有真正调用求解函数时才会
触碰这两个包，因此在没装 OpenCV 的解释器里导入本包不会报错。
"""

from __future__ import annotations

from .gap_solver import BoundingBox, GapCandidate, GapEstimate, solve_gap
from .geometry import (
    js_math_round,
    pe_puzzle_x_from_slide_pos,
    pe_slide_pos_from_puzzle_x,
)

__all__ = [
    "BoundingBox",
    "GapCandidate",
    "GapEstimate",
    "js_math_round",
    "pe_puzzle_x_from_slide_pos",
    "pe_slide_pos_from_puzzle_x",
    "solve_gap",
]
