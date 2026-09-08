package pe

import "errors"

// PE 与设备运行时共享轨迹规模和逻辑时钟边界。
const (
	FreshnessMarginMS      = int64(750)
	MaximumTrackEvents     = 512
	MaximumTrackDurationMS = int64(60_000)
)

var (
	// ErrUnsupportedPE 表示动态脚本的命名、内容或 arg 算法已超出当前安全边界。
	ErrUnsupportedPE = errors.New("unsupported dynamic PE script")
	// ErrKeyNetwork 表示公开 SDK/PE 脚本无法通过本轮路由取得。
	ErrKeyNetwork = errors.New("dynamic PE script download failed")
	// ErrKeyRuntime 表示本机动态 PE/设备运行时不存在或执行失败。
	ErrKeyRuntime = errors.New("dynamic PE runtime failed")
)
