package slider

import (
	"context"
	"errors"

	"github.com/d2120848471/v3/ali-slider-go/internal/application/service"
	"github.com/d2120848471/v3/ali-slider-go/internal/application/solve"
	"github.com/d2120848471/v3/ali-slider-go/internal/bootstrap"
)

// Client 可被多个 goroutine 并发复用。每次 Solve 都冷建独立 Device/V8 会话，
// 持有独立挑战、设备画像与 Verify 尝试位；只共享连接池、密码学熵和
// 不含挑战态的公开 SDK/PE 脚本与画像缓存。
type Client struct {
	options ClientOptions
	service *service.Service
}

// NewClient 创建 Go Client，不发外部请求。每轮 Solve 才冷建 Device/V8 会话；
// 公开 SDK/PE 脚本和结构画像仍按既有 TTL 复用。
func NewClient(options ClientOptions) (*Client, error) {
	resolved := bootstrapOptions(options)
	if !options.defaultsResolved {
		resolved = bootstrap.FillDefaults(resolved)
	}
	application, err := bootstrap.NewService(resolved)
	if err != nil {
		return nil, err
	}
	return &Client{options: publicOptions(resolved), service: application}, nil
}

// Solve 执行一轮挑战。SceneID/Prefix 空值使用 Client 默认配置。
func (client *Client) Solve(ctx context.Context, request Request) (Result, error) {
	outcome, err := client.application().Solve(ctx, solve.Request{
		SceneID: request.SceneID, Prefix: request.Prefix, RPCKeyID: request.RPCKeyID, Proxy: request.Proxy,
	})
	if err != nil {
		return Result{}, publicError(err)
	}
	timings := cloneTimingMap(outcome.TimingsMS)
	return Result{
		OK: outcome.OK, SecurityToken: outcome.SecurityToken,
		VerifyCode: outcome.VerifyCode, VerifyResult: outcome.VerifyResult,
		CertifyID: outcome.CertifyID, SceneID: outcome.SceneID, Proxied: outcome.Proxied,
		ElapsedMS: int64(timings["total"]), TimingsMS: timings,
	}, nil
}

// Prime 仅保留为源码兼容入口。Device 预热/复用已移除；调用它不发外部请求。
//
// Deprecated: 新代码不需要调用 Prime。
func (client *Client) Prime(ctx context.Context) error {
	if err := client.application().Prime(ctx); err != nil {
		return publicError(err)
	}
	return nil
}

// CheckRuntime 立即校验本地 V8 wrapper、C ABI 和 ICU 初始化。HTTP 服务在
// 对外报告 ready 前调用；普通 library 调用方也可用于部署探针。
func (client *Client) CheckRuntime() error {
	if err := client.application().CheckRuntime(); err != nil {
		return publicError(err)
	}
	return nil
}

// PurgeArtifacts 删除超过配置保留期的受管失败样本；不触碰其他文件。
func (client *Client) PurgeArtifacts() (int, error) {
	removed, err := client.application().PurgeArtifacts()
	var failure *solve.Failure
	if errors.As(err, &failure) {
		return removed, publicError(err)
	}
	return removed, err
}

// Close 等待正在执行的 Solve、Prime、CheckRuntime 和 PurgeArtifacts 返回，
// 再关闭 V8 runtime 和空闲连接。多个 goroutine 可安全重复调用。
func (client *Client) Close() {
	client.application().Close()
}

func (client *Client) application() *service.Service {
	if client == nil {
		return nil
	}
	return client.service
}

func cloneTimingMap(value map[string]int) map[string]int {
	result := make(map[string]int, len(value))
	for key, duration := range value {
		result[key] = duration
	}
	return result
}
