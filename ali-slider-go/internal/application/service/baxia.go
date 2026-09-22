package service

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/domain/baxia"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/device"
)

const MaxBaxiaURLBytes = 8192

// BaxiaRequest 只描述生成上下文；不会向 PageURL 或 RequestURL 发送请求。
type BaxiaRequest struct{ PageURL, RequestURL, Proxy string }

func (BaxiaRequest) String() string   { return "service.BaxiaRequest{redacted}" }
func (BaxiaRequest) GoString() string { return "service.BaxiaRequest{redacted}" }

// BaxiaOutcome.Result.Profile 是 SDK 输出编码合同，UAHeaders 来自当次设备画像。
type BaxiaOutcome struct {
	Result    baxia.Result
	UAHeaders map[string]string
	Proxied   bool
	Elapsed   time.Duration
}

func (BaxiaOutcome) String() string   { return "service.BaxiaOutcome{redacted}" }
func (BaxiaOutcome) GoString() string { return "service.BaxiaOutcome{redacted}" }

type BaxiaExecutor interface {
	GenerateBaxia(context.Context, BaxiaRequest) (BaxiaOutcome, error)
}

type BaxiaFailureKind string

const (
	BaxiaFailureInvalidRequest BaxiaFailureKind = "invalid_request"
	BaxiaFailureSource         BaxiaFailureKind = "source"
	BaxiaFailureUnsupportedSDK BaxiaFailureKind = "unsupported_sdk"
	BaxiaFailureRuntime        BaxiaFailureKind = "runtime"
	BaxiaFailureCanceled       BaxiaFailureKind = "canceled"
	BaxiaFailureInternal       BaxiaFailureKind = "internal"
)

// BaxiaFailure 保留 errors.Is 链，但 Error 始终不包含底层正文或代理凭据。
type BaxiaFailure struct {
	Kind  BaxiaFailureKind
	Cause error
}

func (failure *BaxiaFailure) Error() string { return "Baxia execution failed" }
func (failure *BaxiaFailure) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.Cause
}

type BaxiaOptions struct {
	NewClient       func(baxia.ClientOptions) (baxia.Client, error)
	GenerateProfile func() (device.Profile, error)
	V8LibraryPath   string
	Timeout         time.Duration
}

// BaxiaGenerator 不持有跨请求资源；每次请求独立生成画像并释放下载器与 V8 会话。
type BaxiaGenerator struct{ options BaxiaOptions }

func NewBaxiaGenerator(options BaxiaOptions) (*BaxiaGenerator, error) {
	if options.NewClient == nil || options.GenerateProfile == nil {
		return nil, errors.New("baxia client factory and profile generator are required")
	}
	if strings.TrimSpace(options.V8LibraryPath) == "" {
		return nil, errors.New("baxia V8 library path is required")
	}
	if options.Timeout == 0 {
		options.Timeout = DefaultTimeout
	}
	if options.Timeout <= 0 {
		return nil, errors.New("baxia timeout must be positive")
	}
	return &BaxiaGenerator{options: options}, nil
}

// ValidateBaxiaURL 在创建任何下载资源前检查生成上下文，错误不回显 URL。
func ValidateBaxiaURL(value string) error {
	if value == "" || len(value) > MaxBaxiaURLBytes || strings.TrimSpace(value) != value {
		return errors.New("必须是 1..8192 字节的 URL")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Opaque != "" || parsed.Hostname() == "" || parsed.User != nil ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return errors.New("必须是无凭据的绝对 http/https URL")
	}
	return nil
}

func (generator *BaxiaGenerator) GenerateBaxia(ctx context.Context, request BaxiaRequest) (outcome BaxiaOutcome, err error) {
	if ctx == nil || ValidateBaxiaURL(request.PageURL) != nil || ValidateBaxiaURL(request.RequestURL) != nil {
		return BaxiaOutcome{}, &BaxiaFailure{Kind: BaxiaFailureInvalidRequest}
	}
	ctx, cancel := context.WithTimeout(ctx, generator.options.Timeout)
	defer cancel()
	started := time.Now()
	defer func() {
		if err == nil && ctx.Err() != nil {
			err = baxiaFailure(ctx, ctx.Err())
		}
		if err != nil {
			outcome = BaxiaOutcome{}
		} else {
			outcome.Elapsed = time.Since(started)
		}
	}()
	if ctx.Err() != nil {
		return BaxiaOutcome{}, baxiaFailure(ctx, ctx.Err())
	}
	profile, err := generator.options.GenerateProfile()
	if err != nil {
		return BaxiaOutcome{}, &BaxiaFailure{Kind: BaxiaFailureInternal, Cause: err}
	}
	client, openErr := generator.options.NewClient(baxia.ClientOptions{
		Proxy: request.Proxy, Timeout: max(time.Millisecond, min(generator.options.Timeout, 2*time.Minute)),
	})
	if client != nil {
		defer func() { err = joinBaxiaCloseError(err, client.Close()) }()
	}
	if openErr != nil {
		return BaxiaOutcome{}, baxiaFailure(ctx, openErr)
	}
	if client == nil {
		return BaxiaOutcome{}, &BaxiaFailure{Kind: BaxiaFailureInternal}
	}
	session, openErr := client.NewSession(ctx, baxia.Config{
		V8LibraryPath: generator.options.V8LibraryPath, Profile: profile.Clone(),
		PageURL: request.PageURL, Timeout: max(time.Millisecond, min(generator.options.Timeout, 30*time.Second)),
	})
	if session != nil {
		defer func() { err = joinBaxiaCloseError(err, session.Close()) }()
	}
	if openErr != nil {
		return BaxiaOutcome{}, baxiaFailure(ctx, openErr)
	}
	if session == nil {
		return BaxiaOutcome{}, &BaxiaFailure{Kind: BaxiaFailureInternal}
	}
	result, tokenErr := session.Token(ctx, request.RequestURL)
	if tokenErr != nil {
		return BaxiaOutcome{}, baxiaFailure(ctx, tokenErr)
	}
	return BaxiaOutcome{
		Result: result, Proxied: request.Proxy != "",
		UAHeaders: map[string]string{
			"User-Agent": profile.UserAgent, "Accept-Language": profile.AcceptLanguage(),
			"Sec-CH-UA": profile.SecCHUA(), "Sec-CH-UA-Mobile": profile.SecCHUAMobile(),
			"Sec-CH-UA-Platform": profile.SecCHUAPlatform(),
		},
	}, nil
}

func baxiaFailure(ctx context.Context, cause error) error {
	kind := BaxiaFailureInternal
	switch {
	case ctx.Err() != nil:
		kind, cause = BaxiaFailureCanceled, ctx.Err()
	case errors.Is(cause, context.Canceled), errors.Is(cause, context.DeadlineExceeded):
		kind = BaxiaFailureCanceled
	case errors.Is(cause, baxia.ErrSource):
		kind = BaxiaFailureSource
	case errors.Is(cause, baxia.ErrUnsupportedSDK):
		kind = BaxiaFailureUnsupportedSDK
	case errors.Is(cause, baxia.ErrRuntime), errors.Is(cause, baxia.ErrToken), errors.Is(cause, baxia.ErrClosed):
		kind = BaxiaFailureRuntime
	}
	// ErrConfig 也可能来自服务端库路径、画像和初始化设置，不能归为用户输入错误。
	return &BaxiaFailure{Kind: kind, Cause: cause}
}

func joinBaxiaCloseError(previous, closeErr error) error {
	if closeErr == nil {
		return previous
	}
	return errors.Join(previous, &BaxiaFailure{Kind: BaxiaFailureRuntime, Cause: closeErr})
}
