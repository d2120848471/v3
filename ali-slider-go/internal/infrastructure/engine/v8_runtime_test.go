package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/d2120848471/v3/ali-slider-go/internal/domain/pe"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/protocol"
	"github.com/d2120848471/v3/ali-slider-go/internal/platform/v8runtime"
)

func TestCachedV8RuntimeBundles(t *testing.T) {
	first := cachedV8RuntimeBundles()
	second := cachedV8RuntimeBundles()
	if first.err != nil || second.err != nil {
		t.Fatalf("cachedV8RuntimeBundles() error = %v / %v", first.err, second.err)
	}
	if first.sdk == "" || first.pe == "" || first.sdk != second.sdk || first.pe != second.pe {
		t.Fatal("cached V8 bundles are empty or unstable")
	}
}

func TestV8PEEnginePoolReusesAndBounds(t *testing.T) {
	libraryPath := os.Getenv("ALI_SLIDER_V8_TEST_LIBRARY")
	if libraryPath == "" {
		t.Skip("ALI_SLIDER_V8_TEST_LIBRARY is not set")
	}
	resolver := NewKeyResolverWithCapacity(libraryPath, 1)
	first, err := resolver.acquireV8PEEngine(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	resolver.releaseV8PEEngine(first, true)
	second, err := resolver.acquireV8PEEngine(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatal("PE pool did not reuse the preloaded runtime")
	}
	third, err := resolver.acquireV8PEEngine(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	resolver.releaseV8PEEngine(second, true)
	resolver.releaseV8PEEngine(third, true)
	if _, err := third.Eval(context.Background(), "true", "closed.js"); !errors.Is(err, v8runtime.ErrClosed) {
		t.Fatalf("runtime above idle capacity remained open: %v", err)
	}
	if err := resolver.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Eval(context.Background(), "true", "closed.js"); !errors.Is(err, v8runtime.ErrClosed) {
		t.Fatalf("resolver Close left idle runtime open: %v", err)
	}
}

func TestDecodeV8ResultIsStrict(t *testing.T) {
	var decoded struct {
		Value string `json:"value"`
	}
	if err := decodeV8Result(json.RawMessage(`{"value":"ok"}`), &decoded); err != nil || decoded.Value != "ok" {
		t.Fatalf("decodeV8Result() = %+v, %v", decoded, err)
	}
	for _, value := range []json.RawMessage{
		json.RawMessage(`{"value":"ok","extra":true}`),
		json.RawMessage(`{"value":"ok"} {}`),
		json.RawMessage(`{`),
	} {
		if err := decodeV8Result(value, &decoded); err == nil {
			t.Fatalf("decodeV8Result(%s) unexpectedly succeeded", value)
		}
	}

	var numbered struct {
		Value json.Number `json:"value"`
	}
	if err := decodeV8ResultUseNumber(json.RawMessage(`{"value":9007199254740993}`), &numbered); err != nil || numbered.Value.String() != "9007199254740993" {
		t.Fatalf("decodeV8ResultUseNumber() = %+v, %v", numbered, err)
	}
	if err := decodeV8ResultUseNumber(json.RawMessage(`{"value":1} []`), &numbered); err == nil {
		t.Fatal("decodeV8ResultUseNumber(trailing) unexpectedly succeeded")
	}
}

func TestKeyResolverV8LibraryBoundaryErrors(t *testing.T) {
	var nilResolver *KeyResolver
	if _, err := nilResolver.openV8Library(); !errors.Is(err, pe.ErrKeyRuntime) {
		t.Fatalf("nil openV8Library() error = %v", err)
	}
	if err := nilResolver.Close(); err != nil {
		t.Fatalf("nil Close() error = %v", err)
	}

	resolver := &KeyResolver{}
	if _, err := resolver.openV8Library(); !errors.Is(err, pe.ErrKeyRuntime) || !strings.Contains(err.Error(), "path is empty") {
		t.Fatalf("openV8Library(empty) error = %v", err)
	}
	if err := resolver.Close(); err != nil {
		t.Fatalf("Close(unopened) error = %v", err)
	}
	if err := resolver.Close(); err != nil {
		t.Fatalf("Close(second) error = %v", err)
	}
	if _, err := resolver.openV8Library(); !errors.Is(err, pe.ErrKeyRuntime) || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("openV8Library(closed) error = %v", err)
	}

	missing := NewKeyResolver(filepath.Join(t.TempDir(), "missing-v8-library"))
	if _, err := missing.CheckRuntime(); !errors.Is(err, pe.ErrKeyRuntime) || !strings.Contains(err.Error(), "load V8 library") {
		t.Fatalf("CheckRuntime(missing) error = %v", err)
	}
}

func TestValidateRuntimeProfileBridgeOutputRejectsMalformedData(t *testing.T) {
	if _, err := validateRuntimeProfileBridgeOutput(runtimeProfileBridgeOutput{}); !errors.Is(err, pe.ErrUnsupportedPE) {
		t.Fatalf("empty output error = %v", err)
	}
	if _, err := validateRuntimeProfileBridgeOutput(runtimeProfileBridgeOutput{
		Key: "dmmlums5zuewlgt7", Argument: "wrong", Data: "wrong",
	}); !errors.Is(err, pe.ErrUnsupportedPE) || !strings.Contains(err.Error(), "independent key check") {
		t.Fatalf("mismatched argument error = %v", err)
	}
}

func TestRuntimeProfilePureGoParityGate(t *testing.T) {
	profile := oracleProfile()
	input := bridgeInput{
		SceneID: "1ug4aptr", CertifyID: dummyCertifyID,
		Dimensions:      map[string]int{"renderedWidth": 300, "handleWidth": 40},
		ExpectedXPos:    29,
		InitBeginTime:   1_999_999_994_250,
		FirstTouchAgeMS: 700,
	}
	for _, event := range oracleTrack() {
		input.Track = append(input.Track, bridgeTrackEvent{
			Type: event.Type, X: event.X, Y: event.Y, DT: event.DT,
			Force: 0.5, RadiusX: 5, RadiusY: 5,
		})
	}
	result, err := builderWithEntropy(&byteEntropy{data: make([]byte, 15)}, 2_000_000_000_000).Build(
		context.Background(),
		pe.Input{
			SceneID: input.SceneID, CertifyID: input.CertifyID,
			Dimensions: pe.Dimensions{RenderedWidth: 300, HandleWidth: 40},
			Track:      oracleTrack(), ArgumentKey: testArgumentKey, ExpectedXPos: &input.ExpectedXPos,
			InitBeginTimeMS: input.InitBeginTime, FirstTouchAgeMS: input.FirstTouchAgeMS,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := protocol.UnpackData(result.Data)
	if err != nil {
		t.Fatal(err)
	}
	output := runtimeProfileBridgeOutput{
		Key: testArgumentKey, Argument: decoded.Payload.Arg, Data: result.Data,
		PayloadKeys: result.PayloadKeys, TrackKeys: result.TrackKeys,
	}
	resolved, err := validateRuntimeProfileBridgeOutput(output)
	if err != nil {
		t.Fatal(err)
	}
	if !runtimeProfileMatchesPureGo(context.Background(), output, input, profile, resolved) {
		t.Fatal("equivalent V8 payload did not enable the pure-Go path")
	}

	decoded.Payload.SlidePos = "81"
	output.Data, err = protocol.PackData(decoded.Payload, "00112233445566778899aabbccddeeff")
	if err != nil {
		t.Fatal(err)
	}
	if runtimeProfileMatchesPureGo(context.Background(), output, input, profile, resolved) {
		t.Fatal("payload drift incorrectly enabled the pure-Go path")
	}
}
