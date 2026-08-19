// Package config 解析服务配置。优先级固定为：命令行参数 > ALI_SLIDER_* 环境变量 > 默认值。
package config

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/d2120848471/v3/ali-slider-go/internal/v8runtime"
)

const (
	DefaultSceneID = "1ug4aptr"
	DefaultPrefix  = "fsgtmi"
	// 预热/复用已从生产路径移除。旧参数仍接受 0，便于现有
	// 启动脚本平滑升级；非 0 值明确拒绝，不会静默重新启用会话池。
	MaxDevicePrewarmCapacity = 0
	MaxDeviceSessionReserve  = 0
)

// Config 只包含部署和算法边界；逐轮 SceneId、prefix、AaduaneId、proxy 不在此共享。
type Config struct {
	Host string
	Port int
	// MaxConcurrency 是兼容旧名：用于 Client 每 route/host 出站连接与 PE 资源预算，不限制 HTTP 请求进入。
	MaxConcurrency        int
	Timeout               time.Duration
	MinimumConfidence     float64
	SceneID               string
	Prefix                string
	GatherCostMin         int
	GatherCostMax         int
	FirstTouchAgeMin      int
	FirstTouchAgeMax      int
	ArtifactDir           string
	ArtifactRetention     time.Duration
	AssetMaxBytes         int64
	AssetMaxDimension     int
	DevicePrewarmCapacity int
	DeviceSessionReserve  int
	V8RuntimeLibrary      string
}

// Defaults 返回适合本地运行的保守默认值。
func Defaults() Config {
	return Config{
		Host:                  "127.0.0.1",
		Port:                  8000,
		MaxConcurrency:        32,
		Timeout:               25 * time.Second,
		MinimumConfidence:     0.45,
		SceneID:               DefaultSceneID,
		Prefix:                DefaultPrefix,
		GatherCostMin:         180,
		GatherCostMax:         260,
		FirstTouchAgeMin:      650,
		FirstTouchAgeMax:      850,
		ArtifactDir:           filepath.FromSlash("var/artifacts"),
		ArtifactRetention:     7 * 24 * time.Hour,
		AssetMaxBytes:         8 << 20,
		AssetMaxDimension:     16_384,
		DevicePrewarmCapacity: 0,
		DeviceSessionReserve:  0,
		V8RuntimeLibrary:      v8runtime.DefaultLibraryPath(),
	}
}

// Parse 解析 os.Args 风格参数。传 nil getenv 时读取当前进程环境。
func Parse(args []string, getenv func(string) string) (Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	cfg := Defaults()
	var err error
	if cfg.Host, err = envString(getenv, "ALI_SLIDER_HOST", cfg.Host); err != nil {
		return Config{}, err
	}
	if cfg.Port, err = envInt(getenv, "ALI_SLIDER_PORT", cfg.Port); err != nil {
		return Config{}, err
	}
	if cfg.MaxConcurrency, err = envInt(getenv, "ALI_SLIDER_MAX_CONCURRENCY", cfg.MaxConcurrency); err != nil {
		return Config{}, err
	}
	if cfg.Timeout, err = envDuration(getenv, "ALI_SLIDER_TIMEOUT", cfg.Timeout); err != nil {
		return Config{}, err
	}
	if cfg.MinimumConfidence, err = envFloat(getenv, "ALI_SLIDER_MIN_CONFIDENCE", cfg.MinimumConfidence); err != nil {
		return Config{}, err
	}
	if cfg.SceneID, err = envString(getenv, "ALI_SLIDER_SCENE_ID", cfg.SceneID); err != nil {
		return Config{}, err
	}
	if cfg.Prefix, err = envString(getenv, "ALI_SLIDER_PREFIX", cfg.Prefix); err != nil {
		return Config{}, err
	}
	if cfg.GatherCostMin, err = envInt(getenv, "ALI_SLIDER_GATHER_COST_MIN", cfg.GatherCostMin); err != nil {
		return Config{}, err
	}
	if cfg.GatherCostMax, err = envInt(getenv, "ALI_SLIDER_GATHER_COST_MAX", cfg.GatherCostMax); err != nil {
		return Config{}, err
	}
	if cfg.FirstTouchAgeMin, err = envInt(getenv, "ALI_SLIDER_FIRST_TOUCH_AGE_MIN", cfg.FirstTouchAgeMin); err != nil {
		return Config{}, err
	}
	if cfg.FirstTouchAgeMax, err = envInt(getenv, "ALI_SLIDER_FIRST_TOUCH_AGE_MAX", cfg.FirstTouchAgeMax); err != nil {
		return Config{}, err
	}
	if cfg.ArtifactDir, err = envString(getenv, "ALI_SLIDER_ARTIFACT_DIR", cfg.ArtifactDir); err != nil {
		return Config{}, err
	}
	if cfg.ArtifactRetention, err = envDuration(getenv, "ALI_SLIDER_ARTIFACT_RETENTION", cfg.ArtifactRetention); err != nil {
		return Config{}, err
	}
	if cfg.AssetMaxBytes, err = envInt64(getenv, "ALI_SLIDER_ASSET_MAX_BYTES", cfg.AssetMaxBytes); err != nil {
		return Config{}, err
	}
	if cfg.AssetMaxDimension, err = envInt(getenv, "ALI_SLIDER_ASSET_MAX_DIMENSION", cfg.AssetMaxDimension); err != nil {
		return Config{}, err
	}
	if cfg.DevicePrewarmCapacity, err = envInt(getenv, "ALI_SLIDER_DEVICE_PREWARM", cfg.DevicePrewarmCapacity); err != nil {
		return Config{}, err
	}
	if cfg.DeviceSessionReserve, err = envInt(getenv, "ALI_SLIDER_DEVICE_RESERVE", cfg.DeviceSessionReserve); err != nil {
		return Config{}, err
	}
	if cfg.V8RuntimeLibrary, err = envString(getenv, "ALI_SLIDER_V8_LIBRARY", cfg.V8RuntimeLibrary); err != nil {
		return Config{}, err
	}

	set := flag.NewFlagSet("ali-slider-go", flag.ContinueOnError)
	set.StringVar(&cfg.Host, "host", cfg.Host, "监听地址")
	set.IntVar(&cfg.Port, "port", cfg.Port, "监听端口")
	set.IntVar(&cfg.MaxConcurrency, "max-concurrency", cfg.MaxConcurrency, "Client 每 route/host 出站连接与 PE 资源上限（不限制 HTTP 请求数）")
	set.DurationVar(&cfg.Timeout, "timeout", cfg.Timeout, "单轮总超时")
	set.Float64Var(&cfg.MinimumConfidence, "min-confidence", cfg.MinimumConfidence, "最低视觉置信度")
	set.StringVar(&cfg.SceneID, "scene-id", cfg.SceneID, "默认 SceneId")
	set.StringVar(&cfg.Prefix, "prefix", cfg.Prefix, "默认实例 prefix")
	set.IntVar(&cfg.GatherCostMin, "gather-cost-min", cfg.GatherCostMin, "GatherCost 下界毫秒")
	set.IntVar(&cfg.GatherCostMax, "gather-cost-max", cfg.GatherCostMax, "GatherCost 上界毫秒")
	set.IntVar(&cfg.FirstTouchAgeMin, "first-touch-age-min", cfg.FirstTouchAgeMin, "首次触摸年龄下界毫秒")
	set.IntVar(&cfg.FirstTouchAgeMax, "first-touch-age-max", cfg.FirstTouchAgeMax, "首次触摸年龄上界毫秒")
	set.StringVar(&cfg.ArtifactDir, "artifact-dir", cfg.ArtifactDir, "失败样本目录")
	set.DurationVar(&cfg.ArtifactRetention, "artifact-retention", cfg.ArtifactRetention, "失败样本保留时长")
	set.Int64Var(&cfg.AssetMaxBytes, "asset-max-bytes", cfg.AssetMaxBytes, "单张图片最大字节数")
	set.IntVar(&cfg.AssetMaxDimension, "asset-max-dimension", cfg.AssetMaxDimension, "图片最大边长")
	set.IntVar(&cfg.DevicePrewarmCapacity, "device-prewarm", cfg.DevicePrewarmCapacity, "兼容参数，必须为 0（预热已移除）")
	set.IntVar(&cfg.DeviceSessionReserve, "device-reserve", cfg.DeviceSessionReserve, "兼容参数，必须为 0（会话复用已移除）")
	set.StringVar(&cfg.V8RuntimeLibrary, "v8-library", cfg.V8RuntimeLibrary, "设备与动态 PE 使用的内嵌 V8 动态库")
	if err := set.Parse(args); err != nil {
		return Config{}, err
	}
	if set.NArg() != 0 {
		return Config{}, fmt.Errorf("unexpected arguments: %s", strings.Join(set.Args(), " "))
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate 检查配置的资源与协议边界。
func (c Config) Validate() error {
	if net.ParseIP(c.Host) == nil && c.Host != "localhost" {
		return errors.New("host must be an IP address or localhost")
	}
	if c.Port < 1 || c.Port > 65_535 {
		return errors.New("port must be within 1..65535")
	}
	if c.MaxConcurrency < 1 || c.MaxConcurrency > 32 {
		return errors.New("client resource concurrency must be within 1..32")
	}
	if c.Timeout <= 0 || c.Timeout > 5*time.Minute {
		return errors.New("timeout must be within (0,5m]")
	}
	if c.MinimumConfidence < 0 || c.MinimumConfidence > 1 {
		return errors.New("minimum confidence must be within 0..1")
	}
	if c.SceneID == "" || utf8.RuneCountInString(c.SceneID) > 64 {
		return errors.New("default SceneId must contain 1..64 characters")
	}
	if c.Prefix == "" || len(c.Prefix) > 32 || !asciiAlnum(c.Prefix) {
		return errors.New("default prefix must contain 1..32 ASCII alphanumeric characters")
	}
	if c.GatherCostMin < 0 || c.GatherCostMin > c.GatherCostMax {
		return errors.New("invalid gather cost range")
	}
	if c.FirstTouchAgeMin < 1 || c.FirstTouchAgeMin > c.FirstTouchAgeMax {
		return errors.New("invalid first touch age range")
	}
	if c.ArtifactDir == "" || c.ArtifactRetention <= 0 {
		return errors.New("artifact directory and retention must be positive")
	}
	if c.AssetMaxBytes < 1 || c.AssetMaxBytes > 64<<20 || c.AssetMaxDimension < 1 || c.AssetMaxDimension > 16_384 {
		return errors.New("invalid asset limits")
	}
	if c.DevicePrewarmCapacity != 0 || c.DeviceSessionReserve != 0 {
		return errors.New("device prewarm and reserve have been removed; both must be 0")
	}
	if c.V8RuntimeLibrary == "" || len(c.V8RuntimeLibrary) > 4_096 || strings.ContainsRune(c.V8RuntimeLibrary, 0) || !utf8.ValidString(c.V8RuntimeLibrary) {
		return errors.New("V8 runtime library path must contain 1..4096 valid UTF-8 bytes")
	}
	return nil
}

func asciiAlnum(value string) bool {
	for _, char := range []byte(value) {
		if (char < '0' || char > '9') && (char < 'A' || char > 'Z') && (char < 'a' || char > 'z') {
			return false
		}
	}
	return true
}

func envString(getenv func(string) string, key, fallback string) (string, error) {
	if value := strings.TrimSpace(getenv(key)); value != "" {
		return value, nil
	}
	return fallback, nil
}

func envInt(getenv func(string) string, key string, fallback int) (int, error) {
	value := strings.TrimSpace(getenv(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return parsed, nil
}

func envInt64(getenv func(string) string, key string, fallback int64) (int64, error) {
	value := strings.TrimSpace(getenv(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return parsed, nil
}

func envFloat(getenv func(string) string, key string, fallback float64) (float64, error) {
	value := strings.TrimSpace(getenv(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return parsed, nil
}

func envDuration(getenv func(string) string, key string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(getenv(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return parsed, nil
}
