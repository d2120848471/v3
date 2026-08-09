//go:build online

package pe

import (
	"context"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/runtimekit"
	"github.com/d2120848471/v3/ali-slider-go/internal/track"
)

type v8OnlineTransport struct {
	base http.RoundTripper

	mu    sync.Mutex
	calls []string
}

func (transport *v8OnlineTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.base.RoundTrip(request)
	status := 0
	if response != nil {
		status = response.StatusCode
	}
	detail := request.Method + " " + request.URL.Hostname() + request.URL.Path + " status=" + http.StatusText(status)
	if err != nil {
		detail += " error=" + err.Error()
	}
	transport.mu.Lock()
	transport.calls = append(transport.calls, detail)
	transport.mu.Unlock()
	return response, err
}

func (transport *v8OnlineTransport) snapshot() []string {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return append([]string(nil), transport.calls...)
}

// TestOnlineV8DeviceRuntime 请求公开 SDK/Log/FeiLin/PE，跑完
// Device Init → 动态 PE data → 同一 Device VM Complete；不创建验证码，也不调用业务 Init/Verify。
func TestOnlineV8DeviceRuntime(t *testing.T) {
	if os.Getenv("ALI_SLIDER_V8_DEVICE_ONLINE") != "1" {
		t.Skip("set ALI_SLIDER_V8_DEVICE_ONLINE=1 to run the V8 device probe")
	}
	libraryPath := os.Getenv("ALI_SLIDER_V8_TEST_LIBRARY")
	if libraryPath == "" {
		t.Fatal("ALI_SLIDER_V8_TEST_LIBRARY is required")
	}
	sources := runtimekit.NewSystemSources()
	profile, err := device.GenerateProfile(sources.Entropy)
	if err != nil {
		t.Fatal(err)
	}
	baseTransport := http.DefaultTransport.(*http.Transport).Clone()
	baseTransport.Proxy = nil
	defer baseTransport.CloseIdleConnections()
	transport := &v8OnlineTransport{base: baseTransport}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resolver := NewKeyResolver(libraryPath)
	defer func() {
		if err := resolver.Close(); err != nil {
			t.Errorf("KeyResolver.Close() error = %v", err)
		}
	}()
	deviceOptions := DeviceRuntimeOptions{
		Prefix: "fsgtmi", Region: "cn", Timeout: 10 * time.Second,
		GatherCostMin: 180, GatherCostMax: 260, FirstTouchAgeMin: 650, FirstTouchAgeMax: 850,
		Sources: sources,
	}
	session, err := resolver.OpenDevice(ctx, transport, profile, deviceOptions)
	if err != nil {
		t.Logf("V8 Device network calls: %v", transport.snapshot())
		t.Fatalf("OpenDevice() error = %v", err)
	}
	defer session.Close()
	initToken, err := session.InitToken()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("V8 Device init passed: tokenLength=%d", len(initToken))

	runtimeProfile, err := resolver.Resolve(ctx, transport, profile, testDynamicPath)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
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
	expectedX := 166
	runtimeInput := RuntimeInput{
		SceneID: "1ug4aptr", CertifyID: "dummy-certify-id", DeviceToken: initToken,
		CaptchaType: "PUZZLE", Image: "background.png", PuzzleImage: "puzzle.png",
		DeviceConfig: config, VerifyAccessSec: accessSec, VerifySalt: verifySalt,
		Dimensions: RuntimeDimensions{
			ImageWidth: 296, ImageHeight: 200, PuzzleWidth: 52, PuzzleHeight: 200,
			RenderedWidth: 300, HandleWidth: 40,
		},
		Track: trackValue, ExpectedXPos: &expectedX,
		InitBeginTimeMS: time.Now().UnixMilli(), FirstTouchAgeMS: firstTouchAge,
		SDKSource: sdkSource,
	}
	nativeResult, err := resolver.Build(ctx, transport, profile, testDynamicPath, runtimeInput)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	interactions := make([]device.InteractionEvent, len(nativeResult.InteractionEvents))
	for index, event := range nativeResult.InteractionEvents {
		interactions[index] = device.InteractionEvent{
			Type: event.Type, X: event.X, Y: event.Y, TimeStamp: event.TimeStamp, IsTrusted: event.IsTrusted,
		}
	}
	completed, err := session.Complete(
		ctx,
		runtimeInput.CertifyID,
		interactions,
		int(nativeResult.PostInteractionDelayMS+0.5),
	)
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if completed.VerifyToken == "" || completed.FingerprintFieldCount != 142 || completed.RequestCount != 4 {
		t.Fatalf("unexpected V8 completion: fields=%d requests=%d", completed.FingerprintFieldCount, completed.RequestCount)
	}
	t.Logf(
		"V8 Device/PE completed: key=%s dataLength=%d fields=%d requests=%d",
		runtimeProfile.ArgumentKey, len(nativeResult.Data), completed.FingerprintFieldCount, completed.RequestCount,
	)
}
