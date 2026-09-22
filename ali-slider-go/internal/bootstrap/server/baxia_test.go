package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/application/service"
	"github.com/d2120848471/v3/ali-slider-go/internal/application/solve"
	"github.com/d2120848471/v3/ali-slider-go/internal/bootstrap/config"
	"github.com/d2120848471/v3/ali-slider-go/internal/interfaces/httpapi"
)

func TestServeInjectsBaxiaAndForwardsDownloadProxy(t *testing.T) {
	const proxyCredentials = "proxy-user:private-password"
	var proxyCalls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		proxyCalls.Add(1)
		if request.Method != http.MethodConnect || request.Host != "g.alicdn.com:443" {
			t.Errorf("unexpected download route: method=%s host=%s", request.Method, request.Host)
		}
		wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(proxyCredentials))
		if request.Header.Get("Proxy-Authorization") != wantAuth {
			t.Error("proxy authentication was not forwarded")
		}
		w.WriteHeader(http.StatusProxyAuthRequired)
		_, _ = io.WriteString(w, "private-upstream-body")
	}))
	defer proxy.Close()
	address, logs, stop := startBaxiaTestServer(t, 5*time.Second)
	client := &http.Client{Timeout: 2 * time.Second}
	health, err := client.Get(address + httpapi.HealthPath)
	if err != nil {
		t.Fatal(err)
	}
	health.Body.Close()
	if health.StatusCode != http.StatusOK || proxyCalls.Load() != 0 {
		t.Fatal("readiness executed Baxia")
	}
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxyURL.User = url.UserPassword("proxy-user", "private-password")
	payload, err := json.Marshal(map[string]string{
		"pageUrl": "https://page.example/login#form", "requestUrl": "https://api.example/login", "proxy": proxyURL.String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Post(address+httpapi.BaxiaPath, "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	stop()
	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusInternalServerError || result["errorType"] != "BaxiaSourceError" || proxyCalls.Load() != 1 {
		t.Fatalf("production Baxia not routed to proxy: status=%d calls=%d body=%s", response.StatusCode, proxyCalls.Load(), body)
	}
	if strings.Contains(string(body), "private-") || strings.Contains(logs.String(), "private-") || strings.Contains(logs.String(), "proxy-user") {
		t.Fatal("production response or logs exposed proxy credentials or upstream body")
	}
}

func TestServeAcceptsSubMillisecondTimeoutWithBaxia(t *testing.T) {
	address, _, stop := startBaxiaTestServer(t, 500*time.Microsecond)
	response, err := (&http.Client{Timeout: 2 * time.Second}).Get(address + httpapi.HealthPath)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	stop()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("health status=%d", response.StatusCode)
	}
}

func startBaxiaTestServer(t *testing.T, timeout time.Duration) (string, *bytes.Buffer, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	checked := make(chan struct{})
	application, err := service.New(service.Options{
		Executor: runExecutor(func(context.Context, solve.Request) (solve.Outcome, error) {
			t.Error("Baxia request reached slider solver")
			return solve.Outcome{}, nil
		}),
		Resources: runResources{check: func() error { close(checked); return nil }, purge: func() (int, error) { return 0, nil }},
	})
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Port = listener.Addr().(*net.TCPAddr).Port
	cfg.Timeout = timeout
	// 健康检查和代理拒绝都应在加载该不存在的运行库前结束。
	cfg.V8RuntimeLibrary = "/missing-server-v8-library"
	ctx, cancel := context.WithCancel(context.Background())
	logs := new(bytes.Buffer)
	done := make(chan error, 1)
	go func() { done <- serve(ctx, listener, cfg, application, log.New(logs, "", 0)) }()
	var stopOnce sync.Once
	stop := func() {
		t.Helper()
		stopOnce.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("serve: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Error("server shutdown did not complete")
			}
			listener.Close()
			application.Close()
		})
	}
	t.Cleanup(stop)
	select {
	case <-checked:
	case <-time.After(time.Second):
		t.Fatal("server readiness check did not run")
	}
	return "http://" + listener.Addr().String(), logs, stop
}
