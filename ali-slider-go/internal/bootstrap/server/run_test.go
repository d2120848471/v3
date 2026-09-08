package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/application/service"
	"github.com/d2120848471/v3/ali-slider-go/internal/application/solve"
	"github.com/d2120848471/v3/ali-slider-go/internal/bootstrap/config"
	"github.com/d2120848471/v3/ali-slider-go/internal/interfaces/httpapi"
)

func TestServiceOptionsMapsServiceConfiguration(t *testing.T) {
	cfg := config.Defaults()
	cfg.SceneID = "scene"
	cfg.Prefix = "prefix9"
	cfg.MaxConcurrency = 7
	cfg.Timeout = 9 * time.Second
	cfg.V8RuntimeLibrary = "/opt/ali-slider/libali_slider_v8_runtime.so"
	options := serviceOptions(cfg)
	if options.DefaultSceneID != "scene" || options.DefaultPrefix != "prefix9" || options.MaxConcurrency != 7 || options.Timeout != 9*time.Second || options.DevicePrewarmCapacity != 0 || options.DeviceSessionReserve != 0 || options.V8RuntimeLibrary != "/opt/ali-slider/libali_slider_v8_runtime.so" {
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
	err := Run(context.Background(), []string{"--port=0"}, func(string) string { return "" }, logger)
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
	err = Run(context.Background(), []string{
		"--host=127.0.0.1", "--port=" + strconv.Itoa(port),
		"--device-prewarm=0", "--artifact-dir=" + t.TempDir(),
	}, func(string) string { return "" }, logger)
	if err == nil || !strings.Contains(err.Error(), "HTTP 服务退出") {
		t.Fatalf("error=%v", err)
	}
}

func TestHeaderBudgetAcceptsMaximumLegacyQuery(t *testing.T) {
	testServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := int64(len(r.URL.RawQuery)); got != httpapi.MaxRequestBytes {
			t.Errorf("raw query bytes = %d, want %d", got, httpapi.MaxRequestBytes)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	testServer.Config.MaxHeaderBytes = maxHeaderBytes
	testServer.Start()
	t.Cleanup(testServer.Close)

	response, err := testServer.Client().Get(testServer.URL + "/api/slider?" + strings.Repeat("x", int(httpapi.MaxRequestBytes)))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusNoContent)
	}
}

type runExecutor func(context.Context, solve.Request) (solve.Outcome, error)

func (execute runExecutor) Solve(ctx context.Context, request solve.Request) (solve.Outcome, error) {
	return execute(ctx, request)
}

type runResources struct {
	check func() error
	purge func() (int, error)
}

func (resources runResources) CheckRuntime() error          { return resources.check() }
func (resources runResources) PurgeArtifacts() (int, error) { return resources.purge() }
func (runResources) Close()                                 {}

func TestServeReadinessAndContextShutdown(t *testing.T) {
	for _, purgeFailure := range []bool{false, true} {
		t.Run(strconv.FormatBool(purgeFailure), func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			checked := make(chan struct{})
			var purged atomic.Bool
			application, err := service.New(service.Options{
				Executor: runExecutor(func(context.Context, solve.Request) (solve.Outcome, error) { return solve.Outcome{}, nil }),
				Resources: runResources{
					check: func() error { close(checked); return nil },
					purge: func() (int, error) {
						purged.Store(true)
						if purgeFailure {
							return 0, errors.New("fixture purge failure")
						}
						return 1, nil
					},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer application.Close()
			cfg := config.Defaults()
			cfg.Port = listener.Addr().(*net.TCPAddr).Port
			var logs bytes.Buffer
			done := make(chan error, 1)
			go func() { done <- serve(ctx, listener, cfg, application, log.New(&logs, "", 0)) }()
			select {
			case <-checked:
			case <-time.After(time.Second):
				t.Fatal("runtime readiness check was not called")
			}
			client := &http.Client{Timeout: 2 * time.Second}
			response, err := client.Get("http://" + listener.Addr().String() + httpapi.HealthPath)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != http.StatusOK || !purged.Load() {
				t.Fatalf("status=%d purged=%t", response.StatusCode, purged.Load())
			}
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("server did not stop after cancellation")
			}
			if !strings.Contains(logs.String(), "event=shutdown status=ok") {
				t.Fatalf("shutdown logs=%s", logs.String())
			}
			if purgeFailure && !strings.Contains(logs.String(), "event=artifact_purge status=warning") {
				t.Fatalf("purge warning missing: %s", logs.String())
			}
		})
	}
}

func TestRunReportsRuntimeFailureAndReleasesListener(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	err = Run(context.Background(), []string{
		"--host=127.0.0.1", "--port=" + strconv.Itoa(port),
		"--v8-library=" + filepath.Join(t.TempDir(), "missing-v8-library"),
	}, func(string) string { return "" }, log.New(io.Discard, "", 0))
	if err == nil || !strings.Contains(err.Error(), "检查内嵌 V8") {
		t.Fatalf("error=%v", err)
	}
	reopened, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("failed startup retained listener: %v", err)
	}
	reopened.Close()
}
