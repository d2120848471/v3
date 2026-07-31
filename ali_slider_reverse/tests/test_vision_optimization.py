"""视觉求解路径的回归测试。

需要 opencv-python 与 numpy；主协议解释器可以不装，此时整个用例类被跳过。

```bash
PYTHONPATH="$PWD" /path/to/vision-python -m unittest \
    ali_slider_reverse.tests.test_vision_optimization -v
```
"""

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

from ali_slider_reverse.challenge.assets import ChallengeAssets
from ali_slider_reverse.challenge.session import AliSliderClient
from ali_slider_reverse.vision.gap_solver import (
    GapCandidate,
    _snap_chamfer_plateau_to_edge_consensus,
    solve_gap,
)


# 前端密文恢复需要 cryptography，视觉解释器不一定装；用等价形状的替身即可，
# 本用例只关心视觉链路，不触碰任何真实签名。
_FAKE_SECRETS = SimpleNamespace(
    main_rpc_key_id="test-rpc-key-id",
    main_rpc_key_secret="test-rpc-key-secret",
    device_token_salt="test-device-token-salt",
)


@unittest.skipIf(cv2 is None or np is None, "需要 OpenCV/NumPy")
class VisionOptimizationTests(unittest.TestCase):
    def _synthetic_decoy_assets(self) -> tuple[Path, Path]:
        """合成一张"诱饵比真解更强"的图片。

        真实缺口在 x=221，另有一个尺寸相近、contour 支持度更高的高亮矩形在
        x≈87——它能骗过单纯的候选打分，只有完整轮廓的 Chamfer 校验能识破。
        """

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
            base + random.normal(0, 18, (height, width, 3)), 0, 255
        ).astype(np.uint8)

        alpha = np.zeros((height, shadow_width), dtype=np.uint8)
        cv2.rectangle(alpha, (8, 61), (47, 104), 255, -1)
        cv2.circle(alpha, (28, 61), 10, 255, -1)
        cv2.circle(alpha, (47, 82), 10, 255, -1)
        cv2.circle(alpha, (8, 88), 9, 0, -1)

        expected_canvas_left = 221
        true_region = background[
            :, expected_canvas_left : expected_canvas_left + shadow_width
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
        """候选打分被诱饵带偏时，全轴 Chamfer 扫描应当把坐标拉回真解。"""

        background, shadow = self._synthetic_decoy_assets()

        estimate = solve_gap(background, shadow)

        self.assertLessEqual(abs(estimate.x_pos - 221), 1)
        self.assertTrue(
            any(
                candidate.method.startswith("global-chamfer")
                and abs(candidate.canvas_left - 221) <= 1
                for candidate in estimate.candidates
            )
        )

    def test_global_region_snaps_to_multi_threshold_edge_consensus(self) -> None:
        """Chamfer 平台上，只有距离损失足够小的 edge 共识才允许吸附。"""

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

        # 损失 0.0647 ≤ 0.10：吸附到 edge 共识。
        self.assertEqual(
            _snap_chamfer_plateau_to_edge_consensus(
                candidates, {201: 1.9047, 204: 1.9694}, 201, 1.9047
            ),
            (204, 1.9694),
        )
        # 损失 0.1501 > 0.10：保留全局最低点。
        self.assertEqual(
            _snap_chamfer_plateau_to_edge_consensus(
                candidates, {201: 1.9047, 204: 2.0548}, 201, 1.9047
            ),
            (201, 1.9047),
        )

    def test_prewarmed_worker_is_consumed_by_client(self) -> None:
        """预热的视觉 worker 应当被 solve_assets 正常消费并给出同样结果。"""

        background, shadow = self._synthetic_decoy_assets()
        assets = ChallengeAssets(
            background=background,
            shadow=shadow,
            pe_script=background,
            stylesheet=shadow,
            load_timings={},
            stylesheet_loaded=True,
        )
        requests_stub = SimpleNamespace(Session=lambda: SimpleNamespace(proxies={}))

        with mock.patch(
            "ali_slider_reverse.challenge.session._requests_module",
            return_value=requests_stub,
        ), mock.patch(
            "ali_slider_reverse.challenge.session.resolve_frontend_secrets",
            return_value=_FAKE_SECRETS,
        ):
            client = AliSliderClient(vision_python=sys.executable)
            try:
                client.prewarm_vision()
                result = client.solve_assets(assets)
            finally:
                client.close()

        self.assertLessEqual(abs(result.x_pos - 221), 1)
        # xPos → slidePos 的反解也应当在同一次调用里完成。
        self.assertGreater(result.slide_pos, 0)


if __name__ == "__main__":
    unittest.main()
