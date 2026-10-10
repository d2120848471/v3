package waf

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/application/service"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/pe"
	domainwaf "github.com/d2120848471/v3/ali-slider-go/internal/domain/waf"
	"github.com/d2120848471/v3/ali-slider-go/internal/foundation/runtimekit"
	"github.com/d2120848471/v3/ali-slider-go/internal/infrastructure/engine"
	"github.com/d2120848471/v3/ali-slider-go/internal/infrastructure/httpclient"
)

type requestTransportPool interface {
	Get(string) (*http.Transport, bool, error)
	CloseIdleConnections()
}

type requestResolver interface {
	SolveWAF(context.Context, http.RoundTripper, device.Profile, engine.WAFRuntimeOptions) (domainwaf.Result, error)
	Close() error
}

// Executor 的画像、连接池与 KeyResolver 均按请求创建，构造时不联网或加载运行库。
type Executor struct {
	timeout         time.Duration
	sources         runtimekit.Sources
	generateProfile func() (device.Profile, error)
	newPool         func() (requestTransportPool, error)
	newResolver     func() requestResolver
	fetch           func(context.Context, *http.Transport, device.Profile, string) ([]byte, error)
}

func NewExecutor(libraryPath string, timeout time.Duration, sources runtimekit.Sources) (*Executor, error) {
	if strings.TrimSpace(libraryPath) == "" {
		return nil, errors.New("WAF V8 library path is required")
	}
	if timeout == 0 {
		timeout = service.DefaultTimeout
	}
	if timeout <= 0 {
		return nil, errors.New("WAF timeout must be positive")
	}
	if err := sources.Validate(); err != nil {
		return nil, err
	}
	return &Executor{
		timeout: timeout, sources: sources,
		generateProfile: func() (device.Profile, error) { return device.GenerateProfile(sources.Entropy) },
		newPool: func() (requestTransportPool, error) {
			return httpclient.NewTransportPool(1, 4)
		},
		newResolver: func() requestResolver { return engine.NewKeyResolver(libraryPath) },
		fetch:       FetchPage,
	}, nil
}

func (executor *Executor) SolveWAF(ctx context.Context, request service.WAFRequest) (outcome service.WAFOutcome, err error) {
	if ctx == nil || service.ValidateWAFURL(request.PageURL) != nil {
		return service.WAFOutcome{}, &service.WAFFailure{Kind: service.WAFFailureInvalidRequest}
	}
	ctx, cancel := context.WithTimeout(ctx, executor.timeout)
	defer cancel()
	started := time.Now()
	defer func() {
		if err == nil && ctx.Err() != nil {
			err = executionFailure(ctx, ctx.Err())
		}
		if err != nil {
			outcome = service.WAFOutcome{}
		} else {
			outcome.Elapsed = time.Since(started)
		}
	}()
	if err := ctx.Err(); err != nil {
		return service.WAFOutcome{}, executionFailure(ctx, err)
	}
	profile, err := executor.generateProfile()
	if err != nil {
		return service.WAFOutcome{}, &service.WAFFailure{Kind: service.WAFFailureInternal, Cause: err}
	}
	pool, err := executor.newPool()
	if pool != nil {
		defer pool.CloseIdleConnections()
	}
	if err != nil {
		return service.WAFOutcome{}, &service.WAFFailure{Kind: service.WAFFailureInternal, Cause: err}
	}
	if pool == nil {
		return service.WAFOutcome{}, &service.WAFFailure{Kind: service.WAFFailureInternal}
	}
	transport, proxied, err := pool.Get(request.Proxy)
	if err != nil {
		return service.WAFOutcome{}, &service.WAFFailure{Kind: service.WAFFailureInvalidRequest, Cause: err}
	}
	if transport == nil {
		return service.WAFOutcome{}, &service.WAFFailure{Kind: service.WAFFailureInternal}
	}
	resolver := executor.newResolver()
	if resolver == nil {
		return service.WAFOutcome{}, &service.WAFFailure{Kind: service.WAFFailureInternal}
	}
	defer func() {
		if closeErr := resolver.Close(); closeErr != nil {
			err = errors.Join(err, &service.WAFFailure{Kind: service.WAFFailureRuntime, Cause: closeErr})
		}
	}()
	page, err := executor.fetch(ctx, transport, profile.Clone(), request.PageURL)
	if err != nil {
		return service.WAFOutcome{}, executionFailure(ctx, err)
	}
	challenge, err := ParseChallenge(page)
	if err != nil {
		return service.WAFOutcome{}, executionFailure(ctx, err)
	}
	result, err := resolver.SolveWAF(ctx, transport, profile.Clone(), engine.WAFRuntimeOptions{
		PageURL: request.PageURL, Challenge: challenge, Sources: executor.sources,
		Timeout: min(executor.timeout, 5*time.Minute),
	})
	if err != nil {
		return service.WAFOutcome{}, executionFailure(ctx, err)
	}
	if result.VerifyResult && (result.Signature == "" || result.CertifyID == "") {
		return service.WAFOutcome{}, executionFailure(ctx, pe.ErrKeyRuntime)
	}
	outcome = service.WAFOutcome{
		OK: result.VerifyResult, SceneID: challenge.SceneID, CaptchaType: result.CaptchaType, VerifyCode: result.VerifyCode,
		Proxied: proxied,
		UAHeaders: map[string]string{
			"User-Agent": profile.UserAgent, "Accept-Language": profile.AcceptLanguage(),
			"Sec-CH-UA": profile.SecCHUA(), "Sec-CH-UA-Mobile": profile.SecCHUAMobile(),
			"Sec-CH-UA-Platform": profile.SecCHUAPlatform(),
		},
	}
	if result.VerifyResult {
		outcome.UAToken, outcome.UASig = challenge.Token, result.Signature
	}
	return outcome, nil
}

func executionFailure(ctx context.Context, cause error) error {
	kind := service.WAFFailureInternal
	switch {
	case ctx.Err() != nil:
		kind, cause = service.WAFFailureCanceled, ctx.Err()
	case errors.Is(cause, context.Canceled), errors.Is(cause, context.DeadlineExceeded):
		kind = service.WAFFailureCanceled
	case errors.Is(cause, ErrUnsafePageTarget):
		kind = service.WAFFailureInvalidRequest
	case errors.Is(cause, ErrPageSource), errors.Is(cause, pe.ErrKeyNetwork):
		kind = service.WAFFailureSource
	case errors.Is(cause, ErrUnsupportedPage), errors.Is(cause, pe.ErrUnsupportedPE):
		kind = service.WAFFailureUnsupported
	case errors.Is(cause, pe.ErrKeyRuntime):
		kind = service.WAFFailureRuntime
	}
	return &service.WAFFailure{Kind: kind, Cause: cause}
}
