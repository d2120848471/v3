"""滑块图片位置估算。

当前实现使用两条可解释的前端图像证据：

1. ``back.png`` 中“高亮、低饱和”的连通域用于定位拼图缺口；
2. ``shadow.png`` 的 alpha 非透明包围盒用于约束候选连通域尺寸。

读取真实 PNG 时优先使用 Pillow；为了让核心算法在未安装 Pillow 的环境中仍可单测，
也接受实现了 ``convert('RGBA')``、``size`` 和 ``getdata()`` 的图像对象。
"""

from __future__ import annotations

import math
from collections import deque
from dataclasses import dataclass
from os import PathLike
from pathlib import Path
from typing import Any

try:  # Pillow 是可选运行时依赖；本项目不在代码里擅自安装第三方包。
    from PIL import Image as PILImage
except ImportError:  # pragma: no cover - 当前测试环境通过图像协议替身覆盖核心算法。
    PILImage = None


ImageSource = str | PathLike[str] | Any


@dataclass(frozen=True, slots=True)
class BoundingBox:
    """使用 Pillow 风格的半开区间包围盒：``right``/``bottom`` 不包含。"""

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

    @property
    def area(self) -> int:
        return self.width * self.height


@dataclass(frozen=True, slots=True)
class ConnectedComponent:
    bbox: BoundingBox
    pixel_count: int

    @property
    def fill_ratio(self) -> float:
        return self.pixel_count / self.bbox.area if self.bbox.area else 0.0


@dataclass(frozen=True, slots=True)
class SliderSolution:
    """图像坐标与页面滑动坐标之间的估算结果。"""

    puzzle_left: float
    mapping_ratio: float
    puzzle_x: float
    slider_distance: float
    mapping_mode: str
    position_mode: str
    background_bbox: BoundingBox
    shadow_bbox: BoundingBox | None


@dataclass(frozen=True, slots=True)
class GapCandidate:
    """OpenCV 多路检测产生的一个 xPos 候选。"""

    canvas_left: int
    score: float
    method: str
    bbox: BoundingBox | None = None


@dataclass(frozen=True, slots=True)
class GapEstimate:
    """多路候选合并后的缺口估计。"""

    x_pos: int
    confidence: float
    alpha_bbox: BoundingBox
    candidates: tuple[GapCandidate, ...]


def _load_rgba_image(source: ImageSource) -> tuple[Any, bool]:
    """返回 RGBA 图像以及“是否需要由本函数关闭”的标记。"""

    if isinstance(source, (str, PathLike)):
        if PILImage is None:
            raise RuntimeError(
                "从 PNG 路径读取图片需要 Pillow；请在调用环境安装 Pillow，"
                "或传入兼容 Pillow 图像协议的对象"
            )
        opened = PILImage.open(Path(source))
        try:
            return opened.convert("RGBA"), True
        finally:
            opened.close()

    if not hasattr(source, "convert") or not hasattr(source, "size"):
        raise TypeError("image 必须是路径或兼容 Pillow 图像协议的对象")
    converted = source.convert("RGBA")
    return converted, converted is not source


def _rgba_pixels(source: ImageSource) -> tuple[int, int, list[tuple[int, int, int, int]]]:
    image, owned = _load_rgba_image(source)
    try:
        width, height = image.size
        if width <= 0 or height <= 0:
            raise ValueError("图像尺寸必须为正数")
        pixels = list(image.getdata())
        if len(pixels) != width * height:
            raise ValueError("图像像素数量与尺寸不一致")

        normalized: list[tuple[int, int, int, int]] = []
        for pixel in pixels:
            if not isinstance(pixel, (tuple, list)) or len(pixel) != 4:
                raise ValueError("RGBA 图像像素必须包含四个通道")
            normalized.append(tuple(int(channel) for channel in pixel))  # type: ignore[arg-type]
        return width, height, normalized
    finally:
        # PIL 的 convert() 产生独立图像；路径调用由我们负责释放。
        if owned and hasattr(image, "close"):
            image.close()


def alpha_bbox(shadow: ImageSource, *, alpha_threshold: int = 8) -> BoundingBox | None:
    """返回 ``shadow.png`` 中 alpha 大于阈值的最小包围盒。

    全透明图片返回 ``None``。阈值略高于零可排除 PNG 边缘的透明噪点。
    """

    if not 0 <= alpha_threshold <= 255:
        raise ValueError("alpha_threshold 必须位于 0..255")
    width, height, pixels = _rgba_pixels(shadow)
    xs: list[int] = []
    ys: list[int] = []
    for index, (_, _, _, alpha) in enumerate(pixels):
        if alpha > alpha_threshold:
            xs.append(index % width)
            ys.append(index // width)
    if not xs:
        return None
    return BoundingBox(min(xs), min(ys), max(xs) + 1, max(ys) + 1)


def _is_bright_low_saturation(
    pixel: tuple[int, int, int, int],
    *,
    brightness_threshold: float,
    saturation_threshold: float,
    alpha_threshold: int,
) -> bool:
    red, green, blue, alpha = pixel
    if alpha <= alpha_threshold:
        return False

    # 使用感知亮度避免把高蓝通道但肉眼偏暗的区域误认为“亮色”。
    brightness = 0.2126 * red + 0.7152 * green + 0.0722 * blue
    maximum = max(red, green, blue)
    minimum = min(red, green, blue)
    saturation = 0.0 if maximum == 0 else (maximum - minimum) / maximum
    return brightness >= brightness_threshold and saturation <= saturation_threshold


def bright_low_saturation_components(
    background: ImageSource,
    *,
    brightness_threshold: float = 205.0,
    saturation_threshold: float = 0.20,
    alpha_threshold: int = 8,
    min_pixels: int | None = None,
) -> list[ConnectedComponent]:
    """提取 ``back.png`` 中高亮、低饱和的八连通域。

    返回值按像素数从大到小排列。默认最小面积随图片大小缩放，以抑制孤立高光噪点。
    """

    if not 0 <= brightness_threshold <= 255:
        raise ValueError("brightness_threshold 必须位于 0..255")
    if not 0 <= saturation_threshold <= 1:
        raise ValueError("saturation_threshold 必须位于 0..1")
    if not 0 <= alpha_threshold <= 255:
        raise ValueError("alpha_threshold 必须位于 0..255")

    width, height, pixels = _rgba_pixels(background)
    minimum = max(4, round(width * height * 0.0005)) if min_pixels is None else min_pixels
    if minimum <= 0:
        raise ValueError("min_pixels 必须为正数")

    mask = [
        _is_bright_low_saturation(
            pixel,
            brightness_threshold=brightness_threshold,
            saturation_threshold=saturation_threshold,
            alpha_threshold=alpha_threshold,
        )
        for pixel in pixels
    ]
    visited = bytearray(width * height)
    components: list[ConnectedComponent] = []

    for start, selected in enumerate(mask):
        if not selected or visited[start]:
            continue
        queue: deque[int] = deque([start])
        visited[start] = 1
        left = right = start % width
        top = bottom = start // width
        count = 0

        while queue:
            current = queue.popleft()
            x = current % width
            y = current // width
            count += 1
            left = min(left, x)
            right = max(right, x)
            top = min(top, y)
            bottom = max(bottom, y)

            # 八连通能把抗锯齿边缘和斜向相连的拼图轮廓视为同一候选。
            for dy in (-1, 0, 1):
                ny = y + dy
                if ny < 0 or ny >= height:
                    continue
                for dx in (-1, 0, 1):
                    if dx == 0 and dy == 0:
                        continue
                    nx = x + dx
                    if nx < 0 or nx >= width:
                        continue
                    neighbor = ny * width + nx
                    if mask[neighbor] and not visited[neighbor]:
                        visited[neighbor] = 1
                        queue.append(neighbor)

        if count >= minimum:
            components.append(
                ConnectedComponent(
                    bbox=BoundingBox(left, top, right + 1, bottom + 1),
                    pixel_count=count,
                )
            )

    components.sort(key=lambda component: component.pixel_count, reverse=True)
    return components


def _component_score(
    component: ConnectedComponent,
    *,
    image_width: int,
    image_height: int,
    expected_bbox: BoundingBox | None,
) -> float:
    bbox = component.bbox
    image_area = image_width * image_height
    area_fraction = component.pixel_count / image_area

    # 拼图缺口通常不是贯穿图片的大面积白底，触边候选也更可能是天空/边框。
    touches_border = (
        bbox.left == 0
        or bbox.top == 0
        or bbox.right == image_width
        or bbox.bottom == image_height
    )
    border_penalty = 0.15 if touches_border else 1.0
    huge_penalty = 0.05 if area_fraction > 0.25 else 1.0
    aspect = bbox.width / max(bbox.height, 1)
    square_score = math.exp(-abs(math.log(max(aspect, 1e-9))))

    if expected_bbox is not None:
        width_ratio = bbox.width / max(expected_bbox.width, 1)
        height_ratio = bbox.height / max(expected_bbox.height, 1)
        size_score = math.exp(-abs(math.log(width_ratio)) - abs(math.log(height_ratio)))
    else:
        # 没有 shadow 尺寸时，偏好中等、近方形且内部较连贯的区域。
        target_fraction = 0.02
        size_score = math.exp(-abs(math.log(max(area_fraction, 1e-9) / target_fraction)))

    return (
        size_score
        * (0.4 + 0.6 * square_score)
        * (0.4 + 0.6 * component.fill_ratio)
        * border_penalty
        * huge_penalty
    )


def estimate_puzzle_bbox(
    background: ImageSource,
    shadow: ImageSource | None = None,
    *,
    brightness_threshold: float = 205.0,
    saturation_threshold: float = 0.20,
    alpha_threshold: int = 8,
    min_pixels: int | None = None,
) -> tuple[BoundingBox, BoundingBox | None]:
    """估算背景缺口包围盒，并一并返回 shadow 的 alpha 包围盒。"""

    width, height, _ = _rgba_pixels(background)
    expected = alpha_bbox(shadow, alpha_threshold=alpha_threshold) if shadow is not None else None

    # 如果 shadow 本身是整幅不透明图，完整画布尺寸不能代表拼图块，忽略该约束。
    if expected is not None and (
        expected.width >= width * 0.70
        or expected.height >= height * 0.70
        or expected.area >= width * height * 0.35
    ):
        expected_for_score = None
    else:
        expected_for_score = expected

    components = bright_low_saturation_components(
        background,
        brightness_threshold=brightness_threshold,
        saturation_threshold=saturation_threshold,
        alpha_threshold=alpha_threshold,
        min_pixels=min_pixels,
    )
    if not components:
        raise ValueError("未找到符合亮度/饱和度阈值的连通域")

    best = max(
        components,
        key=lambda component: _component_score(
            component,
            image_width=width,
            image_height=height,
            expected_bbox=expected_for_score,
        ),
    )
    return best.bbox, expected


def estimate_puzzle_left(
    background: ImageSource,
    shadow: ImageSource | None = None,
    **kwargs: Any,
) -> float:
    """返回 ``back.png`` 原始像素坐标系中的缺口左边界。"""

    bbox, _ = estimate_puzzle_bbox(background, shadow, **kwargs)
    return float(bbox.left)


def estimate_puzzle_canvas_left(
    background: ImageSource,
    shadow: ImageSource,
    *,
    brightness_threshold: float = 200.0,
    saturation_threshold: float = 0.22,
    alpha_threshold: int = 8,
    peak_ratio: float = 0.98,
) -> float:
    """用 ``shadow`` 的 alpha 形状定位拼图画布在背景中的左坐标。

    阿里返回的 ``shadow.png`` 是“窄画布 + 与背景等高”的拼图层。其非透明形状
    与 ``back.png`` 中的亮色缺口相同。这里把 alpha mask 沿 x 轴滑动，以 Dice
    分数比较“mask 内应为亮色、mask 外不应为亮色”；抗锯齿会让最高点略偏向一侧，
    因而取同一峰内达到最高分 ``peak_ratio`` 的连续区间中点。

    返回的是整个 shadow 画布的左坐标，不是非透明 alpha bbox 的左边界。这正是
    前端 ``xPos`` 所使用的坐标语义。
    """

    if not 0 <= brightness_threshold <= 255:
        raise ValueError("brightness_threshold 必须位于 0..255")
    if not 0 <= saturation_threshold <= 1:
        raise ValueError("saturation_threshold 必须位于 0..1")
    if not 0 <= alpha_threshold <= 255:
        raise ValueError("alpha_threshold 必须位于 0..255")
    if not 0 < peak_ratio <= 1:
        raise ValueError("peak_ratio 必须位于 (0, 1]")

    background_width, background_height, background_pixels = _rgba_pixels(
        background
    )
    shadow_width, shadow_height, shadow_pixels = _rgba_pixels(shadow)
    if shadow_height != background_height:
        raise ValueError("alpha 模板要求 shadow 与 background 等高")
    if shadow_width <= 0 or shadow_width > background_width:
        raise ValueError("shadow 宽度必须位于 1..background_width")

    alpha_mask = [
        pixel[3] > alpha_threshold
        for pixel in shadow_pixels
    ]
    if not any(alpha_mask):
        raise ValueError("shadow 中没有可用的非透明 alpha mask")

    scores: list[float] = []
    for offset in range(background_width - shadow_width + 1):
        true_positive = 0
        false_positive = 0
        false_negative = 0
        for y in range(shadow_height):
            background_row = y * background_width + offset
            shadow_row = y * shadow_width
            for x in range(shadow_width):
                in_shape = alpha_mask[shadow_row + x]
                is_gap = _is_bright_low_saturation(
                    background_pixels[background_row + x],
                    brightness_threshold=brightness_threshold,
                    saturation_threshold=saturation_threshold,
                    alpha_threshold=alpha_threshold,
                )
                if in_shape and is_gap:
                    true_positive += 1
                elif is_gap:
                    false_positive += 1
                elif in_shape:
                    false_negative += 1

        denominator = (
            2 * true_positive + false_positive + false_negative
        )
        scores.append(
            0.0
            if denominator == 0
            else 2.0 * true_positive / denominator
        )

    best_offset = max(range(len(scores)), key=scores.__getitem__)
    best_score = scores[best_offset]
    if best_score <= 0:
        raise ValueError("alpha 模板未匹配到亮色缺口")

    # 只扩展最高点所在的连续峰，避免把远处云层等相似高光一起求平均。
    floor = best_score * peak_ratio
    left = best_offset
    right = best_offset
    while left > 0 and scores[left - 1] >= floor:
        left -= 1
    while right + 1 < len(scores) and scores[right + 1] >= floor:
        right += 1
    return (left + right) / 2.0


def _edge_alignment_chamfer_distances(
    background_gray: Any,
    alpha_mask: Any,
    alpha_box: BoundingBox,
    canvas_lefts: Any,
    *,
    margin: int = 5,
) -> dict[int, float]:
    """批量计算完整 alpha 轮廓与背景边缘的双向距离。

    ``matchTemplate`` 只命中缺口的一条边时也可能得到很高相关系数。双向
    Chamfer 同时要求“alpha 轮廓能找到背景边缘”以及“局部背景边缘能找到
    alpha 轮廓”，用于区分这种局部贴边伪峰。批量入口只计算一次整图 Sobel，
    让全横轴扫描仍能落在热路径预算内。绝对阈值只在当前 8-bit Sobel 定义下
    有意义，因此梯度、局部 ROI 和距离变换都固定在这里。
    """

    try:
        import cv2
        import numpy as np
    except ImportError as exc:  # pragma: no cover - 调用方已要求 OpenCV。
        raise RuntimeError(
            "Chamfer 边缘仲裁需要 opencv-python 与 numpy"
        ) from exc

    if margin < 0:
        raise ValueError("margin 不能为负数")
    if background_gray.ndim != 2 or alpha_mask.ndim != 2:
        raise ValueError("Chamfer 输入必须是二维灰度图和 alpha mask")

    background_height, background_width = background_gray.shape
    shadow_height, shadow_width = alpha_mask.shape
    if shadow_height != background_height:
        raise ValueError("alpha mask 与背景高度不一致")
    positions = tuple(dict.fromkeys(int(value) for value in canvas_lefts))
    if not positions:
        return {}

    binary_alpha = np.asarray(alpha_mask, dtype=bool)
    eroded = cv2.erode(
        binary_alpha.astype(np.uint8),
        cv2.getStructuringElement(cv2.MORPH_CROSS, (3, 3)),
    ).astype(bool)
    alpha_boundary = binary_alpha & ~eroded
    if not bool(np.any(alpha_boundary)):
        return {position: math.inf for position in positions}

    gray_float = np.asarray(background_gray, dtype=np.float32)
    gradient_x = cv2.Sobel(
        gray_float,
        cv2.CV_32F,
        1,
        0,
        ksize=3,
    )
    gradient_y = cv2.Sobel(
        gray_float,
        cv2.CV_32F,
        0,
        1,
        ksize=3,
    )
    magnitude = cv2.magnitude(gradient_x, gradient_y)
    alpha_y, alpha_x = np.where(alpha_boundary)
    distances: dict[int, float] = {}
    maximum_left = background_width - shadow_width
    for canvas_left in positions:
        if not 0 <= canvas_left <= maximum_left:
            distances[canvas_left] = math.inf
            continue

        roi_left = max(0, canvas_left + alpha_box.left - margin)
        roi_right = min(
            background_width,
            canvas_left + alpha_box.right + margin,
        )
        roi_top = max(0, alpha_box.top - margin)
        roi_bottom = min(background_height, alpha_box.bottom + margin)
        if roi_left >= roi_right or roi_top >= roi_bottom:
            distances[canvas_left] = math.inf
            continue

        local_magnitude = magnitude[
            roi_top:roi_bottom,
            roi_left:roi_right,
        ]
        # 120 排除低对比纹理；局部上四分位避免高纹理画面把整块 ROI 都当成边缘。
        gradient_threshold = max(
            120.0,
            float(np.quantile(local_magnitude, 0.75)),
        )
        background_boundary = local_magnitude >= gradient_threshold
        if not bool(np.any(background_boundary)):
            distances[canvas_left] = math.inf
            continue

        local_alpha_boundary = np.zeros(
            background_boundary.shape,
            dtype=bool,
        )
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
            (~background_boundary).astype(np.uint8),
            cv2.DIST_L2,
            5,
        )
        distance_to_alpha = cv2.distanceTransform(
            (~local_alpha_boundary).astype(np.uint8),
            cv2.DIST_L2,
            5,
        )
        alpha_to_background = float(
            distance_to_background[local_alpha_boundary].mean()
        )
        background_to_alpha = float(
            distance_to_alpha[background_boundary].mean()
        )
        distances[canvas_left] = (
            alpha_to_background + background_to_alpha
        ) / 2.0
    return distances


def _edge_alignment_chamfer_distance(
    background_gray: Any,
    alpha_mask: Any,
    alpha_box: BoundingBox,
    canvas_left: int,
    *,
    margin: int = 5,
) -> float:
    """返回单个候选的双向 Chamfer 距离。"""

    return _edge_alignment_chamfer_distances(
        background_gray,
        alpha_mask,
        alpha_box,
        (canvas_left,),
        margin=margin,
    )[canvas_left]


def _edge_consensus_near(
    candidates: list[GapCandidate],
    target_left: int,
    *,
    radius: int = 3,
    minimum_support: int = 3,
) -> int | None:
    """返回全局轮廓峰附近、被多个 edge 阈值共同支持的离散坐标。"""

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
            candidate.score,
            best_scores.get(candidate.canvas_left, 0.0),
        )
    eligible = [
        left
        for left, methods in support.items()
        if len(methods) >= minimum_support
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


def _snap_chamfer_plateau_to_edge_consensus(
    candidates: list[GapCandidate],
    alignment_by_left: dict[int, float],
    global_left: int,
    global_distance: float,
) -> tuple[int, float]:
    """在全局 Chamfer 平台近乎平坦时吸附到多阈值 edge 共识。"""

    edge_consensus_left = _edge_consensus_near(candidates, global_left)
    if edge_consensus_left is None:
        return global_left, global_distance
    edge_consensus_distance = alignment_by_left.get(
        edge_consensus_left,
        math.inf,
    )
    # Chamfer 峰出现近乎平坦的平台时，最低点会受背景纹理拉偏。只有独立
    # edge 共识位于平台 3px 边缘、距离损失不超过 0.10 时才吸附过去；
    # 1–2px 的普通局部差异继续保留全局最低点。
    if (
        abs(edge_consensus_left - global_left) >= 3
        and edge_consensus_distance - global_distance <= 0.10
        and edge_consensus_distance <= 3.0
    ):
        return edge_consensus_left, edge_consensus_distance
    return global_left, global_distance


def estimate_puzzle_canvas_left_cv(
    background_path: str | PathLike[str],
    shadow_path: str | PathLike[str],
) -> GapEstimate:
    """用多路候选与全横轴轮廓校验求 ``xPos``。

    当前拼图的 ``shadow.png`` 是窄画布，alpha 拼图形状通常从画布左侧约 4px
    开始；背景 contour 给出的是“形状左边界”，而协议 ``xPos`` 使用“画布左边界”。
    因此 contour 候选统一减去运行时 alpha bbox.left，不使用版本相关魔数。

    OpenCV/NumPy 是可选依赖；主协议可通过 ``vision_bridge.py`` 交给另一个已安装
    OpenCV 的 Python 解释器执行。
    """

    try:
        import cv2
        import numpy as np
    except ImportError as exc:  # pragma: no cover - 由独立 vision 运行时覆盖。
        raise RuntimeError(
            "OpenCV 求解需要 opencv-python 与 numpy"
        ) from exc

    background = cv2.imread(str(background_path), cv2.IMREAD_COLOR)
    shadow = cv2.imread(str(shadow_path), cv2.IMREAD_UNCHANGED)
    if background is None:
        raise ValueError("无法读取 background PNG")
    if shadow is None or shadow.ndim != 3 or shadow.shape[2] < 4:
        raise ValueError("shadow PNG 必须包含 alpha 通道")

    background_height, background_width = background.shape[:2]
    shadow_height, shadow_width = shadow.shape[:2]
    if shadow_height != background_height:
        raise ValueError("shadow 与 background 高度不一致")
    if not 0 < shadow_width <= background_width:
        raise ValueError("shadow 宽度不合法")

    alpha = shadow[:, :, 3]
    selected_y, selected_x = np.where(alpha > 8)
    if selected_x.size == 0:
        raise ValueError("shadow 中没有非透明拼图形状")
    alpha_box = BoundingBox(
        int(selected_x.min()),
        int(selected_y.min()),
        int(selected_x.max()) + 1,
        int(selected_y.max()) + 1,
    )
    expected_width = max(alpha_box.width, 1)
    expected_height = max(alpha_box.height, 1)
    expected_center_y = (alpha_box.top + alpha_box.bottom) / 2.0

    raw_gray = cv2.cvtColor(background, cv2.COLOR_BGR2GRAY)
    gray = cv2.GaussianBlur(raw_gray, (3, 3), 0)
    hsv = cv2.cvtColor(background, cv2.COLOR_BGR2HSV)
    candidates: list[GapCandidate] = []
    alpha_binary = np.where(alpha > 8, 255, 0).astype(np.uint8)
    alpha_contours, _ = cv2.findContours(
        alpha_binary,
        cv2.RETR_EXTERNAL,
        cv2.CHAIN_APPROX_SIMPLE,
    )
    alpha_contour = (
        max(alpha_contours, key=cv2.contourArea)
        if alpha_contours
        else None
    )

    # 路径一：把 alpha 轮廓裁到真实纵向范围，在背景同一 y 带上横向匹配。
    margin_y = 4
    crop_top = max(0, alpha_box.top - margin_y)
    crop_bottom = min(background_height, alpha_box.bottom + margin_y)
    template_alpha = alpha[crop_top:crop_bottom, :]
    template_edge = cv2.Canny(template_alpha, 20, 80)
    if int(np.count_nonzero(template_edge)) > 8:
        background_crop = gray[crop_top:crop_bottom, :]
        for low, high in ((10, 35), (20, 60), (35, 105), (60, 180)):
            background_edge = cv2.Canny(background_crop, low, high)
            result = cv2.matchTemplate(
                background_edge,
                template_edge,
                cv2.TM_CCOEFF_NORMED,
            ).reshape(-1)
            # 每个阈值保留互相至少相距 8px 的三个峰，交由后续支持度合并。
            chosen: list[int] = []
            for index in np.argsort(result)[::-1]:
                position = int(index)
                if position < 8 or position > background_width - shadow_width:
                    continue
                if any(abs(position - prior) < 8 for prior in chosen):
                    continue
                correlation = float(result[position])
                candidates.append(
                    GapCandidate(
                        canvas_left=position,
                        score=max(0.0, min(1.0, (correlation + 1.0) / 2.0)),
                        method=f"edge-template-{low}-{high}",
                    )
                )
                chosen.append(position)
                if len(chosen) >= 3:
                    break

    # 路径二：高亮低饱和填充区域。雪景等背景容易让 Canny 只返回缺口右边缘，
    # 而填充 mask 会恢复完整拼图轮廓；再用 shadow alpha 轮廓做形状约束。
    for brightness, saturation in (
        (205, 70),
        (220, 70),
        (235, 70),
    ):
        bright_mask = np.where(
            (gray >= brightness) & (hsv[:, :, 1] <= saturation),
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
            if x < 8 or x >= background_width - 8:
                continue
            width_ratio = width / expected_width
            height_ratio = height / expected_height
            if not 0.55 <= width_ratio <= 1.55:
                continue
            if not 0.55 <= height_ratio <= 1.55:
                continue
            center_y = y + height / 2.0
            if abs(center_y - expected_center_y) > max(
                32.0,
                expected_height,
            ):
                continue
            touches_border = (
                y <= 0
                or y + height >= background_height
                or x + width >= background_width
            )
            if touches_border:
                continue

            size_score = math.exp(
                -abs(math.log(max(width_ratio, 1e-9)))
                - abs(math.log(max(height_ratio, 1e-9)))
            )
            y_score = math.exp(
                -abs(center_y - expected_center_y)
                / max(expected_height * 0.35, 8.0)
            )
            aspect_score = math.exp(
                -abs(
                    math.log(
                        max(
                            (width / max(height, 1))
                            / (
                                expected_width
                                / max(expected_height, 1)
                            ),
                            1e-9,
                        )
                    )
                )
            )
            contour_area = float(cv2.contourArea(contour))
            fill_score = min(
                1.0,
                contour_area / max(width * height * 0.45, 1.0),
            )
            shape_score = 0.0
            if alpha_contour is not None and contour_area > 0:
                distance = float(
                    cv2.matchShapes(
                        alpha_contour,
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
            canvas_left = int(round(x - alpha_box.left))
            if 0 <= canvas_left <= background_width - shadow_width:
                candidates.append(
                    GapCandidate(
                        canvas_left=canvas_left,
                        score=max(0.0, min(1.0, score)),
                        method=(
                            f"bright-mask-{brightness}-{saturation}"
                        ),
                        bbox=BoundingBox(
                            x,
                            y,
                            x + width,
                            y + height,
                        ),
                    )
                )

    # 路径三：多阈值 Canny 找拼图轮廓。饱和度只作为弱特征，避免雪景白底主导。
    for low, high in ((10, 35), (20, 60), (35, 105), (60, 180), (90, 230)):
        edges = cv2.Canny(gray, low, high)
        contours, _ = cv2.findContours(
            edges,
            cv2.RETR_LIST,
            cv2.CHAIN_APPROX_SIMPLE,
        )
        for contour in contours:
            x, y, width, height = cv2.boundingRect(contour)
            if x < 8 or x >= background_width - 8:
                continue
            width_ratio = width / expected_width
            height_ratio = height / expected_height
            if not 0.55 <= width_ratio <= 1.55:
                continue
            if not 0.55 <= height_ratio <= 1.55:
                continue
            center_y = y + height / 2.0
            if abs(center_y - expected_center_y) > max(32.0, expected_height):
                continue

            size_score = math.exp(
                -abs(math.log(max(width_ratio, 1e-9)))
                - abs(math.log(max(height_ratio, 1e-9)))
            )
            y_score = math.exp(
                -abs(center_y - expected_center_y)
                / max(expected_height * 0.35, 8.0)
            )
            aspect_score = math.exp(
                -abs(
                    math.log(
                        max(
                            (width / max(height, 1))
                            / (expected_width / max(expected_height, 1)),
                            1e-9,
                        )
                    )
                )
            )
            region = hsv[y : y + height, x : x + width]
            mean_saturation = (
                float(region[:, :, 1].mean()) if region.size else 255.0
            )
            low_saturation_score = 1.0 - mean_saturation / 255.0
            contour_area = float(cv2.contourArea(contour))
            fill_score = min(
                1.0,
                contour_area / max(width * height * 0.35, 1.0),
            )
            score = (
                0.38 * size_score
                + 0.32 * y_score
                + 0.14 * aspect_score
                + 0.10 * low_saturation_score
                + 0.06 * fill_score
            )
            if (
                y <= 0
                or y + height >= background_height
                or x + width >= background_width
            ):
                score *= 0.40
            canvas_left = int(round(x - alpha_box.left))
            if 0 <= canvas_left <= background_width - shadow_width:
                candidates.append(
                    GapCandidate(
                        canvas_left=canvas_left,
                        score=max(0.0, min(1.0, score)),
                        method=f"contour-{low}-{high}",
                        bbox=BoundingBox(x, y, x + width, y + height),
                    )
                )

    if not candidates:
        raise ValueError("OpenCV 未找到可用缺口候选")

    # 同一阈值的内外轮廓可能给出完全相同的 x；它们不是独立证据，先去重，
    # 避免仅靠一个阈值的嵌套 contour 人为抬高支持度。
    unique_candidates: dict[tuple[str, int], GapCandidate] = {}
    for candidate in candidates:
        key = (candidate.method, candidate.canvas_left)
        prior = unique_candidates.get(key)
        if prior is None or candidate.score > prior.score:
            unique_candidates[key] = candidate
    candidates = list(unique_candidates.values())

    # 相差不超过 3px 的候选视为同一峰；多阈值、多方法重复出现会提高可信度。
    groups: list[list[GapCandidate]] = []
    for candidate in sorted(candidates, key=lambda item: item.canvas_left):
        for group in groups:
            center = sum(item.canvas_left for item in group) / len(group)
            if abs(candidate.canvas_left - center) <= 3:
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
        group_score = min(1.0, best_score + support_bonus + method_bonus)
        # 阈值变化会让同一轮廓的边缘系统性外扩/内缩 1–3px；坐标做加权均值
        # 反而会制造一个任何检测器都没有给出的中间像素。组内采用最高分候选，
        # 多阈值/多方法只用于提高 group_score，不用于挪动最终坐标。
        representative_left = max(
            group,
            key=lambda item: item.score,
        ).canvas_left
        ranked.append((group_score, representative_left, group))
    ranked.sort(key=lambda item: item[0], reverse=True)

    best_score, best_left, best_group = ranked[0]
    excluded_groups = {id(best_group)}
    # 旧路径只在少数候选点上计算 Chamfer，候选生成器漏掉真实缺口时无法自救。
    # 这里对所有合法 canvas-left 做一次完整轮廓扫描；整图 Sobel 只计算一次，
    # 后续双峰和贴顶仲裁也复用同一批距离。
    maximum_left = background_width - shadow_width
    alignment_by_left = _edge_alignment_chamfer_distances(
        raw_gray,
        alpha > 8,
        alpha_box,
        range(maximum_left + 1),
    )
    # 复杂纹理可能让 full-width alpha 模板只贴中缺口的一条边，产生相隔约
    # 一个拼图宽度的两个峰。不能固定取左峰：已有样本同时出现“左峰为真”
    # 和“右峰为真”。用完整 alpha 轮廓的双向 Chamfer 距离进行局部仲裁。
    if all(
        item.method.startswith("edge-template-")
        for item in best_group
    ):
        paired: tuple[float, int, list[GapCandidate]] | None = None
        for candidate_group in ranked[1:]:
            candidate_score, candidate_left, group = candidate_group
            separation = abs(best_left - candidate_left)
            if not (
                expected_width * 0.65
                <= separation
                <= expected_width * 1.15
            ):
                continue
            if candidate_score < best_score - 0.12:
                continue
            if not all(
                item.method.startswith("edge-template-")
                for item in group
            ):
                continue
            paired = candidate_group
            break
        if paired is not None:
            paired_score, paired_left, paired_group = paired
            alignment = (
                (
                    alignment_by_left.get(best_left, math.inf),
                    best_left,
                ),
                (
                    alignment_by_left.get(paired_left, math.inf),
                    paired_left,
                ),
            )
            accepted = [
                (distance, left)
                for distance, left in alignment
                if distance <= 3.0
            ]
            if not accepted:
                ordered_alignment = sorted(alignment)
                best_distance, fallback_left = ordered_alignment[0]
                runner_up_distance, _ = ordered_alignment[1]
                if (
                    math.isfinite(best_distance)
                    and math.isfinite(runner_up_distance)
                    and best_distance <= 5.0
                    and runner_up_distance - best_distance >= 1.5
                    and runner_up_distance / best_distance >= 1.30
                ):
                    accepted = [(best_distance, fallback_left)]
            if not accepted:
                raise ValueError(
                    "OpenCV edge 双峰均未通过完整轮廓 Chamfer 校验"
                )
            _, best_left = min(accepted)
            best_score = min(
                1.0,
                (best_score + paired_score) / 2.0 + 0.06,
            )
            excluded_groups.add(id(paired_group))
            candidates.append(
                GapCandidate(
                    canvas_left=best_left,
                    score=best_score,
                    method="edge-pair-chamfer",
                )
            )

    # alpha 形状触及图片上边界时，Canny template 的边界会被裁掉，相关峰
    # 可能在同一局部组内偏移数像素。本轮若另一个独立 contour 方法已在该组
    # 给出不同离散坐标，则只在这些现有候选之间用完整轮廓 Chamfer 仲裁；
    # 不扫描或制造新坐标。改善不足时保留原峰，避免改写既有非边界样本。
    if (
        alpha_box.top == 0
        and any(
            item.method.startswith("edge-template-")
            and item.canvas_left == best_left
            for item in best_group
        )
        and any(
            not item.method.startswith("edge-template-")
            for item in best_group
        )
    ):
        non_edge_candidates = {
            item.canvas_left
            for item in best_group
            if not item.method.startswith("edge-template-")
        }
        local_candidates = sorted(
            {best_left, *non_edge_candidates}
        )
        local_alignment = [
            (
                alignment_by_left.get(candidate_left, math.inf),
                candidate_left,
            )
            for candidate_left in local_candidates
        ]
        original_distance = next(
            distance
            for distance, candidate_left in local_alignment
            if candidate_left == best_left
        )
        refined_distance, refined_left = min(
            local_alignment,
            key=lambda item: (
                item[0],
                abs(item[1] - best_left),
                item[1],
            ),
        )
        if (
            refined_left != best_left
            and refined_left in non_edge_candidates
            and math.isfinite(original_distance)
            and math.isfinite(refined_distance)
            and refined_distance <= 3.0
            and original_distance - refined_distance >= 0.15
        ):
            best_left = refined_left
            candidates.append(
                GapCandidate(
                    canvas_left=best_left,
                    score=best_score,
                    method="edge-local-chamfer",
                )
            )

    # 当旧候选与全局完整轮廓相差至少 4px 时，才允许全轴扫描补回“候选生成器
    # 完全漏检”的坐标。小于 4px 的离散差异继续保留现有多路候选结果，避免
    # Chamfer 的局部平台把已验证锚点系统性挪动 1–3px。
    finite_alignment = sorted(
        (distance, left)
        for left, distance in alignment_by_left.items()
        if left >= 8 and math.isfinite(distance)
    )
    if finite_alignment:
        global_distance, global_left = finite_alignment[0]
        runner_separation = max(12, round(expected_width * 0.55))
        runner_distance = next(
            (
                distance
                for distance, left in finite_alignment[1:]
                if abs(left - global_left) >= runner_separation
            ),
            math.inf,
        )
        if (
            abs(global_left - best_left) >= 4
            and global_distance <= 3.0
            and runner_distance - global_distance >= 0.70
        ):
            selected_left, selected_distance = (
                _snap_chamfer_plateau_to_edge_consensus(
                    candidates,
                    alignment_by_left,
                    global_left,
                    global_distance,
                )
            )
            if selected_distance > 3.0:
                selected_left = global_left
                selected_distance = global_distance
            absolute_score = math.exp(
                -max(0.0, selected_distance - 1.0) / 2.0
            )
            margin_score = (
                1.0
                if not math.isfinite(runner_distance)
                else 1.0
                - math.exp(
                    -max(0.0, runner_distance - global_distance)
                )
            )
            global_score = max(
                0.0,
                min(1.0, 0.70 * absolute_score + 0.30 * margin_score),
            )
            best_left = selected_left
            candidates.append(
                GapCandidate(
                    canvas_left=selected_left,
                    score=global_score,
                    method=(
                        "global-chamfer"
                        if selected_left == global_left
                        else "global-chamfer-edge-consensus"
                    ),
                )
            )

    second_score = max(
        (
            score
            for score, _, group in ranked
            if id(group) not in excluded_groups
        ),
        default=0.0,
    )
    confidence = max(
        0.0,
        min(1.0, 0.65 * best_score + 0.35 * (best_score - second_score)),
    )
    flattened = tuple(
        sorted(
            candidates,
            key=lambda item: (item.score, item.method),
            reverse=True,
        )[:20]
    )
    return GapEstimate(
        x_pos=int(best_left),
        confidence=confidence,
        alpha_bbox=alpha_box,
        candidates=flattened,
    )


def calibrate_mapping_ratio(*, image_width: float, dom_width: float) -> float:
    """由 PNG 原始宽度与 DOM/CSS 显示宽度计算坐标缩放比。

    定义为 ``mapping_ratio = image_width / dom_width``，所以页面滑动距离为
    ``puzzle_left / mapping_ratio``。
    """

    if (
        not math.isfinite(image_width)
        or not math.isfinite(dom_width)
        or image_width <= 0
        or dom_width <= 0
    ):
        raise ValueError("image_width 和 dom_width 必须为正数")
    return float(image_width) / float(dom_width)


def puzzle_left_to_slider_distance(puzzle_left: float, mapping_ratio: float) -> float:
    """通用线性路径：把原图缺口横坐标换算为 DOM/CSS 拼图横坐标。

    对没有额外运动曲线的滑块，这个值也就是手柄位移；当前已验证的动态 PE
    还需要把该 ``puzzle_x`` 代入 :func:`pe_slide_pos_from_puzzle_x`。
    """

    if not math.isfinite(puzzle_left) or puzzle_left < 0:
        raise ValueError("puzzle_left 不能为负数")
    if not math.isfinite(mapping_ratio) or mapping_ratio <= 0:
        raise ValueError("mapping_ratio 必须为正数")
    return float(puzzle_left) / float(mapping_ratio)


def pe_puzzle_x_from_slide_pos(slide_pos: float) -> float:
    """当前 PE 的正向运动式：由手柄位移 ``s`` 得到拼图 CSS 横坐标。"""

    if not math.isfinite(slide_pos) or slide_pos < 0:
        raise ValueError("slide_pos 不能为负数")
    return float(slide_pos) * (3.0 * float(slide_pos) + 65.0) / 845.0


def _math_round_nonnegative(value: float) -> int:
    """复刻非负坐标上的 JavaScript Math.round，避免 Python 银行家舍入。"""

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
    """反解当前 PE 的手柄位移，并限制在 DOM 可移动范围内。

    当前前端已验证的正向式为 ``puzzle_x = s*(3*s+65)/845``，其非负逆根为
    ``s = (-65 + sqrt(65**2 + 12*845*puzzle_x))/6``。最终按
    ``[0, rendered_width-handle_width]`` clamp；默认使用 JavaScript
    ``Math.round`` 的非负数语义取整。
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
    return _math_round_nonnegative(clamped) if rounded else clamped


def map_drag_x(
    *,
    natural_x: float,
    natural_width: float,
    rendered_width: float,
    handle_width: float,
) -> int:
    """把自然图片缺口位置映射为当前 PE 的最终整数手柄位移。"""

    ratio = calibrate_mapping_ratio(
        image_width=natural_width,
        dom_width=rendered_width,
    )
    target_x = puzzle_left_to_slider_distance(natural_x, ratio)
    return int(
        pe_slide_pos_from_puzzle_x(
            target_x,
            rendered_width=rendered_width,
            handle_width=handle_width,
            rounded=True,
        )
    )


def solve_slider(
    background: ImageSource,
    shadow: ImageSource | None = None,
    *,
    mapping_ratio: float | None = None,
    dom_width: float | None = None,
    handle_width: float = 0.0,
    mapping_mode: str = "pe_quadratic",
    position_mode: str = "auto",
    **kwargs: Any,
) -> SliderSolution:
    """完成缺口检测和 DOM 坐标换算。

    ``mapping_ratio`` 与 ``dom_width`` 二选一。传入 ``dom_width`` 时，从背景图片的
    natural width 自动校准比例；二者都不传时比例为 1（原图坐标）。

    默认 ``position_mode="auto"`` 会在 shadow 为等高窄画布时使用 alpha 模板
    匹配，否则退回亮色连通域。默认 ``mapping_mode="pe_quadratic"`` 对应当前动态 PE：先线性换算出 DOM 中的
    ``puzzle_x``，再反解二次运动式得到手柄位移。旧版本或通用滑块可显式传入
    ``mapping_mode="linear"``，此时 ``slider_distance == puzzle_left/mapping_ratio``。
    """

    width, _, _ = _rgba_pixels(background)
    if mapping_ratio is not None and dom_width is not None:
        raise ValueError("mapping_ratio 与 dom_width 只能传一个")
    if dom_width is not None:
        ratio = calibrate_mapping_ratio(image_width=width, dom_width=dom_width)
    else:
        ratio = 1.0 if mapping_ratio is None else float(mapping_ratio)
    if ratio <= 0:
        raise ValueError("mapping_ratio 必须为正数")
    if mapping_mode not in {"pe_quadratic", "linear"}:
        raise ValueError("mapping_mode 只能是 'pe_quadratic' 或 'linear'")
    if position_mode not in {"auto", "alpha_template", "component"}:
        raise ValueError(
            "position_mode 只能是 'auto'、'alpha_template' 或 'component'"
        )

    bbox, shadow_bbox = estimate_puzzle_bbox(background, shadow, **kwargs)
    effective_position_mode = "component"
    puzzle_left = float(bbox.left)
    if shadow is not None and position_mode != "component":
        background_width, background_height, _ = _rgba_pixels(background)
        shadow_width, shadow_height, _ = _rgba_pixels(shadow)
        can_use_template = (
            shadow_height == background_height
            and 0 < shadow_width <= background_width
        )
        if position_mode == "alpha_template" and not can_use_template:
            raise ValueError(
                "alpha_template 要求 shadow 为与 background 等高的窄画布"
            )
        if can_use_template:
            puzzle_left = estimate_puzzle_canvas_left(
                background,
                shadow,
                brightness_threshold=float(
                    kwargs.get("brightness_threshold", 200.0)
                ),
                saturation_threshold=float(
                    kwargs.get("saturation_threshold", 0.22)
                ),
                alpha_threshold=int(kwargs.get("alpha_threshold", 8)),
            )
            effective_position_mode = "alpha_template"
    puzzle_x = puzzle_left_to_slider_distance(puzzle_left, ratio)
    rendered_width = width / ratio
    if mapping_mode == "pe_quadratic":
        distance = float(
            pe_slide_pos_from_puzzle_x(
                puzzle_x,
                rendered_width=rendered_width,
                handle_width=handle_width,
                rounded=True,
            )
        )
    else:
        # 线性模式保留最初的通用定义，不额外做 PE 曲线或手柄宽度修正。
        distance = puzzle_x

    return SliderSolution(
        puzzle_left=puzzle_left,
        mapping_ratio=ratio,
        puzzle_x=puzzle_x,
        slider_distance=distance,
        mapping_mode=mapping_mode,
        position_mode=effective_position_mode,
        background_bbox=bbox,
        shadow_bbox=shadow_bbox,
    )


__all__ = [
    "BoundingBox",
    "ConnectedComponent",
    "SliderSolution",
    "alpha_bbox",
    "bright_low_saturation_components",
    "calibrate_mapping_ratio",
    "estimate_puzzle_bbox",
    "estimate_puzzle_canvas_left",
    "estimate_puzzle_left",
    "map_drag_x",
    "pe_puzzle_x_from_slide_pos",
    "pe_slide_pos_from_puzzle_x",
    "puzzle_left_to_slider_distance",
    "solve_slider",
]
