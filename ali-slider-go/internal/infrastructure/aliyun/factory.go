package aliyun

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/application/solve"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/device"
	deviceclient "github.com/d2120848471/v3/ali-slider-go/internal/infrastructure/aliyun/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/infrastructure/httpclient"
)

const defaultRegion = "cn"

// RoundFactory 共享进程级资源配置；画像、设备与 RPCClient 每轮重新建立。
type RoundFactory struct{ options FactoryOptions }

func NewRoundFactory(options FactoryOptions) (*RoundFactory, error) {
	if options.Timeout <= 0 || options.Timeout > 5*time.Minute {
		return nil, errors.New("solver timeout must be within (0,5m]")
	}
	if options.GatherCostMin < 0 || options.GatherCostMax < options.GatherCostMin {
		return nil, errors.New("solver gather cost range is invalid")
	}
	if options.FirstTouchAgeMin < 1 || options.FirstTouchAgeMax < options.FirstTouchAgeMin {
		return nil, errors.New("solver first-touch range is invalid")
	}
	if options.AssetMaxBytes < 1 || options.AssetMaxBytes > 64<<20 {
		return nil, errors.New("solver asset limits are invalid")
	}
	if err := options.Sources.Validate(); err != nil {
		return nil, err
	}
	if options.GetTransport == nil {
		return nil, errors.New("solver transport getter is required")
	}
	if options.PEKeys == nil {
		return nil, errors.New("solver PE key resolver is required")
	}
	return &RoundFactory{options: options}, nil
}

func (factory *RoundFactory) NewRound(ctx context.Context, request solve.Request) (solve.Round, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	options := factory.options
	var profile device.Profile
	if options.FixedProfile != nil {
		profile = options.FixedProfile.Clone()
	} else {
		var err error
		profile, err = device.GenerateProfile(options.Sources.Entropy)
		if err != nil {
			return nil, dependencyError(solve.FailureInternal, "设备画像生成失败", err)
		}
	}
	transport, proxied, err := options.GetTransport(request.Proxy)
	if errors.Is(err, httpclient.ErrTransportRouteCapacity) {
		return nil, dependencyError(solve.FailureInternal, "传输路由资源已满", err)
	}
	if err != nil || transport == nil {
		return nil, dependencyError(solve.FailureInvalidRequest, "代理配置无效或路由不可用", err)
	}
	var client *deviceclient.Client
	// 动态设备自建环境，只有未注入 OpenDevice 的兼容路径构造原生客户端。
	if options.OpenDevice == nil {
		deviceOptions := deviceclient.DefaultClientOptions(profile, options.Sources)
		deviceOptions.Prefix, deviceOptions.Region, deviceOptions.Timeout = request.Prefix, defaultRegion, options.Timeout
		deviceOptions.GatherCostMin, deviceOptions.GatherCostMax = options.GatherCostMin, options.GatherCostMax
		deviceOptions.FirstTouchAgeMin, deviceOptions.FirstTouchAgeMax = options.FirstTouchAgeMin, options.FirstTouchAgeMax
		client, err = deviceclient.NewClient(deviceOptions, transport)
		if err != nil {
			return nil, dependencyError(solve.FailureInternal, "设备客户端初始化失败", err)
		}
	}
	return &round{options: options, request: request, transport: transport, profile: profile, proxied: proxied, deviceClient: client}, nil
}

var _ solve.RoundFactory = (*RoundFactory)(nil)

// round 状态按阶段顺序写入；Init 后的 Prepare 和下载只读取已绑定状态。
type round struct {
	options      FactoryOptions
	request      solve.Request
	transport    http.RoundTripper
	profile      device.Profile
	proxied      bool
	deviceClient *deviceclient.Client
	session      DeviceSession
	release      func()
	rpcClient    *RPCClient
	initToken    string
	challenge    CaptchaChallenge
}
