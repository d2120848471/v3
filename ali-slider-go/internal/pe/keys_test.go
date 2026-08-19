package pe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"reflect"
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

func TestNewKeyResolverCapacityOnlyLimitsPEIdleRuntimes(t *testing.T) {
	resolver := NewKeyResolverWithCapacity("fixture", 10)
	if resolver.v8PELimit != 10 {
		t.Fatalf("PE idle capacity=%d, want 10", resolver.v8PELimit)
	}

	resolverType := reflect.TypeOf(resolver).Elem()
	for _, fieldName := range []string{"deviceExecutionSlots", "liveDeviceSessions"} {
		if _, exists := resolverType.FieldByName(fieldName); exists {
			t.Fatalf("KeyResolver still contains local execution gate %q", fieldName)
		}
	}
}

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
	baseTime := time.Date(2026, time.August, 8, 12, 0, 0, 0, time.UTC)
	now := baseTime
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

	// 软 TTL 到期后重取非版本化 SDK。字节未变则延长精确
	// 内容路径的已验证画像，不重下 PE、不重跑 V8。
	now = now.Add(keyProfileCacheTTL + time.Second)
	if _, err := resolver.Resolve(context.Background(), transport, device.Profile{}, testDynamicPath); err != nil {
		t.Fatal(err)
	}
	if got := transport.callCount("o.alicdn.com"); got != 2 {
		t.Fatalf("refreshed SDK downloads=%d, want 2", got)
	}
	if got := transport.callCount(keyPEHost); got != 2 {
		t.Fatalf("PE downloads=%d, want 2", got)
	}
	if got := collections.Load(); got != 2 {
		t.Fatalf("collections=%d, want 2", got)
	}

	// 硬 TTL 到期后，即使 SDK 字节不变也必须重下该 PE
	// 并重做完整动态差分。
	now = baseTime.Add(keyProfileHardTTL + time.Second)
	if _, err := resolver.Resolve(context.Background(), transport, device.Profile{}, testDynamicPath); err != nil {
		t.Fatal(err)
	}
	if got := transport.callCount("o.alicdn.com"); got != 3 {
		t.Fatalf("hard refresh SDK downloads=%d, want 3", got)
	}
	if got := transport.callCount(keyPEHost); got != 3 {
		t.Fatalf("hard refresh PE downloads=%d, want 3", got)
	}
	if got := collections.Load(); got != 3 {
		t.Fatalf("hard refresh collections=%d, want 3", got)
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

func TestKeyResolverPrepareCachesSourcesAndSamplesProfileOnce(t *testing.T) {
	transport := newScriptTransport()
	resolver := NewKeyResolver("node")
	var collections atomic.Int32
	resolver.collect = func(context.Context, string, device.Profile, []byte, []byte, time.Time) (RuntimeProfile, error) {
		collections.Add(1)
		return RuntimeProfile{ArgumentKey: testArgumentKey}, nil
	}

	for range 2 {
		if err := resolver.Prepare(context.Background(), transport, device.Profile{}, testDynamicPath); err != nil {
			t.Fatal(err)
		}
	}
	if got := collections.Load(); got != 1 {
		t.Fatalf("profile collections=%d, want 1", got)
	}
	if got := transport.callCount("o.alicdn.com"); got != 1 {
		t.Fatalf("SDK downloads=%d, want 1", got)
	}
	if got := transport.callCount(keyPEHost); got != 1 {
		t.Fatalf("PE downloads=%d, want 1", got)
	}

	profile, err := resolver.Resolve(context.Background(), transport, device.Profile{}, testDynamicPath)
	if err != nil || profile.ArgumentKey != testArgumentKey {
		t.Fatalf("Resolve after Prepare: profile=%+v err=%v", profile, err)
	}
	if got := collections.Load(); got != 1 {
		t.Fatalf("cached profile collections=%d, want 1", got)
	}
	if transport.callCount("o.alicdn.com") != 1 || transport.callCount(keyPEHost) != 1 {
		t.Fatal("explicit Resolve downloaded already prepared scripts")
	}
}

func TestKeyResolverPrepareCachesProfileFailureForV8Fallback(t *testing.T) {
	transport := newScriptTransport()
	resolver := NewKeyResolver("node")
	var collections atomic.Int32
	resolver.collect = func(context.Context, string, device.Profile, []byte, []byte, time.Time) (RuntimeProfile, error) {
		collections.Add(1)
		return RuntimeProfile{}, fmt.Errorf("%w: parity mismatch", ErrUnsupportedPE)
	}
	for range 2 {
		if err := resolver.Prepare(context.Background(), transport, device.Profile{}, testDynamicPath); err != nil {
			t.Fatalf("Prepare() should preserve V8 fallback: %v", err)
		}
	}
	if got := collections.Load(); got != 1 {
		t.Fatalf("failed profile collections=%d, want 1", got)
	}
	path, _ := normalizeStaticPath(testDynamicPath)
	profile, profileErr, sampled := resolver.cachedProfileState(path, resolver.now())
	if !sampled || !errors.Is(profileErr, ErrUnsupportedPE) || profile.PureGoCompatible {
		t.Fatalf("fallback profile state: sampled=%t profile=%+v err=%v", sampled, profile, profileErr)
	}
}

func TestKeyResolverProfileCacheStats(t *testing.T) {
	if got := (*KeyResolver)(nil).ProfileCacheStats(); got != (ProfileCacheStats{}) {
		t.Fatalf("nil resolver stats=%+v", got)
	}

	now := time.Date(2026, time.August, 10, 2, 0, 0, 0, time.UTC)
	resolver := NewKeyResolver("")
	resolver.now = func() time.Time { return now }
	resolver.keys = map[string]cachedRuntimeProfile{
		"source-only": {
			sampledAt: now,
		},
		"compatible": {
			profile: RuntimeProfile{PureGoCompatible: true}, profileSampled: true, sampledAt: now,
		},
		"failed": {
			profileSampled: true, profileErr: ErrUnsupportedPE, sampledAt: now,
		},
		"incompatible": {
			profileSampled: true, sampledAt: now,
		},
		"expired": {
			profileSampled: true, sampledAt: now.Add(-keyProfileCacheTTL),
		},
	}
	want := ProfileCacheStats{SourcePaths: 4, Sampled: 3, Compatible: 1, Failed: 1}
	if got := resolver.ProfileCacheStats(); got != want {
		t.Fatalf("stats=%+v, want %+v", got, want)
	}
}

type blockingPETransport struct {
	base    *scriptTransport
	started chan string
	release chan struct{}
}

func (transport *blockingPETransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Hostname() == keyPEHost {
		transport.started <- request.URL.Path
		select {
		case <-transport.release:
		case <-request.Context().Done():
			return nil, request.Context().Err()
		}
	}
	return transport.base.RoundTrip(request)
}

func TestKeyResolverPrepareAllowsDifferentPathsInParallel(t *testing.T) {
	base := newScriptTransport()
	transport := &blockingPETransport{
		base: base, started: make(chan string, 2), release: make(chan struct{}),
	}
	resolver := NewKeyResolver("node")
	paths := []string{testDynamicPath, "3.29.0/pe.059.aaaaaaaaaaaaaaaa"}
	errorsByPath := make(chan error, len(paths))
	for _, path := range paths {
		go func() {
			errorsByPath <- resolver.Prepare(context.Background(), transport, device.Profile{}, path)
		}()
	}
	for range paths {
		select {
		case <-transport.started:
		case <-time.After(time.Second):
			close(transport.release)
			t.Fatal("different PE paths were serialized")
		}
	}
	close(transport.release)
	for range paths {
		if err := <-errorsByPath; err != nil {
			t.Fatal(err)
		}
	}
	if got := base.callCount("o.alicdn.com"); got != 1 {
		t.Fatalf("SDK downloads=%d, want 1", got)
	}
	if got := base.callCount(keyPEHost); got != len(paths) {
		t.Fatalf("PE downloads=%d, want %d", got, len(paths))
	}
}

func TestKeyResolverSamplesDifferentProfilesInParallel(t *testing.T) {
	transport := newScriptTransport()
	resolver := NewKeyResolver("node")
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	resolver.collect = func(context.Context, string, device.Profile, []byte, []byte, time.Time) (RuntimeProfile, error) {
		started <- struct{}{}
		<-release
		return RuntimeProfile{ArgumentKey: testArgumentKey}, nil
	}
	paths := []string{testDynamicPath, "3.29.0/pe.059.aaaaaaaaaaaaaaaa"}
	errorsByPath := make(chan error, len(paths))
	for _, path := range paths {
		go func() {
			errorsByPath <- resolver.Prepare(context.Background(), transport, device.Profile{}, path)
		}()
	}
	for range paths {
		select {
		case <-started:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("different profile paths were serialized")
		}
	}
	close(release)
	for range paths {
		if err := <-errorsByPath; err != nil {
			t.Fatal(err)
		}
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
