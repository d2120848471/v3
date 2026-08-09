package pe

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/d2120848471/v3/ali-slider-go/internal/v8runtime"
)

func TestBuildV8SDKBundle(t *testing.T) {
	bundle, err := buildV8SDKBundle()
	if err != nil {
		t.Fatalf("buildV8SDKBundle() error = %v", err)
	}
	for _, required := range []string{
		"const TARGET_ORIGIN",
		"function makeBrowserContext",
		"globalThis.__aliSdkModule",
		"globalThis.__aliV8DeviceOpen",
		"globalThis.__aliV8DeviceComplete",
	} {
		if !strings.Contains(bundle, required) {
			t.Errorf("SDK bundle does not contain %q", required)
		}
	}
	for _, forbidden := range []string{
		`from "node:`,
		"\nexport {",
		"import.meta.url",
	} {
		if strings.Contains(bundle, forbidden) {
			t.Errorf("SDK bundle contains forbidden ESM entry %q", forbidden)
		}
	}
}

func TestBuildV8PEBundle(t *testing.T) {
	bundle, err := buildV8PEBundle()
	if err != nil {
		t.Fatalf("buildV8PEBundle() error = %v", err)
	}
	for _, required := range []string{
		"const IDENTIFIER",
		"function makePeBrowserContext",
		"globalThis.__aliV8PERun",
	} {
		if !strings.Contains(bundle, required) {
			t.Errorf("PE bundle does not contain %q", required)
		}
	}
	for _, forbidden := range []string{
		`from "node:`,
		"\nexport {",
		"import.meta.url",
	} {
		if strings.Contains(bundle, forbidden) {
			t.Errorf("PE bundle contains forbidden ESM entry %q", forbidden)
		}
	}
}

func TestExtractV8BridgeCoreRejectsChangedBoundaries(t *testing.T) {
	if _, err := extractV8BridgeCore([]byte("const X = 1;"), "missing"); err == nil {
		t.Fatal("extractV8BridgeCore(missing start) unexpectedly succeeded")
	}
	if _, err := extractV8BridgeCore([]byte("const X = 1;"), "const X"); err == nil {
		t.Fatal("extractV8BridgeCore(missing export) unexpectedly succeeded")
	}
}

func TestV8BundlesParseInNativeEngine(t *testing.T) {
	path := os.Getenv("ALI_SLIDER_V8_TEST_LIBRARY")
	if path == "" {
		t.Skip("ALI_SLIDER_V8_TEST_LIBRARY is not set")
	}
	library, err := v8runtime.Open(path)
	if err != nil {
		t.Fatalf("v8runtime.Open() error = %v", err)
	}
	defer func() {
		if err := library.Close(); err != nil {
			t.Errorf("Library.Close() error = %v", err)
		}
	}()
	engine, err := library.NewWithHost(func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"base64":"AAAAAAAAAAAAAAAAAAAAAA=="}`), nil
	})
	if err != nil {
		t.Fatalf("Library.NewWithHost() error = %v", err)
	}
	defer func() {
		if err := engine.Close(); err != nil {
			t.Errorf("Runtime.Close() error = %v", err)
		}
	}()

	sdkBundle, err := buildV8SDKBundle()
	if err != nil {
		t.Fatalf("buildV8SDKBundle() error = %v", err)
	}
	peBundle, err := buildV8PEBundle()
	if err != nil {
		t.Fatalf("buildV8PEBundle() error = %v", err)
	}
	for _, script := range []struct {
		name   string
		source string
	}{
		{"v8-host-bootstrap.js", v8HostBootstrapSource},
		{"sdk-device-v8-bundle.js", sdkBundle},
		{"pe-key-v8-bundle.js", peBundle},
	} {
		result, err := engine.Eval(context.Background(), script.source, script.name)
		if err != nil || string(result) != "true" {
			t.Fatalf("Eval(%s) = %s, %v", script.name, result, err)
		}
	}
}

func TestKeyResolverChecksNativeRuntime(t *testing.T) {
	path := os.Getenv("ALI_SLIDER_V8_TEST_LIBRARY")
	if path == "" {
		t.Skip("ALI_SLIDER_V8_TEST_LIBRARY is not set")
	}
	resolver := NewKeyResolver(path)
	defer func() {
		if err := resolver.Close(); err != nil {
			t.Errorf("KeyResolver.Close() error = %v", err)
		}
	}()
	version, err := resolver.CheckRuntime()
	if err != nil {
		t.Fatalf("KeyResolver.CheckRuntime() error = %v", err)
	}
	if strings.TrimSpace(version) == "" {
		t.Fatal("KeyResolver.CheckRuntime() returned an empty version")
	}
}
