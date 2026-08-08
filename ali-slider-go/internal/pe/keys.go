package pe

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
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/protocol"
)

const (
	defaultNodeBinary  = "node"
	keySDKURL          = "https://o.alicdn.com/captcha-frontend/aliyunCaptcha/AliyunCaptcha.js"
	keyPEHost          = "g.alicdn.com"
	keyProfileCacheTTL = 5 * time.Minute
	keySDKCacheTTL     = keyProfileCacheTTL
	keyScriptMaxBytes  = int64(2 << 20)
	keyBridgeMaxBytes  = 64 << 10
	keyBridgeTimeout   = 10 * time.Second
	keyBridgeOrigin    = "http://localhost:38185"
	keyBridgeReferer   = keyBridgeOrigin + "/"
	dummyCertifyID     = "0123456789abcdef"
)

var (
	// ErrUnsupportedPE 表示动态脚本的命名、内容或 arg 算法已超出当前安全边界。
	ErrUnsupportedPE = errors.New("unsupported dynamic PE script")
	// ErrKeyNetwork 表示公开 SDK/PE 脚本无法通过本轮路由取得。
	ErrKeyNetwork = errors.New("dynamic PE script download failed")
	// ErrKeyRuntime 表示本机 Node 动态 PE/设备运行时不存在或执行失败。
	ErrKeyRuntime = errors.New("dynamic PE runtime failed")

	staticPathPattern  = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+/pe\.[0-9]{3}\.[a-f0-9]{16}$`)
	argumentKeyPattern = regexp.MustCompile(`^[a-z0-9]{16}$`)
)

//go:embed runtime/sdk_device_bridge.mjs
var sdkDeviceBridgeSource []byte

//go:embed runtime/pe_key_bridge.mjs
var peKeyBridgeSource []byte

// RuntimeProfile 是当前公开 SDK 与精确 PE 分片共同决定的、可安全缓存的
// 运行时结构画像。挑战级 CertifyId、轨迹和 data 不得进入该缓存。
type RuntimeProfile struct {
	ArgumentKey       string
	IncludeScreenInfo bool
}

type keyCollector func(context.Context, string, device.Profile, []byte, []byte, time.Time) (RuntimeProfile, error)

type cachedRuntimeProfile struct {
	profile   RuntimeProfile
	sdkSource []byte
	peSource  []byte
	sampledAt time.Time
}

type cachedSDK struct {
	source    []byte
	fetchedAt time.Time
}

// KeyResolver 按精确 StaticPath 缓存公开 SDK/PE 源码和结构画像五分钟。
// Resolve 的缓存命中不启动 Node；Build 仍在每轮挑战的隔离 VM 内运行缓存脚本。
type KeyResolver struct {
	nodeBinary string
	now        func() time.Time
	collect    keyCollector

	mu   sync.RWMutex
	keys map[string]cachedRuntimeProfile
	sdk  cachedSDK
	miss sync.Mutex
}

// NewKeyResolver 创建进程级 resolver。nodeBinary 为空时使用 PATH 中的 node。
// Node 是生产设备会话和动态 PE data 逐挑战生成所需的运行时。
func NewKeyResolver(nodeBinary string) *KeyResolver {
	if strings.TrimSpace(nodeBinary) == "" {
		nodeBinary = defaultNodeBinary
	}
	return &KeyResolver{
		nodeBinary: nodeBinary,
		now:        time.Now,
		collect:    collectRuntimeProfile,
		keys:       make(map[string]cachedRuntimeProfile),
	}
}

// Resolve 返回本轮 StaticPath 的结构画像。缓存键是完整版本化路径，绝不把
// 一个“最新画像”覆盖给并存的其他 PE 分片。
func (resolver *KeyResolver) Resolve(ctx context.Context, transport http.RoundTripper, profile device.Profile, staticPath string) (RuntimeProfile, error) {
	if resolver == nil {
		return RuntimeProfile{}, fmt.Errorf("%w: resolver is nil", ErrKeyRuntime)
	}
	if ctx == nil {
		return RuntimeProfile{}, fmt.Errorf("%w: context is nil", ErrKeyRuntime)
	}
	path, err := normalizeStaticPath(staticPath)
	if err != nil {
		return RuntimeProfile{}, err
	}
	now := resolver.now()
	if cached, ok := resolver.cachedKey(path, now); ok {
		return cached, nil
	}
	if transport == nil {
		return RuntimeProfile{}, fmt.Errorf("%w: transport is nil", ErrKeyNetwork)
	}

	// 新分片很少出现；串行化 miss 可以同时避免重复下载 SDK、重复启动 Node，
	// 而所有稳定流量仍只走上面的读锁命中路径。
	resolver.miss.Lock()
	defer resolver.miss.Unlock()
	now = resolver.now()
	if cached, ok := resolver.cachedKey(path, now); ok {
		return cached, nil
	}
	if err := ctx.Err(); err != nil {
		return RuntimeProfile{}, err
	}

	sdkSource, err := resolver.sdkSource(ctx, transport, profile, now)
	if err != nil {
		return RuntimeProfile{}, err
	}
	peURL := (&url.URL{
		Scheme: "https",
		Host:   keyPEHost,
		Path:   "/captcha-frontend/dynamicJS/" + path + ".js",
	}).String()
	peSource, err := downloadPublicScript(ctx, transport, profile, peURL, map[string]bool{keyPEHost: true}, keyScriptMaxBytes)
	if err != nil {
		return RuntimeProfile{}, err
	}
	if len(peSource) < 1_000 || !bytes.Contains(peSource, []byte("CaptchaConstructor")) {
		return RuntimeProfile{}, fmt.Errorf("%w: PE script structure mismatch", ErrUnsupportedPE)
	}
	resolved, err := resolver.collect(ctx, resolver.nodeBinary, profile, sdkSource, peSource, now)
	if err != nil {
		return RuntimeProfile{}, err
	}
	if !argumentKeyPattern.MatchString(resolved.ArgumentKey) {
		return RuntimeProfile{}, fmt.Errorf("%w: invalid argument key", ErrUnsupportedPE)
	}
	resolver.mu.Lock()
	resolver.keys[path] = cachedRuntimeProfile{
		profile: resolved, sdkSource: bytes.Clone(sdkSource), peSource: bytes.Clone(peSource), sampledAt: now,
	}
	resolver.mu.Unlock()
	return resolved, nil
}

func (resolver *KeyResolver) cachedKey(path string, now time.Time) (RuntimeProfile, bool) {
	resolver.mu.RLock()
	defer resolver.mu.RUnlock()
	cached, ok := resolver.keys[path]
	if !ok {
		return RuntimeProfile{}, false
	}
	age := now.Sub(cached.sampledAt)
	if age < 0 || age >= keyProfileCacheTTL {
		return RuntimeProfile{}, false
	}
	return cached.profile, true
}

func (resolver *KeyResolver) sdkSource(ctx context.Context, transport http.RoundTripper, profile device.Profile, now time.Time) ([]byte, error) {
	resolver.mu.RLock()
	if len(resolver.sdk.source) > 0 && now.Sub(resolver.sdk.fetchedAt) >= 0 && now.Sub(resolver.sdk.fetchedAt) < keySDKCacheTTL {
		cached := bytes.Clone(resolver.sdk.source)
		resolver.mu.RUnlock()
		return cached, nil
	}
	resolver.mu.RUnlock()

	source, err := downloadPublicScript(ctx, transport, profile, keySDKURL, map[string]bool{
		"o.alicdn.com": true,
		"g.alicdn.com": true,
	}, keyScriptMaxBytes)
	if err != nil {
		return nil, err
	}
	if len(source) < 1_000 || !bytes.Contains(source, []byte("AliyunCaptcha")) {
		return nil, fmt.Errorf("%w: SDK script structure mismatch", ErrUnsupportedPE)
	}
	resolver.mu.Lock()
	if len(resolver.sdk.source) > 0 && !bytes.Equal(resolver.sdk.source, source) {
		// 每份结构画像都依赖采样时的非版本化 SDK；SDK 内容改变时，旧路径
		// 即使自身 TTL 尚未到期也不能继续和新 Device VM 混用。
		resolver.keys = make(map[string]cachedRuntimeProfile)
	}
	resolver.sdk = cachedSDK{source: bytes.Clone(source), fetchedAt: now}
	resolver.mu.Unlock()
	return source, nil
}

func normalizeStaticPath(value string) (string, error) {
	path := strings.TrimSuffix(strings.TrimSpace(value), ".js")
	if !staticPathPattern.MatchString(path) {
		return "", fmt.Errorf("%w: invalid StaticPath", ErrUnsupportedPE)
	}
	return path, nil
}

func downloadPublicScript(ctx context.Context, transport http.RoundTripper, profile device.Profile, rawURL string, allowedHosts map[string]bool, limit int64) ([]byte, error) {
	if limit < 1 {
		return nil, fmt.Errorf("%w: invalid byte limit", ErrKeyRuntime)
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
		return nil, fmt.Errorf("%w: build request", ErrKeyRuntime)
	}
	if err := validatePublicScriptURL(request.URL, allowedHosts); err != nil {
		return nil, fmt.Errorf("%w: invalid URL", ErrKeyRuntime)
	}
	for name, value := range profile.BrowserHeaders(keyBridgeReferer, "", "script", "no-cors", false) {
		request.Header.Set(name, value)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: request", ErrKeyNetwork)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("%w: HTTP %d", ErrKeyNetwork, response.StatusCode)
	}
	if response.ContentLength > limit {
		return nil, fmt.Errorf("%w: response too large", ErrUnsupportedPE)
	}
	content, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read response", ErrKeyNetwork)
	}
	if int64(len(content)) > limit {
		return nil, fmt.Errorf("%w: response too large", ErrUnsupportedPE)
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

func collectRuntimeProfile(ctx context.Context, nodeBinary string, profile device.Profile, sdkSource, peSource []byte, now time.Time) (RuntimeProfile, error) {
	if ctx == nil {
		return RuntimeProfile{}, fmt.Errorf("%w: context is nil", ErrKeyRuntime)
	}
	nodePath, err := exec.LookPath(nodeBinary)
	if err != nil {
		return RuntimeProfile{}, fmt.Errorf("%w: Node executable not found", ErrKeyRuntime)
	}
	temporaryDirectory, err := os.MkdirTemp("", "ali-slider-pe-key-")
	if err != nil {
		return RuntimeProfile{}, fmt.Errorf("%w: create temporary directory", ErrKeyRuntime)
	}
	defer os.RemoveAll(temporaryDirectory)
	// macOS 的临时目录常经 /var → /private/var 符号链接；Node ESM 会把
	// import.meta.url 规范化到真实路径。统一路径后，bridge 才能可靠识别自身
	// 是主模块，而不是误判为被 import 后静默退出。
	runtimeDirectory, err := filepath.EvalSymlinks(temporaryDirectory)
	if err != nil {
		return RuntimeProfile{}, fmt.Errorf("%w: resolve temporary directory", ErrKeyRuntime)
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
			return RuntimeProfile{}, fmt.Errorf("%w: write %s", ErrKeyRuntime, label)
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
		return RuntimeProfile{}, fmt.Errorf("%w: encode device profile", ErrKeyRuntime)
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
		return RuntimeProfile{}, fmt.Errorf("%w: encode bridge input", ErrKeyRuntime)
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
			return RuntimeProfile{}, fmt.Errorf("%w: bridge timeout", ErrKeyRuntime)
		}
		return RuntimeProfile{}, fmt.Errorf("%w: bridge process", ErrKeyRuntime)
	}

	var output struct {
		Key         string   `json:"key"`
		Argument    string   `json:"argument"`
		Data        string   `json:"data"`
		PayloadKeys []string `json:"payloadKeys"`
		TrackKeys   []string `json:"trackKeys"`
	}
	decoder := json.NewDecoder(bytes.NewReader(stdout.buffer.Bytes()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&output); err != nil {
		return RuntimeProfile{}, fmt.Errorf("%w: bridge output", ErrKeyRuntime)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return RuntimeProfile{}, fmt.Errorf("%w: bridge output", ErrKeyRuntime)
	}
	if !argumentKeyPattern.MatchString(output.Key) || output.Argument == "" || output.Data == "" {
		return RuntimeProfile{}, fmt.Errorf("%w: bridge result", ErrUnsupportedPE)
	}
	transformed, err := protocol.Transform64([]byte(dummyCertifyID), output.Key, false)
	if err != nil || base64.StdEncoding.EncodeToString(transformed) != output.Argument {
		return RuntimeProfile{}, fmt.Errorf("%w: independent key check", ErrUnsupportedPE)
	}
	decoded, err := protocol.UnpackData(output.Data)
	if err != nil || decoded.Payload.Arg != output.Argument {
		return RuntimeProfile{}, fmt.Errorf("%w: independent data check", ErrUnsupportedPE)
	}
	if !slices.Equal(output.PayloadKeys, []string{"TrackList", "TrackStartTime", "VerifyTime", "xPos", "slidePos", "arg"}) {
		return RuntimeProfile{}, fmt.Errorf("%w: payload schema changed", ErrUnsupportedPE)
	}
	legacyTrackKeys := []string{"mc", "tc", "mu", "te", "mp", "tmv", "mm", "ks", "fi", "startTime", "si"}
	currentTrackKeys := legacyTrackKeys[:len(legacyTrackKeys)-1]
	includeScreenInfo := false
	switch {
	case slices.Equal(output.TrackKeys, currentTrackKeys):
		if decoded.Payload.TrackList.SI != "" {
			return RuntimeProfile{}, fmt.Errorf("%w: current TrackList mismatch", ErrUnsupportedPE)
		}
	case slices.Equal(output.TrackKeys, legacyTrackKeys):
		if decoded.Payload.TrackList.SI == "" {
			return RuntimeProfile{}, fmt.Errorf("%w: legacy TrackList mismatch", ErrUnsupportedPE)
		}
		includeScreenInfo = true
	default:
		return RuntimeProfile{}, fmt.Errorf("%w: TrackList schema changed", ErrUnsupportedPE)
	}
	return RuntimeProfile{ArgumentKey: output.Key, IncludeScreenInfo: includeScreenInfo}, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("unexpected trailing JSON")
	}
	return nil
}
