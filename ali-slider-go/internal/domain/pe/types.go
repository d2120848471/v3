package pe

import (
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/protocol"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/track"
)

type Dimensions struct {
	RenderedWidth int
	HandleWidth   int
}

type Input struct {
	SceneID           string
	CertifyID         string
	Dimensions        Dimensions
	Track             []track.Event
	StaticPath        string
	ArgumentKey       string
	IncludeScreenInfo bool
	ExpectedXPos      *int
	InitBeginTimeMS   int64
	FirstTouchAgeMS   int
}

type GetterArgument struct {
	ValueType       string
	Length          int
	Value           string
	EqualsCertifyID bool
	EqualsSceneID   bool
}

type GetterPlan struct {
	Owner         string
	DerivedAtMS   float64
	ArgumentCount int
	Arguments     []GetterArgument
}

type InteractionEvent struct {
	Type      string
	X         float64
	Y         float64
	TimeStamp float64
	IsTrusted bool
}

type Result struct {
	Data                     string
	TrackEventCount          int
	PayloadKeys              []string
	TrackKeys                []string
	XPos                     int
	SlidePos                 int
	TrackStartTimeMS         int64
	VerifyTimeMS             int64
	TargetFirstTouchAgeMS    int
	FirstTouchAgeMS          float64
	TouchDurationMS          float64
	LastTouchToVerifyMS      float64
	PostInteractionDelayMS   float64
	DispatchLatenessMS       float64
	DeviceGetterPlans        []GetterPlan
	DataMousemoveEventCount  int
	PostGetterDataEventCount int
	InteractionEvents        []InteractionEvent
}

// RuntimeDimensions 是动态 PE 同轮看到的原图与渲染尺寸。
type RuntimeDimensions struct {
	ImageWidth    int
	ImageHeight   int
	PuzzleWidth   int
	PuzzleHeight  int
	RenderedWidth int
	HandleWidth   int
}

// RuntimeInput 只承载一轮挑战的数据。它不得进入分钟级脚本缓存。
type RuntimeInput struct {
	SceneID         string
	CertifyID       string
	DeviceToken     string
	CaptchaType     string
	Image           string
	PuzzleImage     string
	DeviceConfig    protocol.DeviceConfig
	VerifyAccessSec string
	VerifySalt      string
	Dimensions      RuntimeDimensions
	Track           []track.Event
	ExpectedXPos    *int
	InitBeginTimeMS int64
	FirstTouchAgeMS int
	// SDKSource 是本轮 Device VM 实际使用的公开 SDK 快照；不进入 JSON，
	// 避免同一轮在缓存刷新边界前后混用不同版本的 SDK。
	SDKSource []byte
}

// RuntimeProfile 是当前公开 SDK 与精确 PE 分片共同决定的、可安全缓存的
// 运行时结构画像。挑战级 CertifyId、轨迹和 data 不得进入该缓存。
type RuntimeProfile struct {
	ArgumentKey       string
	IncludeScreenInfo bool
	// PureGoCompatible 只能由当前 SDK + 精确 PE 分片的
	// V8 oracle 与纯 Go Builder 完整差分通过后置为 true。
	PureGoCompatible bool
}
