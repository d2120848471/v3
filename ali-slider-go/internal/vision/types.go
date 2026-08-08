// Package vision 使用纯 Go 实现滑块缺口识别。
//
// 所有求解状态均属于单次调用，包内没有可变全局状态，可并发使用。
package vision

const (
	alphaThreshold = 8
	shapeThreshold = 127

	blendK    = 0.224
	blendB    = 187.0
	fillLow   = 150.0
	fillHigh  = 244.0
	fillSlack = 6.0

	chromaSigma  = 16.0
	fillStdSigma = 18.0

	inset    = 2.0
	edgeSoft = 0.7
	outer    = 4.0

	sampleIn          = 3
	sampleOut         = 4
	stepMin           = 6.0
	blendTolerance    = 26.0
	minimumBoundaryN  = 8
	peakSeparation    = 10
	peaksPerCurve     = 4
	probeRadius       = 3
	chamferTrigger    = 0.45
	chamferMaxDist    = 3.0
	chamferMinMargin  = 0.70
	chamferLeftMargin = 8

	defaultRenderedWidth = 300.0
	defaultHandleWidth   = 40.0
)

// Limits 限制解码前的输入资源，防止压缩炸弹与过大图像占尽内存。
type Limits struct {
	MaxBytes     int
	MaxDimension int
	MaxPixels    int64
}

// DefaultLimits 返回与服务默认配置配套的保守边界。
func DefaultLimits() Limits {
	return Limits{MaxBytes: 8 << 20, MaxDimension: 16_384, MaxPixels: 16 << 20}
}

// BoundingBox 是右、下边界不包含在内的 alpha 包围盒。
type BoundingBox struct {
	Left   int `json:"left"`
	Top    int `json:"top"`
	Right  int `json:"right"`
	Bottom int `json:"bottom"`
}

func (b BoundingBox) width() int { return b.Right - b.Left }

// Candidate 记录一个互相分离的候选假设。
type Candidate struct {
	CanvasLeft int         `json:"xPos"`
	Score      float64     `json:"score"`
	Method     string      `json:"method"`
	Box        BoundingBox `json:"-"`
}

// Estimate 是一次缺口识别结果。XPos 是 shadow 画布在背景图中的左坐标。
type Estimate struct {
	XPos       int
	SlidePos   int
	Confidence float64
	AlphaBox   BoundingBox
	Candidates []Candidate
}
