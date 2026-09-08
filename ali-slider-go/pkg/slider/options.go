package slider

import (
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/bootstrap"
)

// ClientOptions 配置可并发复用的 Go Client。直接使用零值结构时会按
// DefaultClientOptions 补齐。如需显式传递有意义的零（如置信度 0、
// GatherCost 0..0），应先调用 DefaultClientOptions 再覆盖。
type ClientOptions struct {
	DefaultSceneID string
	DefaultPrefix  string
	// MaxConcurrency 是兼容旧名：控制每 route/host 出站连接与 PE 空闲引擎保留量。
	// 活跃 PE、Device 整轮会话和 HTTP/Solve 调用不受本地信号量限制。
	MaxConcurrency     int
	MaxTransportRoutes int
	Timeout            time.Duration
	MinimumConfidence  float64
	GatherCostMin      int
	GatherCostMax      int
	FirstTouchAgeMin   int
	FirstTouchAgeMax   int
	ArtifactDir        string
	ArtifactRetention  time.Duration
	AssetMaxBytes      int64
	AssetMaxDimension  int
	AssetMaxPixels     int64
	// DevicePrewarmCapacity / DeviceSessionReserve 仅保留为升级兼容字段。
	// 生产路径不再预热或复用 Device 会话，两者必须为 0。
	DevicePrewarmCapacity int
	DeviceSessionReserve  int
	DeviceSessionMaxAge   time.Duration // Deprecated: 仅保留字段兼容，当前实现忽略该值。
	V8RuntimeLibrary      string

	defaultsResolved bool
}

// DefaultClientOptions 返回服务与嵌入式 library 共用的生产默认值。
func DefaultClientOptions() ClientOptions {
	return publicOptions(bootstrap.DefaultOptions())
}

func bootstrapOptions(options ClientOptions) bootstrap.Options {
	return bootstrap.Options{
		DefaultSceneID:        options.DefaultSceneID,
		DefaultPrefix:         options.DefaultPrefix,
		MaxConcurrency:        options.MaxConcurrency,
		MaxTransportRoutes:    options.MaxTransportRoutes,
		Timeout:               options.Timeout,
		MinimumConfidence:     options.MinimumConfidence,
		GatherCostMin:         options.GatherCostMin,
		GatherCostMax:         options.GatherCostMax,
		FirstTouchAgeMin:      options.FirstTouchAgeMin,
		FirstTouchAgeMax:      options.FirstTouchAgeMax,
		ArtifactDir:           options.ArtifactDir,
		ArtifactRetention:     options.ArtifactRetention,
		AssetMaxBytes:         options.AssetMaxBytes,
		AssetMaxDimension:     options.AssetMaxDimension,
		AssetMaxPixels:        options.AssetMaxPixels,
		DevicePrewarmCapacity: options.DevicePrewarmCapacity,
		DeviceSessionReserve:  options.DeviceSessionReserve,
		DeviceSessionMaxAge:   options.DeviceSessionMaxAge,
		V8RuntimeLibrary:      options.V8RuntimeLibrary,
	}
}

func publicOptions(options bootstrap.Options) ClientOptions {
	return ClientOptions{
		DefaultSceneID:        options.DefaultSceneID,
		DefaultPrefix:         options.DefaultPrefix,
		MaxConcurrency:        options.MaxConcurrency,
		MaxTransportRoutes:    options.MaxTransportRoutes,
		Timeout:               options.Timeout,
		MinimumConfidence:     options.MinimumConfidence,
		GatherCostMin:         options.GatherCostMin,
		GatherCostMax:         options.GatherCostMax,
		FirstTouchAgeMin:      options.FirstTouchAgeMin,
		FirstTouchAgeMax:      options.FirstTouchAgeMax,
		ArtifactDir:           options.ArtifactDir,
		ArtifactRetention:     options.ArtifactRetention,
		AssetMaxBytes:         options.AssetMaxBytes,
		AssetMaxDimension:     options.AssetMaxDimension,
		AssetMaxPixels:        options.AssetMaxPixels,
		DevicePrewarmCapacity: options.DevicePrewarmCapacity,
		DeviceSessionReserve:  options.DeviceSessionReserve,
		DeviceSessionMaxAge:   options.DeviceSessionMaxAge,
		V8RuntimeLibrary:      options.V8RuntimeLibrary,
		defaultsResolved:      true,
	}
}
