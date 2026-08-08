package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/pkg/slider"
)

type solverFunc func(context.Context, slider.Request) (slider.Result, error)

func (function solverFunc) Solve(ctx context.Context, request slider.Request) (slider.Result, error) {
	return function(ctx, request)
}

func newTestHandler(t *testing.T, solver slider.Solver, mutate func(*Options)) *Handler {
	t.Helper()
	options := Options{
		Solver:         solver,
		DefaultSceneID: "default-scene",
		DefaultPrefix:  "default1",
		Timeout:        25 * time.Second,
	}
	if mutate != nil {
		mutate(&options)
	}
	handler, err := New(options)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return handler
}

func performRequest(handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func decodeObject(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &object); err != nil {
		t.Fatalf("response is not JSON: %v; body=%q", err, response.Body.String())
	}
	return object
}

func TestSolveAliasesUnknownFieldsAndBusinessFailure(t *testing.T) {
	var calls atomic.Int32
	var received slider.Request
	handler := newTestHandler(t, solverFunc(func(_ context.Context, request slider.Request) (slider.Result, error) {
		calls.Add(1)
		received = request
		return slider.Result{
			OK:            false,
			SecurityToken: "response-token",
			VerifyCode:    "F015",
			VerifyResult:  false,
			CertifyID:     "response-certify-id",
			SceneID:       request.SceneID,
			Proxied:       request.Proxy != "",
			ElapsedMS:     27,
			TimingsMS:     map[string]int{"total": 27},
		}, nil
	}), nil)

	response := performRequest(handler, http.MethodPost, SolvePath, `{
		"sceneId":"lower-scene",
		"Prefix":"prefix7",
		"aaduaneId":"rpc-key",
		"Proxy":"proxy.invalid:8080",
		"futureField":{"ignored":true}
	}`)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("solver calls = %d, want 1", calls.Load())
	}
	want := slider.Request{
		SceneID: "lower-scene", Prefix: "prefix7", RPCKeyID: "rpc-key", Proxy: "http://proxy.invalid:8080",
	}
	if received != want {
		t.Fatalf("request = %#v, want %#v", received, want)
	}
	body := decodeObject(t, response)
	if body["ok"] != false || body["VerifyCode"] != "F015" {
		t.Fatalf("business failure response = %#v", body)
	}
	traceID, _ := body["traceId"].(string)
	if len(traceID) != 12 || response.Header().Get("X-Trace-ID") != traceID {
		t.Fatalf("trace header/body mismatch: header=%q body=%q", response.Header().Get("X-Trace-ID"), traceID)
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", response.Header().Get("Cache-Control"))
	}
}

func TestCanonicalAliasPrecedenceAndDefaults(t *testing.T) {
	var requests []slider.Request
	var mu sync.Mutex
	handler := newTestHandler(t, solverFunc(func(_ context.Context, request slider.Request) (slider.Result, error) {
		mu.Lock()
		requests = append(requests, request)
		mu.Unlock()
		return slider.Result{}, nil
	}), nil)

	first := performRequest(handler, http.MethodPost, SolvePath, `{
		"SceneId":"canonical","sceneId":"alias",
		"prefix":"canon9","Prefix":"alias9",
		"AaduaneId":"rpc-canonical","aaduaneId":"rpc-alias",
		"proxy":"direct.invalid:80","Proxy":"alias.invalid:81"
	}`)
	if first.Code != http.StatusOK {
		t.Fatalf("canonical request status = %d: %s", first.Code, first.Body.String())
	}
	second := performRequest(handler, http.MethodPost, SolvePath, `{}`)
	if second.Code != http.StatusOK {
		t.Fatalf("default request status = %d: %s", second.Code, second.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(requests))
	}
	if requests[0] != (slider.Request{SceneID: "canonical", Prefix: "canon9", RPCKeyID: "rpc-canonical", Proxy: "http://direct.invalid:80"}) {
		t.Fatalf("canonical precedence = %#v", requests[0])
	}
	if requests[1] != (slider.Request{SceneID: "default-scene", Prefix: "default1"}) {
		t.Fatalf("defaults = %#v", requests[1])
	}
}

func TestRequestValidationNeverCallsSolver(t *testing.T) {
	tests := map[string]string{
		"top level null":     `null`,
		"top level array":    `[]`,
		"invalid JSON":       `{`,
		"trailing JSON":      `{} {}`,
		"scene type":         `{"SceneId":1}`,
		"scene length":       `{"SceneId":"` + strings.Repeat("界", 65) + `"}`,
		"rpc key type":       `{"AaduaneId":false}`,
		"rpc key length":     `{"AaduaneId":"` + strings.Repeat("a", 129) + `"}`,
		"prefix punctuation": `{"prefix":"bad-prefix"}`,
		"prefix non ASCII":   `{"prefix":"中文"}`,
		"proxy type":         `{"proxy":7}`,
		"proxy scheme":       `{"proxy":"ftp://proxy.invalid:21"}`,
		"proxy missing host": `{"proxy":"http://:8080"}`,
		"proxy invalid port": `{"proxy":"http://proxy.invalid:70000"}`,
		"proxy path":         `{"proxy":"socks5://proxy.invalid:1080/path"}`,
		"proxy query":        `{"proxy":"socks5://proxy.invalid:1080?x=1"}`,
		"socks4 password":    `{"proxy":"socks4://user:password@proxy.invalid:1080"}`,
		"socks5 no password": `{"proxy":"socks5://user@proxy.invalid:1080"}`,
	}

	for name, requestBody := range tests {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			handler := newTestHandler(t, solverFunc(func(context.Context, slider.Request) (slider.Result, error) {
				calls.Add(1)
				return slider.Result{}, nil
			}), nil)
			response := performRequest(handler, http.MethodPost, SolvePath, requestBody)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", response.Code, response.Body.String())
			}
			if calls.Load() != 0 {
				t.Fatalf("solver calls = %d, want 0", calls.Load())
			}
			if response.Header().Get("X-Trace-ID") == "" || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("required headers missing: %#v", response.Header())
			}
		})
	}
}

func TestRequestBodyLimit(t *testing.T) {
	var calls atomic.Int32
	handler := newTestHandler(t, solverFunc(func(context.Context, slider.Request) (slider.Result, error) {
		calls.Add(1)
		return slider.Result{}, nil
	}), nil)
	body := `{"unknown":"` + strings.Repeat("x", int(maxRequestBytes)) + `"}`
	response := performRequest(handler, http.MethodPost, SolvePath, body)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}
	if calls.Load() != 0 {
		t.Fatalf("solver calls = %d, want 0", calls.Load())
	}
}

func TestOnlyFrozenRoutesAreExposed(t *testing.T) {
	var calls atomic.Int32
	handler := newTestHandler(t, solverFunc(func(context.Context, slider.Request) (slider.Result, error) {
		calls.Add(1)
		return slider.Result{}, nil
	}), nil)

	if response := performRequest(handler, http.MethodGet, SolvePath, ""); response.Code != http.StatusNotFound {
		t.Fatalf("GET solve status = %d, want 404", response.Code)
	}
	if calls.Load() != 0 {
		t.Fatalf("GET solve called solver %d times", calls.Load())
	}
	health := performRequest(handler, http.MethodGet, HealthPath, "")
	if health.Code != http.StatusOK || health.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("health response = %d, headers=%#v", health.Code, health.Header())
	}
	openAPI := performRequest(handler, http.MethodGet, OpenAPIPath, "")
	if openAPI.Code != http.StatusOK {
		t.Fatalf("OpenAPI status = %d", openAPI.Code)
	}
	var document map[string]any
	if err := json.Unmarshal(openAPI.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	paths := document["paths"].(map[string]any)
	if len(paths) != 3 {
		t.Fatalf("OpenAPI paths = %#v", paths)
	}
	solve := paths[SolvePath].(map[string]any)["post"].(map[string]any)
	responses := solve["responses"].(map[string]any)
	if _, exists := responses["429"]; exists {
		t.Fatal("OpenAPI still advertises local 429")
	}
	success := responses["200"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	properties := success["properties"].(map[string]any)
	for _, field := range []string{"ok", "securityToken", "VerifyCode", "VerifyResult", "certifyId", "sceneId", "proxied", "elapsedMs", "timingsMs", "traceId"} {
		if _, exists := properties[field]; !exists {
			t.Errorf("OpenAPI success schema missing %q", field)
		}
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	for _, obsolete := range []string{"Retry-After", "TooManyChallenges", "activeChallenges", "retryAfterMs", "recommendedClientTimeoutMs"} {
		if bytes.Contains(encoded, []byte(obsolete)) {
			t.Errorf("OpenAPI still contains obsolete local-overload field %q", obsolete)
		}
	}
}

func TestConcurrentRequestsAlwaysEnterSolver(t *testing.T) {
	const requestCount = 64
	entered := make(chan struct{}, requestCount)
	release := make(chan struct{})
	handler := newTestHandler(t, solverFunc(func(ctx context.Context, _ slider.Request) (slider.Result, error) {
		entered <- struct{}{}
		select {
		case <-release:
			return slider.Result{}, nil
		case <-ctx.Done():
			return slider.Result{}, ctx.Err()
		}
	}), nil)

	responses := make(chan *httptest.ResponseRecorder, requestCount)
	for range requestCount {
		go func() {
			responses <- performRequest(handler, http.MethodPost, SolvePath, `{}`)
		}()
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for index := 0; index < requestCount; index++ {
		select {
		case <-entered:
		case <-timer.C:
			close(release)
			t.Fatalf("only %d/%d requests entered Solver", index, requestCount)
		}
	}
	close(release)
	for range requestCount {
		response := <-responses
		if response.Code != http.StatusOK || response.Header().Get("Retry-After") != "" {
			t.Fatalf("concurrent response status=%d Retry-After=%q body=%s", response.Code, response.Header().Get("Retry-After"), response.Body.String())
		}
		if response.Header().Get("X-Trace-ID") == "" || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("required headers missing: %#v", response.Header())
		}
	}
}

func TestHandlerTimeoutReachesEveryCustomSolverCall(t *testing.T) {
	var calls atomic.Int32
	handler := newTestHandler(t, solverFunc(func(ctx context.Context, _ slider.Request) (slider.Result, error) {
		calls.Add(1)
		<-ctx.Done()
		return slider.Result{}, ctx.Err()
	}), func(options *Options) {
		options.Timeout = 15 * time.Millisecond
	})

	started := time.Now()
	first := performRequest(handler, http.MethodPost, SolvePath, `{}`)
	if first.Code != http.StatusInternalServerError || time.Since(started) > time.Second {
		t.Fatalf("timeout response status=%d elapsed=%s", first.Code, time.Since(started))
	}
	second := performRequest(handler, http.MethodPost, SolvePath, `{}`)
	if second.Code != http.StatusInternalServerError || calls.Load() != 2 {
		t.Fatalf("second timeout response: status=%d calls=%d", second.Code, calls.Load())
	}
}

func TestLogsNeverContainSensitiveValues(t *testing.T) {
	const (
		token      = "token-must-not-appear"
		certifyID  = "certify-must-not-appear"
		rawRequest = "raw-body-must-not-appear"
	)
	var output bytes.Buffer
	handler := newTestHandler(t, solverFunc(func(context.Context, slider.Request) (slider.Result, error) {
		return slider.Result{
			OK:            true,
			SecurityToken: token,
			VerifyCode:    "T001",
			VerifyResult:  true,
			CertifyID:     certifyID,
		}, nil
	}), func(options *Options) {
		options.Logger = log.New(&output, "", 0)
	})

	response := performRequest(handler, http.MethodPost, SolvePath, `{"unknown":"`+rawRequest+`"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	logged := output.String()
	for _, sensitive := range []string{token, certifyID, rawRequest, "securityToken", "certifyId"} {
		if strings.Contains(logged, sensitive) {
			t.Fatalf("log leaked %q: %q", sensitive, logged)
		}
	}
}

func TestSolverErrorAndPanicReturnSanitized500(t *testing.T) {
	tests := map[string]slider.Solver{
		"error": solverFunc(func(context.Context, slider.Request) (slider.Result, error) {
			return slider.Result{}, errors.New("upstream-secret-body")
		}),
		"panic": solverFunc(func(context.Context, slider.Request) (slider.Result, error) {
			panic("panic-secret-body")
		}),
	}
	for name, implementation := range tests {
		t.Run(name, func(t *testing.T) {
			var logs bytes.Buffer
			handler := newTestHandler(t, implementation, func(options *Options) {
				options.Logger = log.New(&logs, "", 0)
			})
			response := performRequest(handler, http.MethodPost, SolvePath, `{}`)
			if response.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", response.Code)
			}
			if strings.Contains(response.Body.String(), "secret-body") || strings.Contains(logs.String(), "secret-body") {
				t.Fatalf("unsanitized error leaked: body=%q logs=%q", response.Body.String(), logs.String())
			}
			if response.Header().Get("X-Trace-ID") == "" || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("required headers missing: %#v", response.Header())
			}
		})
	}
}
