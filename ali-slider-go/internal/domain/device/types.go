package device

import "github.com/d2120848471/v3/ali-slider-go/internal/domain/track"

// Result 只暴露 challenge 编排需要的合同。String/GoString 永不打印 token。
type Result struct {
	InitToken             string
	VerifyToken           string
	RequestActions        []string
	RequestCount          int
	FingerprintFieldCount int
	GetterArgumentCount   int
	InteractionEventCount int
	CollectorStartedMS    int64
	TokenTimeMS           int64
	GatherCost            int
}

func (Result) String() string   { return "device.Result{redacted}" }
func (Result) GoString() string { return "device.Result{redacted}" }

// InteractionEvent 是设备 Log3 接受的逻辑 mousemove 事件。
type InteractionEvent struct {
	Type      string  `json:"type"`
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	TimeStamp float64 `json:"timeStamp"`
	IsTrusted bool    `json:"isTrusted"`
}

// TracelessInput 绑定已经由同一 DeviceToken 初始化的无痕挑战。
type TracelessInput struct {
	SceneID     string
	CertifyID   string
	StaticPath  string
	CaptchaType string
}

// TracelessResult 保留阿里 Verify 响应的现有滑块业务字段。
type TracelessResult struct {
	SecurityToken  string
	VerifyCode     string
	VerifyResult   bool
	CertifyID      string
	RequestActions []string
}

func (TracelessResult) String() string   { return "pe.TracelessResult{redacted}" }
func (TracelessResult) GoString() string { return "pe.TracelessResult{redacted}" }

func (result TracelessResult) Succeeded() bool {
	return result.VerifyCode == "T001" && result.VerifyResult && result.SecurityToken != ""
}

// SlidingInput 绑定同一 DeviceToken 初始化的无图拖动挑战及官方组件轨迹。
type SlidingInput struct {
	SceneID     string
	CertifyID   string
	StaticPath  string
	CaptchaType string
	Track       []track.Event
	SlideWidth  int
	HandleWidth int
}

// SlidingResult 把 SDK success 归一到既有公共完成态字段。
type SlidingResult struct {
	SecurityToken  string
	VerifyCode     string
	VerifyResult   bool
	CertifyID      string
	RequestActions []string
}

func (SlidingResult) String() string   { return "pe.SlidingResult{redacted}" }
func (SlidingResult) GoString() string { return "pe.SlidingResult{redacted}" }

func (result SlidingResult) Succeeded() bool {
	return result.VerifyCode == "T001" && result.VerifyResult && result.SecurityToken != ""
}
