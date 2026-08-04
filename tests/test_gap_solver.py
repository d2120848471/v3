"""缺口求解器的可证伪回归测试。

测试图完全由 NumPy/OpenCV 合成，不保存或提交真实 challenge 图片。困难场景模拟
两类同时出现的证据：中部大片低饱和亮色区域会诱骗外观相关，右侧真实缺口只有
完整轮廓仍然可靠。没有 OpenCV 的主协议解释器允许跳过；CI/视觉解释器会实际运行。
"""

from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

try:
    import cv2
    import numpy as np
except ImportError:  # pragma: no cover - 主协议解释器允许不安装视觉依赖。
    cv2 = None
    np = None

from ali_slider_reverse.vision.gap_solver import solve_gap


@unittest.skipIf(cv2 is None or np is None, "需要 OpenCV/NumPy")
class GapSolverTests(unittest.TestCase):
    def _edge_decoy_assets(self, *, include_gap: bool) -> tuple[Path, Path]:
        temporary = tempfile.TemporaryDirectory(prefix="ali-gap-test-")
        self.addCleanup(temporary.cleanup)
        root = Path(temporary.name)

        height, width, shadow_width = 200, 296, 52
        random = np.random.default_rng(16)

        # 中部是一大片近乎恒定的低饱和亮色区域；它在颜色/平坦度上很像缺口，
        # 但没有拼图的完整边界。右侧深色纹理让真实缺口的颜色模型变弱。
        background = np.empty((height, width, 3), dtype=np.float32)
        background[:] = (165, 185, 200)
        background[95:195, 70:220] = (210, 220, 230)
        texture = random.normal(0, 45, (height, 56, 3))
        background[:, 240:296] = np.clip(np.asarray((20, 45, 70)) + texture, 0, 180)
        background = np.clip(background, 0, 255).astype(np.uint8)

        alpha = np.zeros((height, shadow_width), dtype=np.uint8)
        cv2.rectangle(alpha, (4, 126), (47, 179), 255, -1)
        cv2.circle(alpha, (26, 126), 11, 255, -1)
        cv2.circle(alpha, (47, 151), 10, 255, -1)
        cv2.circle(alpha, (4, 156), 9, 0, -1)
        alpha = cv2.GaussianBlur(alpha, (3, 3), 0)

        if include_gap:
            solid = (alpha > 127).astype(np.uint8)
            fill = cv2.erode(
                solid, cv2.getStructuringElement(cv2.MORPH_ELLIPSE, (5, 5))
            ).astype(bool)
            region = background[:, 241:293].astype(np.float32)
            # 较低不透明度故意削弱固定颜色模型；完整轮廓仍保持清楚。
            region[fill] = 0.5 * region[fill] + 124
            background[:, 241:293] = np.clip(region, 0, 255).astype(np.uint8)

        shadow = np.zeros((height, shadow_width, 4), dtype=np.uint8)
        shadow[:, :, 3] = alpha
        background_path = root / "back.png"
        shadow_path = root / "shadow.png"
        self.assertTrue(cv2.imwrite(str(background_path), background))
        self.assertTrue(cv2.imwrite(str(shadow_path), shadow))
        return background_path, shadow_path

    def test_low_confidence_decoy_uses_full_contour_fallback(self) -> None:
        background, shadow = self._edge_decoy_assets(include_gap=True)

        estimate = solve_gap(background, shadow)

        self.assertLessEqual(abs(estimate.x_pos - 241), 1)
        self.assertGreaterEqual(estimate.confidence, 0.45)
        self.assertTrue(
            any(
                candidate.method == "global-chamfer"
                for candidate in estimate.candidates
            )
        )

    def test_full_contour_fallback_rejects_scene_without_gap(self) -> None:
        background, shadow = self._edge_decoy_assets(include_gap=False)

        estimate = solve_gap(background, shadow)

        self.assertLess(estimate.confidence, 0.45)
        self.assertFalse(
            any(
                candidate.method == "global-chamfer"
                for candidate in estimate.candidates
            )
        )


if __name__ == "__main__":
    unittest.main()
