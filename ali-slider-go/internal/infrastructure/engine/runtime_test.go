package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/domain/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/pe"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/protocol"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/track"
)

func validNativeRuntimeFixture(t *testing.T) (nativeBridgeOutput, pe.RuntimeInput) {
	t.Helper()
	expectedX := 29
	input := pe.RuntimeInput{
		SceneID: "scene-id", CertifyID: "certify-id", DeviceToken: "device-token",
		Image: "background.png", PuzzleImage: "puzzle.png",
		DeviceConfig: protocol.DeviceConfig{
			Key: "0123456789abcdef", Switch: 1, SessionID: "session-id", Version: "1",
			Timestamp: "2000000000000", IP: "127.0.0.1", ExtraSegments: []string{"extra"},
		},
		Dimensions: pe.RuntimeDimensions{
			ImageWidth: 300, ImageHeight: 200, PuzzleWidth: 60, PuzzleHeight: 60,
			RenderedWidth: 300, HandleWidth: 40,
		},
		Track: []track.Event{
			{Type: "touchstart", X: 0, Y: 0, DT: 0, Force: 0.5, RadiusX: 5, RadiusY: 5},
			{Type: "touchmove", X: 30, Y: 0, DT: 20, Force: 0.5, RadiusX: 5, RadiusY: 5},
			{Type: "touchmove", X: 80, Y: 0, DT: 20, Force: 0.5, RadiusX: 5, RadiusY: 5},
			{Type: "touchend", X: 80, Y: 0, DT: 10, Force: 0.5, RadiusX: 5, RadiusY: 5},
		},
		ExpectedXPos: &expectedX, InitBeginTimeMS: 1_999_999_999_000, FirstTouchAgeMS: 700,
	}
	const (
		trackStart = int64(2_000_000_000_000)
		verifyTime = trackStart + 800
		argument   = "native-argument"
	)
	payload := protocol.DataPayload{
		TrackList: protocol.TrackList{
			MC: "94,548,700,1", TC: "94,548,700,1", MU: "174,548,750,1", TE: "174,548,750,1",
			MP: "94,548,700,1", TMV: "104,548,720,1",
			MM: "94,548,700,1|104,548,720,1|124,548,740,1|174,548,750,1|174,548,766,1",
			KS: "0,0,0,1", FI: "0,0,0,1", StartTime: trackStart,
		},
		TrackStartTime: trackStart, VerifyTime: verifyTime,
		XPos: "29", SlidePos: "80", Arg: argument,
	}
	data, err := protocol.PackData(payload, "00112233445566778899aabbccddeeff")
	if err != nil {
		t.Fatal(err)
	}
	argumentLength := len(input.CertifyID)
	argumentValue := input.CertifyID
	output := nativeBridgeOutput{
		VerifyParam: nativeVerifyParam{
			SceneID: input.SceneID, CertifyID: input.CertifyID,
			DeviceToken: input.DeviceToken, Data: data,
		},
		TrackEventCount: len(input.Track), NativeDeviceTokenMatched: true,
		DeviceGetterCalls: []nativeGetterCall{{
			Owner: "z_um", ObservedAtMS: json.Number("750"), ArgumentCount: 1,
			Arguments: []nativeGetterArgument{{
				ValueType: "string", Length: &argumentLength, Value: &argumentValue,
				EqualsCertifyID: true,
			}},
		}},
		PayloadMeta: nativePayloadMeta{
			Keys: []string{"TrackList", "TrackStartTime", "VerifyTime", "xPos", "slidePos", "arg"},
			XPos: json.Number("29"), SlidePos: "80", TrackStartTime: trackStart,
			VerifyTime: json.Number("2000000000800"), Argument: argument,
			TrackKeys:             []string{"mc", "tc", "mu", "te", "mp", "tmv", "mm", "ks", "fi", "startTime"},
			TargetFirstTouchAgeMS: 700, ActualFirstTouchAgeMS: json.Number("700"), DispatchLatenessMS: float64(0),
		},
	}
	return output, input
}

func TestValidateNativePEOutputAcceptsCurrentRuntimeContract(t *testing.T) {
	output, input := validNativeRuntimeFixture(t)
	result, err := validateNativePEOutput(output, input)
	if err != nil {
		t.Fatal(err)
	}
	if result.XPos != 29 || result.SlidePos != 80 || result.TrackEventCount != 4 ||
		result.DataMousemoveEventCount != 5 || result.PostGetterDataEventCount != 1 ||
		len(result.InteractionEvents) != 4 || len(result.DeviceGetterPlans) != 1 ||
		result.FirstTouchAgeMS != 700 || result.TouchDurationMS != 50 || result.LastTouchToVerifyMS != 50 {
		t.Fatalf("unexpected native result: %+v", result)
	}
}

func TestValidateNativePEOutputRejectsContractDrift(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*nativeBridgeOutput, *pe.RuntimeInput)
	}{
		{"session", func(o *nativeBridgeOutput, _ *pe.RuntimeInput) { o.VerifyParam.SceneID = "other" }},
		{"payload schema", func(o *nativeBridgeOutput, _ *pe.RuntimeInput) { o.PayloadMeta.Keys = []string{"arg"} }},
		{"track schema", func(o *nativeBridgeOutput, _ *pe.RuntimeInput) { o.PayloadMeta.TrackKeys = []string{"mm"} }},
		{"data", func(o *nativeBridgeOutput, _ *pe.RuntimeInput) { o.VerifyParam.Data = "invalid" }},
		{"argument", func(o *nativeBridgeOutput, _ *pe.RuntimeInput) { o.PayloadMeta.Argument = "other" }},
		{"xPos", func(o *nativeBridgeOutput, _ *pe.RuntimeInput) { o.PayloadMeta.XPos = "29.5" }},
		{"slidePos", func(o *nativeBridgeOutput, _ *pe.RuntimeInput) { o.PayloadMeta.SlidePos = struct{}{} }},
		{"track start", func(o *nativeBridgeOutput, _ *pe.RuntimeInput) { o.PayloadMeta.TrackStartTime = 0 }},
		{"verify time", func(o *nativeBridgeOutput, _ *pe.RuntimeInput) { o.PayloadMeta.VerifyTime = int64(1) }},
		{"expected position", func(_ *nativeBridgeOutput, in *pe.RuntimeInput) { value := 40; in.ExpectedXPos = &value }},
		{"getter count", func(o *nativeBridgeOutput, _ *pe.RuntimeInput) { o.DeviceGetterCalls = nil }},
		{"getter shape", func(o *nativeBridgeOutput, _ *pe.RuntimeInput) { o.DeviceGetterCalls[0].Owner = "window" }},
		{"getter argument", func(o *nativeBridgeOutput, _ *pe.RuntimeInput) {
			o.DeviceGetterCalls[0].Arguments[0].EqualsCertifyID = false
		}},
		{"mousemove", func(o *nativeBridgeOutput, _ *pe.RuntimeInput) {
			decoded, _ := protocol.UnpackData(o.VerifyParam.Data)
			decoded.Payload.TrackList.MM = "broken"
			o.VerifyParam.Data, _ = protocol.PackData(decoded.Payload, "00112233445566778899aabbccddeeff")
		}},
		{"touchstart", func(o *nativeBridgeOutput, _ *pe.RuntimeInput) {
			decoded, _ := protocol.UnpackData(o.VerifyParam.Data)
			decoded.Payload.TrackList.TC = ""
			o.VerifyParam.Data, _ = protocol.PackData(decoded.Payload, "00112233445566778899aabbccddeeff")
		}},
		{"touchend", func(o *nativeBridgeOutput, _ *pe.RuntimeInput) {
			decoded, _ := protocol.UnpackData(o.VerifyParam.Data)
			decoded.Payload.TrackList.TE = ""
			o.VerifyParam.Data, _ = protocol.PackData(decoded.Payload, "00112233445566778899aabbccddeeff")
		}},
		{"target age", func(o *nativeBridgeOutput, _ *pe.RuntimeInput) { o.PayloadMeta.TargetFirstTouchAgeMS = 701 }},
		{"actual age", func(o *nativeBridgeOutput, _ *pe.RuntimeInput) { o.PayloadMeta.ActualFirstTouchAgeMS = "bad" }},
		{"dispatch", func(o *nativeBridgeOutput, _ *pe.RuntimeInput) { o.PayloadMeta.DispatchLatenessMS = 251 }},
		{"timing", func(o *nativeBridgeOutput, _ *pe.RuntimeInput) {
			o.DeviceGetterCalls[0].ObservedAtMS = json.Number("600")
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			output, input := validNativeRuntimeFixture(t)
			test.mutate(&output, &input)
			if _, err := validateNativePEOutput(output, input); !errors.Is(err, pe.ErrUnsupportedPE) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestMarshalNativeBridgeInputValidationAndDefaults(t *testing.T) {
	_, valid := validNativeRuntimeFixture(t)
	payload, err := marshalNativeBridgeInput(valid)
	if err != nil {
		t.Fatal(err)
	}
	var decoded nativeBridgeInput
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.CaptchaType != "PUZZLE" || decoded.VerifyArgProfile["accessSec"] != "runtime" || decoded.VerifyArgProfile["sessionIdSalt"] != "runtime" {
		t.Fatalf("defaults not applied: %+v", decoded)
	}

	tests := []struct {
		name   string
		mutate func(*pe.RuntimeInput)
	}{
		{"required", func(in *pe.RuntimeInput) { in.SceneID = "" }},
		{"clock", func(in *pe.RuntimeInput) { in.FirstTouchAgeMS = 0 }},
		{"dimensions", func(in *pe.RuntimeInput) { in.Dimensions.HandleWidth = in.Dimensions.RenderedWidth + 1 }},
		{"config", func(in *pe.RuntimeInput) { in.DeviceConfig.Key = "short" }},
		{"track count", func(in *pe.RuntimeInput) { in.Track = in.Track[:2] }},
		{"first delta", func(in *pe.RuntimeInput) { in.Track[0].DT = 1 }},
		{"force", func(in *pe.RuntimeInput) { in.Track[1].Force = math.NaN() }},
		{"radius", func(in *pe.RuntimeInput) { in.Track[1].RadiusX = 0 }},
		{"duration", func(in *pe.RuntimeInput) { in.Track[1].DT = int(pe.MaximumTrackDurationMS + 1) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, input := validNativeRuntimeFixture(t)
			test.mutate(&input)
			if _, err := marshalNativeBridgeInput(input); err == nil {
				t.Fatal("invalid input accepted")
			}
		})
	}
	if firstNonEmpty("value", "fallback") != "value" || firstNonEmpty("", "fallback") != "fallback" {
		t.Fatal("firstNonEmpty contract changed")
	}
}

func TestNativeNumberAndEventParsers(t *testing.T) {
	for _, value := range []any{json.Number("12"), "12", 12, int64(12)} {
		if got, err := decimalInteger(value); err != nil || got != 12 {
			t.Fatalf("decimalInteger(%T)=%d,%v", value, got, err)
		}
	}
	for _, value := range []any{"", "1.2", "1e2", float64(1), struct{}{}} {
		if _, err := decimalInteger(value); err == nil {
			t.Fatalf("decimalInteger accepted %#v", value)
		}
	}
	for _, value := range []any{json.Number("1.5"), float64(2), 3, int64(4)} {
		if _, err := finiteNumber(value); err != nil {
			t.Fatalf("finiteNumber(%T): %v", value, err)
		}
	}
	for _, value := range []any{json.Number("bad"), math.NaN(), math.Inf(1), "1"} {
		if _, err := finiteNumber(value); err == nil {
			t.Fatalf("finiteNumber accepted %#v", value)
		}
	}
	valid, err := parseNativeEvents("1,2,3,1|2,3,4,1")
	if err != nil || len(valid) != 2 {
		t.Fatalf("valid events=%+v err=%v", valid, err)
	}
	for _, value := range []string{"", "1,2,3", "x,2,3,1", "1,2,3,0", "1,2,4,1|1,2,3,1", "10001,2,3,1", "1,2,180001,1", "1,2,0,1|1,2,60501,1"} {
		if _, err := parseNativeEvents(value); err == nil {
			t.Fatalf("events accepted %q", value)
		}
	}
}

func writeExecutableStub(t *testing.T, output []byte, exitCode int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable fixture")
	}
	text := string(output)
	if strings.Contains(text, "'") {
		t.Fatal("stub output contains a shell quote")
	}
	path := filepath.Join(t.TempDir(), "node-stub")
	script := "#!/bin/sh\nprintf '%s\\n' '" + text + "'\nexit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunNativePEAndResolverBuildWithIsolatedProcess(t *testing.T) {
	output, input := validNativeRuntimeFixture(t)
	encoded, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	stub := writeExecutableStub(t, encoded, 0)
	result, err := runNativePE(context.Background(), stub, device.Profile{}, []byte("sdk"), []byte("pe"), input)
	if err != nil || result.XPos != 29 {
		t.Fatalf("run result=%+v err=%v", result, err)
	}

	now := time.Now()
	resolver := newNodeKeyResolver(stub)
	resolver.now = func() time.Time { return now }
	path, _ := normalizeStaticPath(testDynamicPath)
	resolver.keys[path] = cachedRuntimeProfile{
		profile:   pe.RuntimeProfile{ArgumentKey: testArgumentKey},
		sdkSource: []byte("sdk"), peSource: []byte("pe"), sampledAt: now,
	}
	result, err = resolver.Build(context.Background(), nil, device.Profile{}, testDynamicPath, input)
	if err != nil || result.TrackEventCount != len(input.Track) {
		t.Fatalf("Build result=%+v err=%v", result, err)
	}
	if _, _, ok := resolver.cachedRuntimeSources(path, now.Add(keyProfileCacheTTL)); ok {
		t.Fatal("expired runtime scripts remained available")
	}
	input.SDKSource = []byte("AliyunCaptcha" + strings.Repeat("s", 1_100))
	if _, err := resolver.Build(context.Background(), nil, device.Profile{}, testDynamicPath, input); err != nil {
		t.Fatalf("session SDK snapshot: %v", err)
	}
	input.SDKSource = []byte("short")
	if _, err := resolver.Build(context.Background(), nil, device.Profile{}, testDynamicPath, input); !errors.Is(err, pe.ErrUnsupportedPE) {
		t.Fatalf("invalid session SDK: %v", err)
	}
}

func TestResolverBuildUsesOnlySelfVerifiedPureGoProfile(t *testing.T) {
	_, input := validNativeRuntimeFixture(t)
	now := time.Now()
	resolver := newNodeKeyResolver(filepath.Join(t.TempDir(), "missing-node"))
	resolver.now = func() time.Time { return now }
	path, _ := normalizeStaticPath(testDynamicPath)
	resolver.keys[path] = cachedRuntimeProfile{
		profile: pe.RuntimeProfile{
			ArgumentKey: testArgumentKey, PureGoCompatible: true,
		},
		profileSampled: true,
		sdkSource:      []byte("sdk"),
		peSource:       []byte("pe"),
		sampledAt:      now,
	}
	result, err := resolver.Build(context.Background(), nil, device.Profile{}, testDynamicPath, input)
	if err != nil {
		t.Fatalf("self-verified fast Build() error = %v", err)
	}
	decoded, err := protocol.UnpackData(result.Data)
	if err != nil {
		t.Fatal(err)
	}
	wantArgumentBytes, err := protocol.Transform64([]byte(input.CertifyID), testArgumentKey, false)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Payload.Arg != base64.StdEncoding.EncodeToString(wantArgumentBytes) || result.XPos != 29 || result.SlidePos != 80 || len(result.InteractionEvents) != len(input.Track) {
		t.Fatalf("pure-Go result contract mismatch: x=%d slide=%d interactions=%d", result.XPos, result.SlidePos, len(result.InteractionEvents))
	}

	resolver.keys[path] = cachedRuntimeProfile{
		profile: pe.RuntimeProfile{
			ArgumentKey: testArgumentKey, PureGoCompatible: false,
		},
		profileSampled: true,
		sdkSource:      []byte("sdk"),
		peSource:       []byte("pe"),
		sampledAt:      now,
	}
	if _, err := resolver.Build(context.Background(), nil, device.Profile{}, testDynamicPath, input); !errors.Is(err, pe.ErrKeyRuntime) {
		t.Fatalf("unverified profile did not fall back to V8/Node runtime: %v", err)
	}
}

func TestRunNativePERejectsProcessAndOutputFailures(t *testing.T) {
	_, input := validNativeRuntimeFixture(t)
	var absentContext context.Context
	if _, err := runNativePE(absentContext, "node", device.Profile{}, nil, nil, input); !errors.Is(err, pe.ErrKeyRuntime) {
		t.Fatalf("nil context: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runNativePE(ctx, "node", device.Profile{}, nil, nil, input); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context: %v", err)
	}
	if _, err := runNativePE(context.Background(), filepath.Join(t.TempDir(), "missing"), device.Profile{}, nil, nil, input); !errors.Is(err, pe.ErrKeyRuntime) {
		t.Fatalf("missing runtime: %v", err)
	}
	invalid := writeExecutableStub(t, []byte("not-json"), 0)
	if _, err := runNativePE(context.Background(), invalid, device.Profile{}, nil, nil, input); !errors.Is(err, pe.ErrKeyRuntime) {
		t.Fatalf("invalid output: %v", err)
	}
	failing := writeExecutableStub(t, []byte("failure"), 1)
	if _, err := runNativePE(context.Background(), failing, device.Profile{}, nil, nil, input); !errors.Is(err, pe.ErrKeyRuntime) {
		t.Fatalf("process failure: %v", err)
	}
}
