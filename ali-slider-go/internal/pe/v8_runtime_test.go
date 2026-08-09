package pe

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
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
	if _, err := nilResolver.openV8Library(); !errors.Is(err, ErrKeyRuntime) {
		t.Fatalf("nil openV8Library() error = %v", err)
	}
	if err := nilResolver.Close(); err != nil {
		t.Fatalf("nil Close() error = %v", err)
	}

	resolver := &KeyResolver{}
	if _, err := resolver.openV8Library(); !errors.Is(err, ErrKeyRuntime) || !strings.Contains(err.Error(), "path is empty") {
		t.Fatalf("openV8Library(empty) error = %v", err)
	}
	if err := resolver.Close(); err != nil {
		t.Fatalf("Close(unopened) error = %v", err)
	}
	if err := resolver.Close(); err != nil {
		t.Fatalf("Close(second) error = %v", err)
	}
	if _, err := resolver.openV8Library(); !errors.Is(err, ErrKeyRuntime) || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("openV8Library(closed) error = %v", err)
	}

	missing := NewKeyResolver(filepath.Join(t.TempDir(), "missing-v8-library"))
	if _, err := missing.CheckRuntime(); !errors.Is(err, ErrKeyRuntime) || !strings.Contains(err.Error(), "load V8 library") {
		t.Fatalf("CheckRuntime(missing) error = %v", err)
	}
}

func TestValidateRuntimeProfileBridgeOutputRejectsMalformedData(t *testing.T) {
	if _, err := validateRuntimeProfileBridgeOutput(runtimeProfileBridgeOutput{}); !errors.Is(err, ErrUnsupportedPE) {
		t.Fatalf("empty output error = %v", err)
	}
	if _, err := validateRuntimeProfileBridgeOutput(runtimeProfileBridgeOutput{
		Key: "dmmlums5zuewlgt7", Argument: "wrong", Data: "wrong",
	}); !errors.Is(err, ErrUnsupportedPE) || !strings.Contains(err.Error(), "independent key check") {
		t.Fatalf("mismatched argument error = %v", err)
	}
}
