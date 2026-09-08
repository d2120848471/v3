// Package solve 编排单轮挑战，依赖由调用方提供的会话端口。
package solve

import (
	"context"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/domain/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/track"
	"github.com/d2120848471/v3/ali-slider-go/internal/foundation/runtimekit"
)

// Request 是内部单轮请求；HTTP 别名和默认值在公共层处理。
type Request struct{ SceneID, Prefix, RPCKeyID, Proxy string }

func (Request) String() string   { return "solve.Request{redacted}" }
func (Request) GoString() string { return "solve.Request{redacted}" }

// Outcome 只包含公共结果所需字段，不保留上游原始响应。
type Outcome struct {
	OK                        bool
	SecurityToken, VerifyCode string
	VerifyResult              bool
	CertifyID, SceneID        string
	Proxied                   bool
	TimingsMS                 map[string]int
}

func (Outcome) String() string   { return "solve.Outcome{redacted}" }
func (Outcome) GoString() string { return "solve.Outcome{redacted}" }

type Options struct {
	Timeout           time.Duration
	MinimumConfidence float64
	AssetMaxBytes     int64
	AssetMaxDimension int
	AssetMaxPixels    int64
	Sources           runtimekit.Sources
	Factory           RoundFactory
	Recorder          FailureRecorder
}

// RoundFactory 仅建立当轮配置；网络访问从 OpenDevice 开始。
type RoundFactory interface {
	NewRound(context.Context, Request) (Round, error)
}

// Round 独占单轮设备、画像和验证码状态。PreparePuzzle 与 DownloadImages
// 允许并发；其他方法按求解阶段顺序调用。CloseDevice 必须释放本轮设备资源。
type Round interface {
	Proxied() bool
	OpenDevice(context.Context) error
	Init(context.Context) (Challenge, error)
	PreparePuzzle(context.Context) error
	DownloadImages(context.Context) (Images, error)
	BuildPuzzle(context.Context, PuzzleInput) (PuzzleProof, error)
	CompleteDevice(context.Context, CompletionPlan) (string, error)
	SolveTraceless(context.Context) (Verification, error)
	SolveSliding(context.Context, []track.Event, int, int) (Verification, error)
	Verify(context.Context, VerificationInput) (Verification, error)
	CloseDevice()
}

type ChallengeType string

const (
	Puzzle    ChallengeType = "puzzle"
	Sliding   ChallengeType = "sliding"
	Traceless ChallengeType = "traceless"
)

type Challenge struct {
	Type      ChallengeType
	CertifyID string
}

func (Challenge) String() string   { return "solve.Challenge{redacted}" }
func (Challenge) GoString() string { return "solve.Challenge{redacted}" }

type Images struct {
	Background, Shadow []byte
	TimingsMS          map[string]int
}
type PuzzleInput struct {
	XPos, SlidePos                                     int
	Track                                              []track.Event
	ImageWidth, ImageHeight, PuzzleWidth, PuzzleHeight int
	RenderedWidth, HandleWidth                         int
}
type PuzzleProof struct {
	SlidePos     int
	Data         string
	VerifyTimeMS int64
	Completion   CompletionPlan
}
type CompletionPlan struct {
	GetterArgument string
	Interactions   []device.InteractionEvent
	PostDelayMS    int
}
type VerificationInput struct {
	DeviceToken, Data string
	VerifyTimeMS      int64
}
type Verification struct {
	VerifyCode               string
	VerifyResult             bool
	SecurityToken, CertifyID string
}

func (value Verification) Succeeded() bool {
	return value.VerifyCode == "T001" && value.VerifyResult && value.SecurityToken != ""
}
func (Verification) String() string   { return "solve.Verification{redacted}" }
func (Verification) GoString() string { return "solve.Verification{redacted}" }

type FailureSample struct {
	Background, Shadow []byte
	Reason, Stage      string
	Confidence         float64
	XPos, SlidePos     int
	TimingsMS          map[string]int
}

// FailureRecorder 返回记录错误；求解器按既有 best-effort 语义不改变业务结果。
type FailureRecorder interface{ SaveFailure(FailureSample) error }
