package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/application/service"
	"github.com/d2120848471/v3/ali-slider-go/internal/application/solve"
)

type solverFunc func(context.Context, solve.Request) (solve.Outcome, error)

func (function solverFunc) Solve(ctx context.Context, request solve.Request) (solve.Outcome, error) {
	return function(ctx, request)
}

func newTestHandler(t *testing.T, solver service.Executor, mutate func(*Options)) *Handler {
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
	var received solve.Request
	handler := newTestHandler(t, solverFunc(func(_ context.Context, request solve.Request) (solve.Outcome, error) {
		calls.Add(1)
		received = request
		return solve.Outcome{
			OK:            false,
			SecurityToken: "response-token",
			VerifyCode:    "F015",
			VerifyResult:  false,
			CertifyID:     "response-certify-id",
			SceneID:       request.SceneID,
			Proxied:       request.Proxy != "",
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
	want := solve.Request{
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
	if response.Header().Get("X-Content-Type-Options") != "nosniff" || response.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("security headers missing: %#v", response.Header())
	}
}

func TestCanonicalAliasPrecedenceAndDefaults(t *testing.T) {
	var requests []solve.Request
	var mu sync.Mutex
	handler := newTestHandler(t, solverFunc(func(_ context.Context, request solve.Request) (solve.Outcome, error) {
		mu.Lock()
		requests = append(requests, request)
		mu.Unlock()
		return solve.Outcome{}, nil
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
	if requests[0] != (solve.Request{SceneID: "canonical", Prefix: "canon9", RPCKeyID: "rpc-canonical", Proxy: "http://direct.invalid:80"}) {
		t.Fatalf("canonical precedence = %#v", requests[0])
	}
	if requests[1] != (solve.Request{SceneID: "default-scene", Prefix: "default1"}) {
		t.Fatalf("defaults = %#v", requests[1])
	}
}

func TestLegacyGETQueryCompatibility(t *testing.T) {
	var requests []solve.Request
	handler := newTestHandler(t, solverFunc(func(_ context.Context, request solve.Request) (solve.Outcome, error) {
		requests = append(requests, request)
		return solve.Outcome{}, nil
	}), nil)

	tests := []struct {
		name string
		path string
		want solve.Request
	}{
		{
			name: "canonical keys take precedence and duplicate keys use last value",
			path: SolvePath + "?sceneId=alias&SceneId=first&SceneId=canonical%20scene" +
				"&Prefix=alias9&prefix=canon9&aaduaneId=alias&AaduaneId=rpc%20key" +
				"&Proxy=alias.invalid%3A81&proxy=socks5h%3A%2F%2Fuser%3Apass%40proxy.invalid%3A1080&futureField=ignored",
			want: solve.Request{
				SceneID: "canonical scene", Prefix: "canon9", RPCKeyID: "rpc key",
				Proxy: "socks5h://user:pass@proxy.invalid:1080",
			},
		},
		{
			name: "aliases",
			path: SolvePath + "?sceneId=alias-scene&Prefix=alias9&aaduaneId=alias-key&Proxy=proxy.invalid%3A8080",
			want: solve.Request{SceneID: "alias-scene", Prefix: "alias9", RPCKeyID: "alias-key", Proxy: "http://proxy.invalid:8080"},
		},
		{
			name: "blank canonical keys suppress aliases and use defaults",
			path: SolvePath + "?SceneId=&sceneId=ignored&prefix=+&Prefix=ignored&AaduaneId=&aaduaneId=ignored&proxy=&Proxy=ignored.invalid%3A80",
			want: solve.Request{SceneID: "default-scene", Prefix: "default1"},
		},
		{
			name: "empty query",
			path: SolvePath,
			want: solve.Request{SceneID: "default-scene", Prefix: "default1"},
		},
		{
			name: "query null is a string",
			path: SolvePath + "?SceneId=null",
			want: solve.Request{SceneID: "null", Prefix: "default1"},
		},
		{
			name: "raw query at exact limit",
			path: SolvePath + "?future=" + strings.Repeat("x", int(MaxRequestBytes)-len("future=")),
			want: solve.Request{SceneID: "default-scene", Prefix: "default1"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := performRequest(handler, http.MethodGet, test.path, "")
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
			}
			if response.Header().Get("X-Trace-ID") == "" || response.Header().Get("Retry-After") != "" {
				t.Fatalf("legacy GET headers = %#v", response.Header())
			}
			if got := requests[len(requests)-1]; got != test.want {
				t.Fatalf("request = %#v, want %#v", got, test.want)
			}
		})
	}
	if len(requests) != len(tests) {
		t.Fatalf("solver calls = %d, want %d", len(requests), len(tests))
	}
}

func TestLegacyGETQueryValidationNeverCallsSolver(t *testing.T) {
	var calls atomic.Int32
	handler := newTestHandler(t, solverFunc(func(context.Context, solve.Request) (solve.Outcome, error) {
		calls.Add(1)
		return solve.Outcome{}, nil
	}), nil)
	tests := []struct {
		name     string
		rawQuery string
	}{
		{name: "invalid URL encoding", rawQuery: "SceneId=%zz"},
		{name: "query too large", rawQuery: "future=" + strings.Repeat("x", int(MaxRequestBytes))},
		{name: "scene too long", rawQuery: "SceneId=" + strings.Repeat("a", 65)},
		{name: "invalid prefix", rawQuery: "prefix=bad-prefix"},
		{name: "invalid proxy", rawQuery: "proxy=ftp%3A%2F%2Fproxy.invalid%3A21"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, SolvePath, nil)
			request.URL.RawQuery = test.rawQuery
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", response.Code, response.Body.String())
			}
			body := decodeObject(t, response)
			if body["errorType"] != "ApiRequestError" || response.Header().Get("X-Trace-ID") != body["traceId"] {
				t.Fatalf("legacy GET error = %#v headers=%#v", body, response.Header())
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid legacy GET requests called Solver %d times", calls.Load())
	}
}

func TestSolveParameterSourcesStaySeparated(t *testing.T) {
	var requests []solve.Request
	handler := newTestHandler(t, solverFunc(func(_ context.Context, request solve.Request) (solve.Outcome, error) {
		requests = append(requests, request)
		return solve.Outcome{}, nil
	}), nil)

	post := performRequest(handler, http.MethodPost, SolvePath+"?SceneId=query-scene", `{}`)
	if post.Code != http.StatusOK {
		t.Fatalf("POST status = %d: %s", post.Code, post.Body.String())
	}
	get := performRequest(handler, http.MethodGet, SolvePath+"?SceneId=query-scene", `{"SceneId":"body-scene"}`)
	if get.Code != http.StatusOK {
		t.Fatalf("GET status = %d: %s", get.Code, get.Body.String())
	}
	for _, form := range []struct {
		name        string
		contentType string
		body        string
	}{
		{name: "urlencoded", contentType: "application/x-www-form-urlencoded", body: "SceneId=form-scene"},
		{name: "multipart", contentType: "multipart/form-data; boundary=legacy", body: "--legacy\r\nContent-Disposition: form-data; name=SceneId\r\n\r\nform-scene\r\n--legacy--\r\n"},
	} {
		t.Run(form.name, func(t *testing.T) {
			formRequest := httptest.NewRequest(http.MethodPost, SolvePath, strings.NewReader(form.body))
			formRequest.Header.Set("Content-Type", form.contentType)
			formResponse := httptest.NewRecorder()
			handler.ServeHTTP(formResponse, formRequest)
			if formResponse.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", formResponse.Code, formResponse.Body.String())
			}
		})
	}

	want := []solve.Request{
		{SceneID: "default-scene", Prefix: "default1"},
		{SceneID: "query-scene", Prefix: "default1"},
	}
	if len(requests) != len(want) {
		t.Fatalf("solver calls = %d, want %d", len(requests), len(want))
	}
	for index := range want {
		if requests[index] != want[index] {
			t.Errorf("request[%d] = %#v, want %#v", index, requests[index], want[index])
		}
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
			handler := newTestHandler(t, solverFunc(func(context.Context, solve.Request) (solve.Outcome, error) {
				calls.Add(1)
				return solve.Outcome{}, nil
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
	handler := newTestHandler(t, solverFunc(func(context.Context, solve.Request) (solve.Outcome, error) {
		calls.Add(1)
		return solve.Outcome{}, nil
	}), nil)
	body := `{"unknown":"` + strings.Repeat("x", int(MaxRequestBytes)) + `"}`
	response := performRequest(handler, http.MethodPost, SolvePath, body)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}
	if calls.Load() != 0 {
		t.Fatalf("solver calls = %d, want 0", calls.Load())
	}
}

func TestEmbeddedAPITestPage(t *testing.T) {
	var calls atomic.Int32
	handler := newTestHandler(t, solverFunc(func(context.Context, solve.Request) (solve.Outcome, error) {
		calls.Add(1)
		return solve.Outcome{}, nil
	}), nil)

	first := performRequest(handler, http.MethodGet, TestPagePath, "")
	if first.Code != http.StatusOK {
		t.Fatalf("page status = %d, want 200; body=%s", first.Code, first.Body.String())
	}
	if first.Header().Get("Content-Type") != "text/html; charset=utf-8" || first.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("page response headers = %#v", first.Header())
	}
	for name, want := range map[string]string{
		"X-Content-Type-Options":       "nosniff",
		"Referrer-Policy":              "no-referrer",
		"X-Frame-Options":              "DENY",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Cross-Origin-Opener-Policy":   "same-origin",
	} {
		if got := first.Header().Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if first.Header().Get("Permissions-Policy") == "" {
		t.Fatal("Permissions-Policy is missing")
	}

	csp := first.Header().Get("Content-Security-Policy")
	nonceMatch := regexp.MustCompile(`script-src 'nonce-([0-9a-f]{32})'`).FindStringSubmatch(csp)
	if len(nonceMatch) != 2 {
		t.Fatalf("CSP has no 128-bit script nonce: %q", csp)
	}
	nonce := nonceMatch[1]
	for _, directive := range []string{
		"default-src 'none'", "connect-src 'self'", "script-src-attr 'none'",
		"style-src 'nonce-" + nonce + "'", "style-src-attr 'none'", "form-action 'none'", "frame-ancestors 'none'",
	} {
		if !strings.Contains(csp, directive) {
			t.Errorf("CSP missing %q: %q", directive, csp)
		}
	}
	for _, forbidden := range []string{"'unsafe-inline'", "'unsafe-eval'"} {
		if strings.Contains(csp, forbidden) {
			t.Errorf("CSP contains %q: %q", forbidden, csp)
		}
	}

	body := first.Body.String()
	for _, required := range []string{
		`id="api-test-console"`, `"/api/slider"`, `"/api/bxua"`, `"/api/waf"`, `fetch(endpoint,`, `fetch("/health"`,
		`"SceneId"`, `"prefix"`, `"AaduaneId"`, `"proxy"`, `AbortController`, `TextEncoder`,
		`response.status === 200`, `!Array.isArray(lastResponse)`, `lastResponse.VerifyCode === "T001"`,
		`lastResponse.VerifyResult === true`, `lastResponse.securityToken.length > 0`,
	} {
		if !strings.Contains(body, required) {
			t.Errorf("page missing %q", required)
		}
	}
	if strings.Count(body, `nonce="`+nonce+`"`) != 2 || strings.Contains(body, testPageNoncePlaceholder) {
		t.Fatalf("page nonce substitution is incomplete")
	}
	// 调用示例也包含 fetch 文本；真实网络目的地与触发时机由页面行为测试执行脚本核对。
	for _, forbidden := range []string{
		`<script src=`, `rel="stylesheet"`, "innerHTML", "outerHTML", "insertAdjacentHTML",
		"localStorage", "sessionStorage", "indexedDB", "caches.", "document.cookie", "navigator.sendBeacon", "serviceWorker",
		"XMLHttpRequest", "WebSocket", "EventSource", "eval(", "new Function",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("page contains forbidden browser API/resource %q", forbidden)
		}
	}

	second := performRequest(handler, http.MethodGet, TestPagePath, "")
	secondMatch := regexp.MustCompile(`script-src 'nonce-([0-9a-f]{32})'`).FindStringSubmatch(second.Header().Get("Content-Security-Policy"))
	if len(secondMatch) != 2 || secondMatch[1] == nonce {
		t.Fatalf("consecutive page nonces are not unique: first=%q second=%#v", nonce, secondMatch)
	}
	for _, method := range []string{http.MethodPost, http.MethodHead} {
		response := performRequest(handler, method, TestPagePath, `{}`)
		if response.Code != http.StatusNotFound {
			t.Errorf("%s page status = %d, want 404", method, response.Code)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("page routes called Solver %d times", calls.Load())
	}
}

func TestEmbeddedAPITestPageNonceFailure(t *testing.T) {
	original := readTestPageNonce
	readTestPageNonce = func([]byte) (int, error) { return 0, errors.New("sensitive entropy failure") }
	t.Cleanup(func() { readTestPageNonce = original })

	var calls atomic.Int32
	handler := newTestHandler(t, solverFunc(func(context.Context, solve.Request) (solve.Outcome, error) {
		calls.Add(1)
		return solve.Outcome{}, nil
	}), nil)
	response := performRequest(handler, http.MethodGet, TestPagePath, "")
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", response.Code, response.Body.String())
	}
	if response.Header().Get("Content-Type") != "text/plain; charset=utf-8" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("page failure headers = %#v", response.Header())
	}
	if strings.Contains(response.Body.String(), "sensitive entropy failure") {
		t.Fatalf("page failure leaked cause: %q", response.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("page nonce failure called Solver %d times", calls.Load())
	}
}

func TestBrowserOriginBoundaryPreservesLegacyClients(t *testing.T) {
	var calls atomic.Int32
	handler := newTestHandler(t, solverFunc(func(context.Context, solve.Request) (solve.Outcome, error) {
		calls.Add(1)
		return solve.Outcome{}, nil
	}), nil)

	request := func(method, origin, fetchSite string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "http://127.0.0.1:8000"+SolvePath, nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if fetchSite != "" {
			req.Header.Set("Sec-Fetch-Site", fetchSite)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}

	for _, method := range []string{http.MethodPost, http.MethodGet} {
		for _, test := range []struct {
			name      string
			origin    string
			fetchSite string
		}{
			{name: "cross-site metadata", fetchSite: "cross-site"},
			{name: "same-site metadata", fetchSite: "same-site"},
			{name: "foreign origin", origin: "https://attacker.example"},
			{name: "opaque origin", origin: "null"},
		} {
			t.Run(method+"/"+test.name, func(t *testing.T) {
				response := request(method, test.origin, test.fetchSite)
				if response.Code != http.StatusForbidden {
					t.Fatalf("status = %d, want 403; body=%s", response.Code, response.Body.String())
				}
				body := decodeObject(t, response)
				if body["errorType"] != "ApiOriginError" || response.Header().Get("X-Trace-ID") != body["traceId"] {
					t.Fatalf("cross-origin response = %#v headers=%#v", body, response.Header())
				}
			})
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("cross-origin requests called Solver %d times", calls.Load())
	}

	for _, method := range []string{http.MethodPost, http.MethodGet} {
		for _, test := range []struct {
			name      string
			origin    string
			fetchSite string
		}{
			{name: "same-origin page", origin: "http://127.0.0.1:8000", fetchSite: "same-origin"},
			{name: "user initiated browser request", fetchSite: "none"},
			{name: "legacy headerless client"},
		} {
			t.Run(method+"/"+test.name, func(t *testing.T) {
				response := request(method, test.origin, test.fetchSite)
				if response.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
				}
			})
		}
	}
	if calls.Load() != 6 {
		t.Fatalf("allowed requests called Solver %d times, want 6", calls.Load())
	}
}

func TestOnlyFrozenRoutesAreExposed(t *testing.T) {
	var calls atomic.Int32
	handler := newTestHandler(t, solverFunc(func(context.Context, solve.Request) (solve.Outcome, error) {
		calls.Add(1)
		return solve.Outcome{}, nil
	}), nil)

	for _, method := range []string{http.MethodHead, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions} {
		if response := performRequest(handler, method, SolvePath, ""); response.Code != http.StatusNotFound {
			t.Errorf("%s solve status = %d, want 404", method, response.Code)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("unsupported solve methods called solver %d times", calls.Load())
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
	if len(paths) != 6 {
		t.Fatalf("OpenAPI paths = %#v", paths)
	}
	page := paths[TestPagePath].(map[string]any)
	if len(page) != 1 {
		t.Fatalf("test page methods = %#v", page)
	}
	pageContent := page["get"].(map[string]any)["responses"].(map[string]any)["200"].(map[string]any)["content"].(map[string]any)
	if _, exists := pageContent["text/html"]; !exists {
		t.Fatalf("test page OpenAPI content = %#v", pageContent)
	}
	pageFailureContent := page["get"].(map[string]any)["responses"].(map[string]any)["500"].(map[string]any)["content"].(map[string]any)
	if _, exists := pageFailureContent["text/plain"]; !exists {
		t.Fatalf("test page OpenAPI failure content = %#v", pageFailureContent)
	}
	solvePath := paths[SolvePath].(map[string]any)
	if len(solvePath) != 2 {
		t.Fatalf("solve methods = %#v", solvePath)
	}
	legacyGET := solvePath["get"].(map[string]any)
	if legacyGET["deprecated"] != true {
		t.Fatalf("legacy GET deprecated = %#v", legacyGET["deprecated"])
	}
	parameters := legacyGET["parameters"].([]any)
	if len(parameters) != 8 {
		t.Fatalf("legacy GET parameters = %d, want 8", len(parameters))
	}
	parameterNames := make(map[string]bool, len(parameters))
	for _, rawParameter := range parameters {
		parameter := rawParameter.(map[string]any)
		parameterNames[parameter["name"].(string)] = true
	}
	for _, name := range []string{"SceneId", "sceneId", "prefix", "Prefix", "AaduaneId", "aaduaneId", "proxy", "Proxy"} {
		if !parameterNames[name] {
			t.Errorf("legacy GET parameter missing %q", name)
		}
	}
	for _, method := range []string{"get", "post"} {
		responses := solvePath[method].(map[string]any)["responses"].(map[string]any)
		if len(responses) != 4 {
			t.Errorf("OpenAPI %s response count = %d, want 4", method, len(responses))
		}
		for _, status := range []string{"200", "400", "403", "500"} {
			if _, exists := responses[status]; !exists {
				t.Errorf("OpenAPI %s response missing %s", method, status)
			}
		}
	}
	solve := solvePath["post"].(map[string]any)
	responses := solve["responses"].(map[string]any)
	success := responses["200"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	properties := success["properties"].(map[string]any)
	for _, field := range []string{"ok", "securityToken", "VerifyCode", "VerifyResult", "certifyId", "sceneId", "proxied", "elapsedMs", "timingsMs", "traceId"} {
		if _, exists := properties[field]; !exists {
			t.Errorf("OpenAPI success schema missing %q", field)
		}
	}
	if _, exists := properties["captchaVerifyParam"]; exists {
		t.Error("OpenAPI still exposes prebuilt captchaVerifyParam")
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
	handler := newTestHandler(t, solverFunc(func(ctx context.Context, _ solve.Request) (solve.Outcome, error) {
		entered <- struct{}{}
		select {
		case <-release:
			return solve.Outcome{}, nil
		case <-ctx.Done():
			return solve.Outcome{}, ctx.Err()
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
	handler := newTestHandler(t, solverFunc(func(ctx context.Context, _ solve.Request) (solve.Outcome, error) {
		calls.Add(1)
		<-ctx.Done()
		return solve.Outcome{}, ctx.Err()
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
	handler := newTestHandler(t, solverFunc(func(context.Context, solve.Request) (solve.Outcome, error) {
		return solve.Outcome{
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
	tests := map[string]service.Executor{
		"error": solverFunc(func(context.Context, solve.Request) (solve.Outcome, error) {
			return solve.Outcome{}, errors.New("upstream-secret-body")
		}),
		"panic": solverFunc(func(context.Context, solve.Request) (solve.Outcome, error) {
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
