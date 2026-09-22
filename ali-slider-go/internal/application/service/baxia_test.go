package service

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/domain/baxia"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/device"
)

type baxiaClientStub struct {
	newSession func(context.Context, baxia.Config) (baxia.Session, error)
	close      func() error
}

func (client baxiaClientStub) NewSession(ctx context.Context, config baxia.Config) (baxia.Session, error) {
	return client.newSession(ctx, config)
}
func (client baxiaClientStub) Close() error { return client.close() }

type baxiaSessionStub struct {
	token func(context.Context, string) (baxia.Result, error)
	close func() error
}

func (session baxiaSessionStub) Token(ctx context.Context, requestURL string) (baxia.Result, error) {
	return session.token(ctx, requestURL)
}
func (baxiaSessionStub) Profile() baxia.RuntimeProfile { return baxia.RuntimeProfile{} }
func (session baxiaSessionStub) Close() error          { return session.close() }

func baxiaDeviceFixture() device.Profile {
	return device.Profile{
		UserAgent: "fixture-mobile-UA", Mobile: true, UAPlatform: "Android",
		Language: "zh-CN", Languages: []string{"zh-CN", "zh", "en-US"},
		UABrands: []device.UABrand{{Brand: "Chromium", Version: "150"}},
	}
}

func newBaxiaFixture(t *testing.T, options BaxiaOptions) *BaxiaGenerator {
	t.Helper()
	if options.V8LibraryPath == "" {
		options.V8LibraryPath = "/server/libv8-fixture"
	}
	if options.GenerateProfile == nil {
		options.GenerateProfile = func() (device.Profile, error) { return baxiaDeviceFixture(), nil }
	}
	generator, err := NewBaxiaGenerator(options)
	if err != nil {
		t.Fatal(err)
	}
	return generator
}

func baxiaRequestFixture() BaxiaRequest {
	return BaxiaRequest{PageURL: "https://page.example/login#form", RequestURL: "https://api.example/login"}
}

func TestBaxiaRequestOwnsProfileProxyAndResources(t *testing.T) {
	for _, timeout := range []time.Duration{8 * time.Second, 5 * time.Minute} {
		t.Run(timeout.String(), func(t *testing.T) {
			var profiles, clients int
			var events []string
			request := baxiaRequestFixture()
			request.Proxy = "socks5h://username:password@proxy.example:1080"
			result := baxia.Result{
				Token: "token", Version: 7, SDKURL: "https://g.alicdn.com/AWSC/fireyejs/1.2.3/fireyejs.js",
				SDKHash: "sdk-sha", AWSCHash: "awsc-sha",
				Profile: baxia.RuntimeProfile{Version: 7, Prefix: "prefix", Alphabet: "alphabet", SDKHash: "sdk-sha"},
			}
			generator := newBaxiaFixture(t, BaxiaOptions{
				Timeout: timeout,
				GenerateProfile: func() (device.Profile, error) {
					profiles++
					profile := baxiaDeviceFixture()
					profile.UserAgent = fmt.Sprintf("request-UA-%d", profiles)
					return profile, nil
				},
				NewClient: func(options baxia.ClientOptions) (baxia.Client, error) {
					clients++
					if options.Proxy != request.Proxy || options.Timeout != min(timeout, 2*time.Minute) {
						t.Fatalf("download options not preserved: proxy match=%t timeout=%s", options.Proxy == request.Proxy, options.Timeout)
					}
					return baxiaClientStub{
						newSession: func(ctx context.Context, config baxia.Config) (baxia.Session, error) {
							if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > timeout {
								t.Fatalf("request deadline not propagated: %v %t", deadline, ok)
							}
							wantProfile := baxiaDeviceFixture()
							wantProfile.UserAgent = fmt.Sprintf("request-UA-%d", profiles)
							if !reflect.DeepEqual(config.Profile, wantProfile) || config.PageURL != request.PageURL || config.V8LibraryPath != "/server/libv8-fixture" {
								t.Fatal("session did not receive the request profile and server configuration")
							}
							if len(config.SDKSource) != 0 || config.UAOptions != nil || config.Timeout != min(timeout, 30*time.Second) {
								t.Fatal("session should dynamically download the SDK with bounded initialization")
							}
							return baxiaSessionStub{
								token: func(_ context.Context, requestURL string) (baxia.Result, error) {
									if requestURL != request.RequestURL {
										t.Fatal("token request URL not preserved")
									}
									return result, nil
								},
								close: func() error { events = append(events, "session"); return nil },
							}, nil
						},
						close: func() error { events = append(events, "client"); return nil },
					}, nil
				},
			})
			for index := 1; index <= 2; index++ {
				outcome, err := generator.GenerateBaxia(context.Background(), request)
				if err != nil {
					t.Fatal(err)
				}
				wantHeaders := map[string]string{
					"User-Agent": fmt.Sprintf("request-UA-%d", index), "Accept-Language": "zh-CN,zh;q=0.9,en-US;q=0.8",
					"Sec-CH-UA": `"Chromium";v="150"`, "Sec-CH-UA-Mobile": "?1", "Sec-CH-UA-Platform": `"Android"`,
				}
				if !reflect.DeepEqual(outcome.UAHeaders, wantHeaders) || outcome.Result != result || !outcome.Proxied || outcome.Elapsed <= 0 {
					t.Fatal("result, headers, proxy flag or elapsed time does not match the request")
				}
			}
			if profiles != 2 || clients != 2 || !reflect.DeepEqual(events, []string{"session", "client", "session", "client"}) {
				t.Fatalf("resources not isolated and closed: profiles=%d clients=%d events=%v", profiles, clients, events)
			}
		})
	}
}

func TestBaxiaCancellationReachesDownloadAndToken(t *testing.T) {
	for _, stage := range []string{"before", "download", "token", "timeout"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var clientCloses, sessionCloses, profiles atomic.Int32
			entered := make(chan struct{})
			block := func(ctx context.Context) error {
				close(entered)
				<-ctx.Done()
				return ctx.Err()
			}
			options := BaxiaOptions{
				GenerateProfile: func() (device.Profile, error) { profiles.Add(1); return baxiaDeviceFixture(), nil },
				NewClient: func(baxia.ClientOptions) (baxia.Client, error) {
					return baxiaClientStub{
						newSession: func(ctx context.Context, _ baxia.Config) (baxia.Session, error) {
							if stage == "download" {
								return nil, block(ctx)
							}
							return baxiaSessionStub{
								token: func(ctx context.Context, _ string) (baxia.Result, error) { return baxia.Result{}, block(ctx) },
								close: func() error { sessionCloses.Add(1); return nil },
							}, nil
						},
						close: func() error { clientCloses.Add(1); return nil },
					}, nil
				},
			}
			if stage == "before" {
				cancel()
			}
			if stage == "timeout" {
				options.Timeout = 15 * time.Millisecond
			}
			generator := newBaxiaFixture(t, options)
			done := make(chan error, 1)
			go func() {
				_, err := generator.GenerateBaxia(ctx, baxiaRequestFixture())
				done <- err
			}()
			if stage != "before" {
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("execution did not enter blocking dependency")
				}
				if stage != "timeout" {
					cancel()
				}
			}
			select {
			case err := <-done:
				wantCause := context.Canceled
				if stage == "timeout" {
					wantCause = context.DeadlineExceeded
				}
				assertBaxiaFailure(t, err, BaxiaFailureCanceled, wantCause)
			case <-time.After(time.Second):
				t.Fatal("cancellation did not finish request")
			}
			wantClients, wantSessions := int32(1), int32(1)
			if stage == "before" {
				wantClients, wantSessions = 0, 0
				if profiles.Load() != 0 {
					t.Fatal("canceled request generated a profile")
				}
			} else if stage == "download" {
				wantSessions = 0
			}
			if clientCloses.Load() != wantClients || sessionCloses.Load() != wantSessions {
				t.Fatalf("closes=%d/%d want=%d/%d", clientCloses.Load(), sessionCloses.Load(), wantClients, wantSessions)
			}
		})
	}
}

func TestBaxiaFailureAndCloseNeverReturnPartialToken(t *testing.T) {
	for _, stage := range []string{"profile", "client", "session", "token", "session-close", "client-close"} {
		t.Run(stage, func(t *testing.T) {
			cause := errors.New("secret-proxy-password and upstream body")
			var clientCloses, sessionCloses int
			generator := newBaxiaFixture(t, BaxiaOptions{
				GenerateProfile: func() (device.Profile, error) {
					if stage == "profile" {
						return device.Profile{}, cause
					}
					return baxiaDeviceFixture(), nil
				},
				NewClient: func(baxia.ClientOptions) (baxia.Client, error) {
					client := baxiaClientStub{
						newSession: func(context.Context, baxia.Config) (baxia.Session, error) {
							session := baxiaSessionStub{
								token: func(context.Context, string) (baxia.Result, error) {
									if stage == "token" {
										return baxia.Result{Token: "partial-token"}, cause
									}
									return baxia.Result{Token: "partial-token"}, nil
								},
								close: func() error {
									sessionCloses++
									if stage == "session-close" {
										return cause
									}
									return nil
								},
							}
							if stage == "session" {
								return session, cause
							}
							return session, nil
						},
						close: func() error {
							clientCloses++
							if stage == "client-close" {
								return cause
							}
							return nil
						},
					}
					if stage == "client" {
						return client, cause
					}
					return client, nil
				},
			})
			outcome, err := generator.GenerateBaxia(context.Background(), baxiaRequestFixture())
			kind := BaxiaFailureInternal
			if strings.HasSuffix(stage, "-close") {
				kind = BaxiaFailureRuntime
			}
			assertBaxiaFailure(t, err, kind, cause)
			if !reflect.DeepEqual(outcome, BaxiaOutcome{}) || strings.Contains(err.Error(), "secret") {
				t.Fatal("failed request returned token or exposed diagnostic details")
			}
			wantClients, wantSessions := 1, 1
			if stage == "profile" {
				wantClients, wantSessions = 0, 0
			}
			if stage == "client" {
				wantSessions = 0
			}
			if clientCloses != wantClients || sessionCloses != wantSessions {
				t.Fatalf("resources not closed after %s: client=%d session=%d", stage, clientCloses, sessionCloses)
			}
		})
	}
}

func TestBaxiaErrorClassification(t *testing.T) {
	for _, test := range []struct {
		cause error
		kind  BaxiaFailureKind
	}{
		{baxia.ErrSource, BaxiaFailureSource}, {baxia.ErrUnsupportedSDK, BaxiaFailureUnsupportedSDK},
		{baxia.ErrRuntime, BaxiaFailureRuntime}, {baxia.ErrToken, BaxiaFailureRuntime}, {baxia.ErrClosed, BaxiaFailureRuntime},
		{baxia.ErrConfig, BaxiaFailureInternal}, {context.Canceled, BaxiaFailureCanceled}, {context.DeadlineExceeded, BaxiaFailureCanceled},
	} {
		t.Run(string(test.kind)+"/"+test.cause.Error(), func(t *testing.T) {
			err := baxiaFailure(context.Background(), fmt.Errorf("secret diagnosis: %w", test.cause))
			assertBaxiaFailure(t, err, test.kind, test.cause)
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("failure exposed cause")
			}
		})
	}
}

func TestBaxiaInvalidRequestDoesNotCreateResources(t *testing.T) {
	var calls int
	generator := newBaxiaFixture(t, BaxiaOptions{NewClient: func(baxia.ClientOptions) (baxia.Client, error) {
		calls++
		return nil, errors.New("unexpected client")
	}})
	for _, request := range []BaxiaRequest{
		{}, {PageURL: "https://page.example", RequestURL: "file:///tmp/input"},
		{PageURL: "https://user:password@page.example", RequestURL: "https://api.example"},
	} {
		_, err := generator.GenerateBaxia(context.Background(), request)
		assertBaxiaFailure(t, err, BaxiaFailureInvalidRequest, nil)
	}
	if calls != 0 {
		t.Fatal("invalid request created a client")
	}
}

func assertBaxiaFailure(t *testing.T, err error, kind BaxiaFailureKind, cause error) {
	t.Helper()
	var failure *BaxiaFailure
	if !errors.As(err, &failure) || failure.Kind != kind || (cause != nil && !errors.Is(err, cause)) {
		t.Fatalf("failure mismatch: error=%v kind=%s cause preserved=%t", err, kind, errors.Is(err, cause))
	}
}
