package bootstrap

import (
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/d2120848471/v3/ali-slider-go/internal/bootstrap/config"
)

// Options 配置可并发复用的 Go Client。由入口显式选择 FillDefaults 或 DefaultOptions，
// 避免把有意义的零值覆盖为默认值。
type Options struct {
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
}

// DefaultOptions 返回 HTTP 与 Go SDK 共用的生产资源默认值。
func DefaultOptions() Options {
	defaults := config.Defaults()
	return Options{
		DefaultSceneID: defaults.SceneID, DefaultPrefix: defaults.Prefix,
		MaxConcurrency: defaults.MaxConcurrency, MaxTransportRoutes: 32,
		Timeout: defaults.Timeout, MinimumConfidence: defaults.MinimumConfidence,
		GatherCostMin: defaults.GatherCostMin, GatherCostMax: defaults.GatherCostMax,
		FirstTouchAgeMin: defaults.FirstTouchAgeMin, FirstTouchAgeMax: defaults.FirstTouchAgeMax,
		ArtifactDir: defaults.ArtifactDir, ArtifactRetention: defaults.ArtifactRetention,
		AssetMaxBytes: defaults.AssetMaxBytes, AssetMaxDimension: defaults.AssetMaxDimension,
		AssetMaxPixels: 16 << 20, DevicePrewarmCapacity: defaults.DevicePrewarmCapacity,
		DeviceSessionReserve: defaults.DeviceSessionReserve,
		V8RuntimeLibrary:     defaults.V8RuntimeLibrary,
	}
}

func FillDefaults(options Options) Options {
	defaults := DefaultOptions()
	if options.DefaultSceneID == "" {
		options.DefaultSceneID = defaults.DefaultSceneID
	}
	if options.DefaultPrefix == "" {
		options.DefaultPrefix = defaults.DefaultPrefix
	}
	if options.MaxConcurrency == 0 {
		options.MaxConcurrency = defaults.MaxConcurrency
	}
	if options.MaxTransportRoutes == 0 {
		options.MaxTransportRoutes = defaults.MaxTransportRoutes
	}
	if options.Timeout == 0 {
		options.Timeout = defaults.Timeout
	}
	if options.MinimumConfidence == 0 {
		options.MinimumConfidence = defaults.MinimumConfidence
	}
	if options.GatherCostMin == 0 && options.GatherCostMax == 0 {
		options.GatherCostMin, options.GatherCostMax = defaults.GatherCostMin, defaults.GatherCostMax
	}
	if options.FirstTouchAgeMin == 0 && options.FirstTouchAgeMax == 0 {
		options.FirstTouchAgeMin, options.FirstTouchAgeMax = defaults.FirstTouchAgeMin, defaults.FirstTouchAgeMax
	}
	if options.ArtifactDir == "" {
		options.ArtifactDir = defaults.ArtifactDir
	}
	if options.ArtifactRetention == 0 {
		options.ArtifactRetention = defaults.ArtifactRetention
	}
	if options.AssetMaxBytes == 0 {
		options.AssetMaxBytes = defaults.AssetMaxBytes
	}
	if options.AssetMaxDimension == 0 {
		options.AssetMaxDimension = defaults.AssetMaxDimension
	}
	if options.AssetMaxPixels == 0 {
		options.AssetMaxPixels = defaults.AssetMaxPixels
	}
	if options.V8RuntimeLibrary == "" {
		options.V8RuntimeLibrary = defaults.V8RuntimeLibrary
	}
	return options
}

func (options Options) Validate() error {
	if options.DefaultSceneID == "" || utf8.RuneCountInString(options.DefaultSceneID) > 64 {
		return errors.New("default SceneId must contain 1..64 characters")
	}
	if !asciiAlphanumeric(options.DefaultPrefix, 32) {
		return errors.New("default prefix must contain 1..32 ASCII alphanumeric characters")
	}
	if options.MaxConcurrency < 1 || options.MaxConcurrency > 32 || options.MaxTransportRoutes < 1 || options.MaxTransportRoutes > 32 {
		return errors.New("transport connections and routes must be within 1..32")
	}
	if options.Timeout <= 0 || options.Timeout > 5*time.Minute || options.MinimumConfidence < 0 || options.MinimumConfidence > 1 {
		return errors.New("timeout or confidence is outside the supported range")
	}
	if options.GatherCostMin < 0 || options.GatherCostMax < options.GatherCostMin || options.FirstTouchAgeMin < 1 || options.FirstTouchAgeMax < options.FirstTouchAgeMin {
		return errors.New("device timing range is invalid")
	}
	if options.ArtifactDir == "" || options.ArtifactRetention <= 0 {
		return errors.New("artifact directory and retention are required")
	}
	if options.AssetMaxBytes < 1 || options.AssetMaxBytes > 64<<20 || options.AssetMaxDimension < 1 || options.AssetMaxDimension > 16_384 || options.AssetMaxPixels < 1 {
		return errors.New("asset limits are invalid")
	}
	if options.DevicePrewarmCapacity != 0 || options.DeviceSessionReserve != 0 {
		return errors.New("device prewarm and reserve have been removed; both must be 0")
	}
	if options.V8RuntimeLibrary == "" || len(options.V8RuntimeLibrary) > 4_096 || strings.ContainsRune(options.V8RuntimeLibrary, 0) || !utf8.ValidString(options.V8RuntimeLibrary) {
		return errors.New("V8 runtime library path must contain 1..4096 valid UTF-8 bytes")
	}
	return nil
}

func asciiAlphanumeric(value string, maximum int) bool {
	if value == "" || len(value) > maximum {
		return false
	}
	for _, character := range []byte(value) {
		if (character < '0' || character > '9') && (character < 'A' || character > 'Z') && (character < 'a' || character > 'z') {
			return false
		}
	}
	return true
}
