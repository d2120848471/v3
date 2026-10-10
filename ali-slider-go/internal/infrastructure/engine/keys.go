package engine

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/domain/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/pe"
	"github.com/d2120848471/v3/ali-slider-go/internal/platform/v8runtime"
)

const (
	defaultNodeBinary   = "node"
	keySDKURL           = "https://o.alicdn.com/captcha-frontend/aliyunCaptcha/AliyunCaptcha.js"
	keyPEHost           = "g.alicdn.com"
	keyCaptchaAssetHost = "x.alicdn.com"
	keyProfileCacheTTL  = 5 * time.Minute
	keyProfileHardTTL   = 30 * time.Minute
	keySDKCacheTTL      = keyProfileCacheTTL
	keyScriptMaxBytes   = int64(2 << 20)
	keyBridgeMaxBytes   = 64 << 10
	keyBridgeTimeout    = 10 * time.Second
	keyBridgeOrigin     = "http://localhost:38185"
	keyBridgeReferer    = keyBridgeOrigin + "/"
	dummyCertifyID      = "0123456789abcdef"
	maxV8PEIdle         = 32
)

var (
	staticPathPattern  = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+/pe\.[0-9]{3}\.[a-f0-9]{16}$`)
	argumentKeyPattern = regexp.MustCompile(`^[a-z0-9]{16}$`)
)

//go:embed runtime/sdk_device_bridge.mjs
var sdkDeviceBridgeSource []byte

//go:embed runtime/pe_key_bridge.mjs
var peKeyBridgeSource []byte

// ProfileCacheStats 是不含路径、key 或挑战字段的脱敏缓存计数。
type ProfileCacheStats struct {
	SourcePaths int
	Sampled     int
	Compatible  int
	Failed      int
}

type keyCollector func(context.Context, string, device.Profile, []byte, []byte, time.Time) (pe.RuntimeProfile, error)

type cachedRuntimeProfile struct {
	profile         pe.RuntimeProfile
	profileSampled  bool
	profileErr      error
	sdkSource       []byte
	peSource        []byte
	sampledAt       time.Time
	sourceFetchedAt time.Time
}

type cachedSDK struct {
	source    []byte
	fetchedAt time.Time
}

type pendingScriptLoad struct {
	done chan struct{}
	err  error
}

// KeyResolver 按精确 StaticPath 缓存公开 SDK/PE 源码和结构画像。
// SDK 每五分钟字节复核，PE/profile 最多三十分钟强制重采样。
// Resolve 的缓存命中不启动 VM；Build 在自校验兼容时纯 Go 构造，
// 否则独占一个预加载 Isolate 并在新浏览器 context 中运行。
type KeyResolver struct {
	v8LibraryPath string
	nodeBinary    string // 仅供 Node oracle 测试路径使用。
	now           func() time.Time
	collect       keyCollector

	v8Mu      sync.Mutex
	v8Library *v8runtime.Library
	v8PEIdle  []*v8runtime.Runtime
	v8PELimit int
	v8Closed  bool

	mu           sync.RWMutex
	keys         map[string]cachedRuntimeProfile
	sdk          cachedSDK
	sdkLoad      *pendingScriptLoad
	peLoads      map[string]*pendingScriptLoad
	profileLoads map[string]*pendingScriptLoad
}

// NewKeyResolver 创建进程级内嵌 V8 resolver。libraryPath 为空时从服务
// 可执行文件所在目录加载当前平台的默认 wrapper 文件。
func NewKeyResolver(libraryPath string) *KeyResolver {
	return NewKeyResolverWithCapacity(libraryPath, 1)
}

// NewKeyResolverWithCapacity 创建最多保留 capacity 个预加载 PE Isolate 的 resolver。
// 活跃调用仍各用独立 Isolate；这里只限制任务结束后的空闲保留量。
func NewKeyResolverWithCapacity(libraryPath string, capacity int) *KeyResolver {
	if strings.TrimSpace(libraryPath) == "" {
		libraryPath = v8runtime.DefaultLibraryPath()
	}
	capacity = max(1, min(capacity, maxV8PEIdle))
	return &KeyResolver{
		v8LibraryPath: libraryPath,
		v8PELimit:     capacity,
		now:           time.Now,
		keys:          make(map[string]cachedRuntimeProfile),
		peLoads:       make(map[string]*pendingScriptLoad),
		profileLoads:  make(map[string]*pendingScriptLoad),
	}
}

// newNodeKeyResolver 只保留历史 Node 路径作为 oracle 和回滚测试，不由生产构造器调用。
func newNodeKeyResolver(nodeBinary string) *KeyResolver {
	if strings.TrimSpace(nodeBinary) == "" {
		nodeBinary = defaultNodeBinary
	}
	resolver := NewKeyResolver("")
	resolver.v8LibraryPath = ""
	resolver.nodeBinary = nodeBinary
	resolver.collect = collectRuntimeProfile
	return resolver
}

// Resolve 返回本轮 StaticPath 的结构画像。缓存键是完整版本化路径，绝不把
// 一个“最新画像”覆盖给并存的其他 PE 分片。
func (resolver *KeyResolver) Resolve(ctx context.Context, transport http.RoundTripper, profile device.Profile, staticPath string) (pe.RuntimeProfile, error) {
	if resolver == nil {
		return pe.RuntimeProfile{}, fmt.Errorf("%w: resolver is nil", pe.ErrKeyRuntime)
	}
	if ctx == nil {
		return pe.RuntimeProfile{}, fmt.Errorf("%w: context is nil", pe.ErrKeyRuntime)
	}
	path, err := normalizeStaticPath(staticPath)
	if err != nil {
		return pe.RuntimeProfile{}, err
	}
	return resolver.ensureRuntimeProfile(ctx, transport, profile, path)
}

// Prepare 下载并缓存当轮精确 SDK/PE 源码，并在软/硬 TTL
// 要求时为该精确分片执行一次 V8/纯 Go 差分。差分不通过
// 不会让 Prepare 失败；Build 会自动保留原 V8 路径。
func (resolver *KeyResolver) Prepare(ctx context.Context, transport http.RoundTripper, profile device.Profile, staticPath string) error {
	if resolver == nil {
		return fmt.Errorf("%w: resolver is nil", pe.ErrKeyRuntime)
	}
	if ctx == nil {
		return fmt.Errorf("%w: context is nil", pe.ErrKeyRuntime)
	}
	path, err := normalizeStaticPath(staticPath)
	if err != nil {
		return err
	}
	if transport == nil {
		return fmt.Errorf("%w: transport is nil", pe.ErrKeyNetwork)
	}
	if err := resolver.prepareRuntimeSources(ctx, transport, profile, path); err != nil {
		return err
	}
	_, _ = resolver.ensureRuntimeProfile(ctx, transport, profile, path)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	// 采样/差分失败只关闭该分片快路，不影响原生 V8 Build。
	return nil
}

func (resolver *KeyResolver) ensureRuntimeProfile(
	ctx context.Context,
	transport http.RoundTripper,
	profile device.Profile,
	path string,
) (pe.RuntimeProfile, error) {
	if cached, cachedErr, sampled := resolver.cachedProfileState(path, resolver.now()); sampled {
		return cached, cachedErr
	}
	if transport == nil {
		return pe.RuntimeProfile{}, fmt.Errorf("%w: transport is nil", pe.ErrKeyNetwork)
	}
	if err := resolver.prepareRuntimeSources(ctx, transport, profile, path); err != nil {
		return pe.RuntimeProfile{}, err
	}
	if cached, cachedErr, sampled := resolver.cachedProfileState(path, resolver.now()); sampled {
		return cached, cachedErr
	}

	resolver.mu.Lock()
	if cached, cachedErr, sampled := resolver.cachedProfileStateLocked(path, resolver.now()); sampled {
		resolver.mu.Unlock()
		return cached, cachedErr
	}
	if pending := resolver.profileLoads[path]; pending != nil {
		done := pending.done
		resolver.mu.Unlock()
		select {
		case <-done:
			if cached, cachedErr, sampled := resolver.cachedProfileState(path, resolver.now()); sampled {
				return cached, cachedErr
			}
			if pending.err != nil {
				return pe.RuntimeProfile{}, pending.err
			}
			return pe.RuntimeProfile{}, fmt.Errorf("%w: runtime profile missing after sample", pe.ErrKeyRuntime)
		case <-ctx.Done():
			return pe.RuntimeProfile{}, ctx.Err()
		}
	}
	pending := &pendingScriptLoad{done: make(chan struct{})}
	resolver.profileLoads[path] = pending
	resolver.mu.Unlock()

	now := resolver.now()
	sdkSource, peSource, ok := resolver.cachedRuntimeSources(path, now)
	var resolved pe.RuntimeProfile
	var sampleErr error
	if !ok {
		sampleErr = fmt.Errorf("%w: runtime scripts missing", pe.ErrKeyRuntime)
	} else if resolver.collect != nil {
		resolved, sampleErr = resolver.collect(ctx, resolver.nodeBinary, profile, sdkSource, peSource, now)
	} else {
		resolved, sampleErr = resolver.collectV8RuntimeProfile(ctx, profile, sdkSource, peSource, now)
	}
	if sampleErr == nil && !argumentKeyPattern.MatchString(resolved.ArgumentKey) {
		sampleErr = fmt.Errorf("%w: invalid argument key", pe.ErrUnsupportedPE)
	}

	resolver.mu.Lock()
	cached, stillCurrent := resolver.keys[path]
	if stillCurrent {
		stillCurrent = bytes.Equal(cached.sdkSource, sdkSource) && bytes.Equal(cached.peSource, peSource)
	}
	cacheFailure := sampleErr != nil && !errors.Is(sampleErr, context.Canceled) && !errors.Is(sampleErr, context.DeadlineExceeded)
	if stillCurrent && (sampleErr == nil || cacheFailure) {
		cached.profileSampled = true
		cached.profile = resolved
		cached.profileErr = sampleErr
		resolver.keys[path] = cached
	} else if sampleErr == nil {
		sampleErr = fmt.Errorf("%w: runtime scripts expired", pe.ErrKeyRuntime)
	}
	pending.err = sampleErr
	delete(resolver.profileLoads, path)
	close(pending.done)
	resolver.mu.Unlock()
	return resolved, sampleErr
}

func (resolver *KeyResolver) prepareRuntimeSources(ctx context.Context, transport http.RoundTripper, profile device.Profile, path string) error {
	if _, _, ok := resolver.cachedRuntimeSources(path, resolver.now()); ok {
		return nil
	}
	sdkSource, err := resolver.sdkSource(ctx, transport, profile, resolver.now())
	if err != nil {
		return err
	}
	if _, _, ok := resolver.cachedRuntimeSources(path, resolver.now()); ok {
		return nil
	}

	resolver.mu.Lock()
	if _, _, ok := resolver.cachedRuntimeSourcesLocked(path, resolver.now()); ok {
		resolver.mu.Unlock()
		return nil
	}
	if pending := resolver.peLoads[path]; pending != nil {
		done := pending.done
		resolver.mu.Unlock()
		select {
		case <-done:
			return pending.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	pending := &pendingScriptLoad{done: make(chan struct{})}
	resolver.peLoads[path] = pending
	resolver.mu.Unlock()

	peURL := (&url.URL{
		Scheme: "https",
		Host:   keyPEHost,
		Path:   "/captcha-frontend/dynamicJS/" + path + ".js",
	}).String()
	peSource, loadErr := downloadPublicScript(ctx, transport, profile, peURL, map[string]bool{keyPEHost: true}, keyScriptMaxBytes)
	if loadErr == nil && (len(peSource) < 1_000 || !bytes.Contains(peSource, []byte("CaptchaConstructor"))) {
		loadErr = fmt.Errorf("%w: PE script structure mismatch", pe.ErrUnsupportedPE)
	}

	resolver.mu.Lock()
	if loadErr == nil {
		// SDK 可在 PE 下载期间到期刷新；只有当前 SDK
		// 仍是本轮快照时才将二者绑在同一缓存项。
		if !bytes.Equal(resolver.sdk.source, sdkSource) {
			loadErr = fmt.Errorf("%w: SDK changed while loading PE", pe.ErrKeyRuntime)
		} else {
			storedAt := resolver.now()
			resolver.keys[path] = cachedRuntimeProfile{
				sdkSource: bytes.Clone(sdkSource), peSource: bytes.Clone(peSource),
				sampledAt: storedAt, sourceFetchedAt: storedAt,
			}
		}
	}
	pending.err = loadErr
	delete(resolver.peLoads, path)
	close(pending.done)
	resolver.mu.Unlock()
	return loadErr
}

func (resolver *KeyResolver) cachedKey(path string, now time.Time) (pe.RuntimeProfile, bool) {
	cached, err, sampled := resolver.cachedProfileState(path, now)
	return cached, sampled && err == nil && argumentKeyPattern.MatchString(cached.ArgumentKey)
}

// ProfileCacheStats 返回当前 TTL 内的脱敏分片统计。
func (resolver *KeyResolver) ProfileCacheStats() ProfileCacheStats {
	if resolver == nil {
		return ProfileCacheStats{}
	}
	now := resolver.now()
	resolver.mu.RLock()
	defer resolver.mu.RUnlock()
	var stats ProfileCacheStats
	for _, cached := range resolver.keys {
		if !runtimeProfileCacheFresh(cached, now) {
			continue
		}
		stats.SourcePaths++
		if !cached.profileSampled {
			continue
		}
		stats.Sampled++
		if cached.profileErr != nil {
			stats.Failed++
		} else if cached.profile.PureGoCompatible {
			stats.Compatible++
		}
	}
	return stats
}

func (resolver *KeyResolver) cachedProfileState(path string, now time.Time) (pe.RuntimeProfile, error, bool) {
	resolver.mu.RLock()
	defer resolver.mu.RUnlock()
	return resolver.cachedProfileStateLocked(path, now)
}

// cachedProfileStateLocked 要求调用方已持有 resolver.mu 的读锁或写锁。
func (resolver *KeyResolver) cachedProfileStateLocked(path string, now time.Time) (pe.RuntimeProfile, error, bool) {
	cached, ok := resolver.keys[path]
	if !ok {
		return pe.RuntimeProfile{}, nil, false
	}
	if !runtimeProfileCacheFresh(cached, now) {
		return pe.RuntimeProfile{}, nil, false
	}
	if !cached.profileSampled {
		return pe.RuntimeProfile{}, nil, false
	}
	return cached.profile, cached.profileErr, true
}

func runtimeProfileCacheFresh(cached cachedRuntimeProfile, now time.Time) bool {
	age := now.Sub(cached.sampledAt)
	if age < 0 || age >= keyProfileCacheTTL {
		return false
	}
	sourceAt := cached.sourceFetchedAt
	if sourceAt.IsZero() {
		// 兼容旧单测 fixture 和进程内已有项。
		sourceAt = cached.sampledAt
	}
	sourceAge := now.Sub(sourceAt)
	return sourceAge >= 0 && sourceAge < keyProfileHardTTL
}

func (resolver *KeyResolver) sdkSource(ctx context.Context, transport http.RoundTripper, profile device.Profile, _ time.Time) ([]byte, error) {
	return resolver.sdkSourceWithReferer(ctx, transport, profile, keyBridgeReferer)
}

func (resolver *KeyResolver) sdkSourceWithReferer(ctx context.Context, transport http.RoundTripper, profile device.Profile, referer string) ([]byte, error) {
	resolver.mu.Lock()
	// 必须在获锁后取时间：否则晚获锁的 goroutine 可能拿着
	// 比新 `fetchedAt` 更旧的 now，把刚写入的 SDK 缓存误判为未命中。
	now := resolver.now()
	if len(resolver.sdk.source) > 0 && now.Sub(resolver.sdk.fetchedAt) >= 0 && now.Sub(resolver.sdk.fetchedAt) < keySDKCacheTTL {
		cached := bytes.Clone(resolver.sdk.source)
		resolver.mu.Unlock()
		return cached, nil
	}
	if pending := resolver.sdkLoad; pending != nil {
		done := pending.done
		resolver.mu.Unlock()
		select {
		case <-done:
			if pending.err != nil {
				return nil, pending.err
			}
			resolver.mu.RLock()
			cached := bytes.Clone(resolver.sdk.source)
			resolver.mu.RUnlock()
			if len(cached) == 0 {
				return nil, fmt.Errorf("%w: SDK cache missing after load", pe.ErrKeyRuntime)
			}
			return cached, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	pending := &pendingScriptLoad{done: make(chan struct{})}
	resolver.sdkLoad = pending
	resolver.mu.Unlock()

	source, loadErr := downloadPublicScriptWithReferer(ctx, transport, profile, keySDKURL, map[string]bool{
		"o.alicdn.com": true,
		"g.alicdn.com": true,
	}, keyScriptMaxBytes, referer)
	if loadErr == nil && (len(source) < 1_000 || !bytes.Contains(source, []byte("AliyunCaptcha"))) {
		loadErr = fmt.Errorf("%w: SDK script structure mismatch", pe.ErrUnsupportedPE)
	}

	resolver.mu.Lock()
	refreshedAt := resolver.now()
	sameSDK := loadErr == nil && len(resolver.sdk.source) > 0 && bytes.Equal(resolver.sdk.source, source)
	if loadErr == nil && len(resolver.sdk.source) > 0 && !sameSDK {
		// 每份结构画像都依赖采样时的非版本化 SDK；SDK 内容改变时，旧路径
		// 即使自身 TTL 尚未到期也不能继续和新 Device VM 混用。
		resolver.keys = make(map[string]cachedRuntimeProfile)
	}
	if sameSDK {
		// 精确 PE 路径含内容版本。SDK 字节未变时，延长已验证
		// 画像的软 TTL，避免每五分钟对几十个分片重跑 V8。
		// 硬 TTL 到期后仍必须重下 PE 并完整采样。
		for path, cached := range resolver.keys {
			sourceAt := cached.sourceFetchedAt
			if sourceAt.IsZero() {
				sourceAt = cached.sampledAt
			}
			sourceAge := refreshedAt.Sub(sourceAt)
			if sourceAge >= 0 && sourceAge < keyProfileHardTTL {
				cached.sampledAt = refreshedAt
				resolver.keys[path] = cached
			}
		}
	}
	if loadErr == nil {
		resolver.sdk = cachedSDK{source: bytes.Clone(source), fetchedAt: refreshedAt}
	}
	pending.err = loadErr
	resolver.sdkLoad = nil
	close(pending.done)
	resolver.mu.Unlock()
	if loadErr != nil {
		return nil, loadErr
	}
	return source, nil
}

func normalizeStaticPath(value string) (string, error) {
	path := strings.TrimSuffix(strings.TrimSpace(value), ".js")
	if !staticPathPattern.MatchString(path) {
		return "", fmt.Errorf("%w: invalid StaticPath", pe.ErrUnsupportedPE)
	}
	return path, nil
}

func downloadPublicScript(ctx context.Context, transport http.RoundTripper, profile device.Profile, rawURL string, allowedHosts map[string]bool, limit int64) ([]byte, error) {
	return downloadPublicScriptWithReferer(ctx, transport, profile, rawURL, allowedHosts, limit, keyBridgeReferer)
}

func downloadPublicScriptWithReferer(ctx context.Context, transport http.RoundTripper, profile device.Profile, rawURL string, allowedHosts map[string]bool, limit int64, referer string) ([]byte, error) {
	if limit < 1 {
		return nil, fmt.Errorf("%w: invalid byte limit", pe.ErrKeyRuntime)
	}
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 4 {
				return errors.New("redirect limit exceeded")
			}
			return validatePublicScriptURL(request.URL, allowedHosts)
		},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build request", pe.ErrKeyRuntime)
	}
	if err := validatePublicScriptURL(request.URL, allowedHosts); err != nil {
		return nil, fmt.Errorf("%w: invalid URL", pe.ErrKeyRuntime)
	}
	for name, value := range profile.BrowserHeaders(referer, "", "script", "no-cors", false) {
		request.Header.Set(name, value)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: request", pe.ErrKeyNetwork)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("%w: HTTP %d", pe.ErrKeyNetwork, response.StatusCode)
	}
	if response.ContentLength > limit {
		return nil, fmt.Errorf("%w: response too large", pe.ErrUnsupportedPE)
	}
	content, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read response", pe.ErrKeyNetwork)
	}
	if int64(len(content)) > limit {
		return nil, fmt.Errorf("%w: response too large", pe.ErrUnsupportedPE)
	}
	return content, nil
}

func validatePublicScriptURL(value *url.URL, allowedHosts map[string]bool) error {
	if value == nil || value.Scheme != "https" || value.User != nil || value.Port() != "" || value.RawQuery != "" || value.Fragment != "" || !allowedHosts[strings.ToLower(value.Hostname())] {
		return errors.New("script URL left the HTTPS allowlist")
	}
	return nil
}

type bridgeProfile struct {
	ProfileID           string               `json:"profileId"`
	Family              string               `json:"family"`
	UserAgent           string               `json:"userAgent"`
	AppVersion          string               `json:"appVersion"`
	Platform            string               `json:"platform"`
	Vendor              string               `json:"vendor"`
	Mobile              bool                 `json:"mobile"`
	PDFViewer           bool                 `json:"pdfViewer"`
	SecCHUA             string               `json:"secChUa"`
	SecCHUAMobile       string               `json:"secChUaMobile"`
	SecCHUAPlatform     string               `json:"secChUaPlatform"`
	AcceptLanguage      string               `json:"acceptLanguage"`
	Brands              []device.UABrand     `json:"brands"`
	FullVersionList     []device.UABrand     `json:"fullVersionList"`
	UAPlatform          string               `json:"uaPlatform"`
	UAPlatformVersion   string               `json:"uaPlatformVersion"`
	UAModel             string               `json:"uaModel"`
	UAFullVersion       string               `json:"uaFullVersion"`
	UAArchitecture      string               `json:"uaArchitecture"`
	UABitness           string               `json:"uaBitness"`
	Language            string               `json:"language"`
	Languages           []string             `json:"languages"`
	Screen              device.ScreenProfile `json:"screen"`
	GPU                 device.GPUProfile    `json:"gpu"`
	HardwareConcurrency int                  `json:"hardwareConcurrency"`
	DeviceMemory        int                  `json:"deviceMemory"`
	MaxTouchPoints      int                  `json:"maxTouchPoints"`
	CanvasSeed          string               `json:"canvasSeed"`
	TextMetricScale     float64              `json:"textMetricScale"`
}

type bridgeTrackEvent struct {
	Type    string  `json:"type"`
	X       int     `json:"x"`
	Y       int     `json:"y"`
	DT      int     `json:"dt"`
	Force   float64 `json:"force"`
	RadiusX float64 `json:"radiusX"`
	RadiusY float64 `json:"radiusY"`
}

type bridgeInput struct {
	SceneID          string             `json:"sceneId"`
	CertifyID        string             `json:"certifyId"`
	DeviceToken      string             `json:"deviceToken"`
	CaptchaType      string             `json:"captchaType"`
	Image            string             `json:"image"`
	PuzzleImage      string             `json:"puzzleImage"`
	VerifyArgProfile map[string]string  `json:"verifyArgProfile"`
	DeviceConfig     map[string]any     `json:"deviceConfig"`
	Dimensions       map[string]int     `json:"dimensions"`
	Track            []bridgeTrackEvent `json:"track"`
	ExpectedXPos     int                `json:"expectedXPos"`
	InitBeginTime    int64              `json:"initBeginTime"`
	FirstTouchAgeMS  int                `json:"firstTouchAgeMs"`
}

type cappedWriter struct {
	buffer bytes.Buffer
	limit  int
}

func (writer *cappedWriter) Write(value []byte) (int, error) {
	remaining := writer.limit - writer.buffer.Len()
	if remaining <= 0 {
		return 0, errors.New("bridge output limit exceeded")
	}
	if len(value) > remaining {
		written, _ := writer.buffer.Write(value[:remaining])
		return written, errors.New("bridge output limit exceeded")
	}
	return writer.buffer.Write(value)
}

func collectRuntimeProfile(ctx context.Context, nodeBinary string, profile device.Profile, sdkSource, peSource []byte, now time.Time) (pe.RuntimeProfile, error) {
	if ctx == nil {
		return pe.RuntimeProfile{}, fmt.Errorf("%w: context is nil", pe.ErrKeyRuntime)
	}
	nodePath, err := exec.LookPath(nodeBinary)
	if err != nil {
		return pe.RuntimeProfile{}, fmt.Errorf("%w: Node executable not found", pe.ErrKeyRuntime)
	}
	temporaryDirectory, err := os.MkdirTemp("", "ali-slider-pe-key-")
	if err != nil {
		return pe.RuntimeProfile{}, fmt.Errorf("%w: create temporary directory", pe.ErrKeyRuntime)
	}
	defer os.RemoveAll(temporaryDirectory)
	// macOS 的临时目录常经 /var → /private/var 符号链接；Node ESM 会把
	// import.meta.url 规范化到真实路径。统一路径后，bridge 才能可靠识别自身
	// 是主模块，而不是误判为被 import 后静默退出。
	runtimeDirectory, err := filepath.EvalSymlinks(temporaryDirectory)
	if err != nil {
		return pe.RuntimeProfile{}, fmt.Errorf("%w: resolve temporary directory", pe.ErrKeyRuntime)
	}

	paths := map[string]struct {
		name    string
		content []byte
	}{
		"sdkBridge": {name: "sdk_device_bridge.mjs", content: sdkDeviceBridgeSource},
		"keyBridge": {name: "pe_key_bridge.mjs", content: peKeyBridgeSource},
		"sdk":       {name: "AliyunCaptcha.js", content: sdkSource},
		"pe":        {name: "pe.js", content: peSource},
	}
	resolvedPaths := make(map[string]string, len(paths))
	for label, item := range paths {
		path := filepath.Join(runtimeDirectory, item.name)
		if err := os.WriteFile(path, item.content, 0o600); err != nil {
			return pe.RuntimeProfile{}, fmt.Errorf("%w: write %s", pe.ErrKeyRuntime, label)
		}
		resolvedPaths[label] = path
	}

	profileJSON, err := json.Marshal(bridgeProfile{
		ProfileID: profile.ProfileID, Family: profile.Family, UserAgent: profile.UserAgent,
		AppVersion: profile.AppVersion, Platform: profile.Platform, Vendor: profile.Vendor,
		Mobile: profile.Mobile, PDFViewer: profile.PDFViewer, SecCHUA: profile.SecCHUA(),
		SecCHUAMobile: profile.SecCHUAMobile(), SecCHUAPlatform: profile.SecCHUAPlatform(),
		AcceptLanguage: profile.AcceptLanguage(), Brands: profile.UABrands,
		FullVersionList: profile.UAFullVersions, UAPlatform: profile.UAPlatform,
		UAPlatformVersion: profile.UAPlatformVersion, UAModel: profile.UAModel,
		UAFullVersion: profile.UAFullVersion, UAArchitecture: profile.UAArchitecture,
		UABitness: profile.UABitness, Language: profile.Language,
		Languages: profile.Languages, Screen: profile.Screen, GPU: profile.GPU,
		HardwareConcurrency: profile.HardwareConcurrency, DeviceMemory: profile.DeviceMemory,
		MaxTouchPoints: profile.MaxTouchPoints, CanvasSeed: profile.CanvasSeed,
		TextMetricScale: profile.TextMetricScale,
	})
	if err != nil {
		return pe.RuntimeProfile{}, fmt.Errorf("%w: encode device profile", pe.ErrKeyRuntime)
	}
	payload, err := json.Marshal(bridgeInput{
		SceneID: "1ug4aptr", CertifyID: dummyCertifyID, DeviceToken: "dummy-device-token",
		CaptchaType: "PUZZLE", Image: "background.png", PuzzleImage: "puzzle.png",
		VerifyArgProfile: map[string]string{"accessSec": "dummy-access", "sessionIdSalt": "dummy-salt"},
		DeviceConfig:     map[string]any{},
		Dimensions:       map[string]int{"imageWidth": 300, "imageHeight": 200, "puzzleWidth": 60, "puzzleHeight": 60, "renderedWidth": 300, "handleWidth": 40},
		Track: []bridgeTrackEvent{
			{Type: "touchstart", X: 0, Y: 0, DT: 0, Force: 0.5, RadiusX: 5, RadiusY: 5},
			{Type: "touchmove", X: 30, Y: 0, DT: 20, Force: 0.5, RadiusX: 5, RadiusY: 5},
			{Type: "touchmove", X: 80, Y: 0, DT: 20, Force: 0.5, RadiusX: 5, RadiusY: 5},
			{Type: "touchend", X: 80, Y: 0, DT: 10, Force: 0.5, RadiusX: 5, RadiusY: 5},
		},
		ExpectedXPos: 29, InitBeginTime: now.Add(-2 * time.Second).UnixMilli(), FirstTouchAgeMS: 700,
	})
	if err != nil {
		return pe.RuntimeProfile{}, fmt.Errorf("%w: encode bridge input", pe.ErrKeyRuntime)
	}

	operationContext, cancel := context.WithTimeout(ctx, keyBridgeTimeout)
	defer cancel()
	command := exec.CommandContext(operationContext, nodePath,
		resolvedPaths["keyBridge"],
		"--sdk", resolvedPaths["sdk"],
		"--pe", resolvedPaths["pe"],
		"--output-mode", "profile",
		"--prefix", "fsgtmi",
		"--region", "cn",
		"--timeout-ms", fmt.Sprint(keyBridgeTimeout.Milliseconds()),
		"--device-profile", base64.StdEncoding.EncodeToString(profileJSON),
	)
	command.Dir = runtimeDirectory
	// 不把服务进程的代理、云凭据或其他环境变量暴露给公开脚本运行时。
	// Bridge 所需输入全部经显式参数/stdin 提供，Node 内建模块不依赖 PATH。
	command.Env = []string{}
	command.Stdin = bytes.NewReader(payload)
	stdout := &cappedWriter{limit: keyBridgeMaxBytes}
	stderr := &cappedWriter{limit: keyBridgeMaxBytes}
	command.Stdout, command.Stderr = stdout, stderr
	if err := command.Run(); err != nil {
		if operationContext.Err() != nil {
			return pe.RuntimeProfile{}, fmt.Errorf("%w: bridge timeout", pe.ErrKeyRuntime)
		}
		return pe.RuntimeProfile{}, fmt.Errorf("%w: bridge process", pe.ErrKeyRuntime)
	}

	var output runtimeProfileBridgeOutput
	decoder := json.NewDecoder(bytes.NewReader(stdout.buffer.Bytes()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&output); err != nil {
		return pe.RuntimeProfile{}, fmt.Errorf("%w: bridge output", pe.ErrKeyRuntime)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return pe.RuntimeProfile{}, fmt.Errorf("%w: bridge output", pe.ErrKeyRuntime)
	}
	return validateRuntimeProfileBridgeOutput(output)
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("unexpected trailing JSON")
	}
	return nil
}
