//go:build online

package pe

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/protocol"
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

// TestOnlineV8DeviceRuntimeColdRounds 请求公开 SDK/Log/FeiLin/PE，跑完两轮
// 独立 Open → Device Init → 动态 PE data → Complete → Close。它不创建
// 验证码，也不调用业务 Init/Verify。
func TestOnlineV8DeviceRuntimeColdRounds(t *testing.T) {
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
	runtimeProfile, err := resolver.Resolve(ctx, transport, profile, testDynamicPath)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	firstSessionID, firstToken := runOnlineV8ColdDeviceRound(
		t, ctx, resolver, transport, profile, sources, deviceOptions, "dummy-certify-id-1",
	)
	secondSessionID, secondToken := runOnlineV8ColdDeviceRound(
		t, ctx, resolver, transport, profile, sources, deviceOptions, "dummy-certify-id-2",
	)
	if firstSessionID == secondSessionID || firstToken == secondToken {
		t.Fatal("independent V8 Device rounds reused previous session/token state")
	}
	t.Logf("V8 Device/PE two cold rounds completed: key=%s", runtimeProfile.ArgumentKey)
}

func runOnlineV8ColdDeviceRound(
	t *testing.T,
	ctx context.Context,
	resolver *KeyResolver,
	transport http.RoundTripper,
	profile device.Profile,
	sources runtimekit.Sources,
	options DeviceRuntimeOptions,
	certifyID string,
) (string, string) {
	t.Helper()
	openStarted := time.Now()
	session, err := resolver.OpenDevice(ctx, transport, profile, options)
	if err != nil {
		if loggedTransport, ok := transport.(*v8OnlineTransport); ok {
			t.Logf("V8 Device network calls: %v", loggedTransport.snapshot())
		}
		t.Fatalf("OpenDevice() error = %v", err)
	}
	t.Logf("V8 Device cold open completed in %s", time.Since(openStarted))
	defer session.Close()
	return runOnlineV8DeviceRound(t, ctx, resolver, transport, profile, sources, session, certifyID)
}

func runOnlineV8DeviceRound(
	t *testing.T,
	ctx context.Context,
	resolver *KeyResolver,
	transport http.RoundTripper,
	profile device.Profile,
	sources runtimekit.Sources,
	session *DeviceRuntimeSession,
	certifyID string,
) (string, string) {
	t.Helper()
	result, err := executeOnlineV8DeviceRound(ctx, resolver, transport, profile, sources, session, certifyID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf(
		"V8 Device/PE round completed in %s: build=%s complete=%s dataLength=%d fields=%d requests=%d",
		result.elapsed, result.buildElapsed, result.completeElapsed, result.dataLength, result.fieldCount, result.requestCount,
	)
	return result.sessionID, result.initToken
}

type onlineV8DeviceRoundResult struct {
	sessionID       string
	initToken       string
	elapsed         time.Duration
	buildElapsed    time.Duration
	completeElapsed time.Duration
	dataLength      int
	fieldCount      int
	requestCount    int
}

func executeOnlineV8DeviceRound(
	ctx context.Context,
	resolver *KeyResolver,
	transport http.RoundTripper,
	profile device.Profile,
	sources runtimekit.Sources,
	session *DeviceRuntimeSession,
	certifyID string,
) (onlineV8DeviceRoundResult, error) {
	roundStarted := time.Now()
	initToken, err := session.InitToken()
	if err != nil {
		return onlineV8DeviceRoundResult{}, fmt.Errorf("read init token: %w", err)
	}
	firstTouchAge, err := session.TargetFirstTouchAgeMS()
	if err != nil {
		return onlineV8DeviceRoundResult{}, fmt.Errorf("read first-touch age: %w", err)
	}
	trackValue, err := track.LoadDefault(206, sources.Entropy)
	if err != nil {
		return onlineV8DeviceRoundResult{}, fmt.Errorf("load track: %w", err)
	}
	config, err := session.PEDeviceConfig()
	if err != nil {
		return onlineV8DeviceRoundResult{}, fmt.Errorf("read device config: %w", err)
	}
	accessSec, verifySalt, err := session.PEVerifyArgProfile()
	if err != nil {
		return onlineV8DeviceRoundResult{}, fmt.Errorf("read verify profile: %w", err)
	}
	sdkSource, err := session.PESDKSource()
	if err != nil {
		return onlineV8DeviceRoundResult{}, fmt.Errorf("read SDK source: %w", err)
	}
	expectedX := 166
	runtimeInput := RuntimeInput{
		SceneID: "1ug4aptr", CertifyID: certifyID, DeviceToken: initToken,
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
	buildStarted := time.Now()
	nativeResult, err := runPreparedV8PE(ctx, resolver, profile, testDynamicPath, runtimeInput)
	buildElapsed := time.Since(buildStarted)
	if err != nil {
		return onlineV8DeviceRoundResult{}, fmt.Errorf("Build() error: %w", err)
	}
	interactions := make([]device.InteractionEvent, len(nativeResult.InteractionEvents))
	for index, event := range nativeResult.InteractionEvents {
		interactions[index] = device.InteractionEvent{
			Type: event.Type, X: event.X, Y: event.Y, TimeStamp: event.TimeStamp, IsTrusted: event.IsTrusted,
		}
	}
	completeStarted := time.Now()
	completed, err := session.Complete(
		ctx,
		runtimeInput.CertifyID,
		interactions,
		int(nativeResult.PostInteractionDelayMS+0.5),
	)
	if err != nil {
		return onlineV8DeviceRoundResult{}, fmt.Errorf("Complete() error: %w", err)
	}
	if completed.VerifyToken == "" || completed.FingerprintFieldCount != 142 || completed.RequestCount != 4 {
		return onlineV8DeviceRoundResult{}, fmt.Errorf(
			"unexpected V8 completion: fields=%d requests=%d",
			completed.FingerprintFieldCount, completed.RequestCount,
		)
	}
	return onlineV8DeviceRoundResult{
		sessionID: config.SessionID, initToken: initToken, elapsed: time.Since(roundStarted),
		buildElapsed: buildElapsed, completeElapsed: time.Since(completeStarted), dataLength: len(nativeResult.Data),
		fieldCount: completed.FingerprintFieldCount, requestCount: completed.RequestCount,
	}, nil
}

// TestOnlineV8DeviceRuntimeConcurrentColdFlows 验证最多 10 个 job 都并发执行独立的
// Open/Complete/Close，期间不经过本地 Device 执行或生命周期门限。
// 它不创建 Captcha Init/Verify，只访问公开 SDK/Device/PE 组件。
func TestOnlineV8DeviceRuntimeConcurrentColdFlows(t *testing.T) {
	if os.Getenv("ALI_SLIDER_V8_DEVICE_CONCURRENT_ONLINE") != "1" {
		t.Skip("set ALI_SLIDER_V8_DEVICE_CONCURRENT_ONLINE=1 to run the concurrent V8 device probe")
	}
	libraryPath := os.Getenv("ALI_SLIDER_V8_TEST_LIBRARY")
	if libraryPath == "" {
		t.Fatal("ALI_SLIDER_V8_TEST_LIBRARY is required")
	}
	attempts := 10
	if raw := os.Getenv("ALI_SLIDER_V8_DEVICE_CONCURRENCY"); raw != "" {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil || parsed < 1 || parsed > 10 {
			t.Fatal("ALI_SLIDER_V8_DEVICE_CONCURRENCY must be within 1..10")
		}
		attempts = parsed
	}
	sources := runtimekit.NewSystemSources()
	profiles := make([]device.Profile, attempts)
	profileIDs := make(map[string]struct{}, attempts)
	for index := range profiles {
		profile, err := device.GenerateProfile(sources.Entropy)
		if err != nil {
			t.Fatal(err)
		}
		if _, duplicate := profileIDs[profile.ProfileID]; duplicate {
			t.Fatal("generated duplicate concurrent profile id")
		}
		profileIDs[profile.ProfileID] = struct{}{}
		profiles[index] = profile
	}
	baseTransport := http.DefaultTransport.(*http.Transport).Clone()
	baseTransport.Proxy = nil
	defer baseTransport.CloseIdleConnections()
	resolver := NewKeyResolverWithCapacity(libraryPath, attempts)
	defer func() {
		if closeErr := resolver.Close(); closeErr != nil {
			t.Errorf("KeyResolver.Close() error = %v", closeErr)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	deviceOptions := DeviceRuntimeOptions{
		Prefix: "fsgtmi", Region: "cn", Timeout: 10 * time.Second,
		GatherCostMin: 180, GatherCostMax: 260, FirstTouchAgeMin: 650, FirstTouchAgeMax: 850,
		Sources: sources,
	}
	if _, err := resolver.Resolve(ctx, baseTransport, profiles[0], testDynamicPath); err != nil {
		t.Fatalf("Resolve() before concurrent OpenDevice error = %v", err)
	}

	roundResults := make(chan error, attempts)
	var workers sync.WaitGroup
	for index := range attempts {
		workers.Add(1)
		go func() {
			defer workers.Done()
			session, openErr := resolver.OpenDevice(ctx, baseTransport, profiles[index], deviceOptions)
			if openErr != nil {
				roundResults <- fmt.Errorf("round %d open: %w", index, openErr)
				return
			}
			defer session.Close()
			_, roundErr := executeOnlineV8DeviceRound(
				ctx, resolver, baseTransport, profiles[index], sources, session,
				fmt.Sprintf("dummy-concurrent-certify-id-%d", index),
			)
			if roundErr != nil {
				roundErr = fmt.Errorf("round %d: %w", index, roundErr)
			}
			roundResults <- roundErr
		}()
	}
	workers.Wait()
	close(roundResults)
	for roundErr := range roundResults {
		if roundErr != nil {
			t.Error(roundErr)
		}
	}
}

// TestOnlineV8PEPerformance 只下载公开 SDK/PE，不请求 Device、
// Captcha Init 或 Verify。它分开测量首次、热复用和并发 PE 执行。
func TestOnlineV8PEPerformance(t *testing.T) {
	if os.Getenv("ALI_SLIDER_V8_PE_PERF_ONLINE") != "1" {
		t.Skip("set ALI_SLIDER_V8_PE_PERF_ONLINE=1 to run the public V8 PE performance probe")
	}
	libraryPath := os.Getenv("ALI_SLIDER_V8_TEST_LIBRARY")
	if libraryPath == "" {
		t.Fatal("ALI_SLIDER_V8_TEST_LIBRARY is required")
	}
	const concurrency = 10
	sources := runtimekit.NewSystemSources()
	profile, err := device.GenerateProfile(sources.Entropy)
	if err != nil {
		t.Fatal(err)
	}
	baseTransport := http.DefaultTransport.(*http.Transport).Clone()
	baseTransport.Proxy = nil
	defer baseTransport.CloseIdleConnections()
	resolver := NewKeyResolverWithCapacity(libraryPath, concurrency)
	defer func() {
		if err := resolver.Close(); err != nil {
			t.Errorf("KeyResolver.Close() error = %v", err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	prepareStarted := time.Now()
	if err := resolver.Prepare(ctx, baseTransport, profile, testDynamicPath); err != nil {
		t.Fatal(err)
	}
	prepareElapsed := time.Since(prepareStarted)

	expectedX := 29
	baseInput := RuntimeInput{
		SceneID: "1ug4aptr", DeviceToken: "dummy-device-token", CaptchaType: "PUZZLE",
		Image: "background.png", PuzzleImage: "puzzle.png",
		DeviceConfig: protocol.DeviceConfig{
			Key: "0123456789abcdef", Switch: 1, SessionID: "dummy-session",
			Version: "1.5.1", Timestamp: "2000000000000", IP: "203.0.113.8",
		},
		VerifyAccessSec: "dummy-access", VerifySalt: "dummy-salt",
		Dimensions: RuntimeDimensions{
			ImageWidth: 300, ImageHeight: 200, PuzzleWidth: 60, PuzzleHeight: 60,
			RenderedWidth: 300, HandleWidth: 40,
		},
		Track: []track.Event{
			{Type: "touchstart", X: 0, Y: 0, DT: 0, Force: 0.5, RadiusX: 5, RadiusY: 5},
			{Type: "touchmove", X: 30, Y: 0, DT: 20, Force: 0.5, RadiusX: 5, RadiusY: 5},
			{Type: "touchmove", X: 80, Y: 0, DT: 20, Force: 0.5, RadiusX: 5, RadiusY: 5},
			{Type: "touchend", X: 80, Y: 0, DT: 10, Force: 0.5, RadiusX: 5, RadiusY: 5},
		},
		ExpectedXPos: &expectedX, FirstTouchAgeMS: 700,
	}
	run := func(index int) (time.Duration, error) {
		input := baseInput
		input.CertifyID = fmt.Sprintf("dummy-certify-%04d", index)
		input.InitBeginTimeMS = time.Now().Add(-2 * time.Second).UnixMilli()
		started := time.Now()
		_, buildErr := runPreparedV8PE(ctx, resolver, profile, testDynamicPath, input)
		return time.Since(started), buildErr
	}

	first, err := run(0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := run(1)
	if err != nil {
		t.Fatal(err)
	}
	firstBatch := runV8PEBatch(t, concurrency, 100, run)
	secondBatch := runV8PEBatch(t, concurrency, 200, run)
	t.Logf(
		"V8 PE timings: prepare=%s first=%s second=%s firstConcurrent=%v secondConcurrent=%v",
		prepareElapsed, first, second, firstBatch, secondBatch,
	)
}

// TestOnlineV8PEPureGoParity 只请求公开 SDK/PE，用当前分片动态
// 采样出的 key/schema 比较 V8 与纯 Go Builder。它不创建验证码，
// 也不调用 Captcha Init/Verify。
func TestOnlineV8PEPureGoParity(t *testing.T) {
	if os.Getenv("ALI_SLIDER_V8_PE_PARITY_ONLINE") != "1" {
		t.Skip("set ALI_SLIDER_V8_PE_PARITY_ONLINE=1 to run the V8/pure-Go parity probe")
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
	resolver := NewKeyResolverWithCapacity(libraryPath, 1)
	defer func() {
		if err := resolver.Close(); err != nil {
			t.Errorf("KeyResolver.Close() error = %v", err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	runtimeProfile, err := resolver.Resolve(ctx, baseTransport, profile, testDynamicPath)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if !runtimeProfile.PureGoCompatible {
		t.Fatal("current V8 profile did not enable the self-verified pure-Go path")
	}
	expectedX := 29
	trackValue := []track.Event{
		{Type: "touchstart", X: 0, Y: 0, DT: 0, Force: 0.5, RadiusX: 5, RadiusY: 5},
		{Type: "touchmove", X: 30, Y: 0, DT: 20, Force: 0.5, RadiusX: 5, RadiusY: 5},
		{Type: "touchmove", X: 80, Y: 0, DT: 20, Force: 0.5, RadiusX: 5, RadiusY: 5},
		{Type: "touchend", X: 80, Y: 0, DT: 10, Force: 0.5, RadiusX: 5, RadiusY: 5},
	}
	const firstTouchAgeMS = 700
	initBeginTimeMS := time.Now().Add(-2 * time.Second).UnixMilli()
	runtimeInput := RuntimeInput{
		SceneID: "1ug4aptr", CertifyID: "dummy-parity-certify", DeviceToken: "dummy-device-token",
		CaptchaType: "PUZZLE", Image: "background.png", PuzzleImage: "puzzle.png",
		DeviceConfig: protocol.DeviceConfig{
			Key: "0123456789abcdef", Switch: 1, SessionID: "dummy-session",
			Version: "1.5.1", Timestamp: "2000000000000", IP: "203.0.113.8",
		},
		VerifyAccessSec: "dummy-access", VerifySalt: "dummy-salt",
		Dimensions: RuntimeDimensions{
			ImageWidth: 300, ImageHeight: 200, PuzzleWidth: 60, PuzzleHeight: 60,
			RenderedWidth: 300, HandleWidth: 40,
		},
		Track: trackValue, ExpectedXPos: &expectedX,
		InitBeginTimeMS: initBeginTimeMS, FirstTouchAgeMS: firstTouchAgeMS,
	}
	v8Started := time.Now()
	v8Result, err := runPreparedV8PE(ctx, resolver, profile, testDynamicPath, runtimeInput)
	if err != nil {
		t.Fatalf("V8 Build() error = %v", err)
	}
	v8Elapsed := time.Since(v8Started)
	v8Decoded, err := protocol.UnpackData(v8Result.Data)
	if err != nil {
		t.Fatalf("unpack V8 data: %v", err)
	}

	var trackDurationMS int64
	for _, event := range trackValue {
		trackDurationMS += int64(event.DT)
	}
	// 固定墙钟并直接采用 V8 的起点，避免两次运行的真实时间差
	// 被误判为算法差异。
	goNowMS := v8Result.TrackStartTimeMS + firstTouchAgeMS + trackDurationMS + freshnessMarginMS + 1_000
	goBuilder := Builder{
		Profile: profile,
		Sources: runtimekit.Sources{
			Clock:   fixedClock{now: time.UnixMilli(goNowMS)},
			Entropy: sources.Entropy,
		},
	}
	goStarted := time.Now()
	goResult, err := goBuilder.Build(ctx, Input{
		SceneID: runtimeInput.SceneID, CertifyID: runtimeInput.CertifyID,
		Dimensions: Dimensions{RenderedWidth: runtimeInput.Dimensions.RenderedWidth, HandleWidth: runtimeInput.Dimensions.HandleWidth},
		Track:      trackValue, StaticPath: testDynamicPath,
		ArgumentKey: runtimeProfile.ArgumentKey, IncludeScreenInfo: runtimeProfile.IncludeScreenInfo,
		ExpectedXPos: &expectedX, InitBeginTimeMS: v8Result.TrackStartTimeMS,
		FirstTouchAgeMS: firstTouchAgeMS,
	})
	if err != nil {
		t.Fatalf("pure-Go Build() error = %v", err)
	}
	goElapsed := time.Since(goStarted)
	goDecoded, err := protocol.UnpackData(goResult.Data)
	if err != nil {
		t.Fatalf("unpack pure-Go data: %v", err)
	}

	mismatches := make([]string, 0, 8)
	compareText := func(label, v8Value, goValue string) {
		if v8Value == goValue {
			return
		}
		v8Hash := sha256.Sum256([]byte(v8Value))
		goHash := sha256.Sum256([]byte(goValue))
		t.Logf(
			"parity %s differs: v8Len=%d v8SHA=%x goLen=%d goSHA=%x",
			label, len(v8Value), v8Hash[:6], len(goValue), goHash[:6],
		)
		mismatches = append(mismatches, label)
	}
	compareValue := func(label string, v8Value, goValue any) {
		left, right := fmt.Sprint(v8Value), fmt.Sprint(goValue)
		if left == right {
			return
		}
		t.Logf("parity %s differs: v8=%s go=%s", label, left, right)
		mismatches = append(mismatches, label)
	}
	compareText("payloadJSON", v8Decoded.JSONText, goDecoded.JSONText)
	compareText("arg", v8Decoded.Payload.Arg, goDecoded.Payload.Arg)
	compareValue("xPos", v8Decoded.Payload.XPos, goDecoded.Payload.XPos)
	compareValue("slidePos", v8Decoded.Payload.SlidePos, goDecoded.Payload.SlidePos)
	compareValue("TrackStartTime", v8Decoded.Payload.TrackStartTime, goDecoded.Payload.TrackStartTime)
	compareValue("VerifyTime", v8Decoded.Payload.VerifyTime, goDecoded.Payload.VerifyTime)
	compareValue("TrackList.startTime", v8Decoded.Payload.TrackList.StartTime, goDecoded.Payload.TrackList.StartTime)
	compareText("TrackList.mc", v8Decoded.Payload.TrackList.MC, goDecoded.Payload.TrackList.MC)
	compareText("TrackList.tc", v8Decoded.Payload.TrackList.TC, goDecoded.Payload.TrackList.TC)
	compareText("TrackList.mu", v8Decoded.Payload.TrackList.MU, goDecoded.Payload.TrackList.MU)
	compareText("TrackList.te", v8Decoded.Payload.TrackList.TE, goDecoded.Payload.TrackList.TE)
	compareText("TrackList.mp", v8Decoded.Payload.TrackList.MP, goDecoded.Payload.TrackList.MP)
	compareText("TrackList.tmv", v8Decoded.Payload.TrackList.TMV, goDecoded.Payload.TrackList.TMV)
	compareText("TrackList.mm", v8Decoded.Payload.TrackList.MM, goDecoded.Payload.TrackList.MM)
	compareText("TrackList.ks", v8Decoded.Payload.TrackList.KS, goDecoded.Payload.TrackList.KS)
	compareText("TrackList.fi", v8Decoded.Payload.TrackList.FI, goDecoded.Payload.TrackList.FI)
	compareText("TrackList.si", v8Decoded.Payload.TrackList.SI, goDecoded.Payload.TrackList.SI)
	if !reflect.DeepEqual(v8Result.InteractionEvents, goResult.InteractionEvents) {
		t.Logf("parity interaction events differ: v8=%v go=%v", v8Result.InteractionEvents, goResult.InteractionEvents)
		mismatches = append(mismatches, "interactionEvents")
	}
	compareValue("postInteractionDelayMS", v8Result.PostInteractionDelayMS, goResult.PostInteractionDelayMS)
	t.Logf(
		"V8/pure-Go parity timings: resolveKey=%s v8=%s pureGo=%s fields=%d",
		runtimeProfile.ArgumentKey, v8Elapsed, goElapsed, len(mismatches),
	)
	if len(mismatches) > 0 {
		t.Fatalf("pure-Go output is not equivalent to current V8; mismatched fields=%v", mismatches)
	}
}

func runPreparedV8PE(
	ctx context.Context,
	resolver *KeyResolver,
	profile device.Profile,
	staticPath string,
	input RuntimeInput,
) (Result, error) {
	path, err := normalizeStaticPath(staticPath)
	if err != nil {
		return Result{}, err
	}
	sdkSource, peSource, ok := resolver.cachedRuntimeSources(path, resolver.now())
	if !ok {
		return Result{}, fmt.Errorf("prepared V8 sources are unavailable")
	}
	if len(input.SDKSource) > 0 {
		sdkSource = append([]byte(nil), input.SDKSource...)
	}
	return resolver.runV8PE(ctx, profile, sdkSource, peSource, input)
}

func runV8PEBatch(t *testing.T, concurrency, offset int, run func(int) (time.Duration, error)) []time.Duration {
	t.Helper()
	started := make(chan struct{})
	results := make(chan struct {
		duration time.Duration
		err      error
	}, concurrency)
	for index := range concurrency {
		go func() {
			<-started
			duration, err := run(offset + index)
			results <- struct {
				duration time.Duration
				err      error
			}{duration: duration, err: err}
		}()
	}
	close(started)
	durations := make([]time.Duration, 0, concurrency)
	for range concurrency {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		durations = append(durations, result.duration)
	}
	sort.Slice(durations, func(left, right int) bool { return durations[left] < durations[right] })
	return durations
}
