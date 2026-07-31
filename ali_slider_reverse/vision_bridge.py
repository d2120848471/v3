"""OpenCV 缺口求解的独立 Python 进程入口。"""

from __future__ import annotations

import argparse
import json
import sys

from .image_solver import estimate_puzzle_canvas_left_cv


def _solve_payload(background: str, shadow: str) -> dict[str, object]:
    estimate = estimate_puzzle_canvas_left_cv(background, shadow)
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


def _worker_main() -> int:
    # 在收到图片路径前完成 OpenCV/NumPy 的冷导入。调用方可把这 200ms 左右的
    # 固定成本与 DeviceToken、Init 和资源下载重叠，真正识别阶段只保留算法耗时。
    __import__("cv2")
    __import__("numpy")
    print('{"ready":true}', flush=True)
    for line in sys.stdin:
        request = json.loads(line)
        if not isinstance(request, dict):
            raise ValueError("worker 请求必须是 JSON 对象")
        background = request.get("background")
        shadow = request.get("shadow")
        if not isinstance(background, str) or not isinstance(shadow, str):
            raise ValueError("worker 请求缺少 background/shadow 路径")
        print(
            json.dumps(
                _solve_payload(background, shadow),
                ensure_ascii=False,
                separators=(",", ":"),
            ),
            flush=True,
        )
    return 0


def main() -> int:
    parser = argparse.ArgumentParser(
        description="从 back.png / shadow.png 计算阿里滑块 xPos"
    )
    parser.add_argument("--background")
    parser.add_argument("--shadow")
    parser.add_argument(
        "--worker",
        action="store_true",
        help="预载 OpenCV 后通过 stdin/stdout 处理 JSON 行请求",
    )
    args = parser.parse_args()

    if args.worker:
        return _worker_main()
    if args.background is None or args.shadow is None:
        parser.error("非 worker 模式必须提供 --background 和 --shadow")
    payload = _solve_payload(args.background, args.shadow)
    print(json.dumps(payload, ensure_ascii=False, separators=(",", ":")))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
