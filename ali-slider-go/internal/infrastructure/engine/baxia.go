package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/d2120848471/v3/ali-slider-go/internal/domain/baxia"
	"github.com/d2120848471/v3/ali-slider-go/internal/foundation/runtimekit"
	"github.com/d2120848471/v3/ali-slider-go/internal/platform/v8runtime"
)

//go:embed runtime/baxia_bridge.js
var baxiaBridgeSource string

const (
	baxiaDefaultTimeout   = 5 * time.Second
	baxiaMaximumTimeout   = 30 * time.Second
	baxiaMaximumSDKBytes  = 2 << 20
	baxiaMaximumTokenSize = 64 << 10
)

type baxiaRuntime interface {
	Call(context.Context, string, json.RawMessage) (json.RawMessage, error)
	Close() error
}

// BaxiaSession 独占 V8 Isolate 和动态库，Token 串行复用同一 Fireye 实例。
// Close 会取消正在运行的调用，等待其退出后释放资源。
type BaxiaSession struct {
	engine    baxiaRuntime
	library   io.Closer
	lifetime  context.Context
	cancel    context.CancelFunc
	gate      chan struct{}
	closeOnce sync.Once
	closeErr  error
	profile   baxia.RuntimeProfile
}

var _ baxia.Session = (*BaxiaSession)(nil)

// Profile 返回当前 SDK 实例恢复出的外层编码信息；不包含内部加密密钥。
func (session *BaxiaSession) Profile() baxia.RuntimeProfile {
	if session == nil {
		return baxia.RuntimeProfile{}
	}
	return session.profile
}

// NewBaxiaSession 仅加载调用方提供的 SDK 字节；没有网络 transport 或 Node 进程。
func NewBaxiaSession(ctx context.Context, config baxia.Config) (*BaxiaSession, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is nil", baxia.ErrConfig)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	config, profileJSON, err := normalizeBaxiaConfig(config)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, config.Timeout)
	defer cancel()

	library, err := v8runtime.Open(config.V8LibraryPath)
	if err != nil {
		return nil, fmt.Errorf("%w: load V8 library: %v", baxia.ErrRuntime, err)
	}
	engine, err := library.NewWithHost(newV8HostHandler(
		nil, runtimekit.NewSystemSources().Entropy, false, nil,
	))
	if err != nil {
		return nil, errors.Join(fmt.Errorf("%w: create V8 runtime: %v", baxia.ErrRuntime, err), library.Close())
	}
	lifetime, stop := context.WithCancel(context.Background())
	session := &BaxiaSession{
		engine: engine, library: library, lifetime: lifetime, cancel: stop,
		gate: make(chan struct{}, 1),
	}
	opened := false
	defer func() {
		if !opened {
			_ = session.Close()
		}
	}()

	if err := loadBaxiaRuntimeScripts(ctx, engine, false); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(struct {
		SDKSource     string           `json:"sdkSource"`
		DeviceProfile json.RawMessage  `json:"deviceProfile"`
		PageURL       string           `json:"pageURL"`
		UAOptions     *baxia.UAOptions `json:"uaOptions"`
		TimeoutMS     int64            `json:"timeoutMs"`
	}{string(config.SDKSource), profileJSON, config.PageURL, config.UAOptions, config.Timeout.Milliseconds()})
	if err != nil {
		return nil, fmt.Errorf("%w: encode initialization", baxia.ErrConfig)
	}
	result, err := engine.Call(ctx, "__aliV8BaxiaOpen", payload)
	if err != nil {
		return nil, baxiaRuntimeError(ctx, "initialize Fireye", err)
	}
	var stage baxiaProfileStage
	if err := decodeV8Result(result, &stage); err != nil {
		return nil, fmt.Errorf("%w: invalid getter sampling result: %v", baxia.ErrUnsupportedSDK, err)
	}
	session.profile, err = recoverBaxiaProfile(stage, fmt.Sprintf("%x", sha256.Sum256(config.SDKSource)))
	if err != nil {
		return nil, err
	}
	opened = true
	return session, nil
}

// Token 把 requestURL 写入初始化时的 options 并调用 Fireye getter。
// 执行期间取消或 SDK 异常会关闭该会话，避免继续使用部分更新的 SDK 状态。
func (session *BaxiaSession) Token(ctx context.Context, requestURL string) (baxia.Result, error) {
	if session == nil || session.lifetime == nil {
		return baxia.Result{}, baxia.ErrClosed
	}
	if ctx == nil {
		return baxia.Result{}, fmt.Errorf("%w: context is nil", baxia.ErrConfig)
	}
	if err := ctx.Err(); err != nil {
		return baxia.Result{}, err
	}
	if err := validateBaxiaURL(requestURL); err != nil {
		return baxia.Result{}, fmt.Errorf("%w: request URL %v", baxia.ErrConfig, err)
	}
	select {
	case <-ctx.Done():
		return baxia.Result{}, ctx.Err()
	case <-session.lifetime.Done():
		return baxia.Result{}, baxia.ErrClosed
	case session.gate <- struct{}{}:
	}
	poisoned := false
	defer func() {
		<-session.gate
		if poisoned {
			_ = session.Close()
		}
	}()
	if session.lifetime.Err() != nil {
		return baxia.Result{}, baxia.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return baxia.Result{}, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(session.lifetime, cancel)
	defer stop()
	payload, _ := json.Marshal(struct {
		RequestURL string `json:"requestURL"`
	}{requestURL})
	raw, err := session.engine.Call(runCtx, "__aliV8BaxiaToken", payload)
	if err != nil {
		failure := baxiaRuntimeError(ctx, "generate token", err)
		poisoned = true
		session.cancel()
		if ctx.Err() != nil {
			return baxia.Result{}, ctx.Err()
		}
		if errors.Is(err, context.Canceled) {
			return baxia.Result{}, baxia.ErrClosed
		}
		return baxia.Result{}, failure
	}
	var result baxia.Result
	if err := decodeV8Result(raw, &result); err != nil {
		poisoned = true
		session.cancel()
		return baxia.Result{}, fmt.Errorf("%w: getter output is invalid", baxia.ErrToken)
	}
	if err := validateBaxiaToken(result, session.profile); err != nil {
		poisoned = true
		session.cancel()
		return baxia.Result{}, err
	}
	result.SDKHash = session.profile.SDKHash
	result.Profile = session.profile
	return result, nil
}

func (session *BaxiaSession) Close() error {
	if session == nil || session.lifetime == nil {
		return nil
	}
	session.cancel()
	session.closeOnce.Do(func() {
		session.gate <- struct{}{}
		defer func() { <-session.gate }()
		session.closeErr = errors.Join(session.engine.Close(), session.library.Close())
	})
	return session.closeErr
}

func baxiaRuntimeError(ctx context.Context, operation string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if strings.Contains(err.Error(), "BAXIA_SDK_CONTRACT") {
		return fmt.Errorf("%w: %s: %v", baxia.ErrUnsupportedSDK, operation, err)
	}
	return fmt.Errorf("%w: %s: %v", baxia.ErrRuntime, operation, err)
}

func normalizeBaxiaConfig(config baxia.Config) (baxia.Config, json.RawMessage, error) {
	return normalizeBaxiaEnvironment(config, true)
}

func normalizeBaxiaEnvironment(config baxia.Config, requireSDK bool) (baxia.Config, json.RawMessage, error) {
	fail := func(reason string) (baxia.Config, json.RawMessage, error) {
		return baxia.Config{}, nil, fmt.Errorf("%w: %s", baxia.ErrConfig, reason)
	}
	if strings.TrimSpace(config.V8LibraryPath) == "" {
		return fail("V8 library path is required")
	}
	if requireSDK && (len(config.SDKSource) == 0 || len(config.SDKSource) > baxiaMaximumSDKBytes || !utf8.Valid(config.SDKSource)) {
		return fail("SDK source must be nonempty UTF-8 within 2 MiB")
	}
	config.SDKSource = bytes.Clone(config.SDKSource)
	if err := validateBaxiaURL(config.PageURL); err != nil {
		return fail("page URL " + err.Error())
	}
	if config.Timeout == 0 {
		config.Timeout = baxiaDefaultTimeout
	}
	if config.Timeout < time.Millisecond || config.Timeout > baxiaMaximumTimeout {
		return fail("initialization timeout must be between 1 ms and 30 s")
	}
	options := baxia.DefaultUAOptions()
	if config.UAOptions != nil {
		options = *config.UAOptions
	}
	for _, value := range []int{options.MaxMTLog, options.MaxKSLog, options.MaxFocusLog, options.MaxNGPLog} {
		if value < 0 || value > 1000 {
			return fail("UA log limits must be between 0 and 1000")
		}
	}
	if options.OnlyHost != 0 && options.OnlyHost != 1 {
		return fail("OnlyHost must be 0 or 1")
	}
	if len(options.Location) == 0 || len(options.Location) > 32 || strings.TrimSpace(options.Location) != options.Location || strings.ContainsAny(options.Location, "\r\n\x00") {
		return fail("UA location must be nonempty and within 32 bytes")
	}
	if options.LoadTime < 0 || options.LoadTime > 30_000 || options.Timeout < 1 || options.Timeout > 30_000 {
		return fail("UA loadTime or timeout is outside its supported range")
	}
	config.UAOptions = &options
	profile := config.Profile
	if profile.UserAgent == "" || profile.Platform == "" || profile.Language == "" ||
		len(profile.Languages) == 0 || profile.Languages[0] != profile.Language ||
		len(profile.UABrands) == 0 || len(profile.UAFullVersions) == 0 ||
		profile.Screen.Width < 1 || profile.Screen.Height < 1 ||
		profile.Screen.InnerWidth < 1 || profile.Screen.InnerHeight < 1 ||
		profile.Screen.OuterWidth < 1 || profile.Screen.OuterHeight < 1 ||
		!positiveBaxiaNumber(profile.Screen.DevicePixelRatio) ||
		profile.GPU.MaxTextureSize < 1 || profile.GPU.UnmaskedVendor == "" || profile.GPU.UnmaskedRenderer == "" ||
		profile.HardwareConcurrency < 1 || profile.DeviceMemory < 1 || profile.MaxTouchPoints < 0 ||
		profile.CanvasSeed == "" || !positiveBaxiaNumber(profile.TextMetricScale) {
		return fail("device profile is incomplete or invalid")
	}
	profileJSON, err := marshalBridgeProfile(profile)
	if err != nil || len(profileJSON) > 8<<10 {
		return fail("device profile cannot be encoded within 8 KiB")
	}
	return config, profileJSON, nil
}

func positiveBaxiaNumber(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func validateBaxiaURL(value string) error {
	if len(value) == 0 || len(value) > 8192 || strings.TrimSpace(value) != value {
		return errors.New("must be nonempty and within 8192 bytes")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Opaque != "" || parsed.Hostname() == "" ||
		parsed.User != nil || parsed.Scheme != "https" && parsed.Scheme != "http" {
		return errors.New("must be an absolute HTTP(S) URL without credentials")
	}
	return nil
}
