// Package baxia 由 Go 获取当前 Fireye SDK，在进程内 V8 中生成 bx-ua。
// 它不发送登录请求，不生成 bx_et、bx-umidtoken 或验证码通过凭据。
package baxia

import (
	"context"

	"github.com/d2120848471/v3/ali-slider-go/internal/bootstrap"
	domain "github.com/d2120848471/v3/ali-slider-go/internal/domain/baxia"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/foundation/runtimekit"
)

type (
	// Config 固定 SDK 字节、页面 URL、设备画像和初始化执行上限。
	Config = domain.Config
	// ClientOptions 控制公开脚本下载的代理与构造超时。
	ClientOptions = domain.ClientOptions
	// RuntimeProfile 包含当前 SDK 的实测版本、前缀、外层编码表及摘要。
	RuntimeProfile = domain.RuntimeProfile
	// UAOptions 对应 AWSC 交给 Fireye 的采集配置。
	UAOptions = domain.UAOptions
	// Result 包含完整 bx-ua 值、SDK 版本与源码摘要。
	Result = domain.Result
	// Profile 与现有验证码 SDK 共享设备字段，调用方需保持 HTTP UA 一致。
	Profile       = device.Profile
	ScreenProfile = device.ScreenProfile
	GPUProfile    = device.GPUProfile
	UABrand       = device.UABrand
)

var (
	ErrConfig         = domain.ErrConfig
	ErrRuntime        = domain.ErrRuntime
	ErrClosed         = domain.ErrClosed
	ErrToken          = domain.ErrToken
	ErrUnsupportedSDK = domain.ErrUnsupportedSDK
	ErrSource         = domain.ErrSource
)

// Session 持有独立 SDK 状态。同一会话的 Token 与 Close 可安全并发调用。
type Session struct {
	inner domain.Session
}

// NewSession 在 SDKSource 为空时发现并下载当前 SDK；有字节时离线加载。
// SDK 的采样、生成始终在禁网 V8 中执行。
func NewSession(ctx context.Context, config Config) (*Session, error) {
	inner, err := bootstrap.NewBaxiaSession(ctx, config)
	if err != nil {
		return nil, err
	}
	return &Session{inner: inner}, nil
}

// Token 使用同一 SDK 状态为 requestURL 生成 bx-ua，不发送该请求。
func (session *Session) Token(ctx context.Context, requestURL string) (Result, error) {
	if session == nil || session.inner == nil {
		return Result{}, ErrClosed
	}
	return session.inner.Token(ctx, requestURL)
}

// Profile 返回本会话从原始 SDK 恢复并验证的输出编码合同。
func (session *Session) Profile() RuntimeProfile {
	if session == nil || session.inner == nil {
		return RuntimeProfile{}
	}
	return session.inner.Profile()
}

// Client 复用公开脚本的条件请求缓存，不共享设备或 token 会话。
type Client struct {
	inner domain.Client
}

// NewClient 构造下载器但不联网；每次 NewSession 都向 CDN 重新确认脚本。
func NewClient(options ClientOptions) (*Client, error) {
	inner, err := bootstrap.NewBaxiaClient(options)
	if err != nil {
		return nil, err
	}
	return &Client{inner: inner}, nil
}

// NewSession 获取当前站点适用的 SDK 并恢复 profile，已有会话不受更新影响。
func (client *Client) NewSession(ctx context.Context, config Config) (*Session, error) {
	if client == nil || client.inner == nil {
		return nil, ErrClosed
	}
	inner, err := client.inner.NewSession(ctx, config)
	if err != nil {
		return nil, err
	}
	return &Session{inner: inner}, nil
}

// Close 取消并等待正在构造的会话，释放下载连接；已创建的 Session 单独关闭。
func (client *Client) Close() error {
	if client == nil || client.inner == nil {
		return nil
	}
	return client.inner.Close()
}

// Close 取消活动调用并等待其退出，然后释放会话；重复调用安全。
func (session *Session) Close() error {
	if session == nil || session.inner == nil {
		return nil
	}
	return session.inner.Close()
}

// DefaultUAOptions 返回本次观察到的 Baxia 2.5.37 配置。
func DefaultUAOptions() UAOptions { return domain.DefaultUAOptions() }

// GenerateProfile 沿用项目现有的移动 Chromium 画像生成器。
// 它不会复制当前浏览器的设备状态；如需指定设备，应自行填写 Profile。
func GenerateProfile() (Profile, error) {
	return device.GenerateProfile(runtimekit.NewSystemSources().Entropy)
}
