package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/application/service"
	"github.com/d2120848471/v3/ali-slider-go/internal/application/solve"
)

type wafExecutorFunc func(context.Context, service.WAFRequest) (service.WAFOutcome, error)

func (execute wafExecutorFunc) SolveWAF(ctx context.Context, request service.WAFRequest) (service.WAFOutcome, error) {
	return execute(ctx, request)
}

func newWAFHandler(t *testing.T, executor service.WAFExecutor, mutate func(*Options)) *Handler {
	t.Helper()
	return newTestHandler(t, solverFunc(func(context.Context, solve.Request) (solve.Outcome, error) {
		t.Error("WAF request reached slider executor")
		return solve.Outcome{}, nil
	}), func(options *Options) {
		options.WAF = executor
		options.Baxia = baxiaExecutorFunc(func(context.Context, service.BaxiaRequest) (service.BaxiaOutcome, error) {
			t.Error("WAF request reached Baxia executor")
			return service.BaxiaOutcome{}, nil
		})
		if mutate != nil {
			mutate(options)
		}
	})
}

func wafRequestJSON() string { return `{"pageUrl":"https://example.com/pc/index.html?orgId=fixture"}` }

func TestWAFRequiresPageURL(t *testing.T) {
	handler := newTestHandler(t, solverFunc(func(context.Context, solve.Request) (solve.Outcome, error) {
		t.Error("WAF request reached slider executor")
		return solve.Outcome{}, nil
	}), nil)
	response := performRequest(handler, http.MethodPost, "/api/waf", `{}`)
	if response.Code != http.StatusBadRequest || decodeObject(t, response)["errorType"] != "ApiRequestError" {
		t.Fatalf("missing WAF pageUrl response=%d %s", response.Code, response.Body.String())
	}
}

func TestWAFHTTPContractAndUnknownFields(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(fmt.Sprint(accepted), func(t *testing.T) {
			var received service.WAFRequest
			outcome := service.WAFOutcome{
				OK: accepted, SceneID: "fixture-waf-scene", CaptchaType: "SLIDING", UAToken: "fixture-token", UASig: "fixture-signature",
				VerifyCode: "T001",
				UAHeaders:  map[string]string{"User-Agent": "fixture-user-agent"}, Proxied: true, Elapsed: 123 * time.Millisecond,
			}
			if !accepted {
				outcome.VerifyCode, outcome.UAToken, outcome.UASig = "F015", "", ""
			}
			handler := newWAFHandler(t, wafExecutorFunc(func(_ context.Context, request service.WAFRequest) (service.WAFOutcome, error) {
				received = request
				return outcome, nil
			}), nil)
			response := performRequest(handler, http.MethodPost, WAFPath+"?pageUrl=https://ignored.example.com", `{
				"pageUrl":" https://example.com/pc/index.html?orgId=fixture ", "Proxy":"proxy.example:8080",
				"SceneId":"ignored", "sceneId":null, "userId":0, "userUserId":false,
				"traceid":"ignored", "token":"ignored", "region":"ignored", "language":"ignored", "prefix":"ignored", "requestUrl":"https://never.example.com", "type":"POST",
				"sdkSource":"never execute", "sdkPath":"/not/read", "v8LibraryPath":"/not/loaded", "future":{"ignored":true}
			}`)
			if response.Code != http.StatusOK || received != (service.WAFRequest{PageURL: "https://example.com/pc/index.html?orgId=fixture", Proxy: "http://proxy.example:8080"}) {
				t.Fatalf("request contract response=%d normalized=%t", response.Code, received.PageURL == "https://example.com/pc/index.html?orgId=fixture")
			}
			traceID := response.Header().Get("X-Trace-ID")
			want := map[string]any{
				"ok": accepted, "sceneId": outcome.SceneID, "captchaType": "SLIDING", "u_atoken": outcome.UAToken, "u_asig": outcome.UASig,
				"verifyCode": outcome.VerifyCode,
				"uaHeaders":  map[string]any{"User-Agent": "fixture-user-agent"}, "proxied": true, "elapsedMs": float64(123), "traceId": traceID,
			}
			if len(traceID) != 12 || !reflect.DeepEqual(decodeObject(t, response), want) {
				t.Fatalf("response contract=%#v", decodeObject(t, response))
			}
			assertBaxiaPrivateHeaders(t, response)
		})
	}
}

func TestWAFURLValidationAndRedactedContracts(t *testing.T) {
	for _, value := range []string{
		"https://example.com/", "https://example.com:443/page?orgId=fixture", "https://sub.example.com./page",
		"https://xn--fsqu00a.com/path", "https://example.com/path#",
	} {
		err := service.ValidateWAFURL(value)
		if strings.HasSuffix(value, "#") {
			if err == nil {
				t.Fatal("empty fragment accepted")
			}
		} else if err != nil {
			t.Fatalf("valid public HTTPS URL rejected: %v", err)
		}
	}
	var previous string
	for _, value := range []string{
		"", " https://example.com", "https://example.com ", "/page", "https:page", "http://example.com", "file:///tmp/sdk",
		"https://user:secret@example.com", "https://example.com:8443", "https://example.com:", "https://example.com:0443",
		"https://example.com#secret", "https://127.0.0.1", "https://[::1]", "https://127.1", "https://2130706433",
		"https://localhost", "https://api.localhost", "https://example.local", "https://example.internal", "https://example.test",
		"https://api", "https://-bad.example.com", "https://bad_.example.com", "https://例子.com", "https://example.com/\nsecret",
		"https://example.com/" + strings.Repeat("界", service.MaxWAFURLBytes/3),
	} {
		err := service.ValidateWAFURL(value)
		if err == nil {
			t.Fatalf("invalid URL accepted: %q", value)
		}
		if previous != "" && err.Error() != previous {
			t.Fatal("URL validation must return a fixed diagnostic")
		}
		previous = err.Error()
	}
	secret := errors.New("upstream-secret")
	failure := &service.WAFFailure{Kind: service.WAFFailureSource, Cause: secret}
	if !errors.Is(failure, secret) || strings.Contains(failure.Error(), "secret") {
		t.Fatal("WAF failure must preserve errors.Is without exposing cause")
	}
	for _, value := range []any{
		service.WAFRequest{PageURL: "https://example.com/?secret=value", Proxy: "http://user:secret@proxy.example"},
		service.WAFOutcome{UAToken: "secret-token", UASig: "secret-signature"},
	} {
		if strings.Contains(fmt.Sprintf("%v %#v", value, value), "secret") {
			t.Fatal("formatted WAF request or outcome leaked sensitive fields")
		}
	}
}

func TestWAFHTTPInvalidInputsDoNotExecute(t *testing.T) {
	invalidBodies := map[string]string{
		"empty": "", "null": "null", "array": "[]", "scalar": `"text"`, "malformed": "{", "trailing": wafRequestJSON() + " {}", "missing": `{}`,
	}
	for name, value := range map[string]any{
		"empty": "", "null": nil, "number": 1, "relative": "/page", "http": "http://example.com", "credentials": "https://user:secret@example.com",
		"port": "https://example.com:8443", "ip": "https://127.0.0.1", "local": "https://api.localhost", "fragment": "https://example.com#secret",
		"over limit": "https://example.com/" + strings.Repeat("界", service.MaxWAFURLBytes/3),
	} {
		encoded, err := json.Marshal(map[string]any{"pageUrl": value})
		if err != nil {
			t.Fatal(err)
		}
		invalidBodies["pageUrl/"+name] = string(encoded)
	}
	for name, value := range map[string]any{
		"number": 1, "scheme": "ftp://proxy.example", "path": "http://proxy.example/path", "port": "http://proxy.example:65536",
		"query": "http://proxy.example?password=secret", "socks4 password": "socks4://user:secret@proxy.example", "socks5 no password": "socks5://user@proxy.example",
	} {
		encoded, err := json.Marshal(map[string]any{"pageUrl": "https://example.com", "proxy": value})
		if err != nil {
			t.Fatal(err)
		}
		invalidBodies["proxy/"+name] = string(encoded)
	}
	for name, body := range invalidBodies {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			var logs bytes.Buffer
			handler := newWAFHandler(t, wafExecutorFunc(func(context.Context, service.WAFRequest) (service.WAFOutcome, error) {
				calls.Add(1)
				return service.WAFOutcome{}, nil
			}), func(options *Options) { options.Logger = log.New(&logs, "", 0) })
			response := performRequest(handler, http.MethodPost, WAFPath, body)
			got := decodeObject(t, response)
			if response.Code != http.StatusBadRequest || calls.Load() != 0 || got["errorType"] != "ApiRequestError" || got["ok"] != false || len(got) != 4 {
				t.Fatalf("invalid input response=%d calls=%d contract=%#v", response.Code, calls.Load(), got)
			}
			if strings.Contains(response.Body.String(), "secret") || strings.Contains(logs.String(), "secret") {
				t.Fatal("input diagnostic leaked a field value")
			}
			assertBaxiaPrivateHeaders(t, response)
		})
	}
}

func TestWAFProxyAliasesAndBodyLimits(t *testing.T) {
	for _, test := range []struct{ fields, want string }{
		{`"Proxy":"proxy.example:8080"`, "http://proxy.example:8080"},
		{`"proxy":" SOCKS5H://user:secret@proxy.example:1080 "`, "socks5h://user:secret@proxy.example:1080"},
		{`"proxy":"","Proxy":"ignored.example:8080"`, ""},
		{`"proxy":null,"Proxy":"ignored.example:8080"`, ""},
	} {
		var received string
		handler := newWAFHandler(t, wafExecutorFunc(func(_ context.Context, request service.WAFRequest) (service.WAFOutcome, error) {
			received = request.Proxy
			return service.WAFOutcome{}, nil
		}), nil)
		response := performRequest(handler, http.MethodPost, WAFPath, strings.TrimSuffix(wafRequestJSON(), "}")+","+test.fields+"}")
		if response.Code != http.StatusOK || received != test.want {
			t.Fatalf("proxy alias status=%d preserved=%t", response.Code, received == test.want)
		}
	}
	var calls atomic.Int32
	handler := newWAFHandler(t, wafExecutorFunc(func(context.Context, service.WAFRequest) (service.WAFOutcome, error) {
		calls.Add(1)
		return service.WAFOutcome{}, nil
	}), nil)
	prefix := "https://example.com/"
	maxURL := prefix + strings.Repeat("x", service.MaxWAFURLBytes-len(prefix))
	encoded, _ := json.Marshal(map[string]string{"pageUrl": maxURL})
	if response := performRequest(handler, http.MethodPost, WAFPath, string(encoded)); response.Code != http.StatusOK {
		t.Fatalf("maximum URL rejected: %s", response.Body.String())
	}
	base := wafRequestJSON()
	atLimit := base + strings.Repeat(" ", int(MaxRequestBytes)-len(base))
	if response := performRequest(handler, http.MethodPost, WAFPath, atLimit); response.Code != http.StatusOK {
		t.Fatal("maximum JSON body rejected")
	}
	for _, contentLength := range []int64{-1, MaxRequestBytes + 1} {
		request := httptest.NewRequest(http.MethodPost, WAFPath, strings.NewReader(atLimit+" "))
		request.ContentLength = contentLength
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("oversized body accepted with ContentLength %d", contentLength)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("executor calls=%d want=2", calls.Load())
	}
}

func TestWAFOriginAndMethodBoundaries(t *testing.T) {
	var calls atomic.Int32
	handler := newWAFHandler(t, wafExecutorFunc(func(context.Context, service.WAFRequest) (service.WAFOutcome, error) {
		calls.Add(1)
		return service.WAFOutcome{}, nil
	}), nil)
	for _, test := range []struct {
		origin, site string
		status       int
	}{
		{"", "", http.StatusOK}, {"http://api.example", "same-origin", http.StatusOK}, {"", "none", http.StatusOK},
		{"https://foreign.example", "", http.StatusForbidden}, {"null", "", http.StatusForbidden},
		{"", "cross-site", http.StatusForbidden}, {"", "same-site", http.StatusForbidden},
	} {
		request := httptest.NewRequest(http.MethodPost, "http://api.example"+WAFPath, strings.NewReader(wafRequestJSON()))
		request.Header.Set("Origin", test.origin)
		request.Header.Set("Sec-Fetch-Site", test.site)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != test.status || (test.status == http.StatusForbidden && decodeObject(t, response)["errorType"] != "ApiOriginError") {
			t.Fatalf("origin=%q site=%q status=%d", test.origin, test.site, response.Code)
		}
		assertBaxiaPrivateHeaders(t, response)
	}
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions} {
		if response := performRequest(handler, method, WAFPath, wafRequestJSON()); response.Code != http.StatusNotFound {
			t.Fatalf("%s accepted", method)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("executor calls=%d want=3", calls.Load())
	}
}

func TestWAFErrorsAndPanicAreSanitized(t *testing.T) {
	secret := errors.New("proxy-user:secret-password and upstream-secret-body")
	for _, test := range []struct {
		name   string
		err    error
		status int
		kind   string
	}{
		{"invalid", &service.WAFFailure{Kind: service.WAFFailureInvalidRequest, Cause: secret}, 400, "InvalidRequest"},
		{"source", &service.WAFFailure{Kind: service.WAFFailureSource, Cause: secret}, 500, "WAFSourceError"},
		{"unsupported", &service.WAFFailure{Kind: service.WAFFailureUnsupported, Cause: secret}, 500, "WAFUnsupportedError"},
		{"runtime", &service.WAFFailure{Kind: service.WAFFailureRuntime, Cause: secret}, 500, "WAFRuntimeError"},
		{"canceled", &service.WAFFailure{Kind: service.WAFFailureCanceled, Cause: secret}, 500, "WAFCanceled"},
		{"internal", &service.WAFFailure{Kind: service.WAFFailureInternal, Cause: secret}, 500, "InternalError"},
		{"unknown", secret, 500, "InternalError"}, {"panic", nil, 500, "InternalError"}, {"missing executor", nil, 500, "InternalError"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			var executor service.WAFExecutor = wafExecutorFunc(func(context.Context, service.WAFRequest) (service.WAFOutcome, error) {
				if test.name == "panic" {
					panic(secret)
				}
				return service.WAFOutcome{UAToken: "secret-partial-token", UASig: "secret-partial-signature"}, test.err
			})
			if test.name == "missing executor" {
				executor = nil
			}
			handler := newWAFHandler(t, executor, func(options *Options) { options.Logger = log.New(&logs, "", 0) })
			response := performRequest(handler, http.MethodPost, WAFPath, wafRequestJSON())
			body := decodeObject(t, response)
			if response.Code != test.status || body["errorType"] != test.kind || body["ok"] != false || len(body) != 4 {
				t.Fatalf("error response=%d %#v", response.Code, body)
			}
			if strings.Contains(response.Body.String(), "secret") || strings.Contains(logs.String(), "secret") {
				t.Fatal("execution diagnostic or partial signature leaked")
			}
			assertBaxiaPrivateHeaders(t, response)
		})
	}
}

func TestWAFRequestContextAndTimeoutReachExecutor(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprint(canceled), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if canceled {
				cancel()
			}
			var cause error
			handler := newWAFHandler(t, wafExecutorFunc(func(ctx context.Context, _ service.WAFRequest) (service.WAFOutcome, error) {
				<-ctx.Done()
				cause = ctx.Err()
				return service.WAFOutcome{}, cause
			}), func(options *Options) { options.Timeout = 15 * time.Millisecond })
			request := httptest.NewRequest(http.MethodPost, WAFPath, strings.NewReader(wafRequestJSON())).WithContext(ctx)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			want := context.DeadlineExceeded
			if canceled {
				want = context.Canceled
			}
			if cause != want || response.Code != http.StatusInternalServerError || decodeObject(t, response)["errorType"] != "WAFCanceled" {
				t.Fatal("request cancellation was not preserved")
			}
		})
	}
}
