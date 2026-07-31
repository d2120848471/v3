"""基于 OpenCV 的缺口定位：从 ``back.png`` 与 ``shadow.png`` 求 ``xPos``。

## 坐标语义

阿里返回的 ``shadow.png`` 是一张**窄画布**：与背景等高，内部只有一小块非透明的
拼图形状，形状左边界通常距画布左侧约 4px。协议需要的 ``xPos`` 是**整个画布**在
``back.png`` 原始像素坐标系中的左坐标，不是 alpha 形状的左边界：

```text
xPos = 匹配到的 alpha 形状位置 - alpha_bbox.left
```

所有 contour 类候选都要做这个换算，且换算用的是运行时实测的 ``alpha_bbox.left``，
不是版本相关的魔数。

## 求解策略

单一检测器在真实图片上都会翻车——雪景让 Canny 只剩缺口右边缘，云层高光会伪装成
缺口，复杂纹理会让模板只贴中一条边。因此这里跑三路互相独立的检测，再用双向
Chamfer 距离仲裁：

```text
路径一  edge-template   4 组 Canny 阈值 × alpha 轮廓模板匹配
路径二  bright-mask     3 组亮度/饱和度阈值 × 连通区域 + 形状匹配
路径三  contour         5 组 Canny 阈值 × 轮廓包围盒
        ↓
     去重 → 按 ±3px 分组 → 组内取最高分，多方法支持提高组分
        ↓
  双向 Chamfer 仲裁（双峰 / 贴顶 / 全轴兜底）
        ↓
     xPos + confidence
```

**双向** Chamfer 是关键：``matchTemplate`` 只命中缺口一条边时也能得到很高的相关
系数，而双向距离同时要求"alpha 轮廓能找到背景边缘"和"局部背景边缘能找到 alpha
轮廓"，可以否掉这种局部贴边的伪峰。

置信度不足时调用方应当停止，不要靠扫描邻近坐标去消耗 Verify 次数。

OpenCV/NumPy 是可选依赖，仅在此模块内惰性导入：主协议解释器不需要装，图像识别
交给 :mod:`ali_slider_reverse.entrypoints.vision_worker` 在独立解释器中执行。
"""

from __future__ import annotations

import math
from dataclasses import dataclass, field
from os import PathLike
from typing import Any

from ..errors import VisionError


# --------------------------------------------------------------------------
# 检测参数
#
# 这些阈值组是在真实样本上调出来的，改动会直接影响识别率。多组阈值并存是刻意
# 设计：单一阈值在雪景、夜景或高纹理背景上表现差异很大，让它们各自出候选、再靠
# 支持度与 Chamfer 仲裁收敛，比调一组"万能阈值"稳定得多。
# --------------------------------------------------------------------------

_ALPHA_THRESHOLD = 8
"""alpha 大于此值即视为拼图形状，略高于 0 以排除 PNG 边缘的透明噪点。"""

_EDGE_TEMPLATE_CANNY = ((10, 35), (20, 60), (35, 105), (60, 180))
_BRIGHT_MASK_PROFILES = ((205, 70), (220, 70), (235, 70))
_CONTOUR_CANNY = ((10, 35), (20, 60), (35, 105), (60, 180), (90, 230))

_SIZE_RATIO_MIN = 0.55
_SIZE_RATIO_MAX = 1.55
"""候选包围盒相对 alpha 形状的宽高比允许区间。"""

_BORDER_MARGIN_PX = 8
"""横向边缘保护带：紧贴图片左右边界的候选一律丢弃。"""

_PEAK_SEPARATION_PX = 8
"""同一 Canny 阈值内，两个峰至少要相距这么远才算独立候选。"""

_PEAKS_PER_THRESHOLD = 3
"""每组 edge-template 阈值最多保留的峰数。"""

_GROUP_RADIUS_PX = 3
"""候选分组半径：相差不超过此值的候选视为同一个峰。"""

_CHAMFER_ACCEPT_DISTANCE = 3.0
"""双向 Chamfer 距离的可接受上限；超过说明轮廓根本没对齐。"""


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
    """单个检测器给出的一个 ``xPos`` 候选。"""

    canvas_left: int
    """已换算到画布坐标系的 xPos。"""

    score: float
    """归一化到 0..1 的候选得分，仅在同一方法内可直接比较。"""

    method: str
    """产出该候选的检测方法，形如 ``edge-template-20-60``。"""

    bbox: BoundingBox | None = None
    """contour 类候选在背景图中的包围盒，供调试定位。"""


@dataclass(frozen=True, slots=True)
class GapEstimate:
    """多路候选合并、仲裁后的最终缺口估计。"""

    x_pos: int
    confidence: float
    alpha_bbox: BoundingBox
    candidates: tuple[GapCandidate, ...]


@dataclass(slots=True, eq=False)
class _Frame:
    """一次求解过程中所有阶段共享的图像与形状约束。"""

    raw_gray: Any
    """未经模糊的灰度图，只用于 Chamfer 的 Sobel 梯度。"""

    gray: Any
    """高斯模糊后的灰度图，供各路候选检测使用。"""

    hsv: Any
    alpha: Any
    alpha_box: BoundingBox
    alpha_contour: Any | None
    width: int
    height: int
    shadow_width: int

    expected_width: int = field(init=False)
    expected_height: int = field(init=False)
    expected_center_y: float = field(init=False)

    def __post_init__(self) -> None:
        self.expected_width = max(self.alpha_box.width, 1)
        self.expected_height = max(self.alpha_box.height, 1)
        self.expected_center_y = (
            self.alpha_box.top + self.alpha_box.bottom
        ) / 2.0

    @property
    def max_canvas_left(self) -> int:
        """画布左坐标的合法上界。"""

        return self.width - self.shadow_width


def _opencv() -> tuple[Any, Any]:
    """惰性导入 OpenCV/NumPy。

    Python 的模块缓存让重复调用几乎零成本，因此各阶段就地取用即可，不必把模块
    对象层层传递。
    """

    try:
        import cv2
        import numpy as np
    except ImportError as exc:  # pragma: no cover - 由独立 vision 运行时覆盖。
        raise VisionError("OpenCV 求解需要 opencv-python 与 numpy") from exc
    return cv2, np


# --------------------------------------------------------------------------
# 图像载入
# --------------------------------------------------------------------------


def _load_frame(
    background_path: str | PathLike[str],
    shadow_path: str | PathLike[str],
) -> _Frame:
    """读入两张图并预计算各阶段共用的派生量。"""

    cv2, np = _opencv()

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
    selected_y, selected_x = np.where(alpha > _ALPHA_THRESHOLD)
    if selected_x.size == 0:
        raise VisionError("shadow 中没有非透明拼图形状")
    alpha_box = BoundingBox(
        int(selected_x.min()),
        int(selected_y.min()),
        int(selected_x.max()) + 1,
        int(selected_y.max()) + 1,
    )

    alpha_binary = np.where(alpha > _ALPHA_THRESHOLD, 255, 0).astype(np.uint8)
    alpha_contours, _ = cv2.findContours(
        alpha_binary,
        cv2.RETR_EXTERNAL,
        cv2.CHAIN_APPROX_SIMPLE,
    )
    alpha_contour = (
        max(alpha_contours, key=cv2.contourArea) if alpha_contours else None
    )

    raw_gray = cv2.cvtColor(background, cv2.COLOR_BGR2GRAY)
    return _Frame(
        raw_gray=raw_gray,
        gray=cv2.GaussianBlur(raw_gray, (3, 3), 0),
        hsv=cv2.cvtColor(background, cv2.COLOR_BGR2HSV),
        alpha=alpha,
        alpha_box=alpha_box,
        alpha_contour=alpha_contour,
        width=width,
        height=height,
        shadow_width=shadow_width,
    )


# --------------------------------------------------------------------------
# 候选检测：三条互相独立的路径
# --------------------------------------------------------------------------


def _edge_template_candidates(frame: _Frame) -> list[GapCandidate]:
    """路径一：把 alpha 轮廓裁到真实纵向范围，在背景同一 y 带上横向匹配。

    模板与背景都先做 Canny，再用 ``TM_CCOEFF_NORMED`` 相关。每组阈值保留互相
    至少相距 ``_PEAK_SEPARATION_PX`` 的前几个峰，交由后续支持度合并——同一阈值
    里挨得很近的多个峰不是独立证据。
    """

    cv2, np = _opencv()
    candidates: list[GapCandidate] = []

    margin_y = 4
    crop_top = max(0, frame.alpha_box.top - margin_y)
    crop_bottom = min(frame.height, frame.alpha_box.bottom + margin_y)
    template_edge = cv2.Canny(frame.alpha[crop_top:crop_bottom, :], 20, 80)
    if int(np.count_nonzero(template_edge)) <= 8:
        # 模板边缘太少，相关系数没有意义，直接放弃这条路径。
        return candidates

    background_crop = frame.gray[crop_top:crop_bottom, :]
    for low, high in _EDGE_TEMPLATE_CANNY:
        result = cv2.matchTemplate(
            cv2.Canny(background_crop, low, high),
            template_edge,
            cv2.TM_CCOEFF_NORMED,
        ).reshape(-1)

        chosen: list[int] = []
        for index in np.argsort(result)[::-1]:
            position = int(index)
            if position < _BORDER_MARGIN_PX or position > frame.max_canvas_left:
                continue
            if any(
                abs(position - prior) < _PEAK_SEPARATION_PX for prior in chosen
            ):
                continue
            # TM_CCOEFF_NORMED 值域是 -1..1，线性映射到 0..1 便于与其他方法比较。
            correlation = float(result[position])
            candidates.append(
                GapCandidate(
                    canvas_left=position,
                    score=max(0.0, min(1.0, (correlation + 1.0) / 2.0)),
                    method=f"edge-template-{low}-{high}",
                )
            )
            chosen.append(position)
            if len(chosen) >= _PEAKS_PER_THRESHOLD:
                break
    return candidates


def _shape_scores(
    frame: _Frame,
    x: int,
    y: int,
    width: int,
    height: int,
) -> tuple[float, float, float] | None:
    """计算 contour 类候选的三项通用几何得分。

    包围盒尺寸或纵向位置明显不像拼图时返回 ``None``，调用方直接跳过该候选。
    三项得分都是"偏差越小越接近 1"的指数衰减。
    """

    if x < _BORDER_MARGIN_PX or x >= frame.width - _BORDER_MARGIN_PX:
        return None
    width_ratio = width / frame.expected_width
    height_ratio = height / frame.expected_height
    if not _SIZE_RATIO_MIN <= width_ratio <= _SIZE_RATIO_MAX:
        return None
    if not _SIZE_RATIO_MIN <= height_ratio <= _SIZE_RATIO_MAX:
        return None
    center_y = y + height / 2.0
    if abs(center_y - frame.expected_center_y) > max(
        32.0, frame.expected_height
    ):
        return None

    size_score = math.exp(
        -abs(math.log(max(width_ratio, 1e-9)))
        - abs(math.log(max(height_ratio, 1e-9)))
    )
    y_score = math.exp(
        -abs(center_y - frame.expected_center_y)
        / max(frame.expected_height * 0.35, 8.0)
    )
    aspect_score = math.exp(
        -abs(
            math.log(
                max(
                    (width / max(height, 1))
                    / (frame.expected_width / max(frame.expected_height, 1)),
                    1e-9,
                )
            )
        )
    )
    return size_score, y_score, aspect_score


def _touches_border(
    frame: _Frame,
    x: int,
    y: int,
    width: int,
    height: int,
) -> bool:
    """判断包围盒是否贴到图片的上、下或右边界。"""

    return (
        y <= 0 or y + height >= frame.height or x + width >= frame.width
    )


def _bright_mask_candidates(frame: _Frame) -> list[GapCandidate]:
    """路径二：高亮低饱和填充区域。

    雪景等背景容易让 Canny 只返回缺口的右边缘，而填充 mask 能恢复完整拼图轮廓。
    再用 shadow 的 alpha 轮廓做 ``matchShapes`` 形状约束，压制形状不像的高光。
    触边候选在这条路径里直接丢弃——填充 mask 本来就容易把天空整块连成一片。
    """

    cv2, np = _opencv()
    candidates: list[GapCandidate] = []

    for brightness, saturation in _BRIGHT_MASK_PROFILES:
        bright_mask = np.where(
            (frame.gray >= brightness) & (frame.hsv[:, :, 1] <= saturation),
            255,
            0,
        ).astype(np.uint8)
        contours, _ = cv2.findContours(
            bright_mask,
            cv2.RETR_LIST,
            cv2.CHAIN_APPROX_SIMPLE,
        )
        for contour in contours:
            x, y, width, height = cv2.boundingRect(contour)
            scores = _shape_scores(frame, x, y, width, height)
            if scores is None or _touches_border(frame, x, y, width, height):
                continue
            size_score, y_score, aspect_score = scores

            contour_area = float(cv2.contourArea(contour))
            fill_score = min(
                1.0, contour_area / max(width * height * 0.45, 1.0)
            )
            shape_score = 0.0
            if frame.alpha_contour is not None and contour_area > 0:
                distance = float(
                    cv2.matchShapes(
                        frame.alpha_contour,
                        contour,
                        cv2.CONTOURS_MATCH_I1,
                        0.0,
                    )
                )
                shape_score = math.exp(-4.0 * max(distance, 0.0))

            score = (
                0.27 * size_score
                + 0.25 * y_score
                + 0.13 * aspect_score
                + 0.25 * shape_score
                + 0.10 * fill_score
            )
            canvas_left = int(round(x - frame.alpha_box.left))
            if 0 <= canvas_left <= frame.max_canvas_left:
                candidates.append(
                    GapCandidate(
                        canvas_left=canvas_left,
                        score=max(0.0, min(1.0, score)),
                        method=f"bright-mask-{brightness}-{saturation}",
                        bbox=BoundingBox(x, y, x + width, y + height),
                    )
                )
    return candidates


def _contour_candidates(frame: _Frame) -> list[GapCandidate]:
    """路径三：多阈值 Canny 找拼图轮廓。

    饱和度在这里只作为**弱特征**（权重 0.10），避免雪景白底把评分完全主导。
    触边候选不丢弃、只打四折——缺口有时确实靠近边界，直接丢会漏掉真解。
    """

    cv2, _ = _opencv()
    candidates: list[GapCandidate] = []

    for low, high in _CONTOUR_CANNY:
        contours, _hierarchy = cv2.findContours(
            cv2.Canny(frame.gray, low, high),
            cv2.RETR_LIST,
            cv2.CHAIN_APPROX_SIMPLE,
        )
        for contour in contours:
            x, y, width, height = cv2.boundingRect(contour)
            scores = _shape_scores(frame, x, y, width, height)
            if scores is None:
                continue
            size_score, y_score, aspect_score = scores

            region = frame.hsv[y : y + height, x : x + width]
            mean_saturation = (
                float(region[:, :, 1].mean()) if region.size else 255.0
            )
            low_saturation_score = 1.0 - mean_saturation / 255.0
            contour_area = float(cv2.contourArea(contour))
            fill_score = min(
                1.0, contour_area / max(width * height * 0.35, 1.0)
            )

            score = (
                0.38 * size_score
                + 0.32 * y_score
                + 0.14 * aspect_score
                + 0.10 * low_saturation_score
                + 0.06 * fill_score
            )
            if _touches_border(frame, x, y, width, height):
                score *= 0.40

            canvas_left = int(round(x - frame.alpha_box.left))
            if 0 <= canvas_left <= frame.max_canvas_left:
                candidates.append(
                    GapCandidate(
                        canvas_left=canvas_left,
                        score=max(0.0, min(1.0, score)),
                        method=f"contour-{low}-{high}",
                        bbox=BoundingBox(x, y, x + width, y + height),
                    )
                )
    return candidates


# --------------------------------------------------------------------------
# 候选归并
# --------------------------------------------------------------------------


def _dedupe(candidates: list[GapCandidate]) -> list[GapCandidate]:
    """同一方法给出的同一坐标只保留最高分。

    同一 Canny 阈值的内外两条轮廓常常给出完全相同的 x，它们不是独立证据；
    不去重会让单个阈值的嵌套 contour 人为抬高支持度。
    """

    unique: dict[tuple[str, int], GapCandidate] = {}
    for candidate in candidates:
        key = (candidate.method, candidate.canvas_left)
        prior = unique.get(key)
        if prior is None or candidate.score > prior.score:
            unique[key] = candidate
    return list(unique.values())


def _rank_groups(
    candidates: list[GapCandidate],
) -> list[tuple[float, int, list[GapCandidate]]]:
    """把邻近候选归成峰，按组得分从高到低排序。

    返回 ``(组得分, 代表坐标, 组内候选)`` 列表。多阈值/多方法重复命中会通过
    ``support_bonus``/``method_bonus`` 提高组得分，但**不会挪动坐标**：阈值变化
    会让同一轮廓的边缘系统性外扩或内缩 1–3px，对坐标取加权平均反而会造出一个
    任何检测器都没有给出的中间像素。因此代表坐标固定取组内最高分候选。
    """

    groups: list[list[GapCandidate]] = []
    for candidate in sorted(candidates, key=lambda item: item.canvas_left):
        for group in groups:
            center = sum(item.canvas_left for item in group) / len(group)
            if abs(candidate.canvas_left - center) <= _GROUP_RADIUS_PX:
                group.append(candidate)
                break
        else:
            groups.append([candidate])

    ranked: list[tuple[float, int, list[GapCandidate]]] = []
    for group in groups:
        methods = {item.method.split("-", 1)[0] for item in group}
        best_score = max(item.score for item in group)
        support_bonus = min(0.22, 0.025 * (len(group) - 1))
        method_bonus = 0.06 if len(methods) > 1 else 0.0
        ranked.append(
            (
                min(1.0, best_score + support_bonus + method_bonus),
                max(group, key=lambda item: item.score).canvas_left,
                group,
            )
        )
    ranked.sort(key=lambda item: item[0], reverse=True)
    return ranked


# --------------------------------------------------------------------------
# 双向 Chamfer 仲裁
# --------------------------------------------------------------------------


def _chamfer_distances(
    frame: _Frame,
    canvas_lefts: Any,
    *,
    margin: int = 5,
) -> dict[int, float]:
    """批量计算完整 alpha 轮廓与背景边缘的双向 Chamfer 距离。

    整图 Sobel 只算一次，让全横轴扫描也能落在热路径预算内；各候选只在自己的
    局部 ROI 上做距离变换。返回 ``坐标 → 距离``，越小表示轮廓对得越齐。

    绝对阈值（梯度 120）只在当前 8-bit Sobel 的定义下有意义，因此梯度计算、
    局部 ROI 与距离变换都固定在这个函数里，不对外暴露参数。
    """

    cv2, np = _opencv()

    if margin < 0:
        raise ValueError("margin 不能为负数")
    positions = tuple(dict.fromkeys(int(value) for value in canvas_lefts))
    if not positions:
        return {}

    binary_alpha = np.asarray(frame.alpha > _ALPHA_THRESHOLD, dtype=bool)
    eroded = cv2.erode(
        binary_alpha.astype(np.uint8),
        cv2.getStructuringElement(cv2.MORPH_CROSS, (3, 3)),
    ).astype(bool)
    alpha_boundary = binary_alpha & ~eroded
    if not bool(np.any(alpha_boundary)):
        return {position: math.inf for position in positions}

    gray_float = np.asarray(frame.raw_gray, dtype=np.float32)
    magnitude = cv2.magnitude(
        cv2.Sobel(gray_float, cv2.CV_32F, 1, 0, ksize=3),
        cv2.Sobel(gray_float, cv2.CV_32F, 0, 1, ksize=3),
    )
    alpha_y, alpha_x = np.where(alpha_boundary)
    alpha_box = frame.alpha_box

    distances: dict[int, float] = {}
    for canvas_left in positions:
        if not 0 <= canvas_left <= frame.max_canvas_left:
            distances[canvas_left] = math.inf
            continue

        roi_left = max(0, canvas_left + alpha_box.left - margin)
        roi_right = min(frame.width, canvas_left + alpha_box.right + margin)
        roi_top = max(0, alpha_box.top - margin)
        roi_bottom = min(frame.height, alpha_box.bottom + margin)
        if roi_left >= roi_right or roi_top >= roi_bottom:
            distances[canvas_left] = math.inf
            continue

        local_magnitude = magnitude[roi_top:roi_bottom, roi_left:roi_right]
        # 下限 120 排除低对比纹理；局部上四分位避免高纹理画面把整块 ROI 当成边缘。
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
        # 两个方向取平均：只贴中一条边时，另一个方向的距离会很大。
        distances[canvas_left] = (
            float(distance_to_background[local_alpha_boundary].mean())
            + float(distance_to_alpha[background_boundary].mean())
        ) / 2.0
    return distances


def _edge_consensus_near(
    candidates: list[GapCandidate],
    target_left: int,
    *,
    radius: int = 3,
    minimum_support: int = 3,
) -> int | None:
    """返回目标坐标附近、被至少三组 edge 阈值共同支持的离散坐标。"""

    support: dict[int, set[str]] = {}
    best_scores: dict[int, float] = {}
    for candidate in candidates:
        if (
            not candidate.method.startswith("edge-template-")
            or abs(candidate.canvas_left - target_left) > radius
        ):
            continue
        support.setdefault(candidate.canvas_left, set()).add(candidate.method)
        best_scores[candidate.canvas_left] = max(
            candidate.score, best_scores.get(candidate.canvas_left, 0.0)
        )

    eligible = [
        left for left, methods in support.items() if len(methods) >= minimum_support
    ]
    if not eligible:
        return None
    return min(
        eligible,
        key=lambda left: (
            -len(support[left]),
            abs(left - target_left),
            -best_scores[left],
            left,
        ),
    )


def _arbitrate_edge_pair(
    frame: _Frame,
    ranked: list[tuple[float, int, list[GapCandidate]]],
    alignment: dict[int, float],
    best_score: float,
    best_left: int,
    best_group: list[GapCandidate],
) -> tuple[float, int, list[GapCandidate] | None]:
    """仲裁"相隔约一个拼图宽度"的 edge-template 双峰。

    复杂纹理会让 full-width alpha 模板只贴中缺口的一条边，产生两个几乎等分的峰。
    **不能固定取左峰**：已有样本里"左峰为真"和"右峰为真"都出现过。这里用完整
    alpha 轮廓的双向 Chamfer 距离做局部仲裁。

    返回 ``(新得分, 新坐标, 被消耗掉的配对组)``；没有配对时原样返回。
    """

    if not all(item.method.startswith("edge-template-") for item in best_group):
        return best_score, best_left, None

    paired = None
    for entry in ranked[1:]:
        candidate_score, candidate_left, group = entry
        separation = abs(best_left - candidate_left)
        if not (
            frame.expected_width * 0.65
            <= separation
            <= frame.expected_width * 1.15
        ):
            continue
        if candidate_score < best_score - 0.12:
            continue
        if not all(item.method.startswith("edge-template-") for item in group):
            continue
        paired = entry
        break
    if paired is None:
        return best_score, best_left, None

    paired_score, paired_left, paired_group = paired
    pair_alignment = (
        (alignment.get(best_left, math.inf), best_left),
        (alignment.get(paired_left, math.inf), paired_left),
    )
    accepted = [
        (distance, left)
        for distance, left in pair_alignment
        if distance <= _CHAMFER_ACCEPT_DISTANCE
    ]
    if not accepted:
        # 两个峰都没到"对齐"标准时，只在优势足够悬殊的情况下才勉强采信更近的那个。
        ordered = sorted(pair_alignment)
        best_distance, fallback_left = ordered[0]
        runner_up_distance, _ = ordered[1]
        if (
            math.isfinite(best_distance)
            and math.isfinite(runner_up_distance)
            and best_distance <= 5.0
            and runner_up_distance - best_distance >= 1.5
            and runner_up_distance / best_distance >= 1.30
        ):
            accepted = [(best_distance, fallback_left)]
    if not accepted:
        raise VisionError("OpenCV edge 双峰均未通过完整轮廓 Chamfer 校验")

    _, resolved_left = min(accepted)
    return (
        min(1.0, (best_score + paired_score) / 2.0 + 0.06),
        resolved_left,
        paired_group,
    )


def _refine_top_touching(
    frame: _Frame,
    alignment: dict[int, float],
    best_left: int,
    best_group: list[GapCandidate],
) -> int:
    """alpha 形状触及图片上边界时的局部修正。

    此时 Canny 模板的上边缘会被裁掉，相关峰可能在同一组内偏移几个像素。修正
    只在**组内已有的**候选之间用 Chamfer 仲裁，既不扫描也不制造新坐标；改善
    不足时保留原峰，避免改写已经正确的非边界样本。
    """

    if frame.alpha_box.top != 0:
        return best_left
    has_edge_peak = any(
        item.method.startswith("edge-template-")
        and item.canvas_left == best_left
        for item in best_group
    )
    non_edge_lefts = {
        item.canvas_left
        for item in best_group
        if not item.method.startswith("edge-template-")
    }
    if not has_edge_peak or not non_edge_lefts:
        return best_left

    local = [
        (alignment.get(left, math.inf), left)
        for left in sorted({best_left, *non_edge_lefts})
    ]
    original_distance = next(
        distance for distance, left in local if left == best_left
    )
    refined_distance, refined_left = min(
        local,
        key=lambda item: (item[0], abs(item[1] - best_left), item[1]),
    )
    if (
        refined_left != best_left
        and refined_left in non_edge_lefts
        and math.isfinite(original_distance)
        and math.isfinite(refined_distance)
        and refined_distance <= _CHAMFER_ACCEPT_DISTANCE
        and original_distance - refined_distance >= 0.15
    ):
        return refined_left
    return best_left


def _snap_chamfer_plateau_to_edge_consensus(
    candidates: list[GapCandidate],
    alignment: dict[int, float],
    global_left: int,
    global_distance: float,
) -> tuple[int, float]:
    """全局 Chamfer 峰近乎平坦时，吸附到多阈值 edge 共识。

    Chamfer 出现平台时，最低点会被背景纹理拉偏几个像素。只有当独立的 edge 共识
    落在平台 **3px 之外**、且距离损失不超过 0.10、本身也足够对齐时才吸附过去；
    1–2px 的普通局部差异继续保留全局最低点。
    """

    consensus_left = _edge_consensus_near(candidates, global_left)
    if consensus_left is None:
        return global_left, global_distance
    consensus_distance = alignment.get(consensus_left, math.inf)
    if (
        abs(consensus_left - global_left) >= 3
        and consensus_distance - global_distance <= 0.10
        and consensus_distance <= _CHAMFER_ACCEPT_DISTANCE
    ):
        return consensus_left, consensus_distance
    return global_left, global_distance


def _global_chamfer_override(
    frame: _Frame,
    candidates: list[GapCandidate],
    alignment: dict[int, float],
    best_left: int,
) -> tuple[int, float, str] | None:
    """候选生成器完全漏检时，用全横轴 Chamfer 扫描补回坐标。

    门槛刻意设得高：只有当全局最优与现有结果相差 **≥4px**、自身距离足够小、
    且明显优于远处的次优点时才接管。小于 4px 的离散差异一律保留现有多路候选
    结果——Chamfer 的局部平台会把已验证的锚点系统性挪动 1–3px。

    返回 ``(坐标, 置信度, 方法名)``，不满足门槛时返回 ``None``。
    """

    finite = sorted(
        (distance, left)
        for left, distance in alignment.items()
        if left >= _BORDER_MARGIN_PX and math.isfinite(distance)
    )
    if not finite:
        return None

    global_distance, global_left = finite[0]
    runner_separation = max(12, round(frame.expected_width * 0.55))
    runner_distance = next(
        (
            distance
            for distance, left in finite[1:]
            if abs(left - global_left) >= runner_separation
        ),
        math.inf,
    )
    if not (
        abs(global_left - best_left) >= 4
        and global_distance <= _CHAMFER_ACCEPT_DISTANCE
        and runner_distance - global_distance >= 0.70
    ):
        return None

    # Chamfer 峰出现近乎平坦的平台时，最低点会被背景纹理拉偏。
    selected_left, selected_distance = _snap_chamfer_plateau_to_edge_consensus(
        candidates, alignment, global_left, global_distance
    )
    if selected_distance > _CHAMFER_ACCEPT_DISTANCE:
        selected_left, selected_distance = global_left, global_distance

    absolute_score = math.exp(-max(0.0, selected_distance - 1.0) / 2.0)
    margin_score = (
        1.0
        if not math.isfinite(runner_distance)
        else 1.0 - math.exp(-max(0.0, runner_distance - global_distance))
    )
    score = max(0.0, min(1.0, 0.70 * absolute_score + 0.30 * margin_score))
    method = (
        "global-chamfer"
        if selected_left == global_left
        else "global-chamfer-edge-consensus"
    )
    return selected_left, score, method


# --------------------------------------------------------------------------
# 主入口
# --------------------------------------------------------------------------


def solve_gap(
    background_path: str | PathLike[str],
    shadow_path: str | PathLike[str],
) -> GapEstimate:
    """求解缺口位置，返回画布坐标系下的 ``xPos`` 与置信度。"""

    frame = _load_frame(background_path, shadow_path)

    candidates = (
        _edge_template_candidates(frame)
        + _bright_mask_candidates(frame)
        + _contour_candidates(frame)
    )
    if not candidates:
        raise VisionError("OpenCV 未找到可用缺口候选")

    candidates = _dedupe(candidates)
    ranked = _rank_groups(candidates)
    best_score, best_left, best_group = ranked[0]
    excluded_groups = {id(best_group)}

    # 一次性算出全横轴的 Chamfer 距离，后面三种仲裁复用同一批结果。
    alignment = _chamfer_distances(frame, range(frame.max_canvas_left + 1))

    best_score, resolved_left, paired_group = _arbitrate_edge_pair(
        frame, ranked, alignment, best_score, best_left, best_group
    )
    if paired_group is not None:
        excluded_groups.add(id(paired_group))
        best_left = resolved_left
        candidates.append(
            GapCandidate(
                canvas_left=best_left,
                score=best_score,
                method="edge-pair-chamfer",
            )
        )

    refined_left = _refine_top_touching(frame, alignment, best_left, best_group)
    if refined_left != best_left:
        best_left = refined_left
        candidates.append(
            GapCandidate(
                canvas_left=best_left,
                score=best_score,
                method="edge-local-chamfer",
            )
        )

    override = _global_chamfer_override(frame, candidates, alignment, best_left)
    if override is not None:
        best_left, global_score, method = override
        candidates.append(
            GapCandidate(
                canvas_left=best_left, score=global_score, method=method
            )
        )

    # 置信度同时看绝对得分和与次优峰的差距：两个峰分数接近时不应该自信。
    second_score = max(
        (score for score, _, group in ranked if id(group) not in excluded_groups),
        default=0.0,
    )
    confidence = max(
        0.0, min(1.0, 0.65 * best_score + 0.35 * (best_score - second_score))
    )
    return GapEstimate(
        x_pos=int(best_left),
        confidence=confidence,
        alpha_bbox=frame.alpha_box,
        candidates=tuple(
            sorted(
                candidates,
                key=lambda item: (item.score, item.method),
                reverse=True,
            )[:20]
        ),
    )


__all__ = [
    "BoundingBox",
    "GapCandidate",
    "GapEstimate",
    "solve_gap",
]
