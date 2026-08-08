package vision

import (
	"context"
	"errors"
	"math"
)

type boundarySample struct {
	y, innerX, outerX int
}

type frame struct {
	width, height, shadowWidth, maxCanvasLeft int
	bgr                                       []float64
	alpha                                     []uint8
	alphaBox                                  BoundingBox
	lightness, chroma, likelihood             []float64
	fill, ring                                []bool
	fillIndexes, ringIndexes                  []int
	boundary                                  []boundarySample
	template                                  []float64
	likeCurve, lightCurve, chromaCurve        []float64
}

func checkContext(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func buildFrame(ctx context.Context, pair decodedPair) (*frame, error) {
	f := &frame{
		width:         pair.width,
		height:        pair.height,
		shadowWidth:   pair.shadowWidth,
		maxCanvasLeft: pair.width - pair.shadowWidth,
		bgr:           pair.bgr,
		alpha:         pair.alpha,
		lightness:     make([]float64, pair.width*pair.height),
		chroma:        make([]float64, pair.width*pair.height),
		likelihood:    make([]float64, pair.width*pair.height),
	}

	left, top, right, bottom := pair.shadowWidth, pair.height, -1, -1
	for y := 0; y < pair.height; y++ {
		if y&15 == 0 {
			if err := checkContext(ctx); err != nil {
				return nil, err
			}
		}
		for x := 0; x < pair.shadowWidth; x++ {
			if pair.alpha[y*pair.shadowWidth+x] > alphaThreshold {
				left, top = min(left, x), min(top, y)
				right, bottom = max(right, x), max(bottom, y)
			}
		}
	}
	if right < left || bottom < top {
		return nil, errors.New("shadow contains no puzzle shape")
	}
	f.alphaBox = BoundingBox{Left: left, Top: top, Right: right + 1, Bottom: bottom + 1}

	for y := 0; y < pair.height; y++ {
		if y&15 == 0 {
			if err := checkContext(ctx); err != nil {
				return nil, err
			}
		}
		for x := 0; x < pair.width; x++ {
			pixel := y*pair.width + x
			base := pixel * 3
			b, g, r := pair.bgr[base], pair.bgr[base+1], pair.bgr[base+2]
			light, a, labB := opencvCompatibleLab(r, g, b)
			chroma := math.Hypot(a-128, labB-128)
			f.lightness[pixel], f.chroma[pixel] = light, chroma

			var rangeError float64
			for channel := 0; channel < 3; channel++ {
				value := pair.bgr[base+channel]
				below := max(fillLow-value, 0)
				above := max(value-fillHigh, 0)
				rangeError += below*below + above*above
			}
			rangeScore := math.Exp(-(rangeError / 3) / (2 * 12 * 12))
			desaturated := math.Exp(-(chroma * chroma) / (2 * chromaSigma * chromaSigma))
			f.likelihood[pixel] = rangeScore * desaturated
		}
	}

	solid := make([]bool, pair.shadowWidth*pair.height)
	for i, alpha := range pair.alpha {
		solid[i] = alpha > shapeThreshold
	}
	insideDistance := distanceTransformNonZero(solid, pair.shadowWidth, pair.height)
	inverse := make([]bool, len(solid))
	for i := range solid {
		inverse[i] = !solid[i]
	}
	outsideDistance := distanceTransformNonZero(inverse, pair.shadowWidth, pair.height)
	distance := make([]float64, len(solid))
	f.fill, f.ring = make([]bool, len(solid)), make([]bool, len(solid))
	for i := range distance {
		distance[i] = insideDistance[i] - outsideDistance[i]
		f.fill[i] = distance[i] >= inset+1
		f.ring[i] = distance[i] < -1 && distance[i] > -7
		if f.fill[i] {
			f.fillIndexes = append(f.fillIndexes, i)
		}
		if f.ring[i] {
			f.ringIndexes = append(f.ringIndexes, i)
		}
	}
	if len(f.fillIndexes) == 0 || len(f.ringIndexes) == 0 {
		return nil, errors.New("shadow puzzle shape is too small")
	}

	f.prepareBoundary(distance)
	f.template = buildTemplate(distance)
	return f, nil
}

// opencvCompatibleLab 使用 OpenCV 8-bit Lab 的取值范围。OpenCV 内部使用定点
// LUT，纯 Go 版使用 D65/sRGB 公式；边界像素可有约 1 个量化单位差异，由
// Python oracle 阶段 fixture 锁定容差。
func opencvCompatibleLab(red, green, blue float64) (lightness, a, b float64) {
	linear := func(value float64) float64 {
		value /= 255
		if value <= 0.04045 {
			return value / 12.92
		}
		return math.Pow((value+0.055)/1.055, 2.4)
	}
	r, g, bl := linear(red), linear(green), linear(blue)
	x := (0.4124564*r + 0.3575761*g + 0.1804375*bl) / 0.95047
	y := 0.2126729*r + 0.7151522*g + 0.0721750*bl
	z := (0.0193339*r + 0.1191920*g + 0.9503041*bl) / 1.08883
	f := func(value float64) float64 {
		if value > 216.0/24389.0 {
			return math.Cbrt(value)
		}
		return (24389.0/27.0*value + 16) / 116
	}
	fx, fy, fz := f(x), f(y), f(z)
	lStar := 116*fy - 16
	aStar := 500 * (fx - fy)
	bStar := 200 * (fy - fz)
	return clamp255(math.Round(lStar * 255 / 100)), clamp255(math.Round(aStar + 128)), clamp255(math.Round(bStar + 128))
}

func clamp255(value float64) float64 { return min(max(value, 0), 255) }

func (f *frame) prepareBoundary(distance []float64) {
	for y := 0; y < f.height; y++ {
		left, right, count := f.shadowWidth, -1, 0
		for x := 0; x < f.shadowWidth; x++ {
			if distance[y*f.shadowWidth+x] >= inset {
				left, right, count = min(left, x), max(right, x), count+1
			}
		}
		if count < 8 {
			continue
		}
		f.boundary = append(f.boundary,
			boundarySample{y: y, innerX: left + sampleIn, outerX: left - sampleOut},
			boundarySample{y: y, innerX: right - sampleIn, outerX: right + sampleOut},
		)
	}
}

func buildTemplate(distance []float64) []float64 {
	template := make([]float64, len(distance))
	inside, outside := make([]float64, len(distance)), make([]float64, len(distance))
	var insideSum, outsideSum float64
	for i, value := range distance {
		inside[i] = 0.5 * (1 + math.Tanh(0.5*(value-inset)/edgeSoft))
		outside[i] = 0.5 * (1 + math.Tanh(0.5*(-(value + outer*0.5))/edgeSoft))
		insideSum += inside[i]
		outsideSum += outside[i]
	}
	insideSum, outsideSum = max(insideSum, 1), max(outsideSum, 1)
	for i := range template {
		template[i] = inside[i]/insideSum - outside[i]/outsideSum
	}
	return template
}

// distanceTransformNonZero 复刻 cv2.distanceTransform(mask, DIST_L2, 5) 的 5x5
// Chamfer 权重：非零像素到最近零像素的距离。
func distanceTransformNonZero(nonZero []bool, width, height int) []float64 {
	distance := make([]float64, len(nonZero))
	for i, value := range nonZero {
		if value {
			distance[i] = math.Inf(1)
		}
	}
	chamferTransform(distance, width, height)
	return distance
}

func distanceToFeatureInto(feature []bool, width, height int, distance []float64) {
	for i, present := range feature {
		if present {
			distance[i] = 0
		} else {
			distance[i] = math.Inf(1)
		}
	}
	chamferTransform(distance, width, height)
}

func chamferTransform(distance []float64, width, height int) {
	const (
		straight = 1.0
		diagonal = 1.4
		knight   = 2.1969
	)
	relax := func(x, y, nx, ny int, cost float64) {
		if nx < 0 || nx >= width || ny < 0 || ny >= height {
			return
		}
		index, neighbor := y*width+x, ny*width+nx
		distance[index] = min(distance[index], distance[neighbor]+cost)
	}
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			relax(x, y, x-1, y, straight)
			relax(x, y, x-1, y-1, diagonal)
			relax(x, y, x, y-1, straight)
			relax(x, y, x+1, y-1, diagonal)
			relax(x, y, x-2, y-1, knight)
			relax(x, y, x+2, y-1, knight)
			relax(x, y, x-1, y-2, knight)
			relax(x, y, x+1, y-2, knight)
		}
	}
	for y := height - 1; y >= 0; y-- {
		for x := width - 1; x >= 0; x-- {
			relax(x, y, x+1, y, straight)
			relax(x, y, x-1, y+1, diagonal)
			relax(x, y, x, y+1, straight)
			relax(x, y, x+1, y+1, diagonal)
			relax(x, y, x-2, y+1, knight)
			relax(x, y, x+2, y+1, knight)
			relax(x, y, x-1, y+2, knight)
			relax(x, y, x+1, y+2, knight)
		}
	}
}
