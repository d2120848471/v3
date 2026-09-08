package config

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/platform/v8runtime"
)

func TestDefaultsValidate(t *testing.T) {
	config := Defaults()
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	if config.Host != "127.0.0.1" || config.MaxConcurrency != 32 || config.DevicePrewarmCapacity != 0 || config.DeviceSessionReserve != 0 || config.SceneID != DefaultSceneID || config.Prefix != DefaultPrefix || filepath.Base(config.V8RuntimeLibrary) != v8runtime.DefaultLibraryName() {
		t.Fatalf("unexpected defaults: %+v", config)
	}
}

func TestParsePrecedence(t *testing.T) {
	environment := map[string]string{
		"ALI_SLIDER_PORT":            "9000",
		"ALI_SLIDER_TIMEOUT":         "12s",
		"ALI_SLIDER_MAX_CONCURRENCY": "8",
		"ALI_SLIDER_V8_LIBRARY":      "/environment/libali_slider_v8_runtime.so",
	}
	config, err := Parse([]string{"-port=9100", "-max-concurrency=4", "-v8-library=/flag/libali_slider_v8_runtime.so"}, func(key string) string { return environment[key] })
	if err != nil {
		t.Fatal(err)
	}
	if config.Port != 9100 || config.MaxConcurrency != 4 || config.DevicePrewarmCapacity != 0 || config.DeviceSessionReserve != 0 || config.Timeout != 12*time.Second || config.V8RuntimeLibrary != "/flag/libali_slider_v8_runtime.so" {
		t.Fatalf("unexpected precedence: %+v", config)
	}
}

func TestParseRejectsInvalidValues(t *testing.T) {
	if _, err := Parse(nil, func(key string) string {
		if key == "ALI_SLIDER_PORT" {
			return "not-a-port"
		}
		return ""
	}); err == nil {
		t.Fatal("expected environment parse error")
	}
	for _, arguments := range [][]string{{"-max-concurrency=33"}, {"-device-prewarm=1"}, {"-device-reserve=1"}, {"-prefix=bad_prefix"}, {"-host=example.com"}, {"extra"}} {
		if _, err := Parse(arguments, func(string) string { return "" }); err == nil {
			t.Fatalf("expected error for %v", arguments)
		}
	}
	for _, key := range []string{"ALI_SLIDER_DEVICE_PREWARM", "ALI_SLIDER_DEVICE_RESERVE"} {
		if _, err := Parse(nil, func(name string) string {
			if name == key {
				return "1"
			}
			return ""
		}); err == nil {
			t.Fatalf("expected non-zero legacy environment %s to fail", key)
		}
	}
	if _, err := Parse([]string{"-device-prewarm=0", "-device-reserve=0"}, func(string) string { return "" }); err != nil {
		t.Fatalf("zero-valued compatibility flags failed: %v", err)
	}
	invalid := Defaults()
	invalid.V8RuntimeLibrary = ""
	if err := invalid.Validate(); err == nil {
		t.Fatal("empty V8 runtime library path accepted")
	}
}
