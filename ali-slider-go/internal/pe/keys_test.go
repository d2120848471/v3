package pe

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/device"
)

const (
	testDynamicPath = "3.29.0/pe.058.77d5c01b1737016e"
	testArgumentKey = "dmmlums5zuewlgt7"
)

type scriptTransport struct {
	mu    sync.Mutex
	calls []string
	sdk   []byte
	pe    []byte
}

func newScriptTransport() *scriptTransport {
	return &scriptTransport{
		sdk: []byte("AliyunCaptcha" + strings.Repeat("s", 1_100)),
		pe:  []byte("CaptchaConstructor" + strings.Repeat("p", 1_100)),
	}
}

func (transport *scriptTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method != http.MethodGet {
		return nil, errors.New("unexpected method")
	}
	transport.mu.Lock()
	transport.calls = append(transport.calls, request.URL.Hostname())
	transport.mu.Unlock()
	content := transport.pe
	if request.URL.Hostname() == "o.alicdn.com" {
		content = transport.sdk
	} else if request.URL.Hostname() != keyPEHost {
		return nil, errors.New("unexpected host")
	}
	return &http.Response{
		StatusCode:    http.StatusOK,
		ContentLength: int64(len(content)),
		Header:        make(http.Header),
		Body:          io.NopCloser(bytes.NewReader(content)),
		Request:       request,
	}, nil
}

func (transport *scriptTransport) callCount(host string) int {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	count := 0
	for _, calledHost := range transport.calls {
		if calledHost == host {
			count++
		}
	}
	return count
}

func TestKeyResolverCachesExactPathsAndRefreshesSDKOnMiss(t *testing.T) {
	transport := newScriptTransport()
	resolver := NewKeyResolver("node")
	now := time.Date(2026, time.August, 8, 12, 0, 0, 0, time.UTC)
	resolver.now = func() time.Time { return now }
	var collections atomic.Int32
	resolver.collect = func(_ context.Context, _ string, _ device.Profile, sdkSource, peSource []byte, _ time.Time) (RuntimeProfile, error) {
		collections.Add(1)
		if !bytes.Contains(sdkSource, []byte("AliyunCaptcha")) || !bytes.Contains(peSource, []byte("CaptchaConstructor")) {
			return RuntimeProfile{}, errors.New("script marker missing")
		}
		return RuntimeProfile{ArgumentKey: testArgumentKey}, nil
	}

	for range 2 {
		profile, err := resolver.Resolve(context.Background(), transport, device.Profile{}, testDynamicPath+".js")
		if err != nil || profile.ArgumentKey != testArgumentKey || profile.IncludeScreenInfo {
			t.Fatalf("first path: profile=%+v err=%v", profile, err)
		}
	}
	secondPath := "3.29.0/pe.059.aaaaaaaaaaaaaaaa"
	if _, err := resolver.Resolve(context.Background(), transport, device.Profile{}, secondPath); err != nil {
		t.Fatal(err)
	}
	if got := transport.callCount("o.alicdn.com"); got != 1 {
		t.Fatalf("SDK downloads=%d, want 1", got)
	}
	if got := transport.callCount(keyPEHost); got != 2 {
		t.Fatalf("PE downloads=%d, want 2", got)
	}
	if got := collections.Load(); got != 2 {
		t.Fatalf("collections=%d, want 2", got)
	}

	// 非版本化 SDK 可能在同一 PE 路径存活期间改变 Track schema，因此 key+schema
	// 只缓存五分钟；到期后同时重取 SDK/PE 并重新采样。
	now = now.Add(keyProfileCacheTTL + time.Second)
	if _, err := resolver.Resolve(context.Background(), transport, device.Profile{}, testDynamicPath); err != nil {
		t.Fatal(err)
	}
	if got := transport.callCount("o.alicdn.com"); got != 2 {
		t.Fatalf("refreshed SDK downloads=%d, want 2", got)
	}
	if got := transport.callCount(keyPEHost); got != 3 {
		t.Fatalf("PE downloads=%d, want 3", got)
	}
	if got := collections.Load(); got != 3 {
		t.Fatalf("collections=%d, want 3", got)
	}
}

func TestKeyResolverSerializesConcurrentMiss(t *testing.T) {
	transport := newScriptTransport()
	resolver := NewKeyResolver("node")
	var collections atomic.Int32
	resolver.collect = func(context.Context, string, device.Profile, []byte, []byte, time.Time) (RuntimeProfile, error) {
		collections.Add(1)
		return RuntimeProfile{ArgumentKey: testArgumentKey}, nil
	}

	const workers = 24
	start := make(chan struct{})
	errorsByWorker := make(chan error, workers)
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			profile, err := resolver.Resolve(context.Background(), transport, device.Profile{}, testDynamicPath)
			if err == nil && profile.ArgumentKey != testArgumentKey {
				err = errors.New("resolved key mismatch")
			}
			errorsByWorker <- err
		}()
	}
	close(start)
	wait.Wait()
	close(errorsByWorker)
	for err := range errorsByWorker {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := collections.Load(); got != 1 {
		t.Fatalf("collections=%d, want 1", got)
	}
	if got := transport.callCount("o.alicdn.com"); got != 1 {
		t.Fatalf("SDK downloads=%d, want 1", got)
	}
	if got := transport.callCount(keyPEHost); got != 1 {
		t.Fatalf("PE downloads=%d, want 1", got)
	}
}

func TestKeyResolverSamplesKnownPathInsteadOfTrustingStaleSchema(t *testing.T) {
	transport := newScriptTransport()
	resolver := NewKeyResolver("node")
	const knownPath = "3.29.0/pe.091.00665af58b020d81"
	var collections atomic.Int32
	resolver.collect = func(context.Context, string, device.Profile, []byte, []byte, time.Time) (RuntimeProfile, error) {
		collections.Add(1)
		return RuntimeProfile{ArgumentKey: argKeys[knownPath]}, nil
	}
	profile, err := resolver.Resolve(context.Background(), transport, device.Profile{}, knownPath+".js")
	if err != nil {
		t.Fatal(err)
	}
	if profile.ArgumentKey != argKeys[knownPath] || collections.Load() != 1 {
		t.Fatalf("known profile=%+v collections=%d", profile, collections.Load())
	}
	if transport.callCount("o.alicdn.com") != 1 || transport.callCount(keyPEHost) != 1 {
		t.Fatal("known key table incorrectly bypassed current SDK/PE sampling")
	}
}

func TestSDKContentChangeInvalidatesDependentPathProfiles(t *testing.T) {
	transport := newScriptTransport()
	resolver := NewKeyResolver("node")
	now := time.Date(2026, time.August, 8, 12, 0, 0, 0, time.UTC)
	resolver.now = func() time.Time { return now }
	resolver.collect = func(context.Context, string, device.Profile, []byte, []byte, time.Time) (RuntimeProfile, error) {
		return RuntimeProfile{ArgumentKey: testArgumentKey}, nil
	}
	if _, err := resolver.Resolve(context.Background(), transport, device.Profile{}, testDynamicPath); err != nil {
		t.Fatal(err)
	}
	path, _ := normalizeStaticPath(testDynamicPath)
	if _, ok := resolver.cachedKey(path, now); !ok {
		t.Fatal("expected cached path profile")
	}
	transport.sdk = []byte("AliyunCaptcha-updated" + strings.Repeat("n", 1_100))
	now = now.Add(keySDKCacheTTL + time.Second)
	if _, err := resolver.sdkSource(context.Background(), transport, device.Profile{}, now); err != nil {
		t.Fatal(err)
	}
	if _, ok := resolver.cachedKey(path, now); ok {
		t.Fatal("path profile survived an SDK content change")
	}
}

func TestKeyResolverRejectsUnsafePathBeforeNetwork(t *testing.T) {
	transport := newScriptTransport()
	resolver := NewKeyResolver("node")
	for _, path := range []string{
		"../pe.058.77d5c01b1737016e",
		"3.29.0/pe.58.77d5c01b1737016e",
		"3.29.0/pe.058.77d5c01b1737016g",
		"https://example.com/pe.058.77d5c01b1737016e",
	} {
		if _, err := resolver.Resolve(context.Background(), transport, device.Profile{}, path); !errors.Is(err, ErrUnsupportedPE) {
			t.Fatalf("path %q: %v", path, err)
		}
	}
	if len(transport.calls) != 0 {
		t.Fatalf("unsafe paths caused requests: %v", transport.calls)
	}
}

func TestDownloadPublicScriptEnforcesHTTPSHostAndSize(t *testing.T) {
	transport := newScriptTransport()
	if _, err := downloadPublicScript(context.Background(), transport, device.Profile{}, "http://o.alicdn.com/a.js", map[string]bool{"o.alicdn.com": true}, 1024); !errors.Is(err, ErrKeyRuntime) {
		t.Fatalf("HTTP URL: %v", err)
	}
	transport.sdk = bytes.Repeat([]byte("x"), 1_025)
	if _, err := downloadPublicScript(context.Background(), transport, device.Profile{}, "https://o.alicdn.com/a.js", map[string]bool{"o.alicdn.com": true}, 1_024); !errors.Is(err, ErrUnsupportedPE) {
		t.Fatalf("oversize response: %v", err)
	}
	if err := validatePublicScriptURL(mustURL(t, "https://o.alicdn.com:444/a.js"), map[string]bool{"o.alicdn.com": true}); err == nil {
		t.Fatal("non-default HTTPS port accepted")
	}
}

func mustURL(t *testing.T, rawURL string) *url.URL {
	t.Helper()
	value, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestCollectRuntimeProfileReportsMissingNode(t *testing.T) {
	_, err := collectRuntimeProfile(context.Background(), filepath.Join(t.TempDir(), "missing-node"), device.Profile{}, nil, nil, time.Now())
	if !errors.Is(err, ErrKeyRuntime) {
		t.Fatalf("missing Node: %v", err)
	}
}
