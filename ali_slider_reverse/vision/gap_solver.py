"""基于 OpenCV 的缺口定位：从 ``back.png`` 与 ``shadow.png`` 求 ``xPos``。

## 坐标语义

阿里返回的 ``shadow.png`` 是一张**窄画布**：与背景等高，内部只有一小块非透明的
拼图形状。协议需要的 ``xPos`` 是**整个画布**在 ``back.png`` 原始像素坐标系中的左
坐标，因此本模块内部一律以"画布左坐标"为单位滑动，不需要再做 alpha 边界换算。

## 缺口的渲染模型

``back.png`` 的缺口不是抠掉一块，而是在原图上叠了一层拼图形状的半透明白。在 50
张真实样本上线性拟合得到（BGR 三通道系数一致）：

```text
fill = 0.224 * background + 187
```

由此推出三条可直接当判据用的性质：

```text
1  填充色被压进窄区间     背景再黑再白，混合后都逃不出去。上下界性质不同：
                          上界 ``255 - 14α`` 几乎不随不透明度变化，是否掉雪地/云层的
                          硬约束；下界 ``241α`` 随 α 剧烈漂移，因此刻意放宽。
                          详见 ``_FILL_LO`` / ``_FILL_HI``。
2  色度压到原来的 22%     缺口在任何画面上都低饱和。
3  局部对比度也压到 22%   通常平坦，但雪山岩石这类高反差背景下缺口内部仍有起伏，
                          所以平坦度只能当弱证据，不能当门槛。
```

几何上填充区相对 alpha 形状**四周内缩约 2px**，边界是 1px 硬阶跃。按"亮块边缘对齐
alpha 边缘"会系统性偏 2px，因此模板建在带符号距离场上。

## 求解流程

```text
逐像素缺口似然（颜色区间 × 低色度）
   ↓
三路互补的模板相关：似然 CCORR、亮度零均值 NCC、色度零均值 NCC
   ↓                （失效场景互不重叠：雪地让似然处处成立，灰阶画面让色度失效，
   ↓                  大片天空会把零均值相关吸引过去）
各路取峰 → 并集 ± 3px 逐一复核
   ↓
复核：边界阶跃一致性 / 混合模型局部符合率 / 区域可分性 / 颜色区间占比
   ↓
xPos + 置信度
```

置信度不足时调用方应当停止，不要靠扫描邻近坐标去消耗 Verify 次数。

OpenCV/NumPy 是可选依赖，仅在此模块内惰性导入：主协议解释器不需要装，图像识别
交给 :mod:`ali_slider_reverse.entrypoints.vision_worker` 在独立解释器中执行。
"""

from __future__ import annotations

import math
import tempfile
from dataclasses import dataclass
from os import PathLike
from pathlib import Path
from typing import Any

from ..errors import VisionError


# --------------------------------------------------------------------------
# 检测参数
#
# 除 PEAK_SEP / PROBE_RADIUS 外，全部数值都来自真实样本的实测标定，改动会直接
# 影响识别率。改之前请先按 README 第 8 节的方式重建离线基准，不要凭单张图调参。
# --------------------------------------------------------------------------

_ALPHA_THRESHOLD = 8
"""alpha 大于此值即视为拼图形状的一部分，略高于 0 以排除 PNG 边缘的透明噪点。
只用于统计形状范围（``alpha_bbox``）。"""

_SHAPE_THRESHOLD = 127
"""参与几何计算的形状阈值。距离场必须建在 **50% 覆盖率**的边界上——那才是渲染
意义上的形状边缘；用 ``_ALPHA_THRESHOLD`` 会把抗锯齿带整个算进形状，让距离场
整体外扩约 1px。"""

_BLEND_K = 0.224
_BLEND_B = 187.0
"""缺口填充相对背景的混合系数：``fill = K * background + B``。"""

_FILL_LO = 150.0
_FILL_HI = 244.0
_FILL_SLACK = 6.0
"""填充色的可行区间，上下界的性质**完全不同**，不要对称地调。

把混合式展开成不透明度 α 的函数（覆盖色实测约 ``#F1F1F1``）：

```text
fill_max = (1-α)*255 + α*241 = 255 - 14α    α∈[0.6,1.0] 时只在 241~247 之间动
fill_min = α*241                            α∈[0.6,1.0] 时从 145 一直跑到 241
```

上界几乎不随 α 变化，而且正是否掉雪地/云层的那条判据（它们整片又亮又平又低饱和，
只有"亮过 244"能把它们判死），所以必须收紧。下界却随 α 剧烈漂移，写死在当前实测的
187 会让前端一改不透明度就大面积漏检——实测 α 掉到 0.60 时正确率从 99% 塌到 87%。
因此下界放宽到 150（可容忍 α ≥ 0.62），代价只是负样本误放行从 4/50 增到 6/50。

``_FILL_SLACK`` 容纳抗锯齿与压缩噪声。
"""

_CHROMA_SIGMA = 16.0
"""填充色度的高斯宽度：混合把色度压到 22%，缺口在任何画面上都低饱和。"""

_FILL_STD_SIGMA = 18.0
"""填充区亮度标准差的高斯宽度，用作弱证据。"""

_INSET = 2.0
_EDGE_SOFT = 0.7
_OUTER = 4.0
"""边界几何：填充自带符号距离 ``+2`` 开始，过渡带 1px，外侧参考带宽 4px。"""

_SAMPLE_IN = 3
_SAMPLE_OUT = 4
_STEP_MIN = 6.0
_BLEND_TOL = 26.0
"""逐行边界采样的内外偏移、判定阶跃的最小亮度差、混合模型的容差。"""

_PEAK_SEP = 10
_PEAKS_PER_CURVE = 4
_PROBE_RADIUS = 3
"""峰的最小间隔、每条曲线保留的峰数、每个峰向两侧展开复核的半径。"""

_MIN_BOUNDARY_SAMPLES = 8
"""边界采样点少于此数时判定该候选不可评估。"""

_CHAMFER_TRIGGER_CONFIDENCE = 0.45
_CHAMFER_MAX_DISTANCE = 3.0
_CHAMFER_MIN_MARGIN = 0.70
_CHAMFER_BORDER_MARGIN = 8
"""低置信度时才启用的完整轮廓兜底。

正常样本不承担全横轴距离变换的成本。只有当前证据低于默认放行线，且完整 alpha
轮廓的最优距离、远峰差距和现有候选覆盖同时满足时才接管；无缺口或单条背景边缘
不会仅凭一个局部相关峰被抬高。
"""

_CONF_QUALITY_GAIN = 4.5
_CONF_QUALITY_PIVOT = 0.34
_CONF_MARGIN_GAIN = 14.0
_CONF_MARGIN_PIVOT = 0.08
_CONF_STEP_GAIN = 2.5
_CONF_STEP_PIVOT = 0.55
"""置信度 logistic 的增益与中心点，见 :func:`_confidence` 的标定说明。"""


@dataclass(frozen=True, slots=True)
class BoundingBox:
    """半开区间包围盒（Pillow 风格）：``right``/``bottom`` 不包含在内。"""

    left: int
    top: int
    right: int
    bottom: int

    @property
    def width(self) -> int:
        return self.right - self.left

    @property
    def height(self) -> int:
        return self.bottom - self.top


@dataclass(frozen=True, slots=True)
class GapCandidate:
    """单个候选位置及其复核得分。"""

    canvas_left: int
    """候选的 xPos，即 shadow 画布在背景图中的左坐标。"""

    score: float
    """归一化到 0..1 的综合得分。"""

    method: str
    """产出该候选的来源，形如 ``like`` / ``lncc`` / ``cncc`` / ``probe``。"""

    bbox: BoundingBox | None = None
    """候选对应的画布包围盒，供调试定位。"""


@dataclass(frozen=True, slots=True)
class GapEstimate:
    """最终缺口估计。"""

    x_pos: int
    confidence: float
    alpha_bbox: BoundingBox
    candidates: tuple[GapCandidate, ...]


def _opencv() -> tuple[Any, Any]:
    """惰性导入 OpenCV/NumPy。"""

    try:
        import cv2
        import numpy as np
    except ImportError as exc:  # pragma: no cover - 由独立 vision 运行时覆盖。
        raise VisionError("OpenCV 求解需要 opencv-python 与 numpy") from exc
    return cv2, np


# --------------------------------------------------------------------------
# 图像载入与共享派生量
# --------------------------------------------------------------------------


class _Frame:
    """一次求解过程中所有阶段共享的图像、掩码与预计算索引。"""

    def __init__(self, background: Any, alpha: Any) -> None:
        cv2, np = _opencv()

        self.bgr = background.astype(np.float32)
        self.alpha = alpha
        self.width = background.shape[1]
        self.shadow_width = alpha.shape[1]
        self.max_canvas_left = self.width - self.shadow_width

        selected_y, selected_x = np.where(alpha > _ALPHA_THRESHOLD)
        self.alpha_box = BoundingBox(
            int(selected_x.min()),
            int(selected_y.min()),
            int(selected_x.max()) + 1,
            int(selected_y.max()) + 1,
        )

        lab = cv2.cvtColor(background, cv2.COLOR_BGR2LAB).astype(np.float32)
        self.lightness = np.ascontiguousarray(lab[:, :, 0])
        chroma = np.ascontiguousarray(
            np.hypot(lab[:, :, 1] - 128.0, lab[:, :, 2] - 128.0).astype(np.float32)
        )

        # 逐像素「可能是缺口填充」的似然。刻意不含平坦度：高反差背景下缺口内部
        # 并不平坦，把平坦度写进似然会直接漏掉这类样本。
        in_range = self._range_score(self.bgr).astype(np.float32)
        desat = np.exp(-(chroma**2) / (2 * _CHROMA_SIGMA**2)).astype(np.float32)
        self.likelihood = in_range * desat

        solid = (alpha > _SHAPE_THRESHOLD).astype(np.uint8)
        distance = cv2.distanceTransform(solid, cv2.DIST_L2, 5) - cv2.distanceTransform(
            1 - solid, cv2.DIST_L2, 5
        )
        self.fill = distance >= _INSET + 1.0
        self.ring = (distance < -1.0) & (distance > -7.0)
        if not self.fill.any() or not self.ring.any():
            raise VisionError("shadow 拼图形状过小，无法建立缺口判据")

        self._prepare_boundary_samples(distance >= _INSET)
        self._curves = self._build_curves(
            self._build_template(distance), self.lightness, chroma
        )

    # -- 静态判据 --------------------------------------------------------

    @staticmethod
    def _range_score(values: Any) -> Any:
        """软区间：落在 ``[_FILL_LO, _FILL_HI]`` 内得 1，越界按高斯衰减。"""

        _, np = _opencv()
        below = np.maximum(_FILL_LO - values, 0.0)
        above = np.maximum(values - _FILL_HI, 0.0)
        return np.exp(-(below**2 + above**2).mean(axis=-1) / (2 * 12.0**2))

    @staticmethod
    def _build_template(distance: Any) -> Any:
        """带符号距离软模板：填充区取正权，紧贴形状外的一圈取负权。

        过渡带必须做窄（``_EDGE_SOFT``）——缺口边界本身只有 1px，宽过渡带会把
        相关峰摊平，正是"系统性偏 2~3px"的来源。
        """

        _, np = _opencv()

        def sigmoid(x: Any) -> Any:
            return 0.5 * (1.0 + np.tanh(0.5 * x))

        inside = sigmoid((distance - _INSET) / _EDGE_SOFT)
        outside = sigmoid(-(distance + _OUTER * 0.5) / _EDGE_SOFT)
        template = inside / max(inside.sum(), 1.0) - outside / max(outside.sum(), 1.0)
        return template.astype(np.float32)

    def _prepare_boundary_samples(self, fill_edge: Any) -> None:
        """预存逐行左右边界的内外采样偏移，复核时只做一次数组索引。"""

        _, np = _opencv()
        rows: list[int] = []
        inner: list[int] = []
        outer: list[int] = []
        for y in range(fill_edge.shape[0]):
            xs = np.where(fill_edge[y])[0]
            if xs.size < 8:
                continue
            for boundary, sign in ((int(xs.min()), 1), (int(xs.max()), -1)):
                rows.append(y)
                inner.append(boundary + sign * _SAMPLE_IN)
                outer.append(boundary - sign * _SAMPLE_OUT)
        self._rows = np.asarray(rows, np.intp)
        self._x_in = np.asarray(inner, np.intp)
        self._x_out = np.asarray(outer, np.intp)

    def _build_curves(
        self, template: Any, lightness: Any, chroma: Any
    ) -> dict[str, Any]:
        """三路互补的模板相关曲线。

        三者的失效场景不重叠：似然相关在雪地/云层上处处成立，色度相关在灰阶画面
        上失效，亮度零均值相关会被大片天空吸引。取并集后交给同一个复核器裁决，
        比调一路"万能特征"稳得多。
        """

        cv2, _ = _opencv()
        return {
            "like": cv2.matchTemplate(
                self.likelihood, template, cv2.TM_CCORR
            ).reshape(-1),
            "lncc": cv2.matchTemplate(
                lightness, template, cv2.TM_CCOEFF_NORMED
            ).reshape(-1),
            "cncc": cv2.matchTemplate(
                -chroma, template, cv2.TM_CCOEFF_NORMED
            ).reshape(-1),
        }

    # -- 候选生成 --------------------------------------------------------

    def candidates(self) -> tuple[list[int], dict[int, str]]:
        """各曲线取峰、并集后向两侧展开，返回候选坐标与其来源。"""

        _, np = _opencv()
        origin: dict[int, str] = {}
        for name, curve in self._curves.items():
            picked: list[int] = []
            for index in np.argsort(curve)[::-1]:
                index = int(index)
                if all(abs(index - prior) >= _PEAK_SEP for prior in picked):
                    picked.append(index)
                    origin.setdefault(index, name)
                if len(picked) >= _PEAKS_PER_CURVE:
                    break

        expanded = {
            int(min(max(c + dx, 0), self.max_canvas_left))
            for c in origin
            for dx in range(-_PROBE_RADIUS, _PROBE_RADIUS + 1)
        }
        for c in expanded:
            origin.setdefault(c, "probe")
        return sorted(expanded), origin

    # -- 复核 ------------------------------------------------------------

    def _boundary_scores(self, canvas_left: int) -> tuple[float, float]:
        """逐行边界采样，返回 (阶跃一致性, 混合模型符合率)。

        阶跃一致性要求整条轮廓上的内外亮度差方向一致——伪峰通常只有局部一段
        对得上。混合模型符合率则检验内侧像素是否确实等于外侧背景按 ``fill =
        0.224*bg + 187`` 混合后的结果。
        """

        _, np = _opencv()
        x_in = self._x_in + canvas_left
        x_out = self._x_out + canvas_left
        keep = (
            (x_in >= 0) & (x_in < self.width) & (x_out >= 0) & (x_out < self.width)
        )
        if int(keep.sum()) < _MIN_BOUNDARY_SAMPLES:
            return 0.0, 0.0
        rows, x_in, x_out = self._rows[keep], x_in[keep], x_out[keep]

        delta = self.lightness[rows, x_in] - self.lightness[rows, x_out]
        sign = np.sign(np.median(delta)) or 1.0
        step = float(np.mean((np.abs(delta) >= _STEP_MIN) & (np.sign(delta) == sign)))

        predicted = _BLEND_K * self.bgr[rows, x_out] + _BLEND_B
        error = np.sqrt(np.mean((self.bgr[rows, x_in] - predicted) ** 2, axis=1))
        return step, float(np.mean(error < _BLEND_TOL))

    def verify(self, canvas_left: int) -> tuple[float, float]:
        """在候选位置复核缺口的物理特征。

        返回 ``(质量分, 边界阶跃一致性)``——后者除了参与质量分，还单独喂给置信度
        标定，所以一并返回，避免调用方为了拿它再算一遍边界采样。
        """

        _, np = _opencv()
        window = self.bgr[:, canvas_left : canvas_left + self.shadow_width]
        fill_pixels = window[self.fill]

        # 逐像素的物理可能性。用比例而不是中位数：雪地上中位数可能只超界几个
        # 单位，比例却会明显掉下去，这正是区分真缺口与大片雪地的关键。
        in_range = float(
            (
                (fill_pixels >= _FILL_LO - _FILL_SLACK)
                & (fill_pixels <= _FILL_HI + _FILL_SLACK)
            )
            .all(axis=1)
            .mean()
        )
        lightness_window = self.lightness[
            :, canvas_left : canvas_left + self.shadow_width
        ]
        flat = float(
            np.exp(
                -(lightness_window[self.fill].std() ** 2) / (2 * _FILL_STD_SIGMA**2)
            )
        )
        like_window = self.likelihood[
            :, canvas_left : canvas_left + self.shadow_width
        ]
        separation = float(
            min(
                max(
                    like_window[self.fill].mean() - like_window[self.ring].mean(),
                    0.0,
                ),
                1.0,
            )
        )
        step, blend = self._boundary_scores(canvas_left)

        # 判别项（阶跃 / 混合 / 可分性 / 区间占比）决定得分高低，颜色与平坦度
        # 只作为温和门控：缺口压在雪地上时后者处处成立，相乘才不会被带偏。
        evidence = 0.28 * step + 0.24 * blend + 0.30 * separation + 0.18 * in_range
        gate = (in_range**1.2) * (0.55 + 0.45 * flat)
        return float(evidence * (0.35 + 0.65 * gate)), step


# --------------------------------------------------------------------------
# 低置信度完整轮廓兜底
# --------------------------------------------------------------------------


def _chamfer_distances(frame: _Frame, canvas_lefts: Any) -> dict[int, float]:
    """计算完整 alpha 轮廓与背景边缘的双向 Chamfer 距离。

    这条路径只在主求解置信度不足时运行。整图 Sobel 只算一次，每个横坐标仅在
    自己的局部 ROI 内做距离变换；两个方向取平均，避免只贴中一条背景边缘的伪峰。
    """

    cv2, np = _opencv()
    positions = tuple(dict.fromkeys(int(value) for value in canvas_lefts))
    if not positions:
        return {}

    alpha_mask = np.asarray(frame.alpha > _ALPHA_THRESHOLD, dtype=bool)
    eroded = cv2.erode(
        alpha_mask.astype(np.uint8),
        cv2.getStructuringElement(cv2.MORPH_CROSS, (3, 3)),
    ).astype(bool)
    alpha_boundary = alpha_mask & ~eroded
    if not bool(np.any(alpha_boundary)):
        return {position: math.inf for position in positions}

    gray = cv2.cvtColor(frame.bgr.astype(np.uint8), cv2.COLOR_BGR2GRAY).astype(
        np.float32
    )
    magnitude = cv2.magnitude(
        cv2.Sobel(gray, cv2.CV_32F, 1, 0, ksize=3),
        cv2.Sobel(gray, cv2.CV_32F, 0, 1, ksize=3),
    )
    alpha_y, alpha_x = np.where(alpha_boundary)
    margin = 5
    height = frame.bgr.shape[0]

    distances: dict[int, float] = {}
    for canvas_left in positions:
        if not 0 <= canvas_left <= frame.max_canvas_left:
            distances[canvas_left] = math.inf
            continue

        roi_left = max(0, canvas_left + frame.alpha_box.left - margin)
        roi_right = min(
            frame.width, canvas_left + frame.alpha_box.right + margin
        )
        roi_top = max(0, frame.alpha_box.top - margin)
        roi_bottom = min(height, frame.alpha_box.bottom + margin)
        if roi_left >= roi_right or roi_top >= roi_bottom:
            distances[canvas_left] = math.inf
            continue

        local_magnitude = magnitude[roi_top:roi_bottom, roi_left:roi_right]
        gradient_threshold = max(
            120.0, float(np.quantile(local_magnitude, 0.75))
        )
        background_boundary = local_magnitude >= gradient_threshold
        if not bool(np.any(background_boundary)):
            distances[canvas_left] = math.inf
            continue

        local_alpha_boundary = np.zeros(background_boundary.shape, dtype=bool)
        local_x = alpha_x + canvas_left - roi_left
        local_y = alpha_y - roi_top
        inside = (
            (local_x >= 0)
            & (local_x < local_alpha_boundary.shape[1])
            & (local_y >= 0)
            & (local_y < local_alpha_boundary.shape[0])
        )
        local_alpha_boundary[local_y[inside], local_x[inside]] = True
        if not bool(np.any(local_alpha_boundary)):
            distances[canvas_left] = math.inf
            continue

        distance_to_background = cv2.distanceTransform(
            (~background_boundary).astype(np.uint8), cv2.DIST_L2, 5
        )
        distance_to_alpha = cv2.distanceTransform(
            (~local_alpha_boundary).astype(np.uint8), cv2.DIST_L2, 5
        )
        distances[canvas_left] = (
            float(distance_to_background[local_alpha_boundary].mean())
            + float(distance_to_alpha[background_boundary].mean())
        ) / 2.0
    return distances


def _chamfer_fallback(
    frame: _Frame,
    positions: list[int],
    *,
    confidence: float,
    score_position: Any,
) -> tuple[int, float] | None:
    """在低置信度时用全轮廓锁定区域，再用主评分精修局部坐标。"""

    if confidence >= _CHAMFER_TRIGGER_CONFIDENCE:
        return None

    alignment = _chamfer_distances(
        frame, range(frame.max_canvas_left + 1)
    )
    finite = sorted(
        (distance, left)
        for left, distance in alignment.items()
        if left >= _CHAMFER_BORDER_MARGIN and math.isfinite(distance)
    )
    if not finite:
        return None

    global_distance, global_left = finite[0]
    runner_separation = max(12, round(frame.alpha_box.width * 0.55))
    runner_distance = next(
        (
            distance
            for distance, left in finite[1:]
            if abs(left - global_left) >= runner_separation
        ),
        math.inf,
    )
    if not (
        global_distance <= _CHAMFER_MAX_DISTANCE
        and runner_distance - global_distance >= _CHAMFER_MIN_MARGIN
        and any(
            abs(position - global_left) <= _PROBE_RADIUS
            for position in positions
        )
    ):
        return None

    # Chamfer 的局部平台会有 1~3px 离散偏移；它只负责选中正确区域，最终坐标仍
    # 由已经针对当前渲染模型标定过的主评分在邻域内精修。
    local_lefts = range(
        max(0, global_left - _PROBE_RADIUS),
        min(frame.max_canvas_left, global_left + _PROBE_RADIUS) + 1,
    )
    selected_left = max(
        local_lefts,
        key=lambda left: (score_position(left)[0], -abs(left - global_left), -left),
    )
    selected_distance = alignment[selected_left]
    selected_margin = runner_distance - selected_distance
    if not (
        selected_distance <= _CHAMFER_MAX_DISTANCE
        and selected_margin >= _CHAMFER_MIN_MARGIN
    ):
        return None

    absolute_score = math.exp(-max(0.0, selected_distance - 1.0) / 2.0)
    margin_score = (
        1.0
        if not math.isfinite(runner_distance)
        else 1.0 - math.exp(-max(0.0, selected_margin))
    )
    fallback_confidence = min(
        max(0.70 * absolute_score + 0.30 * margin_score, 0.0), 1.0
    )
    if fallback_confidence < _CHAMFER_TRIGGER_CONFIDENCE:
        return None
    return selected_left, fallback_confidence


# --------------------------------------------------------------------------
# 主入口
# --------------------------------------------------------------------------


def _load(
    background_path: str | PathLike[str],
    shadow_path: str | PathLike[str],
) -> _Frame:
    cv2, _ = _opencv()

    background = cv2.imread(str(background_path), cv2.IMREAD_COLOR)
    shadow = cv2.imread(str(shadow_path), cv2.IMREAD_UNCHANGED)
    if background is None:
        raise VisionError("无法读取 background PNG")
    if shadow is None or shadow.ndim != 3 or shadow.shape[2] < 4:
        raise VisionError("shadow PNG 必须包含 alpha 通道")

    height, width = background.shape[:2]
    shadow_height, shadow_width = shadow.shape[:2]
    if shadow_height != height:
        raise VisionError("shadow 与 background 高度不一致")
    if not 0 < shadow_width <= width:
        raise VisionError("shadow 宽度不合法")

    alpha = shadow[:, :, 3]
    if not (alpha > _ALPHA_THRESHOLD).any():
        raise VisionError("shadow 中没有非透明拼图形状")
    return _Frame(background, alpha)


def _confidence(quality: float, margin: float, step: float) -> float:
    """把复核质量、峰间差距与边界阶跃一致性映射成 0..1 的置信度。

    系数是在两组各 50 张的样本上标定的：正样本为真实挑战图，负样本是把同一批图
    的缺口修补掉之后的版本（真正"图里没有缺口"的情形）。按 0.45 的默认门槛，
    正样本全部放行（最低 0.47），负样本 6/50 误放行。

    最低那张是雪山图——缺口正好压在岩石与雪的交界上，判别证据本就薄弱，0.47 是
    对它的诚实评价，不该靠调陡 logistic 把它硬抬上去。

    三项各自的作用：``quality`` 判断该位置像不像缺口，``margin`` 判断有没有别的
    位置同样像（两个峰接近时不该自信），``step`` 判断边界是否真的落在轮廓上。
    """

    raw = (
        _CONF_QUALITY_GAIN * (quality - _CONF_QUALITY_PIVOT)
        + _CONF_MARGIN_GAIN * (margin - _CONF_MARGIN_PIVOT)
        + _CONF_STEP_GAIN * (step - _CONF_STEP_PIVOT)
    )
    return float(min(max(0.5 * (1.0 + math.tanh(0.5 * raw)), 0.0), 1.0))


def solve_gap(
    background_path: str | PathLike[str],
    shadow_path: str | PathLike[str],
) -> GapEstimate:
    """求解缺口位置，返回画布坐标系下的 ``xPos`` 与置信度。"""

    frame = _load(background_path, shadow_path)

    positions, origin = frame.candidates()
    if not positions:
        raise VisionError("OpenCV 未找到可用缺口候选")

    # 似然曲线本身就是物理建模的主证据，复核器负责在其基础上精修与否决，
    # 因此两者加权合并，而不是让复核器单方面推翻曲线。
    like = frame._curves["like"]
    low, high = float(like.min()), float(like.max())
    span = max(high - low, 1e-6)

    score_cache: dict[int, tuple[float, float, float]] = {}

    def score_position(canvas_left: int) -> tuple[float, float, float]:
        cached = score_cache.get(canvas_left)
        if cached is not None:
            return cached
        quality, step = frame.verify(canvas_left)
        total = 0.62 * quality + 0.38 * ((float(like[canvas_left]) - low) / span)
        result = (total, quality, step)
        score_cache[canvas_left] = result
        return result

    ranked: list[tuple[float, int, float, float]] = []
    for canvas_left in positions:
        total, quality, step = score_position(canvas_left)
        ranked.append((total, canvas_left, quality, step))
    ranked.sort(key=lambda item: (-item[0], item[1]))

    total, best_left, quality, step = ranked[0]
    rival = next(
        (item for item in ranked[1:] if abs(item[1] - best_left) >= _PEAK_SEP), None
    )
    margin = total - (rival[0] if rival else 0.0)
    confidence = _confidence(quality, margin, step)

    chamfer = _chamfer_fallback(
        frame,
        positions,
        confidence=confidence,
        score_position=score_position,
    )
    chamfer_candidate: GapCandidate | None = None
    if chamfer is not None:
        best_left, confidence = chamfer
        chamfer_candidate = GapCandidate(
            canvas_left=best_left,
            score=confidence,
            method="global-chamfer",
            bbox=BoundingBox(
                best_left,
                frame.alpha_box.top,
                best_left + frame.shadow_width,
                frame.alpha_box.bottom,
            ),
        )

    # 候选清单只保留互相分离的峰：相邻 1~3px 的探测点属于同一个假设，全列出来
    # 会把真正的备选假设淹掉，排查时反而看不出"还有哪里像缺口"。
    distinct: list[tuple[float, int, float, float]] = []
    for item in ranked:
        if all(abs(item[1] - prior[1]) >= _PEAK_SEP for prior in distinct):
            distinct.append(item)
        if len(distinct) >= 8:
            break

    ranked_candidates = tuple(
        GapCandidate(
            canvas_left=left,
            score=float(min(max(score, 0.0), 1.0)),
            method=origin.get(left, "probe"),
            bbox=BoundingBox(
                left, frame.alpha_box.top, left + frame.shadow_width,
                frame.alpha_box.bottom,
            ),
        )
        for score, left, _, _ in distinct
        if chamfer_candidate is None
        or abs(left - chamfer_candidate.canvas_left) >= _PEAK_SEP
    )
    candidates = (
        ((chamfer_candidate,) if chamfer_candidate is not None else ())
        + ranked_candidates
    )
    return GapEstimate(
        x_pos=int(best_left),
        confidence=confidence,
        alpha_bbox=frame.alpha_box,
        candidates=candidates,
    )


def warm_up() -> None:
    """空跑一次完整求解，把 OpenCV 冷导入与算子首次初始化的开销提前付掉。

    实测：冷导入约 200ms，其后**首次** :func:`solve_gap` 还要再付约 77ms 的算子
    初始化，而热调用只要约 17ms。调用方在拿到真实图片之前先跑这一趟，就能把这
    两笔固定成本与网络往返重叠掉。

    合成图只需能走完"候选 → 复核"全流程，不需要像真实挑战图：平滑噪声背景配一
    块矩形 alpha 就够。任何失败都静默忽略——预热失败最多退回原来的耗时，绝不能
    让调用方起不来。
    """

    try:
        cv2, np = _opencv()
        with tempfile.TemporaryDirectory(prefix="ali-vision-warm-") as directory:
            root = Path(directory)
            background_path = root / "back.png"
            shadow_path = root / "shadow.png"

            rng = np.random.default_rng(0)
            background = (rng.random((200, 296, 3)) * 80 + 100).astype(np.uint8)
            cv2.imwrite(
                str(background_path), cv2.GaussianBlur(background, (15, 15), 0)
            )

            shadow = np.zeros((200, 52, 4), dtype=np.uint8)
            shadow[40:150, 5:47] = 255
            cv2.imwrite(str(shadow_path), shadow)

            solve_gap(background_path, shadow_path)
    except Exception:
        return


__all__ = [
    "BoundingBox",
    "GapCandidate",
    "GapEstimate",
    "solve_gap",
    "warm_up",
]
