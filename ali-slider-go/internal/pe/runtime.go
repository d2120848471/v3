package pe

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/protocol"
	"github.com/d2120848471/v3/ali-slider-go/internal/runtimekit"
	"github.com/d2120848471/v3/ali-slider-go/internal/track"
)

const (
	nativeEventTimeLimitMS      = 180_000
	nativeFirstTouchToleranceMS = 250
	nativeTouchToleranceMS      = 300
	nativePostTouchLimitMS      = 500
)

// RuntimeDimensions 是动态 PE 同轮看到的原图与渲染尺寸。
type RuntimeDimensions struct {
	ImageWidth    int
	ImageHeight   int
	PuzzleWidth   int
	PuzzleHeight  int
	RenderedWidth int
	HandleWidth   int
}

// RuntimeInput 只承载一轮挑战的数据。它不得进入分钟级脚本缓存。
type RuntimeInput struct {
	SceneID         string
	CertifyID       string
	DeviceToken     string
	CaptchaType     string
	Image           string
	PuzzleImage     string
	DeviceConfig    protocol.DeviceConfig
	VerifyAccessSec string
	VerifySalt      string
	Dimensions      RuntimeDimensions
	Track           []track.Event
	ExpectedXPos    *int
	InitBeginTimeMS int64
	FirstTouchAgeMS int
	// SDKSource 是本轮 Device VM 实际使用的公开 SDK 快照；不进入 JSON，
	// 只用于避免预热会话跨缓存刷新点时混用不同 SDK。
	SDKSource []byte
}

type nativeDeviceConfig struct {
	Key            string   `json:"key"`
	Switch         int      `json:"switch"`
	SessionID      string   `json:"sessionId"`
	Version        string   `json:"version"`
	PluginElements string   `json:"pluginElements"`
	PluginResource string   `json:"pluginResource"`
	GlobalVariable string   `json:"globalVariable"`
	Timestamp      string   `json:"timestamp"`
	IP             string   `json:"ip"`
	ExtraSegments  []string `json:"extraSegments"`
}

type nativeBridgeInput struct {
	SceneID          string             `json:"sceneId"`
	CertifyID        string             `json:"certifyId"`
	DeviceToken      string             `json:"deviceToken"`
	CaptchaType      string             `json:"captchaType"`
	Image            string             `json:"image"`
	PuzzleImage      string             `json:"puzzleImage"`
	VerifyArgProfile map[string]string  `json:"verifyArgProfile"`
	DeviceConfig     nativeDeviceConfig `json:"deviceConfig"`
	Dimensions       map[string]int     `json:"dimensions"`
	Track            []bridgeTrackEvent `json:"track"`
	ExpectedXPos     *int               `json:"expectedXPos"`
	InitBeginTime    int64              `json:"initBeginTime"`
	FirstTouchAgeMS  int                `json:"firstTouchAgeMs"`
}

type nativeVerifyParam struct {
	SceneID     string `json:"sceneId"`
	CertifyID   string `json:"certifyId"`
	DeviceToken string `json:"deviceToken"`
	Data        string `json:"data"`
}

type nativeGetterArgument struct {
	ValueType       string  `json:"type"`
	Length          *int    `json:"length"`
	Value           *string `json:"value"`
	EqualsCertifyID bool    `json:"equalsCertifyId"`
	EqualsSceneID   bool    `json:"equalsSceneId"`
}

type nativeGetterCall struct {
	Owner         string                 `json:"owner"`
	ObservedAtMS  json.Number            `json:"observedAtMs"`
	ArgumentCount int                    `json:"argumentCount"`
	Arguments     []nativeGetterArgument `json:"arguments"`
}

type nativePayloadMeta struct {
	Keys                  []string          `json:"keys"`
	ValueTypes            map[string]string `json:"valueTypes"`
	XPos                  any               `json:"xPos"`
	SlidePos              any               `json:"slidePos"`
	TrackStartTime        any               `json:"trackStartTime"`
	VerifyTime            any               `json:"verifyTime"`
	Argument              string            `json:"arg"`
	TrackKeys             []string          `json:"trackKeys"`
	TrackValueTypes       map[string]string `json:"trackValueTypes"`
	TargetFirstTouchAgeMS any               `json:"targetFirstTouchAgeMs"`
	ActualFirstTouchAgeMS any               `json:"actualFirstTouchAgeMs"`
	DispatchLatenessMS    any               `json:"dispatchLatenessMs"`
}

type nativeBridgeOutput struct {
	VerifyParam              nativeVerifyParam  `json:"verifyParam"`
	TrackEventCount          int                `json:"trackEventCount"`
	NativeDeviceTokenMatched bool               `json:"nativeDeviceTokenMatched"`
	DeviceGetterCalls        []nativeGetterCall `json:"deviceGetterCalls"`
	PayloadMeta              nativePayloadMeta  `json:"payloadMeta"`
}

// Build 复用五分钟内缓存的公开 SDK/PE 源码，但让当前分片为每个挑战原生生成
// data。CertifyId、轨迹、时钟和 DeviceConfig 都不会跨挑战复用。
func (resolver *KeyResolver) Build(ctx context.Context, transport http.RoundTripper, profile device.Profile, staticPath string, input RuntimeInput) (Result, error) {
	if resolver == nil {
		return Result{}, fmt.Errorf("%w: resolver is nil", ErrKeyRuntime)
	}
	path, err := normalizeStaticPath(staticPath)
	if err != nil {
		return Result{}, err
	}
	if err := resolver.prepareRuntimeSources(ctx, transport, profile, path); err != nil {
		return Result{}, err
	}
	runtimeProfile, profileErr := resolver.ensureRuntimeProfile(ctx, transport, profile, path)
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	sdkSource, peSource, ok := resolver.cachedRuntimeSources(path, resolver.now())
	if !ok {
		if err := resolver.prepareRuntimeSources(ctx, transport, profile, path); err != nil {
			return Result{}, err
		}
		sdkSource, peSource, ok = resolver.cachedRuntimeSources(path, resolver.now())
		if !ok {
			return Result{}, fmt.Errorf("%w: runtime scripts missing", ErrKeyRuntime)
		}
	}
	if len(input.SDKSource) > 0 {
		if int64(len(input.SDKSource)) > keyScriptMaxBytes || len(input.SDKSource) < 1_000 || !bytes.Contains(input.SDKSource, []byte("AliyunCaptcha")) {
			return Result{}, fmt.Errorf("%w: session SDK structure mismatch", ErrUnsupportedPE)
		}
		sdkSource = bytes.Clone(input.SDKSource)
	}
	if profileErr == nil && runtimeProfile.PureGoCompatible {
		fastResult, fastErr := (Builder{
			Profile: profile,
			Sources: runtimekit.NewSystemSources(),
		}).Build(ctx, Input{
			SceneID: input.SceneID, CertifyID: input.CertifyID,
			Dimensions: Dimensions{
				RenderedWidth: input.Dimensions.RenderedWidth,
				HandleWidth:   input.Dimensions.HandleWidth,
			},
			Track: input.Track, StaticPath: path,
			ArgumentKey: runtimeProfile.ArgumentKey, IncludeScreenInfo: runtimeProfile.IncludeScreenInfo,
			ExpectedXPos: input.ExpectedXPos, InitBeginTimeMS: input.InitBeginTimeMS,
			FirstTouchAgeMS: input.FirstTouchAgeMS,
		})
		if fastErr == nil {
			return fastResult, nil
		}
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		// 当轮输入超出已自校验快路时，保留原生 V8 结果。
	}
	if resolver.v8LibraryPath != "" {
		return resolver.runV8PE(ctx, profile, sdkSource, peSource, input)
	}
	return runNativePE(ctx, resolver.nodeBinary, profile, sdkSource, peSource, input)
}

func (resolver *KeyResolver) cachedRuntimeSources(path string, now time.Time) ([]byte, []byte, bool) {
	resolver.mu.RLock()
	defer resolver.mu.RUnlock()
	return resolver.cachedRuntimeSourcesLocked(path, now)
}

// cachedRuntimeSourcesLocked 要求调用方已持有 resolver.mu 的读锁或写锁。
func (resolver *KeyResolver) cachedRuntimeSourcesLocked(path string, now time.Time) ([]byte, []byte, bool) {
	cached, ok := resolver.keys[path]
	if !ok || len(cached.sdkSource) == 0 || len(cached.peSource) == 0 {
		return nil, nil, false
	}
	if !runtimeProfileCacheFresh(cached, now) {
		return nil, nil, false
	}
	return bytes.Clone(cached.sdkSource), bytes.Clone(cached.peSource), true
}

func runNativePE(ctx context.Context, nodeBinary string, profile device.Profile, sdkSource, peSource []byte, input RuntimeInput) (Result, error) {
	if ctx == nil {
		return Result{}, fmt.Errorf("%w: context is nil", ErrKeyRuntime)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	nodePath, err := exec.LookPath(nodeBinary)
	if err != nil {
		return Result{}, fmt.Errorf("%w: Node executable not found", ErrKeyRuntime)
	}
	payload, err := marshalNativeBridgeInput(input)
	if err != nil {
		return Result{}, fmt.Errorf("%w: invalid build input", ErrUnsupportedPE)
	}
	profileJSON, err := marshalBridgeProfile(profile)
	if err != nil {
		return Result{}, fmt.Errorf("%w: encode device profile", ErrKeyRuntime)
	}

	temporaryDirectory, err := os.MkdirTemp("", "ali-slider-pe-data-")
	if err != nil {
		return Result{}, fmt.Errorf("%w: create temporary directory", ErrKeyRuntime)
	}
	defer os.RemoveAll(temporaryDirectory)
	runtimeDirectory, err := filepath.EvalSymlinks(temporaryDirectory)
	if err != nil {
		return Result{}, fmt.Errorf("%w: resolve temporary directory", ErrKeyRuntime)
	}
	paths := map[string]struct {
		name    string
		content []byte
	}{
		"sdkBridge": {name: "sdk_device_bridge.mjs", content: sdkDeviceBridgeSource},
		"peBridge":  {name: "pe_key_bridge.mjs", content: peKeyBridgeSource},
		"sdk":       {name: "AliyunCaptcha.js", content: sdkSource},
		"pe":        {name: "pe.js", content: peSource},
	}
	resolvedPaths := make(map[string]string, len(paths))
	for label, item := range paths {
		path := filepath.Join(runtimeDirectory, item.name)
		if err := os.WriteFile(path, item.content, 0o600); err != nil {
			return Result{}, fmt.Errorf("%w: write %s", ErrKeyRuntime, label)
		}
		resolvedPaths[label] = path
	}

	operationContext, cancel := context.WithTimeout(ctx, keyBridgeTimeout)
	defer cancel()
	command := exec.CommandContext(operationContext, nodePath,
		resolvedPaths["peBridge"],
		"--sdk", resolvedPaths["sdk"],
		"--pe", resolvedPaths["pe"],
		"--output-mode", "data",
		"--prefix", "fsgtmi",
		"--region", "cn",
		"--timeout-ms", fmt.Sprint(keyBridgeTimeout.Milliseconds()),
		"--device-profile", base64.StdEncoding.EncodeToString(profileJSON),
	)
	command.Dir = runtimeDirectory
	command.Env = []string{}
	command.Stdin = bytes.NewReader(payload)
	stdout := &cappedWriter{limit: keyBridgeMaxBytes}
	stderr := &cappedWriter{limit: keyBridgeMaxBytes}
	command.Stdout, command.Stderr = stdout, stderr
	if err := command.Run(); err != nil {
		if operationContext.Err() != nil {
			return Result{}, fmt.Errorf("%w: bridge timeout", ErrKeyRuntime)
		}
		return Result{}, fmt.Errorf("%w: bridge process", ErrKeyRuntime)
	}

	var output nativeBridgeOutput
	decoder := json.NewDecoder(bytes.NewReader(stdout.buffer.Bytes()))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&output); err != nil {
		return Result{}, fmt.Errorf("%w: bridge output", ErrKeyRuntime)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return Result{}, fmt.Errorf("%w: bridge output", ErrKeyRuntime)
	}
	return validateNativePEOutput(output, input)
}

func marshalBridgeProfile(profile device.Profile) ([]byte, error) {
	return json.Marshal(bridgeProfile{
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
}

func marshalNativeBridgeInput(input RuntimeInput) ([]byte, error) {
	if input.SceneID == "" || input.CertifyID == "" || input.DeviceToken == "" || input.Image == "" || input.PuzzleImage == "" {
		return nil, errors.New("runtime session fields are required")
	}
	if input.CaptchaType == "" {
		input.CaptchaType = "PUZZLE"
	}
	if input.InitBeginTimeMS < 1 || input.FirstTouchAgeMS < 1 {
		return nil, errors.New("runtime clock fields are invalid")
	}
	dimensions := input.Dimensions
	if dimensions.ImageWidth < 1 || dimensions.ImageHeight < 1 || dimensions.PuzzleWidth < 1 || dimensions.PuzzleHeight < 1 || dimensions.RenderedWidth < 1 || dimensions.HandleWidth < 1 || dimensions.HandleWidth > dimensions.RenderedWidth {
		return nil, errors.New("runtime dimensions are invalid")
	}
	if len(input.DeviceConfig.Key) != 16 || input.DeviceConfig.SessionID == "" {
		return nil, errors.New("runtime device config is invalid")
	}
	if len(input.Track) < 3 || len(input.Track) > maximumTrackEvents {
		return nil, errors.New("runtime track count is invalid")
	}
	events := make([]bridgeTrackEvent, len(input.Track))
	var duration int64
	for index, event := range input.Track {
		if event.DT < 0 || index == 0 && event.DT != 0 || math.IsNaN(event.Force) || math.IsInf(event.Force, 0) || event.Force < 0 || event.Force > 1 || math.IsNaN(event.RadiusX) || math.IsInf(event.RadiusX, 0) || event.RadiusX <= 0 || event.RadiusX > 100 || math.IsNaN(event.RadiusY) || math.IsInf(event.RadiusY, 0) || event.RadiusY <= 0 || event.RadiusY > 100 {
			return nil, fmt.Errorf("runtime track event %d is invalid", index)
		}
		duration += int64(event.DT)
		if duration > maximumTrackDurationMS {
			return nil, errors.New("runtime track duration is invalid")
		}
		events[index] = bridgeTrackEvent{Type: event.Type, X: event.X, Y: event.Y, DT: event.DT, Force: event.Force, RadiusX: event.RadiusX, RadiusY: event.RadiusY}
	}
	config := input.DeviceConfig
	return json.Marshal(nativeBridgeInput{
		SceneID: input.SceneID, CertifyID: input.CertifyID, DeviceToken: input.DeviceToken,
		CaptchaType: input.CaptchaType, Image: input.Image, PuzzleImage: input.PuzzleImage,
		VerifyArgProfile: map[string]string{
			"accessSec":     firstNonEmpty(input.VerifyAccessSec, "runtime"),
			"sessionIdSalt": firstNonEmpty(input.VerifySalt, "runtime"),
		},
		DeviceConfig: nativeDeviceConfig{
			Key: config.Key, Switch: config.Switch, SessionID: config.SessionID, Version: config.Version,
			PluginElements: config.PluginElements, PluginResource: config.PluginResource,
			GlobalVariable: config.GlobalVariable, Timestamp: config.Timestamp, IP: config.IP,
			ExtraSegments: append([]string(nil), config.ExtraSegments...),
		},
		Dimensions: map[string]int{
			"imageWidth": dimensions.ImageWidth, "imageHeight": dimensions.ImageHeight,
			"puzzleWidth": dimensions.PuzzleWidth, "puzzleHeight": dimensions.PuzzleHeight,
			"renderedWidth": dimensions.RenderedWidth, "handleWidth": dimensions.HandleWidth,
		},
		Track: events, ExpectedXPos: input.ExpectedXPos, InitBeginTime: input.InitBeginTimeMS,
		FirstTouchAgeMS: input.FirstTouchAgeMS,
	})
}

func firstNonEmpty(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func validateNativePEOutput(output nativeBridgeOutput, input RuntimeInput) (Result, error) {
	verify := output.VerifyParam
	if verify.SceneID != input.SceneID || verify.CertifyID != input.CertifyID || verify.DeviceToken != input.DeviceToken || verify.Data == "" || output.TrackEventCount != len(input.Track) {
		return Result{}, fmt.Errorf("%w: session output mismatch", ErrUnsupportedPE)
	}
	meta := output.PayloadMeta
	payloadKeys := []string{"TrackList", "TrackStartTime", "VerifyTime", "xPos", "slidePos", "arg"}
	legacyTrackKeys := []string{"mc", "tc", "mu", "te", "mp", "tmv", "mm", "ks", "fi", "startTime", "si"}
	currentTrackKeys := legacyTrackKeys[:len(legacyTrackKeys)-1]
	if !slices.Equal(meta.Keys, payloadKeys) || (!slices.Equal(meta.TrackKeys, currentTrackKeys) && !slices.Equal(meta.TrackKeys, legacyTrackKeys)) {
		return Result{}, fmt.Errorf("%w: runtime schema changed", ErrUnsupportedPE)
	}

	decoded, err := protocol.UnpackData(verify.Data)
	if err != nil {
		return Result{}, fmt.Errorf("%w: unpack native data", ErrUnsupportedPE)
	}
	if decoded.Payload.Arg == "" || decoded.Payload.Arg != meta.Argument {
		return Result{}, fmt.Errorf("%w: native argument mismatch", ErrUnsupportedPE)
	}
	if slices.Equal(meta.TrackKeys, currentTrackKeys) && decoded.Payload.TrackList.SI != "" || slices.Equal(meta.TrackKeys, legacyTrackKeys) && decoded.Payload.TrackList.SI == "" {
		return Result{}, fmt.Errorf("%w: native TrackList mismatch", ErrUnsupportedPE)
	}

	xPos64, err := decimalInteger(meta.XPos)
	if err != nil {
		return Result{}, fmt.Errorf("%w: native xPos", ErrUnsupportedPE)
	}
	slidePos64, err := decimalInteger(meta.SlidePos)
	if err != nil {
		return Result{}, fmt.Errorf("%w: native slidePos", ErrUnsupportedPE)
	}
	trackStart, err := decimalInteger(meta.TrackStartTime)
	if err != nil || trackStart < 1 {
		return Result{}, fmt.Errorf("%w: native TrackStartTime", ErrUnsupportedPE)
	}
	verifyTime, err := decimalInteger(meta.VerifyTime)
	if err != nil || verifyTime < trackStart {
		return Result{}, fmt.Errorf("%w: native VerifyTime", ErrUnsupportedPE)
	}
	payloadTrackStart, err := decimalInteger(decoded.Payload.TrackStartTime)
	if err != nil || payloadTrackStart != trackStart {
		return Result{}, fmt.Errorf("%w: encoded TrackStartTime mismatch", ErrUnsupportedPE)
	}
	payloadVerifyTime, err := decimalInteger(decoded.Payload.VerifyTime)
	if err != nil || payloadVerifyTime != verifyTime {
		return Result{}, fmt.Errorf("%w: encoded VerifyTime mismatch", ErrUnsupportedPE)
	}
	trackListStart, err := decimalInteger(decoded.Payload.TrackList.StartTime)
	if err != nil || trackListStart != trackStart {
		return Result{}, fmt.Errorf("%w: encoded TrackList start mismatch", ErrUnsupportedPE)
	}
	xPos, slidePos := int(xPos64), int(slidePos64)
	if strconv.Itoa(xPos) != decoded.Payload.XPos || strconv.Itoa(slidePos) != decoded.Payload.SlidePos || xPos < 0 || slidePos < 0 {
		return Result{}, fmt.Errorf("%w: encoded position mismatch", ErrUnsupportedPE)
	}
	if input.ExpectedXPos != nil && abs(xPos-*input.ExpectedXPos) > 1 {
		return Result{}, fmt.Errorf("%w: native xPos outside tolerance", ErrUnsupportedPE)
	}

	if len(output.DeviceGetterCalls) != 1 {
		return Result{}, fmt.Errorf("%w: native getter count", ErrUnsupportedPE)
	}
	call := output.DeviceGetterCalls[0]
	observedAt, err := finiteNumber(call.ObservedAtMS)
	if err != nil || observedAt < 0 || observedAt > nativeEventTimeLimitMS || (call.Owner != "z_um" && call.Owner != "um") || call.ArgumentCount != 1 || len(call.Arguments) != 1 {
		return Result{}, fmt.Errorf("%w: native getter shape", ErrUnsupportedPE)
	}
	argument := call.Arguments[0]
	if argument.ValueType != "string" || argument.Length == nil || argument.Value == nil || *argument.Value != input.CertifyID || *argument.Length != len(*argument.Value) || !argument.EqualsCertifyID || argument.EqualsSceneID != (input.SceneID == input.CertifyID) {
		return Result{}, fmt.Errorf("%w: native getter argument", ErrUnsupportedPE)
	}

	nativeEvents, err := parseNativeEvents(decoded.Payload.TrackList.MM)
	if err != nil {
		return Result{}, fmt.Errorf("%w: native mousemove stream", ErrUnsupportedPE)
	}
	preGetter := make([]InteractionEvent, 0, len(input.Track))
	for _, event := range nativeEvents {
		if event.TimeStamp <= observedAt {
			preGetter = append(preGetter, event)
		}
	}
	if len(preGetter) != len(input.Track) {
		return Result{}, fmt.Errorf("%w: native pre-getter event count", ErrUnsupportedPE)
	}
	touchStarts, err := parseNativeEventTimes(decoded.Payload.TrackList.TC)
	if err != nil || len(touchStarts) != 1 {
		return Result{}, fmt.Errorf("%w: native touchstart stream", ErrUnsupportedPE)
	}
	touchEnds, err := parseNativeEventTimes(decoded.Payload.TrackList.TE)
	if err != nil || len(touchEnds) != 1 {
		return Result{}, fmt.Errorf("%w: native touchend stream", ErrUnsupportedPE)
	}
	targetFirstAge, err := decimalInteger(meta.TargetFirstTouchAgeMS)
	if err != nil || targetFirstAge != int64(input.FirstTouchAgeMS) {
		return Result{}, fmt.Errorf("%w: native first-touch target", ErrUnsupportedPE)
	}
	actualFirstAge, err := finiteNumber(meta.ActualFirstTouchAgeMS)
	if err != nil {
		return Result{}, fmt.Errorf("%w: native first-touch clock", ErrUnsupportedPE)
	}
	dispatchLateness, err := finiteNumber(meta.DispatchLatenessMS)
	if err != nil || math.Abs(dispatchLateness) > nativeFirstTouchToleranceMS {
		return Result{}, fmt.Errorf("%w: native dispatch lateness", ErrUnsupportedPE)
	}
	var inputDuration float64
	for _, event := range input.Track {
		inputDuration += float64(event.DT)
	}
	firstTouchAge := touchStarts[0]
	touchDuration := touchEnds[0] - firstTouchAge
	lastTouchToVerify := float64(verifyTime-trackStart) - touchEnds[0]
	postInteractionDelay := observedAt - preGetter[len(preGetter)-1].TimeStamp
	if math.Abs(firstTouchAge-float64(input.FirstTouchAgeMS)) > nativeFirstTouchToleranceMS || math.Abs(firstTouchAge-actualFirstAge) > 25 || math.Abs(touchDuration-inputDuration) > nativeTouchToleranceMS || lastTouchToVerify < 0 || lastTouchToVerify > nativePostTouchLimitMS || postInteractionDelay < 0 || postInteractionDelay > nativePostTouchLimitMS {
		return Result{}, fmt.Errorf("%w: native timing contract", ErrUnsupportedPE)
	}

	getterArgument := GetterArgument{
		ValueType: "string", Length: *argument.Length, Value: *argument.Value,
		EqualsCertifyID: true, EqualsSceneID: argument.EqualsSceneID,
	}
	return Result{
		Data: verify.Data, TrackEventCount: output.TrackEventCount,
		PayloadKeys: append([]string(nil), meta.Keys...), TrackKeys: append([]string(nil), meta.TrackKeys...),
		XPos: xPos, SlidePos: slidePos, TrackStartTimeMS: trackStart, VerifyTimeMS: verifyTime,
		TargetFirstTouchAgeMS: input.FirstTouchAgeMS, FirstTouchAgeMS: firstTouchAge,
		TouchDurationMS: touchDuration, LastTouchToVerifyMS: lastTouchToVerify,
		PostInteractionDelayMS: postInteractionDelay, DispatchLatenessMS: dispatchLateness,
		DeviceGetterPlans: []GetterPlan{{
			Owner: call.Owner, DerivedAtMS: observedAt, ArgumentCount: 1,
			Arguments: []GetterArgument{getterArgument},
		}},
		DataMousemoveEventCount:  len(nativeEvents),
		PostGetterDataEventCount: len(nativeEvents) - len(preGetter),
		InteractionEvents:        preGetter,
	}, nil
}

func decimalInteger(value any) (int64, error) {
	var text string
	switch current := value.(type) {
	case json.Number:
		text = current.String()
	case string:
		text = current
	case int:
		return int64(current), nil
	case int64:
		return current, nil
	default:
		return 0, errors.New("value is not an integer")
	}
	if text == "" || strings.ContainsAny(text, ".eE+") {
		return 0, errors.New("value is not a decimal integer")
	}
	return strconv.ParseInt(text, 10, 64)
}

func finiteNumber(value any) (float64, error) {
	var number float64
	var err error
	switch current := value.(type) {
	case json.Number:
		number, err = current.Float64()
	case float64:
		number = current
	case int:
		number = float64(current)
	case int64:
		number = float64(current)
	default:
		err = errors.New("value is not a number")
	}
	if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
		return 0, errors.New("value is not finite")
	}
	return number, nil
}

func parseNativeEventTimes(value string) ([]float64, error) {
	events, err := parseNativeEvents(value)
	if err != nil {
		return nil, err
	}
	times := make([]float64, len(events))
	for index, event := range events {
		times[index] = event.TimeStamp
	}
	return times, nil
}

func parseNativeEvents(value string) ([]InteractionEvent, error) {
	if value == "" {
		return nil, errors.New("event stream is empty")
	}
	records := strings.Split(value, "|")
	if len(records) < 1 || len(records) > maximumTrackEvents+16 {
		return nil, errors.New("event count is outside bounds")
	}
	events := make([]InteractionEvent, len(records))
	previous := -1.0
	for index, record := range records {
		fields := strings.Split(record, ",")
		if len(fields) != 4 {
			return nil, errors.New("event record shape is invalid")
		}
		values := make([]float64, 4)
		for fieldIndex, field := range fields {
			value, err := strconv.ParseFloat(field, 64)
			if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
				return nil, errors.New("event record contains a non-finite number")
			}
			values[fieldIndex] = value
		}
		if math.Abs(values[0]) > 10_000 || math.Abs(values[1]) > 10_000 || values[2] < 0 || values[2] > nativeEventTimeLimitMS || values[3] != 1 || values[2] < previous {
			return nil, errors.New("event record is outside bounds")
		}
		previous = values[2]
		events[index] = InteractionEvent{Type: "mousemove", X: values[0], Y: values[1], TimeStamp: values[2], IsTrusted: true}
	}
	if events[len(events)-1].TimeStamp-events[0].TimeStamp > float64(maximumTrackDurationMS+nativePostTouchLimitMS) {
		return nil, errors.New("event span is outside bounds")
	}
	return events, nil
}
