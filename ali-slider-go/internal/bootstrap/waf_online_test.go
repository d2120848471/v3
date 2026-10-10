//go:build online

package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/application/service"
	"github.com/d2120848471/v3/ali-slider-go/internal/infrastructure/httpclient"
	wafinfra "github.com/d2120848471/v3/ali-slider-go/internal/infrastructure/waf"
	"github.com/d2120848471/v3/ali-slider-go/internal/interfaces/httpapi"
)

// TestOnlineWAF 使用生产组装获取新页面并执行一轮真实 Init/Verify；拒绝不算通过。
func TestOnlineWAF(t *testing.T) {
	pageURL := os.Getenv("ALI_SLIDER_WAF_ONLINE_URL")
	if pageURL == "" {
		t.Skip("set ALI_SLIDER_WAF_ONLINE_URL to run one live WAF challenge")
	}
	library := os.Getenv("ALI_SLIDER_V8_TEST_LIBRARY")
	if library == "" {
		t.Fatal("ALI_SLIDER_V8_TEST_LIBRARY is required")
	}
	const timeout = 35 * time.Second
	executor, err := NewWAFExecutor(library, timeout)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	result, err := executor.SolveWAF(ctx, service.WAFRequest{PageURL: pageURL})
	if err != nil {
		t.Fatalf("live WAF execution failed: %v", err)
	}
	// 只打印状态，页面 token、签名和设备头留在本轮内存。
	t.Logf("live WAF result: ok=%t type=%s verifyCode=%s elapsed=%s", result.OK, result.CaptchaType, result.VerifyCode, result.Elapsed)
	if !result.OK || result.UAToken == "" || result.UASig == "" {
		t.Fatal("live WAF acceptance failed")
	}
}

// TestOnlineWAFHTTP 只提交页面 URL，使用生产 HTTP 入口自动获取挑战并执行验证。
func TestOnlineWAFHTTP(t *testing.T) {
	pageURL := os.Getenv("ALI_SLIDER_WAF_ONLINE_URL")
	if pageURL == "" {
		t.Skip("set ALI_SLIDER_WAF_ONLINE_URL to run one live WAF challenge")
	}
	library := os.Getenv("ALI_SLIDER_V8_TEST_LIBRARY")
	if library == "" {
		t.Fatal("ALI_SLIDER_V8_TEST_LIBRARY is required")
	}
	const timeout = 35 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 2*timeout)
	defer cancel()
	options := DefaultOptions()
	options.V8RuntimeLibrary, options.ArtifactDir = library, t.TempDir()
	solver, err := NewService(options)
	if err != nil {
		t.Fatal("construct service")
	}
	defer solver.Close()
	executor, err := NewWAFExecutor(library, timeout)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpapi.New(httpapi.Options{Solver: solver, WAF: executor, Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	body, err := json.Marshal(map[string]string{
		"pageUrl": pageURL, "proxy": "",
	})
	if err != nil {
		t.Fatal("encode page request")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+httpapi.WAFPath, bytes.NewReader(body))
	if err != nil {
		t.Fatal("construct API request")
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal("send API request")
	}
	defer response.Body.Close()
	var result struct {
		OK         bool              `json:"ok"`
		VerifyCode string            `json:"verifyCode"`
		UAToken    string            `json:"u_atoken"`
		UASig      string            `json:"u_asig"`
		UAHeaders  map[string]string `json:"uaHeaders"`
		ElapsedMS  int64             `json:"elapsedMs"`
		ErrorType  string            `json:"errorType"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal("decode API response")
	}
	t.Logf("live page API: HTTP=%d ok=%t verifyCode=%s elapsedMs=%d errorType=%s", response.StatusCode, result.OK, result.VerifyCode, result.ElapsedMS, result.ErrorType)
	if response.StatusCode != http.StatusOK || !result.OK || result.VerifyCode != "T001" || result.UAToken == "" || result.UASig == "" || result.UAHeaders["User-Agent"] == "" {
		t.Fatal("live page API acceptance failed")
	}
	// 只有显式提供预期业务正文时才额外回访一次，不登录或提交其他业务。
	marker := os.Getenv("ALI_SLIDER_WAF_ONLINE_BODY_MARKER")
	if marker == "" {
		return
	}
	transport, _, err := httpclient.NewTransport("", 4)
	if err != nil {
		t.Fatal("create caller transport")
	}
	defer transport.CloseIdleConnections()
	verifiedURL, err := url.Parse(pageURL)
	if err != nil {
		t.Fatal("parse page URL")
	}
	query := verifiedURL.Query()
	query.Set("u_atoken", result.UAToken)
	query.Set("u_asig", result.UASig)
	verifiedURL.RawQuery = query.Encode()
	request, err = http.NewRequestWithContext(ctx, http.MethodGet, verifiedURL.String(), nil)
	if err != nil {
		t.Fatal("construct page request")
	}
	for key, value := range result.UAHeaders {
		request.Header.Set(key, value)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal("construct caller cookie jar")
	}
	client := &http.Client{Transport: transport, Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err = client.Do(request)
	if err != nil {
		t.Fatal("fetch verified business page")
	}
	defer response.Body.Close()
	page, err := io.ReadAll(io.LimitReader(response.Body, wafinfra.MaxPageBytes+1))
	if err != nil || len(page) > wafinfra.MaxPageBytes {
		t.Fatal("read business page")
	}
	_, challengeErr := wafinfra.ParseChallenge(page)
	matched := strings.Contains(string(page), marker)
	t.Logf("live business page: HTTP=%d expectedContent=%t challenge=%t cookies=%d", response.StatusCode, matched, challengeErr == nil, len(jar.Cookies(verifiedURL)))
	if response.StatusCode != http.StatusOK || !matched || challengeErr == nil {
		t.Fatal("live business page acceptance failed")
	}
}
