//go:build online

package engine

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/domain/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/pe"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/protocol"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/track"
	"github.com/d2120848471/v3/ali-slider-go/internal/foundation/runtimekit"
)

// TestOnlineKeyResolver 是显式启用的公开 CDN/本机 Node 探针，不创建挑战，
// 也不调用 Device、Init 或 Verify。普通 go test 和 CI 永远跳过。
func TestOnlineKeyResolver(t *testing.T) {
	if os.Getenv("ALI_SLIDER_PE_KEY_ONLINE") != "1" {
		t.Skip("set ALI_SLIDER_PE_KEY_ONLINE=1 to run the public PE key probe")
	}
	staticPath := os.Getenv("ALI_SLIDER_PE_STATIC_PATH")
	if staticPath == "" {
		staticPath = testDynamicPath
	}
	profile, err := device.GenerateProfile(runtimekit.NewSystemSources().Entropy)
	if err != nil {
		t.Fatal(err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	resolver := newNodeKeyResolver(os.Getenv("ALI_SLIDER_PE_KEY_NODE"))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	resolved, err := resolver.Resolve(ctx, transport, profile, staticPath)
	if err != nil {
		t.Fatal(err)
	}
	if expected := os.Getenv("ALI_SLIDER_PE_EXPECTED_KEY"); expected != "" && resolved.ArgumentKey != expected {
		t.Fatalf("key=%q, want %q", resolved.ArgumentKey, expected)
	}
	expectedX := 29
	native, err := resolver.Build(ctx, transport, profile, staticPath, pe.RuntimeInput{
		SceneID: "1ug4aptr", CertifyID: dummyCertifyID, DeviceToken: "dummy-device-token",
		CaptchaType: "PUZZLE", Image: "background.png", PuzzleImage: "puzzle.png",
		DeviceConfig: protocol.DeviceConfig{
			Key: "0123456789abcdef", Switch: 1, SessionID: "dummy-session",
			Version: "1.5.1", Timestamp: "2000000000000", IP: "203.0.113.8",
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
		ExpectedXPos: &expectedX, InitBeginTimeMS: time.Now().Add(-2 * time.Second).UnixMilli(), FirstTouchAgeMS: 700,
	})
	if err != nil {
		t.Fatal(err)
	}
	if native.XPos != expectedX || native.SlidePos != 80 || len(native.InteractionEvents) != 4 || native.Data == "" {
		t.Fatalf("native contract mismatch: x=%d slide=%d interactions=%d", native.XPos, native.SlidePos, len(native.InteractionEvents))
	}
	t.Logf("resolved StaticPath=%s key=%s includeScreenInfo=%t", staticPath, resolved.ArgumentKey, resolved.IncludeScreenInfo)
}
