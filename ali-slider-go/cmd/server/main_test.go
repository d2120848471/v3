package main

import (
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/config"
	"github.com/d2120848471/v3/ali-slider-go/internal/server"
)

func TestClientOptionsMapsServiceConfiguration(t *testing.T) {
	cfg := config.Defaults()
	cfg.SceneID = "scene"
	cfg.Prefix = "prefix9"
	cfg.MaxConcurrency = 7
	cfg.Timeout = 9 * time.Second
	cfg.DevicePrewarmCapacity = 3
	cfg.V8RuntimeLibrary = "/opt/ali-slider/libali_slider_v8_runtime.so"
	options := clientOptions(cfg)
	if options.DefaultSceneID != "scene" || options.DefaultPrefix != "prefix9" || options.MaxConcurrency != 7 || options.Timeout != 9*time.Second || options.DevicePrewarmCapacity != 3 || options.V8RuntimeLibrary != "/opt/ali-slider/libali_slider_v8_runtime.so" {
		t.Fatalf("options=%+v", options)
	}
	if options.AssetMaxBytes != cfg.AssetMaxBytes || options.ArtifactRetention != cfg.ArtifactRetention {
		t.Fatalf("resource options=%+v", options)
	}
}

func TestLoopbackDetection(t *testing.T) {
	for _, host := range []string{"localhost", "127.0.0.1", "::1"} {
		if !isLoopbackHost(host) {
			t.Errorf("%q should be loopback", host)
		}
	}
	for _, host := range []string{"0.0.0.0", "192.0.2.1", "invalid"} {
		if isLoopbackHost(host) {
			t.Errorf("%q should not be loopback", host)
		}
	}
}

func TestRunRejectsInvalidConfigBeforeSideEffects(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	err := run([]string{"--port=0"}, func(string) string { return "" }, logger)
	if err == nil || !strings.Contains(err.Error(), "配置无效") {
		t.Fatalf("error=%v", err)
	}
}

func TestRunReportsOccupiedListenerWithoutExternalRequests(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	logger := log.New(io.Discard, "", 0)
	err = run([]string{
		"--host=127.0.0.1", "--port=" + strconv.Itoa(port),
		"--device-prewarm=0", "--artifact-dir=" + t.TempDir(),
	}, func(string) string { return "" }, logger)
	if err == nil || !strings.Contains(err.Error(), "HTTP 服务退出") {
		t.Fatalf("error=%v", err)
	}
}

func TestHeaderBudgetAcceptsMaximumLegacyQuery(t *testing.T) {
	testServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := int64(len(r.URL.RawQuery)); got != server.MaxRequestBytes {
			t.Errorf("raw query bytes = %d, want %d", got, server.MaxRequestBytes)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	testServer.Config.MaxHeaderBytes = maxHeaderBytes
	testServer.Start()
	t.Cleanup(testServer.Close)

	response, err := testServer.Client().Get(testServer.URL + "/api/slider?" + strings.Repeat("x", int(server.MaxRequestBytes)))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusNoContent)
	}
}
