package bootstrap

import (
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/application/service"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/foundation/runtimekit"
)

// NewBaxiaExecutor 组装按请求创建资源的生成器；构造和健康检查都不下载 JS。
func NewBaxiaExecutor(libraryPath string, timeout time.Duration) (service.BaxiaExecutor, error) {
	sources := runtimekit.NewSystemSources()
	return service.NewBaxiaGenerator(service.BaxiaOptions{
		NewClient: NewBaxiaClient,
		GenerateProfile: func() (device.Profile, error) {
			return device.GenerateProfile(sources.Entropy)
		},
		V8LibraryPath: libraryPath, Timeout: timeout,
	})
}
