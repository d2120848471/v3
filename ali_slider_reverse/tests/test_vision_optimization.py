from __future__ import annotations

import sys
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest import mock

try:
    import cv2
    import numpy as np
except ImportError:  # pragma: no cover - 主协议解释器允许不安装 OpenCV。
    cv2 = None
    np = None

from ali_slider_reverse.client import AliSliderClient, ChallengeAssets
from ali_slider_reverse.image_solver import (
    GapCandidate,
    _snap_chamfer_plateau_to_edge_consensus,
    estimate_puzzle_canvas_left_cv,
)


@unittest.skipIf(cv2 is None or np is None, "需要 OpenCV/NumPy")
class VisionOptimizationTests(unittest.TestCase):
    def _synthetic_decoy_assets(self) -> tuple[Path, Path]:
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        target = Path(temporary.name)

        height, width, shadow_width = 168, 296, 56
        random = np.random.default_rng(7)
        y_grid, x_grid = np.indices((height, width))
        base = np.stack(
            [
                90 + 30 * np.sin(x_grid / 17),
                130 + 25 * np.cos(y_grid / 19),
                75 + 20 * np.sin((x_grid + y_grid) / 23),
            ],
            axis=2,
        )
        background = np.clip(
            base + random.normal(0, 18, (height, width, 3)),
            0,
            255,
        ).astype(np.uint8)

        alpha = np.zeros((height, shadow_width), dtype=np.uint8)
        cv2.rectangle(alpha, (8, 61), (47, 104), 255, -1)
        cv2.circle(alpha, (28, 61), 10, 255, -1)
        cv2.circle(alpha, (47, 82), 10, 255, -1)
        cv2.circle(alpha, (8, 88), 9, 0, -1)

        expected_canvas_left = 221
        true_region = background[
            :,
            expected_canvas_left : expected_canvas_left + shadow_width,
        ]
        true_region[alpha > 0] = (238, 238, 238)

        # 尺寸近似的高亮矩形会获得大量 contour 支持，但轮廓不是拼图形状。
        cv2.rectangle(background, (87, 58), (136, 108), (245, 245, 245), -1)
        shadow = np.zeros((height, shadow_width, 4), dtype=np.uint8)
        shadow[:, :, 3] = alpha

        background_path = target / "back.png"
        shadow_path = target / "shadow.png"
        self.assertTrue(cv2.imwrite(str(background_path), background))
        self.assertTrue(cv2.imwrite(str(shadow_path), shadow))
        return background_path, shadow_path

    def test_global_chamfer_recovers_shape_from_stronger_decoy(self) -> None:
        background, shadow = self._synthetic_decoy_assets()

        estimate = estimate_puzzle_canvas_left_cv(background, shadow)

        self.assertLessEqual(abs(estimate.x_pos - 221), 1)
        self.assertTrue(
            any(
                candidate.method.startswith("global-chamfer")
                and abs(candidate.canvas_left - 221) <= 1
                for candidate in estimate.candidates
            )
        )

    def test_global_region_snaps_to_multi_threshold_edge_consensus(self) -> None:
        candidates = [
            GapCandidate(204, 0.55 + index * 0.01, method)
            for index, method in enumerate(
                (
                    "edge-template-10-35",
                    "edge-template-20-60",
                    "edge-template-35-105",
                    "edge-template-60-180",
                )
            )
        ]
        candidates.append(GapCandidate(206, 0.88, "bright-mask-205-70"))

        self.assertEqual(
            _snap_chamfer_plateau_to_edge_consensus(
                candidates,
                {201: 1.9047, 204: 1.9694},
                201,
                1.9047,
            ),
            (204, 1.9694),
        )
        self.assertEqual(
            _snap_chamfer_plateau_to_edge_consensus(
                candidates,
                {201: 1.9047, 204: 2.0548},
                201,
                1.9047,
            ),
            (201, 1.9047),
        )

    def test_prewarmed_bridge_is_consumed_by_client(self) -> None:
        background, shadow = self._synthetic_decoy_assets()
        assets = ChallengeAssets(
            background=background,
            shadow=shadow,
            pe_script=background,
            stylesheet=shadow,
            load_timings={},
            stylesheet_loaded=True,
        )
        requests_stub = SimpleNamespace(Session=lambda: object())

        with mock.patch(
            "ali_slider_reverse.client._requests_module",
            return_value=requests_stub,
        ), mock.patch(
            "ali_slider_reverse.client.resolve_frontend_secrets",
            return_value=object(),
        ):
            client = AliSliderClient(vision_python=sys.executable)
            try:
                client._prewarm_vision()
                result = client.solve_assets(assets)
            finally:
                client._close_vision()

        self.assertLessEqual(abs(result.x_pos - 221), 1)


if __name__ == "__main__":
    unittest.main()
