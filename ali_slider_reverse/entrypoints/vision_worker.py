"""OpenCV 缺口求解的独立进程入口。

两种运行方式：

```text
--worker                 常驻模式：先预载 OpenCV 并空跑一次求解，再按行读 JSON
--background/--shadow    一次性模式：算完即退
```

worker 模式的意义在于把两笔固定成本都提前到收到图片之前：OpenCV/NumPy 约 200ms
的冷导入，以及**首次** ``solve_gap`` 约 77ms 的算子初始化。只预载导入是不够的——
实测冷导入之后的第一次求解仍要约 94ms，而热调用只要约 17ms。
"""

from __future__ import annotations

import argparse
import json
import sys
import tempfile
from pathlib import Path

from ..vision.gap_solver import solve_gap


def _warm_algorithm() -> None:
    """用一张合成图空跑一次完整求解，把算子首次初始化的开销提前付掉。

    合成图只需要能走完"候选 → 复核"全流程，不需要像真实挑战图：平滑噪声背景配
    一块矩形 alpha 就够。

    任何失败都静默忽略——预热失败最多退回原来的耗时，绝不能让 worker 起不来。
    """

    try:
        import cv2
        import numpy

        with tempfile.TemporaryDirectory(prefix="ali-vision-warm-") as directory:
            root = Path(directory)
            background_path = root / "back.png"
            shadow_path = root / "shadow.png"

            rng = numpy.random.default_rng(0)
            background = (rng.random((200, 296, 3)) * 80 + 100).astype(numpy.uint8)
            cv2.imwrite(str(background_path), cv2.GaussianBlur(background, (15, 15), 0))

            shadow = numpy.zeros((200, 52, 4), dtype=numpy.uint8)
            shadow[40:150, 5:47] = 255
            cv2.imwrite(str(shadow_path), shadow)

            solve_gap(background_path, shadow_path)
    except Exception:
        return


def _solve_payload(background: str, shadow: str) -> dict[str, object]:
    """求解一次并转成可 JSON 序列化的结果。"""

    estimate = solve_gap(background, shadow)
    return {
        "xPos": estimate.x_pos,
        "confidence": estimate.confidence,
        "alphaBBox": {
            "left": estimate.alpha_bbox.left,
            "top": estimate.alpha_bbox.top,
            "right": estimate.alpha_bbox.right,
            "bottom": estimate.alpha_bbox.bottom,
        },
        "candidates": [
            {
                "xPos": candidate.canvas_left,
                "score": candidate.score,
                "method": candidate.method,
            }
            for candidate in estimate.candidates
        ],
    }


def _emit(payload: dict[str, object]) -> None:
    print(json.dumps(payload, ensure_ascii=False, separators=(",", ":")), flush=True)


def _run_worker() -> int:
    """常驻模式：预载依赖并热身后按行处理请求。"""

    # 在收到图片路径前完成冷导入与算子初始化，再用 ready 信号告知调用方。
    __import__("cv2")
    __import__("numpy")
    _warm_algorithm()
    print('{"ready":true}', flush=True)

    for line in sys.stdin:
        request = json.loads(line)
        if not isinstance(request, dict):
            raise ValueError("worker 请求必须是 JSON 对象")
        background = request.get("background")
        shadow = request.get("shadow")
        if not isinstance(background, str) or not isinstance(shadow, str):
            raise ValueError("worker 请求缺少 background/shadow 路径")
        _emit(_solve_payload(background, shadow))
    return 0


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="python -m ali_slider_reverse.entrypoints.vision_worker",
        description="从 back.png / shadow.png 计算阿里滑块 xPos",
    )
    parser.add_argument("--background", help="背景图路径")
    parser.add_argument("--shadow", help="拼图图路径")
    parser.add_argument(
        "--worker",
        action="store_true",
        help="预载 OpenCV 后通过 stdin/stdout 处理 JSON 行请求",
    )
    args = parser.parse_args(argv)

    if args.worker:
        return _run_worker()
    if args.background is None or args.shadow is None:
        parser.error("非 worker 模式必须提供 --background 和 --shadow")
    _emit(_solve_payload(args.background, args.shadow))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
