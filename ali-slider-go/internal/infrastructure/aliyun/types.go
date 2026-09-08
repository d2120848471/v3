package aliyun

import (
	"context"
	"net/http"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/application/solve"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/pe"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/protocol"
	"github.com/d2120848471/v3/ali-slider-go/internal/foundation/runtimekit"
)

type TransportGetter func(string) (http.RoundTripper, bool, error)

// DeviceSession 是单轮阿里设备会话合同，由兼容实现或动态 SDK 实现。
type DeviceSession interface {
	InitToken() (string, error)
	PEDeviceConfig() (protocol.DeviceConfig, error)
	TargetFirstTouchAgeMS() (int, error)
	Complete(context.Context, string, []device.InteractionEvent, int) (device.Result, error)
	Close()
}
type DeviceOpenFunc func(context.Context, http.RoundTripper, device.Profile, solve.Request) (DeviceSession, func(), error)

type peVerifyArgProfileProvider interface {
	PEVerifyArgProfile() (string, string, error)
}
type peSDKSourceProvider interface{ PESDKSource() ([]byte, error) }
type deviceProfileProvider interface{ DeviceProfile() device.Profile }
type tracelessDeviceSession interface {
	SolveTraceless(context.Context, device.TracelessInput) (device.TracelessResult, error)
}
type slidingDeviceSession interface {
	SolveSliding(context.Context, device.SlidingInput) (device.SlidingResult, error)
}

type PEKeyResolver interface {
	Prepare(context.Context, http.RoundTripper, device.Profile, string) error
	Build(context.Context, http.RoundTripper, device.Profile, string, pe.RuntimeInput) (pe.Result, error)
}

// FactoryOptions 仅含后端资源和画像配置；工厂构造时不访问网络。
type FactoryOptions struct {
	Timeout                            time.Duration
	GatherCostMin, GatherCostMax       int
	FirstTouchAgeMin, FirstTouchAgeMax int
	AssetMaxBytes                      int64
	Sources                            runtimekit.Sources
	GetTransport                       TransportGetter
	FixedProfile                       *device.Profile
	OpenDevice                         DeviceOpenFunc
	PEKeys                             PEKeyResolver
}
