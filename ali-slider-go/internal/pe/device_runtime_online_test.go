//go:build online

package pe

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/runtimekit"
	"github.com/d2120848471/v3/ali-slider-go/internal/track"
)

// TestOnlineNodeDeviceRuntime 只走公开 SDK 的 FeiLin 初始化，不创建验证码。
func TestOnlineNodeDeviceRuntime(t *testing.T) {
	if os.Getenv("ALI_SLIDER_NODE_DEVICE_ONLINE") != "1" {
		t.Skip("set ALI_SLIDER_NODE_DEVICE_ONLINE=1 to run the Node device probe")
	}
	sources := runtimekit.NewSystemSources()
	profile, err := device.GenerateProfile(sources.Entropy)
	if err != nil {
		t.Fatal(err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	resolver := NewKeyResolver(os.Getenv("ALI_SLIDER_PE_KEY_NODE"))
	session, err := resolver.OpenDevice(ctx, transport, profile, DeviceRuntimeOptions{
		Prefix: "fsgtmi", Region: "cn", Timeout: 25 * time.Second,
		GatherCostMin: 180, GatherCostMax: 260, FirstTouchAgeMin: 650, FirstTouchAgeMax: 850,
		Sources: sources,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result := session.initial
	if result.InitToken == "" || result.FingerprintFieldCount != 111 || result.RequestCount != 3 {
		t.Fatalf("unexpected Node device summary: fields=%d requests=%d", result.FingerprintFieldCount, result.RequestCount)
	}
	firstTouchAge, err := session.TargetFirstTouchAgeMS()
	if err != nil {
		t.Fatal(err)
	}
	trackValue, err := track.LoadDefault(206, sources.Entropy)
	if err != nil {
		t.Fatal(err)
	}
	config, err := session.PEDeviceConfig()
	if err != nil {
		t.Fatal(err)
	}
	accessSec, verifySalt, err := session.PEVerifyArgProfile()
	if err != nil {
		t.Fatal(err)
	}
	sdkSource, err := session.PESDKSource()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Resolve(ctx, transport, profile, testDynamicPath); err != nil {
		t.Fatal(err)
	}
	expectedX := 166
	native, err := resolver.Build(ctx, transport, profile, testDynamicPath, RuntimeInput{
		SceneID: "1ug4aptr", CertifyID: "dummy-certify-id", DeviceToken: result.InitToken,
		CaptchaType: "PUZZLE", Image: "background.png", PuzzleImage: "puzzle.png",
		DeviceConfig: config, VerifyAccessSec: accessSec, VerifySalt: verifySalt,
		Dimensions: RuntimeDimensions{
			ImageWidth: 296, ImageHeight: 200, PuzzleWidth: 52, PuzzleHeight: 200,
			RenderedWidth: 300, HandleWidth: 40,
		},
		Track: trackValue, ExpectedXPos: &expectedX,
		InitBeginTimeMS: time.Now().UnixMilli(), FirstTouchAgeMS: firstTouchAge,
		SDKSource: sdkSource,
	})
	if err != nil {
		t.Fatal(err)
	}
	interactions := make([]device.InteractionEvent, len(native.InteractionEvents))
	for index, event := range native.InteractionEvents {
		interactions[index] = device.InteractionEvent{
			Type: event.Type, X: event.X, Y: event.Y, TimeStamp: event.TimeStamp, IsTrusted: event.IsTrusted,
		}
	}
	completed, err := session.Complete(ctx, "dummy-certify-id", interactions, int(native.PostInteractionDelayMS+0.5))
	if err != nil {
		t.Fatal(err)
	}
	if completed.VerifyToken == "" || completed.GetterArgumentCount != 1 || completed.InteractionEventCount != len(interactions) || completed.RequestCount != 4 {
		t.Fatalf("unexpected Node completion summary: getter=%d interactions=%d requests=%d", completed.GetterArgumentCount, completed.InteractionEventCount, completed.RequestCount)
	}
	t.Logf("Node device completed: fields=%d requests=%d", completed.FingerprintFieldCount, completed.RequestCount)
}
