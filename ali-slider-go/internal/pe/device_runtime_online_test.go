//go:build online

package pe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/protocol"
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
	resolver := newNodeKeyResolver(os.Getenv("ALI_SLIDER_PE_KEY_NODE"))
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

// TestOnlineNodeTracelessRuntime 只访问阿里验证码公开 Init/Verify 端点；
// 不调用任何站点的短信接口，也不把挑战 ID、token 或 success 参数写入日志。
func TestOnlineNodeTracelessRuntime(t *testing.T) {
	if os.Getenv("ALI_SLIDER_NODE_TRACELESS_ONLINE") != "1" {
		t.Skip("set ALI_SLIDER_NODE_TRACELESS_ONLINE=1 to run the Node traceless probe")
	}
	sceneID := os.Getenv("ALI_SLIDER_ONLINE_SCENE_ID")
	if sceneID == "" {
		sceneID = "wa3238du"
	}
	prefix := os.Getenv("ALI_SLIDER_ONLINE_PREFIX")
	if prefix == "" {
		prefix = "1ohgtl"
	}
	sources := runtimekit.NewSystemSources()
	profile, err := device.GenerateProfile(sources.Entropy)
	if err != nil {
		t.Fatal(err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resolver := newNodeKeyResolver(os.Getenv("ALI_SLIDER_PE_KEY_NODE"))
	defer resolver.Close()
	startedAt := time.Now()
	session, err := resolver.OpenDevice(ctx, transport, profile, DeviceRuntimeOptions{
		Prefix: prefix, Region: "cn", Timeout: 12 * time.Second,
		GatherCostMin: 180, GatherCostMax: 260, FirstTouchAgeMin: 650, FirstTouchAgeMax: 850,
		Sources: sources,
	})
	if err != nil {
		t.Fatal(err)
	}
	openedAt := time.Now()
	defer session.Close()
	deviceToken, err := session.InitToken()
	if err != nil {
		t.Fatal(err)
	}
	input, err := onlineTracelessInit(ctx, transport, profile, sources, prefix, sceneID, deviceToken)
	if err != nil {
		t.Fatal(err)
	}
	initializedAt := time.Now()
	result, err := session.SolveTraceless(ctx, input)
	if err != nil {
		session.mu.Lock()
		diagnosticCode := "redacted"
		if session.stderr != nil {
			stderr := session.stderr.buffer.String()
			if matched := regexp.MustCompile(`TRACELESS_[A-Z_]+`).FindString(stderr); matched != "" {
				diagnosticCode = matched
			}
		}
		session.mu.Unlock()
		t.Fatalf("SolveTraceless failed: %v diagnostic=%s", err, diagnosticCode)
	}
	if !result.Succeeded() || result.CertifyID != input.CertifyID {
		t.Fatal("traceless result contract mismatch")
	}
	solvedAt := time.Now()
	t.Logf(
		"TRACELESS completed: code=%s result=%t requests=%d open=%s init=%s solve=%s total=%s",
		result.VerifyCode, result.VerifyResult, len(result.RequestActions),
		openedAt.Sub(startedAt).Round(time.Millisecond),
		initializedAt.Sub(openedAt).Round(time.Millisecond),
		solvedAt.Sub(initializedAt).Round(time.Millisecond),
		solvedAt.Sub(startedAt).Round(time.Millisecond),
	)
}

// TestOnlineNodeSlidingRuntime 只访问阿里验证码公开 Init/Verify/资源/采集端点；
// 不调用 DJI 登录或短信接口，也不输出挑战 ID、token 或 success 参数。
func TestOnlineNodeSlidingRuntime(t *testing.T) {
	if os.Getenv("ALI_SLIDER_NODE_SLIDING_ONLINE") != "1" {
		t.Skip("set ALI_SLIDER_NODE_SLIDING_ONLINE=1 to run the Node sliding probe")
	}
	sceneID := os.Getenv("ALI_SLIDER_ONLINE_SCENE_ID")
	if sceneID == "" {
		sceneID = "159tlu75"
	}
	prefix := os.Getenv("ALI_SLIDER_ONLINE_PREFIX")
	if prefix == "" {
		prefix = "1ulc59"
	}
	sources := runtimekit.NewSystemSources()
	profile, err := device.GenerateProfile(sources.Entropy)
	if err != nil {
		t.Fatal(err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	resolver := newNodeKeyResolver(os.Getenv("ALI_SLIDER_PE_KEY_NODE"))
	defer resolver.Close()
	startedAt := time.Now()
	session, err := resolver.OpenDevice(ctx, transport, profile, DeviceRuntimeOptions{
		Prefix: prefix, Region: "cn", Timeout: 18 * time.Second,
		GatherCostMin: 180, GatherCostMax: 260, FirstTouchAgeMin: 650, FirstTouchAgeMax: 850,
		Sources: sources,
	})
	if err != nil {
		t.Fatal(err)
	}
	openedAt := time.Now()
	defer session.Close()
	deviceToken, err := session.InitToken()
	if err != nil {
		t.Fatal(err)
	}
	input, err := onlineSlidingInit(ctx, transport, profile, sources, prefix, sceneID, deviceToken)
	if err != nil {
		t.Fatal(err)
	}
	initializedAt := time.Now()
	result, err := session.SolveSliding(ctx, input)
	if err != nil {
		session.mu.Lock()
		diagnosticCode := "redacted"
		if session.stderr != nil {
			stderr := session.stderr.buffer.String()
			if matched := regexp.MustCompile(`SLIDING_[A-Z_]+`).FindString(stderr); matched != "" {
				diagnosticCode = matched
			}
		}
		session.mu.Unlock()
		t.Fatalf("SolveSliding failed: %v diagnostic=%s", err, diagnosticCode)
	}
	if !result.Succeeded() || result.CertifyID != input.CertifyID {
		t.Fatal("sliding result contract mismatch")
	}
	solvedAt := time.Now()
	t.Logf(
		"SLIDING completed: code=%s result=%t requests=%d open=%s init=%s solve=%s total=%s",
		result.VerifyCode, result.VerifyResult, len(result.RequestActions),
		openedAt.Sub(startedAt).Round(time.Millisecond),
		initializedAt.Sub(openedAt).Round(time.Millisecond),
		solvedAt.Sub(initializedAt).Round(time.Millisecond),
		solvedAt.Sub(startedAt).Round(time.Millisecond),
	)
}

type onlineImageLessChallenge struct {
	Success       bool
	Code          string
	CertifyID     string
	SecurityToken string
	Image         string
	PuzzleImage   string
	StaticPath    string
	CaptchaType   string
}

func onlineTracelessInit(
	ctx context.Context,
	transport http.RoundTripper,
	profile device.Profile,
	sources runtimekit.Sources,
	prefix, sceneID, deviceToken string,
) (TracelessInput, error) {
	payload, err := onlineImageLessInit(ctx, transport, profile, sources, prefix, sceneID, deviceToken, "popup")
	if err != nil {
		return TracelessInput{}, err
	}
	if !payload.Success || payload.Code != "Success" || payload.CertifyID == "" || payload.StaticPath == "" || !isTracelessType(payload.CaptchaType) || payload.Image != "" || payload.PuzzleImage != "" {
		return TracelessInput{}, errors.New("captcha Init was not image-less TRACELESS")
	}
	return TracelessInput{
		SceneID: sceneID, CertifyID: payload.CertifyID,
		StaticPath: payload.StaticPath, CaptchaType: payload.CaptchaType,
	}, nil
}

func onlineSlidingInit(
	ctx context.Context,
	transport http.RoundTripper,
	profile device.Profile,
	sources runtimekit.Sources,
	prefix, sceneID, deviceToken string,
) (SlidingInput, error) {
	payload, err := onlineImageLessInit(ctx, transport, profile, sources, prefix, sceneID, deviceToken, "embed")
	if err != nil {
		return SlidingInput{}, err
	}
	if !payload.Success || payload.Code != "Success" || payload.CertifyID == "" || payload.StaticPath == "" || !isSlidingType(payload.CaptchaType) || payload.Image != "" || payload.PuzzleImage != "" {
		return SlidingInput{}, fmt.Errorf("captcha Init contract mismatch: success=%t code=%q type=%q image=%t puzzle=%t static=%t initToken=%t", payload.Success, payload.Code, payload.CaptchaType, payload.Image != "", payload.PuzzleImage != "", payload.StaticPath != "", payload.SecurityToken != "")
	}
	trackValue, err := track.LoadDefault(370, sources.Entropy)
	if err != nil {
		return SlidingInput{}, errors.New("generate sliding track")
	}
	return SlidingInput{
		SceneID: sceneID, CertifyID: payload.CertifyID,
		StaticPath: payload.StaticPath, CaptchaType: payload.CaptchaType,
		Track: trackValue, SlideWidth: 418, HandleWidth: 48,
	}, nil
}

func onlineImageLessInit(
	ctx context.Context,
	transport http.RoundTripper,
	profile device.Profile,
	sources runtimekit.Sources,
	prefix, sceneID, deviceToken, mode string,
) (onlineImageLessChallenge, error) {
	secrets, err := protocol.ResolveFrontendSecrets()
	if err != nil {
		return onlineImageLessChallenge{}, errors.New("resolve public frontend credentials")
	}
	nonce, err := runtimekit.UUIDv4(sources.Entropy)
	if err != nil {
		return onlineImageLessChallenge{}, errors.New("generate RPC nonce")
	}
	fields := []protocol.Field{
		{Key: "AaduaneId", Value: secrets.MainRPCKeyID()},
		{Key: "SignatureMethod", Value: "HMAC-SHA1"},
		{Key: "SignatureVersion", Value: "1.0"},
		{Key: "Format", Value: "JSON"},
		{Key: "Timestamp", Value: sources.Clock.Now().UTC().Format("2006-01-02T15:04:05Z")},
		{Key: "Version", Value: "2023-03-05"},
		{Key: "Action", Value: "InitCaptchaV3"},
		{Key: "SceneId", Value: sceneID},
		{Key: "Language", Value: "cn"},
		{Key: "Mode", Value: mode},
		{Key: "DeviceToken", Value: deviceToken},
		{Key: "SignatureNonce", Value: nonce},
	}
	params := make(map[string]string, len(fields))
	for _, field := range fields {
		params[field.Key] = field.Value
	}
	signature, err := protocol.RPCV1Signature(params, secrets.MainRPCKeySecret())
	if err != nil {
		return onlineImageLessChallenge{}, errors.New("sign captcha RPC")
	}
	fields = append(fields, protocol.Field{Key: "Signature", Value: signature})
	body, err := protocol.JSFormURLEncode(fields)
	if err != nil {
		return onlineImageLessChallenge{}, errors.New("encode captcha RPC")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+prefix+".captcha-open.aliyuncs.com/", strings.NewReader(body))
	if err != nil {
		return onlineImageLessChallenge{}, errors.New("build captcha RPC")
	}
	for key, value := range profile.BrowserHeaders("http://localhost:38185/", "http://localhost:38185", "empty", "cors", true) {
		request.Header.Set(key, value)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	response, err := (&http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}).Do(request)
	if err != nil {
		return onlineImageLessChallenge{}, errors.New("captcha Init request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return onlineImageLessChallenge{}, fmt.Errorf("captcha Init HTTP %d", response.StatusCode)
	}
	var payload struct {
		Success       bool   `json:"Success"`
		Code          string `json:"Code"`
		CertifyID     string `json:"CertifyId"`
		SecurityToken string `json:"SecurityToken"`
		Image         string `json:"Image"`
		PuzzleImage   string `json:"PuzzleImage"`
		StaticPath    string `json:"StaticPath"`
		CaptchaType   string `json:"CaptchaType"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 2<<20))
	if err := decoder.Decode(&payload); err != nil {
		return onlineImageLessChallenge{}, errors.New("decode captcha Init response")
	}
	return onlineImageLessChallenge{
		Success: payload.Success, Code: payload.Code, CertifyID: payload.CertifyID,
		SecurityToken: payload.SecurityToken, Image: payload.Image, PuzzleImage: payload.PuzzleImage,
		StaticPath: payload.StaticPath, CaptchaType: payload.CaptchaType,
	}, nil
}
