package bootstrap

import (
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/application/service"
	"github.com/d2120848471/v3/ali-slider-go/internal/foundation/runtimekit"
	wafinfra "github.com/d2120848471/v3/ali-slider-go/internal/infrastructure/waf"
)

// NewWAFExecutor 组装每请求独立的 WAF 执行器；构造不联网、不加载运行库。
func NewWAFExecutor(libraryPath string, timeout time.Duration) (service.WAFExecutor, error) {
	return wafinfra.NewExecutor(libraryPath, timeout, runtimekit.NewSystemSources())
}
