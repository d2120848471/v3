package vision

import (
	"context"
	"math"
	"sort"
)

type point struct{ x, y int }

type chamferResult struct {
	left       int
	confidence float64
}

func (f *frame) chamferFallback(
	ctx context.Context,
	positions []int,
	currentConfidence float64,
	scorePosition func(int) positionScore,
) (chamferResult, bool, error) {
	if currentConfidence >= chamferTrigger {
		return chamferResult{}, false, nil
	}
	alphaMask := make([]bool, len(f.alpha))
	for index, alpha := range f.alpha {
		alphaMask[index] = alpha > alphaThreshold
	}
	alphaBoundary := make([]bool, len(alphaMask))
	alphaPoints := make([]point, 0)
	for y := 0; y < f.height; y++ {
		for x := 0; x < f.shadowWidth; x++ {
			index := y*f.shadowWidth + x
			if !alphaMask[index] || erodedCross(alphaMask, f.shadowWidth, f.height, x, y) {
				continue
			}
			alphaBoundary[index] = true
			alphaPoints = append(alphaPoints, point{x: x, y: y})
		}
	}
	if len(alphaPoints) == 0 {
		return chamferResult{}, false, nil
	}
	if err := checkContext(ctx); err != nil {
		return chamferResult{}, false, err
	}
	magnitude := f.sobelMagnitude()
	alignment, err := f.chamferAlignment(ctx, magnitude, alphaPoints)
	if err != nil {
		return chamferResult{}, false, err
	}

	type distancePosition struct {
		distance float64
		left     int
	}
	finite := make([]distancePosition, 0, len(alignment))
	for left, distance := range alignment {
		if left >= chamferLeftMargin && !math.IsInf(distance, 0) && !math.IsNaN(distance) {
			finite = append(finite, distancePosition{distance: distance, left: left})
		}
	}
	if len(finite) == 0 {
		return chamferResult{}, false, nil
	}
	sort.SliceStable(finite, func(i, j int) bool {
		if finite[i].distance == finite[j].distance {
			return finite[i].left < finite[j].left
		}
		return finite[i].distance < finite[j].distance
	})
	global := finite[0]
	runnerDistance := math.Inf(1)
	runnerSeparation := max(12, int(math.RoundToEven(float64(f.alphaBox.width())*0.55)))
	for _, candidate := range finite[1:] {
		if absInt(candidate.left-global.left) >= runnerSeparation {
			runnerDistance = candidate.distance
			break
		}
	}
	covered := false
	for _, position := range positions {
		if absInt(position-global.left) <= probeRadius {
			covered = true
			break
		}
	}
	if global.distance > chamferMaxDist || runnerDistance-global.distance < chamferMinMargin || !covered {
		return chamferResult{}, false, nil
	}

	selectedLeft := max(0, global.left-probeRadius)
	selectedScore := scorePosition(selectedLeft).total
	for left := selectedLeft + 1; left <= min(f.maxCanvasLeft, global.left+probeRadius); left++ {
		score := scorePosition(left).total
		if score > selectedScore || (score == selectedScore && (absInt(left-global.left) < absInt(selectedLeft-global.left) || (absInt(left-global.left) == absInt(selectedLeft-global.left) && left < selectedLeft))) {
			selectedLeft, selectedScore = left, score
		}
	}
	selectedDistance := alignment[selectedLeft]
	selectedMargin := runnerDistance - selectedDistance
	if selectedDistance > chamferMaxDist || selectedMargin < chamferMinMargin {
		return chamferResult{}, false, nil
	}
	absoluteScore := math.Exp(-max(0, selectedDistance-1) / 2)
	marginScore := 1.0
	if !math.IsInf(runnerDistance, 1) {
		marginScore = 1 - math.Exp(-max(0, selectedMargin))
	}
	fallbackConfidence := min(max(0.70*absoluteScore+0.30*marginScore, 0), 1)
	if fallbackConfidence < chamferTrigger {
		return chamferResult{}, false, nil
	}
	return chamferResult{left: selectedLeft, confidence: fallbackConfidence}, true, nil
}

// chamferAlignment 计算每个横向位置的双向轮廓距离。
func (f *frame) chamferAlignment(ctx context.Context, magnitude []float64, alphaPoints []point) ([]float64, error) {
	alignment := make([]float64, f.maxCanvasLeft+1)
	for i := range alignment {
		alignment[i] = math.Inf(1)
	}
	maxROI := (f.alphaBox.width() + 10) * (f.alphaBox.Bottom - f.alphaBox.Top + 10)
	quantileScratch := make([]float64, 0, maxROI)
	backgroundScratch := make([]bool, maxROI)
	alphaScratch := make([]bool, maxROI)
	distanceBackgroundScratch := make([]float64, maxROI)
	distanceAlphaScratch := make([]float64, maxROI)
	type alphaGeometry struct {
		width, height, offsetX, offsetY int
	}
	var previousAlphaGeometry alphaGeometry
	alphaCount := 0
	for left := 0; left <= f.maxCanvasLeft; left++ {
		if left&7 == 0 {
			if err := checkContext(ctx); err != nil {
				return nil, err
			}
		}
		roiLeft := max(0, left+f.alphaBox.Left-5)
		roiRight := min(f.width, left+f.alphaBox.Right+5)
		roiTop := max(0, f.alphaBox.Top-5)
		roiBottom := min(f.height, f.alphaBox.Bottom+5)
		roiWidth, roiHeight := roiRight-roiLeft, roiBottom-roiTop
		if roiWidth <= 0 || roiHeight <= 0 {
			continue
		}
		quantileScratch = quantileScratch[:0]
		for y := roiTop; y < roiBottom; y++ {
			quantileScratch = append(quantileScratch, magnitude[y*f.width+roiLeft:y*f.width+roiRight]...)
		}
		threshold := max(120, quantile(quantileScratch, 0.75))
		pixelCount := roiWidth * roiHeight
		backgroundBoundary := backgroundScratch[:pixelCount]
		clear(backgroundBoundary)
		backgroundCount := 0
		for y := 0; y < roiHeight; y++ {
			for x := 0; x < roiWidth; x++ {
				if magnitude[(roiTop+y)*f.width+roiLeft+x] >= threshold {
					backgroundBoundary[y*roiWidth+x] = true
					backgroundCount++
				}
			}
		}
		if backgroundCount == 0 {
			continue
		}
		localAlpha := alphaScratch[:pixelCount]
		distanceToAlpha := distanceAlphaScratch[:pixelCount]
		geometry := alphaGeometry{width: roiWidth, height: roiHeight, offsetX: left - roiLeft, offsetY: -roiTop}
		// 同一轮扫描中，alpha 只由 ROI 尺寸和相对偏移决定；边缘裁剪会改变
		// 这些值，此时必须重建。背景边缘随横坐标变化，仍在每个位置重算。
		if geometry != previousAlphaGeometry {
			clear(localAlpha)
			alphaCount = 0
			for _, alpha := range alphaPoints {
				x, y := alpha.x+geometry.offsetX, alpha.y+geometry.offsetY
				if x >= 0 && x < roiWidth && y >= 0 && y < roiHeight {
					localAlpha[y*roiWidth+x] = true
					alphaCount++
				}
			}
			if alphaCount > 0 {
				distanceToFeatureInto(localAlpha, roiWidth, roiHeight, distanceToAlpha)
			}
			previousAlphaGeometry = geometry
		}
		if alphaCount == 0 {
			continue
		}
		distanceToBackground := distanceBackgroundScratch[:pixelCount]
		distanceToFeatureInto(backgroundBoundary, roiWidth, roiHeight, distanceToBackground)
		var alphaToBackground, backgroundToAlpha float64
		for index, isAlpha := range localAlpha {
			if isAlpha {
				alphaToBackground += distanceToBackground[index]
			}
			if backgroundBoundary[index] {
				backgroundToAlpha += distanceToAlpha[index]
			}
		}
		alignment[left] = (alphaToBackground/float64(alphaCount) + backgroundToAlpha/float64(backgroundCount)) / 2
	}

	return alignment, nil
}

func erodedCross(mask []bool, width, height, x, y int) bool {
	if !mask[y*width+x] {
		return false
	}
	for _, delta := range [...]point{{-1, 0}, {1, 0}, {0, -1}, {0, 1}} {
		nx, ny := x+delta.x, y+delta.y
		// OpenCV erode 默认边界对二值图按高值处理。
		if nx >= 0 && nx < width && ny >= 0 && ny < height && !mask[ny*width+nx] {
			return false
		}
	}
	return true
}

func (f *frame) sobelMagnitude() []float64 {
	gray := make([]float64, f.width*f.height)
	for index := range gray {
		base := index * 3
		// OpenCV BGR2GRAY 的 8-bit 定点近似。
		gray[index] = float64((29*int(f.bgr[base]) + 150*int(f.bgr[base+1]) + 77*int(f.bgr[base+2]) + 128) >> 8)
	}
	magnitude := make([]float64, len(gray))
	reflect101 := func(value, size int) int {
		if size <= 1 {
			return 0
		}
		for value < 0 || value >= size {
			if value < 0 {
				value = -value
			}
			if value >= size {
				value = 2*size - value - 2
			}
		}
		return value
	}
	for y := 0; y < f.height; y++ {
		for x := 0; x < f.width; x++ {
			var gx, gy float64
			for ky := -1; ky <= 1; ky++ {
				for kx := -1; kx <= 1; kx++ {
					value := gray[reflect101(y+ky, f.height)*f.width+reflect101(x+kx, f.width)]
					gx += value * float64([3][3]int{{-1, 0, 1}, {-2, 0, 2}, {-1, 0, 1}}[ky+1][kx+1])
					gy += value * float64([3][3]int{{-1, -2, -1}, {0, 0, 0}, {1, 2, 1}}[ky+1][kx+1])
				}
			}
			magnitude[y*f.width+x] = math.Hypot(gx, gy)
		}
	}
	return magnitude
}

// quantile 复制 NumPy 默认 linear 插值；Quickselect 避免每个横坐标完整排序 ROI。
func quantile(values []float64, probability float64) float64 {
	if len(values) == 0 {
		return math.NaN()
	}
	position := probability * float64(len(values)-1)
	lower := int(math.Floor(position))
	upper := int(math.Ceil(position))
	lowerValue := quickSelect(values, lower)
	if lower == upper {
		return lowerValue
	}
	upperValue := quickSelect(values, upper)
	return lowerValue + (upperValue-lowerValue)*(position-float64(lower))
}

func quickSelect(values []float64, target int) float64 {
	left, right := 0, len(values)-1
	for {
		if left == right {
			return values[left]
		}
		pivot := values[(left+right)/2]
		low, scan, high := left, left, right
		for scan <= high {
			switch {
			case values[scan] < pivot:
				values[low], values[scan] = values[scan], values[low]
				low, scan = low+1, scan+1
			case values[scan] > pivot:
				values[scan], values[high] = values[high], values[scan]
				high--
			default:
				scan++
			}
		}
		switch {
		case target < low:
			right = low - 1
		case target > high:
			left = high + 1
		default:
			return values[target]
		}
	}
}
