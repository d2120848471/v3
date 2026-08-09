package pe

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/protocol"
	"github.com/d2120848471/v3/ali-slider-go/internal/runtimekit"
	"github.com/d2120848471/v3/ali-slider-go/internal/v8runtime"
)

type v8RuntimeBundleSet struct {
	sdk string
	pe  string
	err error
}

var (
	v8RuntimeBundlesOnce sync.Once
	v8RuntimeBundleValue v8RuntimeBundleSet
)

type runtimeProfileBridgeOutput struct {
	Key         string   `json:"key"`
	Argument    string   `json:"argument"`
	Data        string   `json:"data"`
	PayloadKeys []string `json:"payloadKeys"`
	TrackKeys   []string `json:"trackKeys"`
}

func cachedV8RuntimeBundles() v8RuntimeBundleSet {
	v8RuntimeBundlesOnce.Do(func() {
		v8RuntimeBundleValue.sdk, v8RuntimeBundleValue.err = buildV8SDKBundle()
		if v8RuntimeBundleValue.err == nil {
			v8RuntimeBundleValue.pe, v8RuntimeBundleValue.err = buildV8PEBundle()
		}
	})
	return v8RuntimeBundleValue
}

func (resolver *KeyResolver) openV8Library() (*v8runtime.Library, error) {
	if resolver == nil {
		return nil, fmt.Errorf("%w: resolver is nil", ErrKeyRuntime)
	}
	resolver.v8Mu.Lock()
	defer resolver.v8Mu.Unlock()
	if resolver.v8Closed {
		return nil, fmt.Errorf("%w: V8 library is closed", ErrKeyRuntime)
	}
	if resolver.v8Library != nil {
		return resolver.v8Library, nil
	}
	if resolver.v8LibraryPath == "" {
		return nil, fmt.Errorf("%w: V8 library path is empty", ErrKeyRuntime)
	}
	library, err := v8runtime.Open(resolver.v8LibraryPath)
	if err != nil {
		return nil, fmt.Errorf("%w: load V8 library: %v", ErrKeyRuntime, err)
	}
	resolver.v8Library = library
	return library, nil
}

// CheckRuntime 立即加载 wrapper、校验 ABI，并初始化 V8/ICU。服务启动时调用，
// 避免动态库缺失或平台不匹配时仍对外报告 ready。
func (resolver *KeyResolver) CheckRuntime() (string, error) {
	library, err := resolver.openV8Library()
	if err != nil {
		return "", err
	}
	version, err := library.Version()
	if err != nil {
		return "", fmt.Errorf("%w: initialize V8 library: %v", ErrKeyRuntime, err)
	}
	if version == "" {
		return "", fmt.Errorf("%w: V8 version is empty", ErrKeyRuntime)
	}
	return version, nil
}

// Close 释放 resolver 持有的 V8 wrapper。调用方必须先结束所有 Device 会话。
func (resolver *KeyResolver) Close() error {
	if resolver == nil {
		return nil
	}
	resolver.v8Mu.Lock()
	defer resolver.v8Mu.Unlock()
	if resolver.v8Closed {
		return nil
	}
	if resolver.v8Library == nil {
		resolver.v8Closed = true
		return nil
	}
	if err := resolver.v8Library.Close(); err != nil {
		return err
	}
	resolver.v8Library = nil
	resolver.v8Closed = true
	return nil
}

func (resolver *KeyResolver) newV8Engine(
	transport http.RoundTripper,
	entropy runtimekit.Entropy,
	networkEnabled bool,
) (*v8runtime.Runtime, error) {
	library, err := resolver.openV8Library()
	if err != nil {
		return nil, err
	}
	engine, err := library.NewWithHost(newV8HostHandler(transport, entropy, networkEnabled))
	if err != nil {
		return nil, fmt.Errorf("%w: create V8 runtime: %v", ErrKeyRuntime, err)
	}
	return engine, nil
}

func loadV8RuntimeScripts(
	ctx context.Context,
	engine *v8runtime.Runtime,
	includePE bool,
) error {
	bundles := cachedV8RuntimeBundles()
	if bundles.err != nil {
		return fmt.Errorf("%w: build V8 bridge: %v", ErrKeyRuntime, bundles.err)
	}
	scripts := []struct {
		name   string
		source string
	}{
		{"v8-host-bootstrap.js", v8HostBootstrapSource},
		{"sdk-device-v8-bundle.js", bundles.sdk},
	}
	if includePE {
		scripts = append(scripts, struct {
			name   string
			source string
		}{"pe-key-v8-bundle.js", bundles.pe})
	}
	for _, script := range scripts {
		result, err := engine.Eval(ctx, script.source, script.name)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("%w: evaluate %s: %v", ErrKeyRuntime, script.name, err)
		}
		if string(result) != "true" {
			return fmt.Errorf("%w: initialize %s", ErrKeyRuntime, script.name)
		}
	}
	return nil
}

func (resolver *KeyResolver) collectV8RuntimeProfile(
	ctx context.Context,
	profile device.Profile,
	sdkSource []byte,
	peSource []byte,
	now time.Time,
) (RuntimeProfile, error) {
	input := bridgeInput{
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
	}
	bridgeJSON, err := json.Marshal(input)
	if err != nil {
		return RuntimeProfile{}, fmt.Errorf("%w: encode bridge input", ErrKeyRuntime)
	}
	result, err := resolver.runV8PEBridge(
		ctx,
		profile,
		sdkSource,
		peSource,
		json.RawMessage(bridgeJSON),
		"profile",
	)
	if err != nil {
		return RuntimeProfile{}, err
	}
	var output runtimeProfileBridgeOutput
	if err := decodeV8Result(result, &output); err != nil {
		return RuntimeProfile{}, fmt.Errorf("%w: bridge output", ErrKeyRuntime)
	}
	return validateRuntimeProfileBridgeOutput(output)
}

func (resolver *KeyResolver) runV8PE(
	ctx context.Context,
	profile device.Profile,
	sdkSource []byte,
	peSource []byte,
	input RuntimeInput,
) (Result, error) {
	bridgeJSON, err := marshalNativeBridgeInput(input)
	if err != nil {
		return Result{}, fmt.Errorf("%w: invalid build input", ErrUnsupportedPE)
	}
	result, err := resolver.runV8PEBridge(
		ctx,
		profile,
		sdkSource,
		peSource,
		json.RawMessage(bridgeJSON),
		"data",
	)
	if err != nil {
		return Result{}, err
	}
	var output nativeBridgeOutput
	if err := decodeV8ResultUseNumber(result, &output); err != nil {
		return Result{}, fmt.Errorf("%w: bridge output", ErrKeyRuntime)
	}
	return validateNativePEOutput(output, input)
}

func (resolver *KeyResolver) runV8PEBridge(
	ctx context.Context,
	profile device.Profile,
	sdkSource []byte,
	peSource []byte,
	bridgeInput json.RawMessage,
	outputMode string,
) (json.RawMessage, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is nil", ErrKeyRuntime)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	profileJSON, err := marshalBridgeProfile(profile)
	if err != nil {
		return nil, fmt.Errorf("%w: encode device profile", ErrKeyRuntime)
	}
	engine, err := resolver.newV8Engine(nil, runtimekit.NewSystemSources().Entropy, false)
	if err != nil {
		return nil, err
	}
	defer engine.Close()
	if err := loadV8RuntimeScripts(ctx, engine, true); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(struct {
		SDKSource     string          `json:"sdkSource"`
		PESource      string          `json:"peSource"`
		BridgeInput   json.RawMessage `json:"bridgeInput"`
		DeviceProfile json.RawMessage `json:"deviceProfile"`
		OutputMode    string          `json:"outputMode"`
		Prefix        string          `json:"prefix"`
		Region        string          `json:"region"`
		TimeoutMS     int64           `json:"timeoutMs"`
	}{
		SDKSource: string(sdkSource), PESource: string(peSource),
		BridgeInput: bridgeInput, DeviceProfile: profileJSON,
		OutputMode: outputMode, Prefix: "fsgtmi", Region: "cn",
		TimeoutMS: keyBridgeTimeout.Milliseconds(),
	})
	if err != nil {
		return nil, fmt.Errorf("%w: encode V8 PE call", ErrKeyRuntime)
	}
	result, err := engine.Call(ctx, "__aliV8PERun", payload)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: V8 PE call: %v", ErrKeyRuntime, err)
	}
	return result, nil
}

func validateRuntimeProfileBridgeOutput(output runtimeProfileBridgeOutput) (RuntimeProfile, error) {
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

func decodeV8Result(value json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return ensureJSONEOF(decoder)
}

func decodeV8ResultUseNumber(value json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return ensureJSONEOF(decoder)
}

func (resolver *KeyResolver) openV8Device(
	ctx context.Context,
	transport http.RoundTripper,
	profile device.Profile,
	options DeviceRuntimeOptions,
	sdkSource []byte,
) (*DeviceRuntimeSession, error) {
	engine, err := resolver.newV8Engine(transport, options.Sources.Entropy, true)
	if err != nil {
		return nil, err
	}
	closeEngine := true
	defer func() {
		if closeEngine {
			_ = engine.Close()
		}
	}()
	if err := loadV8RuntimeScripts(ctx, engine, false); err != nil {
		return nil, err
	}
	profileJSON, err := marshalBridgeProfile(profile)
	if err != nil {
		return nil, fmt.Errorf("%w: encode device profile", ErrKeyRuntime)
	}
	timeoutMS := options.Timeout.Milliseconds()
	if timeoutMS > keyBridgeTimeout.Milliseconds() {
		timeoutMS = keyBridgeTimeout.Milliseconds()
	}
	payload, err := json.Marshal(struct {
		SDKSource     string          `json:"sdkSource"`
		DeviceProfile json.RawMessage `json:"deviceProfile"`
		Prefix        string          `json:"prefix"`
		Region        string          `json:"region"`
		TimeoutMS     int64           `json:"timeoutMs"`
	}{
		SDKSource: string(sdkSource), DeviceProfile: profileJSON,
		Prefix: options.Prefix, Region: options.Region, TimeoutMS: timeoutMS,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: encode V8 Device call", ErrKeyRuntime)
	}
	result, err := engine.Call(ctx, "__aliV8DeviceOpen", payload)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: V8 Device init: %v", ErrKeyRuntime, err)
	}
	var stage deviceBridgeStage
	if err := decodeV8Result(result, &stage); err != nil {
		return nil, fmt.Errorf("%w: V8 Device init output", ErrKeyRuntime)
	}
	session := &DeviceRuntimeSession{
		v8Engine: engine, options: options, sdkSource: bytes.Clone(sdkSource),
	}
	if err := session.acceptInitialStage(stage); err != nil {
		return nil, err
	}
	closeEngine = false
	return session, nil
}

func (session *DeviceRuntimeSession) completeV8Device(
	ctx context.Context,
	engine *v8runtime.Runtime,
	payload []byte,
	eventCount int,
) (device.Result, error) {
	defer func() {
		_ = engine.Close()
		session.mu.Lock()
		if session.v8Engine == engine {
			session.v8Engine = nil
		}
		session.mu.Unlock()
	}()
	result, err := engine.Call(ctx, "__aliV8DeviceComplete", payload)
	if err != nil {
		if ctx.Err() != nil {
			return device.Result{}, ctx.Err()
		}
		return device.Result{}, fmt.Errorf("%w: V8 Device complete: %v", ErrKeyRuntime, err)
	}
	var stage deviceBridgeStage
	if err := decodeV8Result(result, &stage); err != nil {
		return device.Result{}, fmt.Errorf("%w: V8 Device complete output", ErrKeyRuntime)
	}
	return session.acceptVerifyStage(stage, eventCount)
}
