package waf

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/application/service"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/pe"
	domainwaf "github.com/d2120848471/v3/ali-slider-go/internal/domain/waf"
	"github.com/d2120848471/v3/ali-slider-go/internal/foundation/runtimekit"
	"github.com/d2120848471/v3/ali-slider-go/internal/infrastructure/engine"
)

type requestPoolStub struct {
	get   func(string) (*http.Transport, bool, error)
	close func()
}

func (pool requestPoolStub) Get(proxy string) (*http.Transport, bool, error) { return pool.get(proxy) }
func (pool requestPoolStub) CloseIdleConnections()                           { pool.close() }

type requestResolverStub struct {
	solve func(context.Context, http.RoundTripper, device.Profile, engine.WAFRuntimeOptions) (domainwaf.Result, error)
	close func() error
}

func (resolver requestResolverStub) SolveWAF(ctx context.Context, transport http.RoundTripper, profile device.Profile, options engine.WAFRuntimeOptions) (domainwaf.Result, error) {
	return resolver.solve(ctx, transport, profile, options)
}
func (resolver requestResolverStub) Close() error { return resolver.close() }

type runtimeRoundTripFunc func(*http.Request) (*http.Response, error)

func (transport runtimeRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func fixtureExecutor(t *testing.T) *Executor {
	t.Helper()
	executor, err := NewExecutor("/missing-fixture-library", time.Second, runtimekit.NewSystemSources())
	if err != nil {
		t.Fatal(err)
	}
	executor.generateProfile = func() (device.Profile, error) { return fixtureProfile(), nil }
	executor.newPool = func() (requestTransportPool, error) {
		return requestPoolStub{
			get:   func(string) (*http.Transport, bool, error) { return &http.Transport{}, false, nil },
			close: func() {},
		}, nil
	}
	executor.newResolver = func() requestResolver {
		return requestResolverStub{
			solve: func(context.Context, http.RoundTripper, device.Profile, engine.WAFRuntimeOptions) (domainwaf.Result, error) {
				return domainwaf.Result{Signature: "fixture-signature", CertifyID: "fixture-certify", CaptchaType: "SLIDING", VerifyCode: "T001", VerifyResult: true}, nil
			},
			close: func() error { return nil },
		}
	}
	executor.fetch = func(context.Context, *http.Transport, device.Profile, string) ([]byte, error) {
		return fixturePage(fixtureRequestInfo), nil
	}
	return executor
}

func TestExecutorUsesOneProfileAndProxyAndClosesEachRequestsResources(t *testing.T) {
	executor := fixtureExecutor(t)
	generated, pools, resolvers, poolCloses, resolverCloses := 0, 0, 0, 0, 0
	executor.generateProfile = func() (device.Profile, error) {
		generated++
		profile := fixtureProfile()
		profile.UserAgent = fmt.Sprintf("fixture-browser-%d", generated)
		return profile, nil
	}
	var route *http.Transport
	executor.newPool = func() (requestTransportPool, error) {
		pools++
		route = &http.Transport{}
		return requestPoolStub{
			get: func(proxy string) (*http.Transport, bool, error) {
				if proxy != "fixture-proxy" {
					t.Fatal("request proxy was not forwarded")
				}
				return route, true, nil
			},
			close: func() { poolCloses++ },
		}, nil
	}
	var fetchedProfile device.Profile
	executor.fetch = func(_ context.Context, transport *http.Transport, profile device.Profile, pageURL string) ([]byte, error) {
		if transport != route || pageURL != "https://page.example.com/fixture" {
			t.Fatal("page fetch lost request context")
		}
		fetchedProfile = profile.Clone()
		return fixturePage(fixtureRequestInfo), nil
	}
	executor.newResolver = func() requestResolver {
		resolvers++
		return requestResolverStub{
			solve: func(_ context.Context, transport http.RoundTripper, profile device.Profile, options engine.WAFRuntimeOptions) (domainwaf.Result, error) {
				if transport != route || profile.UserAgent != fetchedProfile.UserAgent || profile.UserAgent != fmt.Sprintf("fixture-browser-%d", generated) {
					t.Fatal("runtime did not share page transport and profile")
				}
				if options.Challenge.Token != "fixture-token" || options.PageURL != "https://page.example.com/fixture" || options.Sources.Validate() != nil {
					t.Fatal("runtime did not receive the parsed challenge")
				}
				return domainwaf.Result{Signature: "fixture-signature", CertifyID: "fixture-certify", CaptchaType: "SLIDING", VerifyCode: "T001", VerifyResult: true}, nil
			},
			close: func() error { resolverCloses++; return nil },
		}
	}
	for call := 1; call <= 2; call++ {
		outcome, err := executor.SolveWAF(context.Background(), service.WAFRequest{PageURL: "https://page.example.com/fixture", Proxy: "fixture-proxy"})
		if err != nil || !outcome.OK || outcome.UAToken != "fixture-token" || outcome.UASig != "fixture-signature" ||
			outcome.SceneID != "fixture-scene" || outcome.CaptchaType != "SLIDING" || !outcome.Proxied || outcome.Elapsed <= 0 ||
			outcome.UAHeaders["User-Agent"] != fmt.Sprintf("fixture-browser-%d", call) {
			t.Fatalf("request outcome lost challenge or profile: valid=%t error=%v", outcome.OK, err)
		}
		if generated != call || pools != call || resolvers != call || poolCloses != call || resolverCloses != call {
			t.Fatalf("resources shared or leaked: profiles=%d pools=%d resolvers=%d closes=%d/%d", generated, pools, resolvers, poolCloses, resolverCloses)
		}
	}
}

func TestRuntimeChallengeValidationPrecedesSDKAndV8Loading(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*domainwaf.Challenge)
	}{
		{"missing sceneId", func(challenge *domainwaf.Challenge) { challenge.SceneID = "" }},
		{"blank token", func(challenge *domainwaf.Challenge) { challenge.Token = " \t\n" }},
		{"oversized token", func(challenge *domainwaf.Challenge) { challenge.Token = strings.Repeat("x", 4097) }},
		{"oversized language", func(challenge *domainwaf.Challenge) { challenge.Language = strings.Repeat("x", 65) }},
		{"unsupported region", func(challenge *domainwaf.Challenge) { challenge.Region = "private-region" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			resolver := engine.NewKeyResolver("/missing-fixture-library")
			defer resolver.Close()
			calls := 0
			transport := runtimeRoundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return nil, errors.New("fixture SDK fetch must not run")
			})
			challenge := domainwaf.Challenge{
				Type: "GET", SceneID: "direct-scene", UserID: "direct-user", UserUserID: "direct-user-user",
				TraceID: "direct-trace", Token: "direct-token", Region: "cn",
			}
			test.mutate(&challenge)
			result, err := resolver.SolveWAF(context.Background(), transport, fixtureProfile(), engine.WAFRuntimeOptions{
				PageURL: "https://page.example.com/", Challenge: challenge,
				Timeout: time.Second, Sources: runtimekit.NewSystemSources(),
			})
			if !errors.Is(err, pe.ErrUnsupportedPE) || calls != 0 || result != (domainwaf.Result{}) {
				t.Fatalf("runtime fetched SDK or attempted library loading before challenge validation: calls=%d error=%v", calls, err)
			}
			if strings.Contains(err.Error(), "private-region") {
				t.Fatal("runtime validation disclosed challenge data")
			}
		})
	}
}

func TestExecutorVerificationRejectionCompletesWithoutTokens(t *testing.T) {
	executor := fixtureExecutor(t)
	executor.newResolver = func() requestResolver {
		return requestResolverStub{
			solve: func(context.Context, http.RoundTripper, device.Profile, engine.WAFRuntimeOptions) (domainwaf.Result, error) {
				return domainwaf.Result{CaptchaType: "SLIDING", CertifyID: "fixture-certify", VerifyCode: "F001", Signature: "must-not-return"}, nil
			},
			close: func() error { return nil },
		}
	}
	outcome, err := executor.SolveWAF(context.Background(), service.WAFRequest{PageURL: "https://page.example.com/"})
	if err != nil || outcome.OK || outcome.UAToken != "" || outcome.UASig != "" || outcome.VerifyCode != "F001" || outcome.SceneID != "fixture-scene" || outcome.CaptchaType != "SLIDING" {
		t.Fatalf("verification rejection not represented as business result: error=%v", err)
	}
}

func TestExecutorErrorsCloseResourcesAndNeverReturnPartialOutcome(t *testing.T) {
	for _, test := range []struct {
		name       string
		fetchErr   error
		runtimeErr error
		closeErr   error
		kind       service.WAFFailureKind
	}{
		{"source", ErrPageSource, nil, nil, service.WAFFailureSource},
		{"private target", ErrUnsafePageTarget, nil, nil, service.WAFFailureInvalidRequest},
		{"unsupported page", ErrUnsupportedPage, nil, nil, service.WAFFailureUnsupported},
		{"runtime source", nil, pe.ErrKeyNetwork, nil, service.WAFFailureSource},
		{"unsupported runtime", nil, pe.ErrUnsupportedPE, nil, service.WAFFailureUnsupported},
		{"runtime", nil, pe.ErrKeyRuntime, nil, service.WAFFailureRuntime},
		{"canceled", nil, context.Canceled, nil, service.WAFFailureCanceled},
		{"close", nil, nil, errors.New("fixture-private-close-error"), service.WAFFailureRuntime},
	} {
		t.Run(test.name, func(t *testing.T) {
			executor := fixtureExecutor(t)
			poolClosed, resolverClosed := false, false
			executor.newPool = func() (requestTransportPool, error) {
				return requestPoolStub{
					get:   func(string) (*http.Transport, bool, error) { return &http.Transport{}, false, nil },
					close: func() { poolClosed = true },
				}, nil
			}
			executor.newResolver = func() requestResolver {
				return requestResolverStub{
					solve: func(context.Context, http.RoundTripper, device.Profile, engine.WAFRuntimeOptions) (domainwaf.Result, error) {
						return domainwaf.Result{Signature: "fixture-signature", CertifyID: "fixture-certify", CaptchaType: "SLIDING", VerifyResult: true}, test.runtimeErr
					},
					close: func() error { resolverClosed = true; return test.closeErr },
				}
			}
			executor.fetch = func(context.Context, *http.Transport, device.Profile, string) ([]byte, error) {
				return fixturePage(fixtureRequestInfo), test.fetchErr
			}
			outcome, err := executor.SolveWAF(context.Background(), service.WAFRequest{PageURL: "https://page.example.com/"})
			var failure *service.WAFFailure
			if !errors.As(err, &failure) || failure.Kind != test.kind || !poolClosed || !resolverClosed || outcome.UAToken != "" || outcome.UASig != "" || outcome.UAHeaders != nil || outcome.OK {
				t.Fatalf("failure leaked outcome or resources: kind=%v pool=%t resolver=%t error=%v", failure, poolClosed, resolverClosed, err)
			}
			if strings.Contains(err.Error(), "fixture-private") {
				t.Fatal("failure disclosed its cause")
			}
		})
	}
}

func TestExecutorInvalidOrCanceledRequestDoesNotCreateResources(t *testing.T) {
	executor := fixtureExecutor(t)
	executor.generateProfile = func() (device.Profile, error) {
		t.Fatal("invalid request generated a profile")
		return device.Profile{}, nil
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, test := range []struct {
		ctx  context.Context
		url  string
		kind service.WAFFailureKind
	}{
		{nil, "https://page.example.com/", service.WAFFailureInvalidRequest},
		{context.Background(), "http://page.example.com/", service.WAFFailureInvalidRequest},
		{context.Background(), "https://127.0.0.1/", service.WAFFailureInvalidRequest},
		{canceled, "https://page.example.com/", service.WAFFailureCanceled},
	} {
		_, err := executor.SolveWAF(test.ctx, service.WAFRequest{PageURL: test.url})
		var failure *service.WAFFailure
		if !errors.As(err, &failure) || failure.Kind != test.kind {
			t.Fatalf("invalid request failure: error=%v", err)
		}
	}
}
