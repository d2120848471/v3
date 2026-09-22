package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/baxia"
)

type baxiaExecutorFunc func(context.Context, service.BaxiaRequest) (service.BaxiaOutcome, error)

func (execute baxiaExecutorFunc) GenerateBaxia(ctx context.Context, request service.BaxiaRequest) (service.BaxiaOutcome, error) {
	return execute(ctx, request)
}

func newBaxiaHandler(t *testing.T, executor service.BaxiaExecutor, mutate func(*Options)) *Handler {
	t.Helper()
	return newTestHandler(t, solverFunc(func(context.Context, solve.Request) (solve.Outcome, error) {
		t.Error("Baxia request reached slider executor")
		return solve.Outcome{}, nil
	}), func(options *Options) {
		options.Baxia = executor
		if mutate != nil {
			mutate(options)
		}
	})
}

func baxiaRequestJSON() string {
	return `{"pageUrl":"https://page.example/login#form","requestUrl":"https://api.example/login"}`
}

func TestBaxiaHTTPContractAndUnknownFields(t *testing.T) {
	var received service.BaxiaRequest
	outcome := service.BaxiaOutcome{
		Result: baxia.Result{
			Token: "fixture-bx-ua", Version: 237,
			SDKURL: "https://g.alicdn.com/AWSC/fireyejs/2.5.37/fireyejs.js", SDKHash: "sdk-hash", AWSCHash: "awsc-hash",
			Profile: baxia.RuntimeProfile{Version: 237, Prefix: "prefix", Alphabet: "alphabet", SDKHash: "sdk-hash"},
		},
		UAHeaders: map[string]string{"User-Agent": "fixture-user-agent"}, Proxied: true, Elapsed: 123 * time.Millisecond,
	}
	handler := newBaxiaHandler(t, baxiaExecutorFunc(func(_ context.Context, request service.BaxiaRequest) (service.BaxiaOutcome, error) {
		received = request
		return outcome, nil
	}), nil)
	response := performRequest(handler, http.MethodPost, BaxiaPath+"?pageUrl=https://ignored.example", `{
		"pageUrl":" https://page.example/login#form ","requestUrl":"https://api.example/login",
		"Proxy":"proxy.example:8080","sdkSource":"never execute","sdkPath":"/not/read",
		"v8LibraryPath":"/not/loaded","profile":{"UserAgent":"ignored"},"future":{"ignored":true}
	}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if received != (service.BaxiaRequest{PageURL: "https://page.example/login#form", RequestURL: "https://api.example/login", Proxy: "http://proxy.example:8080"}) {
		t.Fatal("request fields were not normalized, or JSON was merged with the query")
	}
	body := decodeObject(t, response)
	traceID := response.Header().Get("X-Trace-ID")
	want := map[string]any{
		"ok": true, "bx-ua": "fixture-bx-ua", "version": float64(237),
		"sdkURL": outcome.Result.SDKURL, "sdkSha256": "sdk-hash", "awscSha256": "awsc-hash",
		"profile":   map[string]any{"version": float64(237), "prefix": "prefix", "alphabet": "alphabet", "sdkHash": "sdk-hash"},
		"uaHeaders": map[string]any{"User-Agent": "fixture-user-agent"}, "proxied": true,
		"elapsedMs": float64(123), "traceId": traceID,
	}
	if traceID == "" || !reflect.DeepEqual(body, want) {
		t.Fatalf("response contract=%#v", body)
	}
	assertBaxiaPrivateHeaders(t, response)
}

func TestBaxiaProxyAliasesMatchSlider(t *testing.T) {
	for _, test := range []struct{ fields, want string }{
		{`"Proxy":"proxy.example:8080"`, "http://proxy.example:8080"},
		{`"proxy":"https://user:password@proxy.example:8443/"`, "https://user:password@proxy.example:8443/"},
		{`"proxy":"socks4://user@proxy.example:1080"`, "socks4://user@proxy.example:1080"},
		{`"proxy":"socks5://user:password@proxy.example:1080"`, "socks5://user:password@proxy.example:1080"},
		{`"proxy":" SOCKS5H://user:password@proxy.example:1080 "`, "socks5h://user:password@proxy.example:1080"},
		{`"proxy":"","Proxy":"ignored.example:8080"`, ""},
		{`"proxy":null,"Proxy":"ignored.example:8080"`, ""},
		{`"proxy":"first.example:8080","proxy":"last.example:8080"`, "http://last.example:8080"},
	} {
		t.Run(test.fields, func(t *testing.T) {
			var received string
			handler := newBaxiaHandler(t, baxiaExecutorFunc(func(_ context.Context, request service.BaxiaRequest) (service.BaxiaOutcome, error) {
				received = request.Proxy
				return service.BaxiaOutcome{}, nil
			}), nil)
			body := strings.TrimSuffix(baxiaRequestJSON(), "}") + "," + test.fields + "}"
			response := performRequest(handler, http.MethodPost, BaxiaPath, body)
			if response.Code != http.StatusOK || received != test.want {
				t.Fatalf("status=%d proxy preserved=%t", response.Code, received == test.want)
			}
		})
	}
}

func TestBaxiaHTTPInvalidInputsDoNotExecute(t *testing.T) {
	valid := map[string]any{"pageUrl": "https://page.example/login", "requestUrl": "https://api.example/login"}
	invalidBodies := map[string]string{
		"empty": "", "null": "null", "array": "[]", "scalar": `"text"`, "malformed": "{", "trailing": baxiaRequestJSON() + " {}", "missing": `{}`,
	}
	for _, field := range []string{"pageUrl", "requestUrl"} {
		for name, value := range map[string]any{
			"empty": "", "null": nil, "number": 1, "relative": "/login", "opaque": "https:login", "credentials": "https://user:password@example.com/login",
			"file": "file:///tmp/sdk", "missing host": "https://:8080/", "control": "https://example.com/\nsecret",
			"over byte limit": "https://example.com/" + strings.Repeat("界", service.MaxBaxiaURLBytes/3),
		} {
			payload := map[string]any{"pageUrl": valid["pageUrl"], "requestUrl": valid["requestUrl"]}
			payload[field] = value
			encoded, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			invalidBodies[field+"/"+name] = string(encoded)
		}
	}
	for name, value := range map[string]any{
		"number": 1, "scheme": "ftp://proxy.example", "path": "http://proxy.example/path", "port": "http://proxy.example:65536",
		"query": "http://proxy.example?password=secret", "fragment": "http://proxy.example#secret",
		"socks4 password": "socks4://user:password@proxy.example", "socks5 no password": "socks5://user@proxy.example",
	} {
		payload := map[string]any{"pageUrl": valid["pageUrl"], "requestUrl": valid["requestUrl"], "proxy": value}
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		invalidBodies["proxy/"+name] = string(encoded)
	}
	for name, body := range invalidBodies {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			handler := newBaxiaHandler(t, baxiaExecutorFunc(func(context.Context, service.BaxiaRequest) (service.BaxiaOutcome, error) {
				calls.Add(1)
				return service.BaxiaOutcome{}, nil
			}), nil)
			response := performRequest(handler, http.MethodPost, BaxiaPath, body)
			if response.Code != http.StatusBadRequest || calls.Load() != 0 {
				t.Fatalf("status=%d executor calls=%d body=%s", response.Code, calls.Load(), response.Body.String())
			}
			got := decodeObject(t, response)
			if got["errorType"] != "ApiRequestError" || got["ok"] != false || len(got) != 4 {
				t.Fatalf("error contract=%#v", got)
			}
			if strings.Contains(response.Body.String(), "user:password@") || strings.Contains(response.Body.String(), "secret") {
				t.Fatal("input diagnostic leaked a field value")
			}
			assertBaxiaPrivateHeaders(t, response)
		})
	}
}

func TestBaxiaBodyAndURLByteLimits(t *testing.T) {
	var calls atomic.Int32
	handler := newBaxiaHandler(t, baxiaExecutorFunc(func(context.Context, service.BaxiaRequest) (service.BaxiaOutcome, error) {
		calls.Add(1)
		return service.BaxiaOutcome{}, nil
	}), nil)
	prefix := "https://example.com/"
	maxURL := prefix + strings.Repeat("x", service.MaxBaxiaURLBytes-len(prefix))
	encoded, err := json.Marshal(map[string]string{"pageUrl": maxURL, "requestUrl": maxURL})
	if err != nil {
		t.Fatal(err)
	}
	if response := performRequest(handler, http.MethodPost, BaxiaPath, string(encoded)); response.Code != http.StatusOK {
		t.Fatalf("maximum URL rejected: %s", response.Body.String())
	}
	base := baxiaRequestJSON()
	atLimit := base + strings.Repeat(" ", int(MaxRequestBytes)-len(base))
	if response := performRequest(handler, http.MethodPost, BaxiaPath, atLimit); response.Code != http.StatusOK {
		t.Fatal("maximum JSON body rejected")
	}
	for _, contentLength := range []int64{-1, MaxRequestBytes + 1} {
		request := httptest.NewRequest(http.MethodPost, BaxiaPath, strings.NewReader(atLimit+" "))
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

func TestBaxiaOriginAndMethodBoundaries(t *testing.T) {
	var calls atomic.Int32
	handler := newBaxiaHandler(t, baxiaExecutorFunc(func(context.Context, service.BaxiaRequest) (service.BaxiaOutcome, error) {
		calls.Add(1)
		return service.BaxiaOutcome{}, nil
	}), nil)
	for _, test := range []struct {
		origin, site string
		status       int
	}{
		{"", "", http.StatusOK}, {"http://api.example", "same-origin", http.StatusOK}, {"", "none", http.StatusOK},
		{"https://foreign.example", "", http.StatusForbidden}, {"null", "", http.StatusForbidden},
		{"", "cross-site", http.StatusForbidden}, {"", "same-site", http.StatusForbidden},
	} {
		request := httptest.NewRequest(http.MethodPost, "http://api.example"+BaxiaPath, strings.NewReader(baxiaRequestJSON()))
		request.Header.Set("Origin", test.origin)
		request.Header.Set("Sec-Fetch-Site", test.site)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != test.status {
			t.Fatalf("origin=%q site=%q status=%d", test.origin, test.site, response.Code)
		}
		if test.status == http.StatusForbidden && decodeObject(t, response)["errorType"] != "ApiOriginError" {
			t.Fatal("wrong origin error")
		}
		assertBaxiaPrivateHeaders(t, response)
	}
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions} {
		if response := performRequest(handler, method, BaxiaPath, baxiaRequestJSON()); response.Code != http.StatusNotFound {
			t.Fatalf("%s accepted", method)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("executor calls=%d want=3", calls.Load())
	}
}

func TestBaxiaErrorsAndPanicAreSanitized(t *testing.T) {
	secret := errors.New("proxy-user:secret-password and upstream-secret-body")
	for _, test := range []struct {
		name   string
		err    error
		status int
		kind   string
	}{
		{"invalid", &service.BaxiaFailure{Kind: service.BaxiaFailureInvalidRequest, Cause: secret}, 400, "InvalidRequest"},
		{"source", &service.BaxiaFailure{Kind: service.BaxiaFailureSource, Cause: secret}, 500, "BaxiaSourceError"},
		{"unsupported", &service.BaxiaFailure{Kind: service.BaxiaFailureUnsupportedSDK, Cause: secret}, 500, "BaxiaUnsupportedSDKError"},
		{"runtime", &service.BaxiaFailure{Kind: service.BaxiaFailureRuntime, Cause: secret}, 500, "BaxiaRuntimeError"},
		{"canceled", &service.BaxiaFailure{Kind: service.BaxiaFailureCanceled, Cause: secret}, 500, "BaxiaCanceled"},
		{"internal", &service.BaxiaFailure{Kind: service.BaxiaFailureInternal, Cause: secret}, 500, "InternalError"},
		{"unknown", secret, 500, "InternalError"}, {"panic", nil, 500, "InternalError"}, {"missing executor", nil, 500, "InternalError"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			var executor service.BaxiaExecutor = baxiaExecutorFunc(func(context.Context, service.BaxiaRequest) (service.BaxiaOutcome, error) {
				if test.name == "panic" {
					panic(secret)
				}
				return service.BaxiaOutcome{Result: baxia.Result{Token: "secret-partial-token"}}, test.err
			})
			if test.name == "missing executor" {
				executor = nil
			}
			handler := newBaxiaHandler(t, executor, func(options *Options) { options.Logger = log.New(&logs, "", 0) })
			response := performRequest(handler, http.MethodPost, BaxiaPath, baxiaRequestJSON())
			body := decodeObject(t, response)
			if response.Code != test.status || body["errorType"] != test.kind || body["ok"] != false || len(body) != 4 {
				t.Fatalf("error response=%d %#v", response.Code, body)
			}
			if strings.Contains(response.Body.String(), "secret") || strings.Contains(logs.String(), "secret") {
				t.Fatal("execution diagnostic or partial token leaked")
			}
			assertBaxiaPrivateHeaders(t, response)
		})
	}
}

func TestBaxiaRequestContextAndTimeoutReachExecutor(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "cancel"}[canceled], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if canceled {
				cancel()
			}
			var cause error
			handler := newBaxiaHandler(t, baxiaExecutorFunc(func(ctx context.Context, _ service.BaxiaRequest) (service.BaxiaOutcome, error) {
				<-ctx.Done()
				cause = ctx.Err()
				return service.BaxiaOutcome{}, cause
			}), func(options *Options) { options.Timeout = 15 * time.Millisecond })
			request := httptest.NewRequest(http.MethodPost, BaxiaPath, strings.NewReader(baxiaRequestJSON())).WithContext(ctx)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			want := context.DeadlineExceeded
			if canceled {
				want = context.Canceled
			}
			if cause != want || response.Code != http.StatusInternalServerError || decodeObject(t, response)["errorType"] != "BaxiaCanceled" {
				t.Fatal("request cancellation was not preserved")
			}
		})
	}
}

func TestBaxiaSuccessLogsDoNotContainRequestOrToken(t *testing.T) {
	var logs bytes.Buffer
	handler := newBaxiaHandler(t, baxiaExecutorFunc(func(context.Context, service.BaxiaRequest) (service.BaxiaOutcome, error) {
		return service.BaxiaOutcome{Result: baxia.Result{Token: "sensitive-token", SDKHash: "sensitive-hash"}}, nil
	}), func(options *Options) { options.Logger = log.New(&logs, "", 0) })
	body := `{"pageUrl":"https://page.example/sensitive-query","requestUrl":"https://api.example/sensitive-query","proxy":"http://sensitive-user:sensitive-pass@proxy.example:8080"}`
	response := performRequest(handler, http.MethodPost, BaxiaPath, body)
	if response.Code != http.StatusOK || strings.Contains(logs.String(), "sensitive") {
		t.Fatalf("status=%d logs=%q", response.Code, logs.String())
	}
}

func assertBaxiaPrivateHeaders(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	body := decodeObject(t, response)
	if response.Header().Get("X-Trace-ID") == "" || response.Header().Get("X-Trace-ID") != body["traceId"] {
		t.Fatal("trace ID is missing or inconsistent")
	}
	for name, want := range map[string]string{
		"Content-Type": "application/json; charset=utf-8", "Cache-Control": "no-store",
		"X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer",
	} {
		if response.Header().Get(name) != want {
			t.Fatalf("%s=%q want=%q", name, response.Header().Get(name), want)
		}
	}
}
