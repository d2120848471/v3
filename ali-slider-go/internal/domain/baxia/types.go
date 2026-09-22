// Package baxia 定义独立的 Fireye bx-ua 会话合同。
package baxia

import (
	"context"
	"errors"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/domain/device"
)

var (
	ErrConfig         = errors.New("invalid Baxia configuration")
	ErrRuntime        = errors.New("baxia runtime failed")
	ErrClosed         = errors.New("baxia session is closed")
	ErrToken          = errors.New("invalid Baxia token")
	ErrUnsupportedSDK = errors.New("unsupported Fireye SDK")
	ErrSource         = errors.New("baxia SDK source failed")
)

// Config 描述独立会话。SDKSource 为空时由 Go 发现并下载当次 SDK；
// 显式提供字节时保持离线。SDK 内部的网络访问始终禁用。
type Config struct {
	V8LibraryPath string
	SDKSource     []byte
	Profile       device.Profile
	PageURL       string
	// Timeout 限制初始化总耗时；零值使用 5 秒，范围为 1 毫秒至 30 秒。
	Timeout time.Duration
	// UAOptions 为 nil 时使用 DefaultUAOptions；非 nil 时保留显式零值。
	UAOptions *UAOptions
}

// ClientOptions 控制 Go 侧公开脚本下载；不用于 SDK 内部网络。
type ClientOptions struct {
	Proxy string
	// Timeout 限制发现、下载和初始化的总时间；零值使用 30 秒。
	Timeout time.Duration
}

// RuntimeProfile 从当前 SDK 与实际 getter 样本恢复并校验。
// Alphabet 只描述输出外层编码，不代表 SDK 内部的加密密钥。
type RuntimeProfile struct {
	Version  int    `json:"version"`
	Prefix   string `json:"prefix"`
	Alphabet string `json:"alphabet"`
	SDKHash  string `json:"sdkHash"`
}

// UAOptions 对应已观测到的 Fireye 初始化参数。reqUrl 由 Token 单独写入。
type UAOptions struct {
	MaxMTLog    int    `json:"MaxMTLog"`
	MaxKSLog    int    `json:"MaxKSLog"`
	MaxFocusLog int    `json:"MaxFocusLog"`
	MaxNGPLog   int    `json:"MaxNGPLog"`
	OnlyHost    int    `json:"OnlyHost"`
	Location    string `json:"location"`
	LoadTime    int    `json:"loadTime"`
	// Timeout 是 SDK 内部初始化轮询的毫秒数。
	Timeout int `json:"timeout"`
}

func DefaultUAOptions() UAOptions {
	return UAOptions{
		MaxMTLog: 5, MaxKSLog: 5, MaxFocusLog: 1, MaxNGPLog: 5,
		OnlyHost: 1, Location: "cn", LoadTime: 6, Timeout: 2000,
	}
}

// Result 只包含本地 Fireye 输出及源码身份，不表示服务端接受该 token。
type Result struct {
	Token    string         `json:"token"`
	Version  int            `json:"version"`
	SDKHash  string         `json:"sdkSha256"`
	SDKURL   string         `json:"sdkURL,omitempty"`
	AWSCHash string         `json:"awscSha256,omitempty"`
	Profile  RuntimeProfile `json:"profile"`
}

// Session 保留单个 SDK 实例的状态；调用方应在使用完毕后 Close。
type Session interface {
	Token(context.Context, string) (Result, error)
	Profile() RuntimeProfile
	Close() error
}

// Client 为每个新会话重新检查公开 SDK，已有会话继续使用自己的快照。
type Client interface {
	NewSession(context.Context, Config) (Session, error)
	Close() error
}
