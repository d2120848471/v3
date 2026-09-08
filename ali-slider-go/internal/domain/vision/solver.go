package vision

import (
	"context"
	"fmt"
	"math"
	"sort"
)

type curve struct {
	name   string
	values []float64
}

type positionScore struct {
	total, quality, step float64
}

type rankedPosition struct {
	left int
	positionScore
}

// Solve 在内存中解码两张 PNG 并返回缺口坐标。函数无共享可变状态，
// 同一组输入可安全地被多个 goroutine 并发调用。
func Solve(ctx context.Context, backgroundPNG, shadowPNG []byte, limits Limits) (Estimate, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := checkContext(ctx); err != nil {
		return Estimate{}, err
	}
	pair, err := decodePair(backgroundPNG, shadowPNG, limits)
	if err != nil {
		return Estimate{}, err
	}
	if err := checkContext(ctx); err != nil {
		return Estimate{}, err
	}
	f, err := buildFrame(ctx, pair)
	if err != nil {
		return Estimate{}, err
	}
	if err := f.buildCurves(ctx); err != nil {
		return Estimate{}, err
	}
	estimate, err := f.solve(ctx)
	if err != nil {
		return Estimate{}, err
	}
	estimate.SlidePos, err = SlidePosFromPuzzleX(float64(estimate.XPos), defaultRenderedWidth, defaultHandleWidth)
	if err != nil {
		return Estimate{}, fmt.Errorf("convert xPos to slidePos: %w", err)
	}
	return estimate, nil
}

func (f *frame) buildCurves(ctx context.Context) error {
	count := float64(len(f.template))
	var templateSum, templateSquare float64
	for _, value := range f.template {
		templateSum += value
		templateSquare += value * value
	}
	templateVariance := max(templateSquare-templateSum*templateSum/count, 0)
	curveLength := f.maxCanvasLeft + 1
	f.likeCurve = make([]float64, curveLength)
	f.lightCurve = make([]float64, curveLength)
	f.chromaCurve = make([]float64, curveLength)

	for left := 0; left < curveLength; left++ {
		if left&7 == 0 {
			if err := checkContext(ctx); err != nil {
				return err
			}
		}
		var likeProduct float64
		var lightSum, lightSquare, lightProduct float64
		var chromaSum, chromaSquare, chromaProduct float64
		for y := 0; y < f.height; y++ {
			backgroundBase := y*f.width + left
			templateBase := y * f.shadowWidth
			for x := 0; x < f.shadowWidth; x++ {
				backgroundIndex := backgroundBase + x
				templateValue := f.template[templateBase+x]
				likeProduct += f.likelihood[backgroundIndex] * templateValue
				light := f.lightness[backgroundIndex]
				negativeChroma := -f.chroma[backgroundIndex]
				lightSum += light
				lightSquare += light * light
				lightProduct += light * templateValue
				chromaSum += negativeChroma
				chromaSquare += negativeChroma * negativeChroma
				chromaProduct += negativeChroma * templateValue
			}
		}
		f.likeCurve[left] = likeProduct
		f.lightCurve[left] = normalizedCorrelation(lightSum, lightSquare, lightProduct, templateSum, templateVariance, count)
		f.chromaCurve[left] = normalizedCorrelation(chromaSum, chromaSquare, chromaProduct, templateSum, templateVariance, count)
	}
	return nil
}

// normalizedCorrelation 等价于 TM_CCOEFF_NORMED 的数学定义。OpenCV 对接近零
// 方差另有定点容差分支；此处统一返回 0，避免灰阶常量图产生 NaN。
func normalizedCorrelation(valueSum, valueSquare, product, templateSum, templateVariance, count float64) float64 {
	valueVariance := max(valueSquare-valueSum*valueSum/count, 0)
	denominator := math.Sqrt(valueVariance * templateVariance)
	if denominator <= 1e-12 {
		return 0
	}
	value := (product - valueSum*templateSum/count) / denominator
	return min(max(value, -1), 1)
}

func (f *frame) candidatePositions() ([]int, map[int]string) {
	origin := make(map[int]string)
	curves := []curve{{"like", f.likeCurve}, {"lncc", f.lightCurve}, {"cncc", f.chromaCurve}}
	for _, current := range curves {
		indexes := make([]int, len(current.values))
		for i := range indexes {
			indexes[i] = i
		}
		sort.SliceStable(indexes, func(i, j int) bool {
			left, right := current.values[indexes[i]], current.values[indexes[j]]
			if math.IsNaN(left) {
				return false
			}
			if math.IsNaN(right) {
				return true
			}
			if left == right {
				return indexes[i] < indexes[j]
			}
			return left > right
		})
		picked := make([]int, 0, peaksPerCurve)
		for _, index := range indexes {
			separated := true
			for _, prior := range picked {
				if absInt(index-prior) < peakSeparation {
					separated = false
					break
				}
			}
			if !separated {
				continue
			}
			picked = append(picked, index)
			if _, exists := origin[index]; !exists {
				origin[index] = current.name
			}
			if len(picked) == peaksPerCurve {
				break
			}
		}
	}
	expanded := make([]bool, f.maxCanvasLeft+1)
	for position := range origin {
		for delta := -probeRadius; delta <= probeRadius; delta++ {
			expanded[min(max(position+delta, 0), f.maxCanvasLeft)] = true
		}
	}
	positions := make([]int, 0, len(origin)*(probeRadius*2+1))
	for position, include := range expanded {
		if include {
			positions = append(positions, position)
			if _, exists := origin[position]; !exists {
				origin[position] = "probe"
			}
		}
	}
	return positions, origin
}

func (f *frame) verify(canvasLeft int) (quality, step float64) {
	var inRangeCount int
	var lightSum, lightSquare, fillLikelihood float64
	for _, relative := range f.fillIndexes {
		y, x := relative/f.shadowWidth, relative%f.shadowWidth
		backgroundIndex := y*f.width + canvasLeft + x
		base := backgroundIndex * 3
		valid := true
		for channel := 0; channel < 3; channel++ {
			value := f.bgr[base+channel]
			if value < fillLow-fillSlack || value > fillHigh+fillSlack {
				valid = false
			}
		}
		if valid {
			inRangeCount++
		}
		light := f.lightness[backgroundIndex]
		lightSum += light
		lightSquare += light * light
		fillLikelihood += f.likelihood[backgroundIndex]
	}
	fillCount := float64(len(f.fillIndexes))
	inRange := float64(inRangeCount) / fillCount
	variance := max(lightSquare/fillCount-(lightSum/fillCount)*(lightSum/fillCount), 0)
	flat := math.Exp(-variance / (2 * fillStdSigma * fillStdSigma))
	fillLikelihood /= fillCount
	var ringLikelihood float64
	for _, relative := range f.ringIndexes {
		y, x := relative/f.shadowWidth, relative%f.shadowWidth
		ringLikelihood += f.likelihood[y*f.width+canvasLeft+x]
	}
	ringLikelihood /= float64(len(f.ringIndexes))
	separation := min(max(fillLikelihood-ringLikelihood, 0), 1)
	step, blend := f.boundaryScores(canvasLeft)
	evidence := 0.28*step + 0.24*blend + 0.30*separation + 0.18*inRange
	gate := math.Pow(inRange, 1.2) * (0.55 + 0.45*flat)
	return evidence * (0.35 + 0.65*gate), step
}

func (f *frame) boundaryScores(canvasLeft int) (step, blend float64) {
	deltas := make([]float64, 0, len(f.boundary))
	blendMatches := 0
	for _, sample := range f.boundary {
		innerX, outerX := sample.innerX+canvasLeft, sample.outerX+canvasLeft
		if innerX < 0 || innerX >= f.width || outerX < 0 || outerX >= f.width {
			continue
		}
		innerIndex, outerIndex := sample.y*f.width+innerX, sample.y*f.width+outerX
		deltas = append(deltas, f.lightness[innerIndex]-f.lightness[outerIndex])
		var squareError float64
		for channel := 0; channel < 3; channel++ {
			predicted := blendK*f.bgr[outerIndex*3+channel] + blendB
			difference := f.bgr[innerIndex*3+channel] - predicted
			squareError += difference * difference
		}
		if math.Sqrt(squareError/3) < blendTolerance {
			blendMatches++
		}
	}
	if len(deltas) < minimumBoundaryN {
		return 0, 0
	}
	sorted := append([]float64(nil), deltas...)
	sort.Float64s(sorted)
	median := sorted[len(sorted)/2]
	if len(sorted)%2 == 0 {
		median = (sorted[len(sorted)/2-1] + median) / 2
	}
	sign := 1.0
	if median < 0 {
		sign = -1
	}
	stepMatches := 0
	for _, delta := range deltas {
		if math.Abs(delta) >= stepMin && ((delta > 0 && sign > 0) || (delta < 0 && sign < 0)) {
			stepMatches++
		}
	}
	return float64(stepMatches) / float64(len(deltas)), float64(blendMatches) / float64(len(deltas))
}

func confidence(quality, margin, step float64) float64 {
	raw := 4.5*(quality-0.34) + 14*(margin-0.08) + 2.5*(step-0.55)
	return min(max(0.5*(1+math.Tanh(0.5*raw)), 0), 1)
}

func (f *frame) solve(ctx context.Context) (Estimate, error) {
	positions, origin := f.candidatePositions()
	if len(positions) == 0 {
		return Estimate{}, fmt.Errorf("no usable gap candidate")
	}
	likeLow, likeHigh := f.likeCurve[0], f.likeCurve[0]
	for _, value := range f.likeCurve[1:] {
		likeLow, likeHigh = min(likeLow, value), max(likeHigh, value)
	}
	likeSpan := max(likeHigh-likeLow, 1e-6)
	cache := make(map[int]positionScore, len(positions)+probeRadius*2+1)
	scorePosition := func(left int) positionScore {
		if cached, exists := cache[left]; exists {
			return cached
		}
		quality, step := f.verify(left)
		score := positionScore{
			total:   0.62*quality + 0.38*(f.likeCurve[left]-likeLow)/likeSpan,
			quality: quality,
			step:    step,
		}
		cache[left] = score
		return score
	}
	ranked := make([]rankedPosition, 0, len(positions))
	for index, left := range positions {
		if index&15 == 0 {
			if err := checkContext(ctx); err != nil {
				return Estimate{}, err
			}
		}
		ranked = append(ranked, rankedPosition{left: left, positionScore: scorePosition(left)})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].total == ranked[j].total {
			return ranked[i].left < ranked[j].left
		}
		return ranked[i].total > ranked[j].total
	})
	best := ranked[0]
	rivalTotal := 0.0
	for _, rival := range ranked[1:] {
		if absInt(rival.left-best.left) >= peakSeparation {
			rivalTotal = rival.total
			break
		}
	}
	resultConfidence := confidence(best.quality, best.total-rivalTotal, best.step)
	bestLeft := best.left

	var chamferCandidate *Candidate
	if fallback, ok, err := f.chamferFallback(ctx, positions, resultConfidence, scorePosition); err != nil {
		return Estimate{}, err
	} else if ok {
		bestLeft, resultConfidence = fallback.left, fallback.confidence
		candidate := Candidate{
			CanvasLeft: bestLeft,
			Score:      resultConfidence,
			Method:     "global-chamfer",
			Box:        f.candidateBox(bestLeft),
		}
		chamferCandidate = &candidate
	}

	distinct := make([]rankedPosition, 0, 8)
	for _, item := range ranked {
		separated := true
		for _, prior := range distinct {
			if absInt(item.left-prior.left) < peakSeparation {
				separated = false
				break
			}
		}
		if separated {
			distinct = append(distinct, item)
		}
		if len(distinct) == 8 {
			break
		}
	}
	candidates := make([]Candidate, 0, len(distinct)+1)
	if chamferCandidate != nil {
		candidates = append(candidates, *chamferCandidate)
	}
	for _, item := range distinct {
		if chamferCandidate != nil && absInt(item.left-chamferCandidate.CanvasLeft) < peakSeparation {
			continue
		}
		candidates = append(candidates, Candidate{
			CanvasLeft: item.left,
			Score:      min(max(item.total, 0), 1),
			Method:     origin[item.left],
			Box:        f.candidateBox(item.left),
		})
	}
	return Estimate{XPos: bestLeft, Confidence: resultConfidence, AlphaBox: f.alphaBox, Candidates: candidates}, nil
}

func (f *frame) candidateBox(left int) BoundingBox {
	return BoundingBox{Left: left, Top: f.alphaBox.Top, Right: left + f.shadowWidth, Bottom: f.alphaBox.Bottom}
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
