// Package bootstrap 组装应用服务与具体资源，也是生产入口的依赖汇合点。
package bootstrap

import (
	"context"
	"net/http"

	"github.com/d2120848471/v3/ali-slider-go/internal/application/service"
	"github.com/d2120848471/v3/ali-slider-go/internal/application/solve"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/foundation/runtimekit"
	"github.com/d2120848471/v3/ali-slider-go/internal/infrastructure/aliyun"
	"github.com/d2120848471/v3/ali-slider-go/internal/infrastructure/artifact"
	"github.com/d2120848471/v3/ali-slider-go/internal/infrastructure/engine"
	"github.com/d2120848471/v3/ali-slider-go/internal/infrastructure/httpclient"
)

// NewService 只创建本地共享资源；不访问上游，也不加载 V8 动态库。
// options 必须已经由入口通过 DefaultOptions 或 FillDefaults 解析默认值。
func NewService(options Options) (*service.Service, error) {
	application, _, err := assemble(options)
	return application, err
}

func assemble(options Options) (*service.Service, *resources, error) {
	if err := options.Validate(); err != nil {
		return nil, nil, err
	}
	sources := runtimekit.NewSystemSources()
	transports, err := httpclient.NewTransportPool(options.MaxTransportRoutes, options.MaxConcurrency)
	if err != nil {
		return nil, nil, err
	}
	// 先保留直连路由，避免随机代理占满路由表后阻断默认路径。
	if _, _, err := transports.Get(""); err != nil {
		transports.CloseIdleConnections()
		return nil, nil, err
	}
	shared := &resources{
		transports: transports,
		runtime:    engine.NewKeyResolverWithCapacity(options.V8RuntimeLibrary, options.MaxConcurrency),
		artifacts: artifact.Store{
			Directory: options.ArtifactDir, Retention: options.ArtifactRetention, Entropy: sources.Entropy,
		},
	}
	openDevice := func(ctx context.Context, transport http.RoundTripper, profile device.Profile, request solve.Request) (aliyun.DeviceSession, func(), error) {
		session, openErr := shared.runtime.OpenDevice(ctx, transport, profile, engine.DeviceRuntimeOptions{
			Prefix: request.Prefix, Region: "cn", Proxy: request.Proxy, Timeout: options.Timeout,
			GatherCostMin: options.GatherCostMin, GatherCostMax: options.GatherCostMax,
			FirstTouchAgeMin: options.FirstTouchAgeMin, FirstTouchAgeMax: options.FirstTouchAgeMax,
			Sources: sources,
		})
		if openErr != nil || session == nil {
			if session != nil {
				session.Close()
			}
			return nil, nil, openErr
		}
		return session, session.Close, nil
	}
	factory, err := aliyun.NewRoundFactory(aliyun.FactoryOptions{
		Timeout: options.Timeout, GatherCostMin: options.GatherCostMin, GatherCostMax: options.GatherCostMax,
		FirstTouchAgeMin: options.FirstTouchAgeMin, FirstTouchAgeMax: options.FirstTouchAgeMax,
		AssetMaxBytes: options.AssetMaxBytes, Sources: sources,
		GetTransport: func(proxy string) (http.RoundTripper, bool, error) {
			return transports.Get(proxy)
		},
		OpenDevice: openDevice, PEKeys: shared.runtime,
	})
	if err != nil {
		shared.Close()
		return nil, nil, err
	}
	executor, err := solve.New(solve.Options{
		Timeout: options.Timeout, MinimumConfidence: options.MinimumConfidence,
		AssetMaxBytes: options.AssetMaxBytes, AssetMaxDimension: options.AssetMaxDimension,
		AssetMaxPixels: options.AssetMaxPixels, Sources: sources, Factory: factory,
		Recorder: failureRecorder{store: shared.artifacts},
	})
	if err != nil {
		shared.Close()
		return nil, nil, err
	}
	application, err := service.New(service.Options{
		Executor: executor, Resources: shared,
		DefaultSceneID: options.DefaultSceneID, DefaultPrefix: options.DefaultPrefix,
	})
	if err != nil {
		shared.Close()
		return nil, nil, err
	}
	return application, shared, nil
}

type resources struct {
	runtime    *engine.KeyResolver
	transports *httpclient.TransportPool
	artifacts  artifact.Store
}

func (resources *resources) CheckRuntime() error {
	_, err := resources.runtime.CheckRuntime()
	return err
}

func (resources *resources) PurgeArtifacts() (int, error) {
	return resources.artifacts.Purge()
}

func (resources *resources) Close() {
	_ = resources.runtime.Close()
	resources.transports.CloseIdleConnections()
}

type failureRecorder struct{ store artifact.Store }

func (recorder failureRecorder) SaveFailure(sample solve.FailureSample) error {
	_, err := recorder.store.SaveFailure(sample.Background, sample.Shadow, artifact.Metrics{
		Reason: sample.Reason, Stage: sample.Stage, Confidence: sample.Confidence,
		XPos: sample.XPos, SlidePos: sample.SlidePos, TimingsMS: sample.TimingsMS,
	})
	return err
}
