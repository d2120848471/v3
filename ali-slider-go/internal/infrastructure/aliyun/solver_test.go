package aliyun

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/application/solve"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/pe"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/protocol"
	"github.com/d2120848471/v3/ali-slider-go/internal/foundation/runtimekit"
	"github.com/d2120848471/v3/ali-slider-go/internal/infrastructure/artifact"
	"github.com/d2120848471/v3/ali-slider-go/internal/infrastructure/httpclient"
)

const solverFixtureKey = "0123456789abcdef"

func TestChallengeTypesRedactFormatting(t *testing.T) {
	values := []any{
		solve.Request{SceneID: "scene-secret", RPCKeyID: "rpc-secret", Proxy: "http://user:password@proxy.invalid"},
		solve.Outcome{SecurityToken: "token-secret", CertifyID: "certify-secret"},
		CaptchaChallenge{CertifyID: "certify-secret", ImagePath: "asset-secret"},
		VerifyResult{SecurityToken: "token-secret", CertifyID: "certify-secret"},
	}
	for _, value := range values {
		for _, formatted := range []string{fmt.Sprint(value), fmt.Sprintf("%#v", value)} {
			for _, secret := range []string{"scene-secret", "rpc-secret", "password", "token-secret", "certify-secret", "asset-secret"} {
				if strings.Contains(formatted, secret) {
					t.Fatalf("%T formatting leaked %q: %s", value, secret, formatted)
				}
			}
		}
	}
}

type solverClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *solverClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *solverClock) Sleep(ctx context.Context, duration time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	clock.mu.Lock()
	clock.now = clock.now.Add(max(duration, 0))
	clock.mu.Unlock()
	return nil
}

type solverTransport struct {
	mu              sync.Mutex
	clock           *solverClock
	secrets         protocol.FrontendSecrets
	background      []byte
	shadow          []byte
	initCount       int
	verifyCount     int
	deviceActions   []string
	verifySuccess   bool
	verifyError     error
	initMalformed   bool
	verifyMalformed bool
	deviceMalformed bool
	staticPath      string
	captchaType     string
	beforeVerify    func() error
	initUserAgent   string
	verifyUserAgent string
	assetUserAgents []string
}

func newSolverTransport(t *testing.T, fixture string) *solverTransport {
	t.Helper()
	secrets, err := protocol.ResolveFrontendSecrets()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join("..", "..", "domain", "vision", "testdata", "python-edge-decoy", fixture)
	background, err := os.ReadFile(filepath.Join(root, "back.png"))
	if err != nil {
		t.Fatal(err)
	}
	shadow, err := os.ReadFile(filepath.Join(root, "shadow.png"))
	if err != nil {
		t.Fatal(err)
	}
	return &solverTransport{
		clock: &solverClock{now: time.UnixMilli(2_000_000_000_000)}, secrets: secrets,
		background: background, shadow: shadow, verifySuccess: true,
	}
}

func (transport *solverTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method == http.MethodGet {
		transport.mu.Lock()
		transport.assetUserAgents = append(transport.assetUserAgents, request.UserAgent())
		transport.mu.Unlock()
		if request.URL.Hostname() != assetHost {
			return nil, errors.New("unexpected asset host")
		}
		content := transport.background
		if strings.Contains(request.URL.Path, "shadow") {
			content = transport.shadow
		}
		return solverResponse(request, http.StatusOK, content), nil
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	values, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, err
	}
	action := values.Get("Action")
	if strings.Contains(request.URL.Hostname(), "cloudauth-device") {
		return transport.deviceResponse(request, action)
	}
	switch action {
	case "InitCaptchaV3":
		transport.mu.Lock()
		transport.initCount++
		transport.initUserAgent = request.UserAgent()
		identifier := fmt.Sprintf("fixture-certify-%04d", transport.initCount)
		transport.mu.Unlock()
		if transport.initMalformed {
			return solverResponse(request, http.StatusOK, []byte(`{"Success":`)), nil
		}
		staticPath := transport.staticPath
		if staticPath == "" {
			staticPath = "3.29.0/pe.091.00665af58b020d81.js"
		}
		captchaType := transport.captchaType
		if captchaType == "" {
			captchaType = "slider"
		}
		response := map[string]any{
			"Success": true, "Code": "Success", "CertifyId": identifier,
			"StaticPath": staticPath, "CaptchaType": captchaType,
		}
		if !isSDKCaptchaType(captchaType) {
			response["Image"] = "fixtures/back.png"
			response["PuzzleImage"] = "fixtures/shadow.png"
		}
		return solverJSONResponse(request, response)
	case "VerifyCaptchaV3":
		if transport.beforeVerify != nil {
			if err := transport.beforeVerify(); err != nil {
				return nil, err
			}
		}
		transport.mu.Lock()
		transport.verifyCount++
		transport.verifyUserAgent = request.UserAgent()
		verifyError, verifySuccess := transport.verifyError, transport.verifySuccess
		transport.mu.Unlock()
		if verifyError != nil {
			return nil, verifyError
		}
		if transport.verifyMalformed {
			return solverResponse(request, http.StatusOK, []byte(`{"Result":`)), nil
		}
		identifier := values.Get("CertifyId")
		verifyCode := "F015"
		token := ""
		if verifySuccess {
			verifyCode, token = "T001", "fixture-security-token"
		}
		return solverJSONResponse(request, map[string]any{
			"Code": "Success", "Result": map[string]any{
				"VerifyCode": verifyCode, "VerifyResult": verifySuccess,
				"securityToken": token, "certifyId": identifier,
			},
		})
	default:
		return nil, fmt.Errorf("unexpected action %q", action)
	}
}

func (transport *solverTransport) deviceResponse(request *http.Request, action string) (*http.Response, error) {
	transport.mu.Lock()
	transport.deviceActions = append(transport.deviceActions, action)
	index := len(transport.deviceActions)
	transport.mu.Unlock()
	if transport.deviceMalformed {
		return solverResponse(request, http.StatusOK, []byte(`{"Code":`)), nil
	}
	response := map[string]any{"Code": "200"}
	if action == "Log1" {
		plaintext := strings.Join([]string{
			base64.StdEncoding.EncodeToString([]byte(solverFixtureKey)),
			base64.StdEncoding.EncodeToString([]byte("1")),
			fmt.Sprintf("offline-session-%08d", index), "1.5.1", "", "", "",
			fmt.Sprint(transport.clock.Now().UnixMilli() + 10), "203.0.113.8", "1",
		}, "#")
		ciphertext, err := protocol.AESCBCEncryptBase64([]byte(plaintext), transport.secrets.DeviceResponseKey())
		if err != nil {
			return nil, err
		}
		response["ResultObject"] = map[string]string{"DeviceConfig": ciphertext}
	}
	return solverJSONResponse(request, response)
}

func solverJSONResponse(request *http.Request, value any) (*http.Response, error) {
	content, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return solverResponse(request, http.StatusOK, content), nil
}

func solverResponse(request *http.Request, status int, content []byte) *http.Response {
	return &http.Response{
		StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(content))), Request: request,
	}
}

func newIntegrationSolver(t *testing.T, transport *solverTransport, minimumConfidence float64) (*solve.Solver, string, *RoundFactory) {
	return newIntegrationSolverWithTimeout(t, transport, minimumConfidence, 5*time.Second)
}

func newIntegrationSolverWithTimeout(t *testing.T, transport *solverTransport, minimumConfidence float64, timeout time.Duration) (*solve.Solver, string, *RoundFactory) {
	return newIntegrationSolverWithPEKeys(t, transport, minimumConfidence, timeout, nil)
}

func newIntegrationSolverWithPEKeys(t *testing.T, transport *solverTransport, minimumConfidence float64, timeout time.Duration, peKeys PEKeyResolver) (*solve.Solver, string, *RoundFactory) {
	t.Helper()
	if peKeys == nil {
		peKeys = &staticPEKeyResolver{profile: pe.RuntimeProfile{
			ArgumentKey: "0kd8i0mclivjow32", IncludeScreenInfo: true,
		}}
	}
	artifactDirectory := filepath.Join(t.TempDir(), "artifacts")
	sources := runtimekit.Sources{Clock: transport.clock, Entropy: &byteEntropy{}}
	store := &artifact.Store{Directory: artifactDirectory, Retention: 7 * 24 * time.Hour, Entropy: sources.Entropy, Now: transport.clock.Now}
	factory, err := NewRoundFactory(FactoryOptions{
		Timeout:       timeout,
		GatherCostMin: 180, GatherCostMax: 260, FirstTouchAgeMin: 650, FirstTouchAgeMax: 850,
		AssetMaxBytes: 8 << 20, Sources: sources, PEKeys: peKeys,
		GetTransport: func(string) (http.RoundTripper, bool, error) { return transport, false, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	solver, err := solve.New(solve.Options{
		Timeout: timeout, MinimumConfidence: minimumConfidence,
		AssetMaxBytes: 8 << 20, AssetMaxDimension: 16_384, AssetMaxPixels: 16 << 20,
		Sources: sources, Factory: factory, Recorder: integrationRecorder{store: store},
	})
	if err != nil {
		t.Fatal(err)
	}
	return solver, artifactDirectory, factory

}

type integrationRecorder struct{ store *artifact.Store }

func (recorder integrationRecorder) SaveFailure(sample solve.FailureSample) error {
	_, err := recorder.store.SaveFailure(sample.Background, sample.Shadow, artifact.Metrics{
		Reason: sample.Reason, Stage: sample.Stage, Confidence: sample.Confidence,
		XPos: sample.XPos, SlidePos: sample.SlidePos, TimingsMS: sample.TimingsMS,
	})
	return err
}

type staticPEKeyResolver struct {
	mu                sync.Mutex
	profile           pe.RuntimeProfile
	err               error
	paths             []string
	prepareProfileIDs []string
	buildProfileIDs   []string
}

type completedDeviceSession struct{}

func (*completedDeviceSession) InitToken() (string, error) { return "fixture-device-token", nil }
func (*completedDeviceSession) PEDeviceConfig() (protocol.DeviceConfig, error) {
	return protocol.DeviceConfig{Key: solverFixtureKey, SessionID: "fixture-session"}, nil
}
func (*completedDeviceSession) TargetFirstTouchAgeMS() (int, error) { return 700, nil }
func (*completedDeviceSession) Complete(_ context.Context, _ string, interactions []device.InteractionEvent, _ int) (device.Result, error) {
	return device.Result{
		VerifyToken:           "fixture-verify-token",
		GetterArgumentCount:   1,
		InteractionEventCount: len(interactions),
		RequestActions:        []string{"Log1", "Log2", "Log3", "Log2"},
	}, nil
}
func (*completedDeviceSession) Close() {}

type completedTracelessDeviceSession struct {
	completedDeviceSession
	calls int
	input device.TracelessInput
}

type completedSlidingDeviceSession struct {
	completedDeviceSession
	calls int
	input device.SlidingInput
}

func (session *completedSlidingDeviceSession) SolveSliding(_ context.Context, input device.SlidingInput) (device.SlidingResult, error) {
	session.calls++
	session.input = input
	return device.SlidingResult{
		SecurityToken: "fixture-upload-security-token",
		VerifyCode:    "T001", VerifyResult: true, CertifyID: input.CertifyID,
		RequestActions: []string{"Log1", "Log2", "Log3", "InitCaptchaV3", "VerifyCaptchaV3"},
	}, nil
}

func (session *completedTracelessDeviceSession) SolveTraceless(_ context.Context, input device.TracelessInput) (device.TracelessResult, error) {
	session.calls++
	session.input = input
	return device.TracelessResult{
		SecurityToken: "fixture-traceless-security-token",
		VerifyCode:    "T001", VerifyResult: true, CertifyID: input.CertifyID,
		RequestActions: []string{"Log1", "Log2", "Log3", "InitCaptchaV3", "VerifyCaptchaV3"},
	}, nil
}

type profiledCompletedDeviceSession struct {
	completedDeviceSession
	profile device.Profile
}

func (session *profiledCompletedDeviceSession) DeviceProfile() device.Profile {
	return session.profile.Clone()
}

func (resolver *staticPEKeyResolver) Prepare(_ context.Context, _ http.RoundTripper, profile device.Profile, staticPath string) error {
	resolver.mu.Lock()
	resolver.paths = append(resolver.paths, staticPath)
	resolver.prepareProfileIDs = append(resolver.prepareProfileIDs, profile.ProfileID)
	resolver.mu.Unlock()
	return resolver.err
}

func (resolver *staticPEKeyResolver) Build(_ context.Context, _ http.RoundTripper, profile device.Profile, _ string, input pe.RuntimeInput) (pe.Result, error) {
	if resolver.err != nil {
		return pe.Result{}, resolver.err
	}
	resolver.mu.Lock()
	resolver.buildProfileIDs = append(resolver.buildProfileIDs, profile.ProfileID)
	resolver.mu.Unlock()
	if len(input.Track) < 3 || input.DeviceConfig.SessionID == "" {
		return pe.Result{}, errors.New("invalid fixture PE runtime input")
	}
	elapsed := input.FirstTouchAgeMS
	interactions := make([]pe.InteractionEvent, len(input.Track))
	for index, event := range input.Track {
		elapsed += event.DT
		interactions[index] = pe.InteractionEvent{
			Type: "mousemove", X: float64(94 + event.X), Y: float64(548 + event.Y),
			TimeStamp: float64(elapsed), IsTrusted: true,
		}
	}
	slidePos := input.Track[len(input.Track)-1].X
	trackStart := input.InitBeginTimeMS - int64(elapsed) - 750
	argument := pe.GetterArgument{
		ValueType: "string", Length: len(input.CertifyID), Value: input.CertifyID,
		EqualsCertifyID: true, EqualsSceneID: input.CertifyID == input.SceneID,
	}
	return pe.Result{
		Data: "fixture-native-data", TrackEventCount: len(input.Track),
		XPos: *input.ExpectedXPos, SlidePos: slidePos,
		TrackStartTimeMS: trackStart, VerifyTimeMS: trackStart + int64(elapsed),
		DeviceGetterPlans: []pe.GetterPlan{{
			Owner: "z_um", DerivedAtMS: float64(elapsed), ArgumentCount: 1,
			Arguments: []pe.GetterArgument{argument},
		}},
		InteractionEvents: interactions,
	}, nil
}

func TestSolverOfflineCompleteSuccess(t *testing.T) {
	transport := newSolverTransport(t, "gap")
	solver, artifactDirectory, _ := newIntegrationSolver(t, transport, 0.45)
	outcome, err := solver.Solve(context.Background(), solve.Request{SceneID: "1ug4aptr", Prefix: "fsgtmi"})
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.OK || !outcome.VerifyResult || outcome.VerifyCode != "T001" || outcome.SecurityToken == "" || outcome.CertifyID == "" {
		t.Fatalf("outcome=%+v", outcome)
	}
	for _, stage := range []string{"setup", "deviceSession", "init", "resolvePEKey", "downloadAssets", "vision", "buildVerifyData", "completeDevice", "verify", "clientCleanup", "total"} {
		if _, ok := outcome.TimingsMS[stage]; !ok {
			t.Errorf("missing timing %q", stage)
		}
	}
	transport.mu.Lock()
	initCount, verifyCount := transport.initCount, transport.verifyCount
	actions := append([]string(nil), transport.deviceActions...)
	transport.mu.Unlock()
	if initCount != 1 || verifyCount != 1 || strings.Join(actions, ",") != "Log1,Log2,Log3,Log2" {
		t.Fatalf("init=%d verify=%d actions=%v", initCount, verifyCount, actions)
	}
	if entries, err := os.ReadDir(artifactDirectory); !errors.Is(err, os.ErrNotExist) && (err != nil || len(entries) != 0) {
		t.Fatalf("success wrote artifacts: entries=%v err=%v", entries, err)
	}
}

func TestSolverColdDeviceFlowRunsEveryRound(t *testing.T) {
	transport := newSolverTransport(t, "gap")
	solver, _, _ := newIntegrationSolver(t, transport, 0.45)
	for round := range 2 {
		outcome, err := solver.Solve(context.Background(), solve.Request{
			SceneID: fmt.Sprintf("cold-round-%d", round), Prefix: "fsgtmi",
		})
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if !outcome.OK || outcome.VerifyCode != "T001" || !outcome.VerifyResult {
			t.Fatalf("round %d outcome=%+v", round, outcome)
		}
	}

	transport.mu.Lock()
	initCount, verifyCount := transport.initCount, transport.verifyCount
	actions := append([]string(nil), transport.deviceActions...)
	transport.mu.Unlock()
	if initCount != 2 || verifyCount != 2 || strings.Join(actions, ",") != "Log1,Log2,Log3,Log2,Log1,Log2,Log3,Log2" {
		t.Fatalf("init=%d verify=%d actions=%v", initCount, verifyCount, actions)
	}
}

func TestSolverRoutesImageLessTracelessThroughDeviceSession(t *testing.T) {
	transport := newSolverTransport(t, "gap")
	transport.captchaType = "TRACELESS"
	resolver := &staticPEKeyResolver{profile: pe.RuntimeProfile{
		ArgumentKey: "0kd8i0mclivjow32", IncludeScreenInfo: true,
	}}
	solver, artifactDirectory, factory := newIntegrationSolverWithPEKeys(t, transport, 0.45, 5*time.Second, resolver)
	deviceSession := &completedTracelessDeviceSession{}
	factory.options.OpenDevice = func(context.Context, http.RoundTripper, device.Profile, solve.Request) (DeviceSession, func(), error) {
		return deviceSession, func() {}, nil
	}
	outcome, err := solver.Solve(context.Background(), solve.Request{SceneID: "wa3238du", Prefix: "1ohgtl"})
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.OK || !outcome.VerifyResult || outcome.VerifyCode != "T001" || outcome.SecurityToken == "" || outcome.CertifyID == "" {
		t.Fatalf("outcome=%+v", outcome)
	}
	if deviceSession.calls != 1 || deviceSession.input.SceneID != "wa3238du" || deviceSession.input.CertifyID != outcome.CertifyID || !strings.EqualFold(deviceSession.input.CaptchaType, "TRACELESS") {
		t.Fatalf("traceless calls=%d input=%+v", deviceSession.calls, deviceSession.input)
	}
	transport.mu.Lock()
	initCount, verifyCount := transport.initCount, transport.verifyCount
	assetRequests := len(transport.assetUserAgents)
	transport.mu.Unlock()
	resolver.mu.Lock()
	prepareCount, buildCount := len(resolver.paths), len(resolver.buildProfileIDs)
	resolver.mu.Unlock()
	if initCount != 1 || verifyCount != 0 || assetRequests != 0 || prepareCount != 0 || buildCount != 0 {
		t.Fatalf("init=%d rpcVerify=%d assets=%d prepare=%d build=%d", initCount, verifyCount, assetRequests, prepareCount, buildCount)
	}
	for _, stage := range []string{"setup", "deviceSession", "init", "traceless", "clientCleanup", "total"} {
		if _, ok := outcome.TimingsMS[stage]; !ok {
			t.Errorf("missing timing %q", stage)
		}
	}
	if entries, err := os.ReadDir(artifactDirectory); !errors.Is(err, os.ErrNotExist) && (err != nil || len(entries) != 0) {
		t.Fatalf("success wrote artifacts: entries=%v err=%v", entries, err)
	}
}

func TestSolverRoutesImageLessSlidingThroughDeviceSession(t *testing.T) {
	transport := newSolverTransport(t, "gap")
	transport.captchaType = "SLIDING"
	resolver := &staticPEKeyResolver{profile: pe.RuntimeProfile{
		ArgumentKey: "0kd8i0mclivjow32", IncludeScreenInfo: true,
	}}
	solver, artifactDirectory, factory := newIntegrationSolverWithPEKeys(t, transport, 0.45, 5*time.Second, resolver)
	deviceSession := &completedSlidingDeviceSession{}
	factory.options.OpenDevice = func(context.Context, http.RoundTripper, device.Profile, solve.Request) (DeviceSession, func(), error) {
		return deviceSession, func() {}, nil
	}
	outcome, err := solver.Solve(context.Background(), solve.Request{SceneID: "159tlu75", Prefix: "1ulc59"})
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.OK || !outcome.VerifyResult || outcome.VerifyCode != "T001" || outcome.SecurityToken != "fixture-upload-security-token" || outcome.CertifyID == "" {
		t.Fatalf("outcome=%+v", outcome)
	}
	input := deviceSession.input
	if deviceSession.calls != 1 || input.SceneID != "159tlu75" || input.CertifyID != outcome.CertifyID || !strings.EqualFold(input.CaptchaType, "SLIDING") || input.SlideWidth != 418 || input.HandleWidth != 48 || len(input.Track) < 3 || input.Track[len(input.Track)-1].X != 370 {
		t.Fatalf("sliding calls=%d input=%+v", deviceSession.calls, input)
	}
	transport.mu.Lock()
	initCount, verifyCount := transport.initCount, transport.verifyCount
	assetRequests := len(transport.assetUserAgents)
	transport.mu.Unlock()
	resolver.mu.Lock()
	prepareCount, buildCount := len(resolver.paths), len(resolver.buildProfileIDs)
	resolver.mu.Unlock()
	if initCount != 1 || verifyCount != 0 || assetRequests != 0 || prepareCount != 0 || buildCount != 0 {
		t.Fatalf("init=%d rpcVerify=%d assets=%d prepare=%d build=%d", initCount, verifyCount, assetRequests, prepareCount, buildCount)
	}
	for _, stage := range []string{"setup", "deviceSession", "init", "sliding", "clientCleanup", "total"} {
		if _, ok := outcome.TimingsMS[stage]; !ok {
			t.Errorf("missing timing %q", stage)
		}
	}
	if entries, err := os.ReadDir(artifactDirectory); !errors.Is(err, os.ErrNotExist) && (err != nil || len(entries) != 0) {
		t.Fatalf("success wrote artifacts: entries=%v err=%v", entries, err)
	}
}

func TestSolverReleasesCompletedDeviceBeforeVerifyOnce(t *testing.T) {
	transport := newSolverTransport(t, "gap")
	var releases atomic.Int32
	transport.beforeVerify = func() error {
		if got := releases.Load(); got != 1 {
			return fmt.Errorf("release count at Verify=%d, want 1", got)
		}
		return nil
	}
	solver, _, factory := newIntegrationSolver(t, transport, 0.45)
	factory.options.OpenDevice = func(context.Context, http.RoundTripper, device.Profile, solve.Request) (DeviceSession, func(), error) {
		return &completedDeviceSession{}, func() {
			time.Sleep(20 * time.Millisecond)
			releases.Add(1)
		}, nil
	}
	outcome, err := solver.Solve(context.Background(), solve.Request{SceneID: "scene", Prefix: "prefix1"})
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.OK || releases.Load() != 1 || outcome.TimingsMS["clientCleanup"] < 15 {
		t.Fatalf("outcome=%+v releases=%d clientCleanup=%d", outcome, releases.Load(), outcome.TimingsMS["clientCleanup"])
	}
}

func TestSolverUsesOpenedDeviceProfileForWholeRound(t *testing.T) {
	transport := newSolverTransport(t, "gap")
	resolver := &staticPEKeyResolver{profile: pe.RuntimeProfile{
		ArgumentKey: "0kd8i0mclivjow32", IncludeScreenInfo: true,
	}}
	solver, _, factory := newIntegrationSolverWithPEKeys(t, transport, 0.45, 5*time.Second, resolver)
	profile, err := device.GenerateProfile(&byteEntropy{next: 9})
	if err != nil {
		t.Fatal(err)
	}
	profile.ProfileID = "opened-profile"
	profile.UserAgent = "opened-device-user-agent"
	// 初始占位画像不满足纯 Go 客户端要求；动态会话必须使用 Open 后的实际画像。
	factory.options.FixedProfile = &device.Profile{}
	factory.options.OpenDevice = func(context.Context, http.RoundTripper, device.Profile, solve.Request) (DeviceSession, func(), error) {
		return &profiledCompletedDeviceSession{profile: profile}, func() {}, nil
	}
	outcome, err := solver.Solve(context.Background(), solve.Request{SceneID: "scene", Prefix: "prefix1"})
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.OK {
		t.Fatalf("outcome=%+v", outcome)
	}
	transport.mu.Lock()
	initUserAgent, verifyUserAgent := transport.initUserAgent, transport.verifyUserAgent
	assetUserAgents := append([]string(nil), transport.assetUserAgents...)
	transport.mu.Unlock()
	if initUserAgent != profile.UserAgent || verifyUserAgent != profile.UserAgent || len(assetUserAgents) != 2 {
		t.Fatalf("HTTP profile mismatch: init=%q verify=%q assets=%v", initUserAgent, verifyUserAgent, assetUserAgents)
	}
	for _, userAgent := range assetUserAgents {
		if userAgent != profile.UserAgent {
			t.Fatalf("asset used profile %q, want %q", userAgent, profile.UserAgent)
		}
	}
	resolver.mu.Lock()
	prepareIDs := append([]string(nil), resolver.prepareProfileIDs...)
	buildIDs := append([]string(nil), resolver.buildProfileIDs...)
	resolver.mu.Unlock()
	if strings.Join(prepareIDs, ",") != profile.ProfileID || strings.Join(buildIDs, ",") != profile.ProfileID {
		t.Fatalf("PE profiles: prepare=%v build=%v want=%q", prepareIDs, buildIDs, profile.ProfileID)
	}
}

func TestSolverPreparesExactDynamicPESources(t *testing.T) {
	transport := newSolverTransport(t, "gap")
	transport.staticPath = "3.29.0/pe.058.77d5c01b1737016e.js"
	resolver := &staticPEKeyResolver{profile: pe.RuntimeProfile{ArgumentKey: "dmmlums5zuewlgt7"}}
	solver, _, _ := newIntegrationSolverWithPEKeys(t, transport, 0.45, 5*time.Second, resolver)
	outcome, err := solver.Solve(context.Background(), solve.Request{SceneID: "scene", Prefix: "prefix1"})
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.OK || outcome.VerifyCode != "T001" {
		t.Fatalf("outcome=%+v", outcome)
	}
	resolver.mu.Lock()
	paths := append([]string(nil), resolver.paths...)
	resolver.mu.Unlock()
	if len(paths) != 1 || paths[0] != transport.staticPath {
		t.Fatalf("prepared paths=%v", paths)
	}
}

func TestSolverClassifiesPEPrepareFailuresBeforeVerify(t *testing.T) {
	tests := []struct {
		name string
		err  error
		kind solve.FailureKind
	}{
		{name: "network", err: pe.ErrKeyNetwork, kind: solve.FailureNetwork},
		{name: "runtime", err: pe.ErrKeyRuntime, kind: solve.FailureInternal},
		{name: "unsupported", err: pe.ErrUnsupportedPE, kind: solve.FailureProtocol},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transport := newSolverTransport(t, "gap")
			transport.staticPath = "3.29.0/pe.058.77d5c01b1737016e.js"
			resolver := &staticPEKeyResolver{err: test.err}
			solver, _, _ := newIntegrationSolverWithPEKeys(t, transport, 0.45, 5*time.Second, resolver)
			_, err := solver.Solve(context.Background(), solve.Request{SceneID: "scene", Prefix: "prefix1"})
			var failure *solve.Failure
			if !errors.As(err, &failure) || failure.Kind != test.kind || failure.Stage != "resolvePEKey" {
				t.Fatalf("error=%v", err)
			}
			transport.mu.Lock()
			verifyCount := transport.verifyCount
			actions := append([]string(nil), transport.deviceActions...)
			transport.mu.Unlock()
			if verifyCount != 0 || strings.Join(actions, ",") != "Log1,Log2,Log3" {
				t.Fatalf("verify=%d actions=%v", verifyCount, actions)
			}
		})
	}
}

func TestSolverBusinessFailureReturnsOutcomeAndArtifacts(t *testing.T) {
	transport := newSolverTransport(t, "gap")
	transport.verifySuccess = false
	solver, artifactDirectory, _ := newIntegrationSolver(t, transport, 0.45)
	outcome, err := solver.Solve(context.Background(), solve.Request{SceneID: "scene", Prefix: "prefix1"})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.OK || outcome.VerifyResult || outcome.VerifyCode != "F015" || outcome.SecurityToken != "" {
		t.Fatalf("outcome=%+v", outcome)
	}
	assertArtifactSet(t, artifactDirectory, "verify-rejected")
}

func TestSolverLowConfidenceStopsBeforeVerify(t *testing.T) {
	transport := newSolverTransport(t, "no-gap")
	solver, artifactDirectory, _ := newIntegrationSolver(t, transport, 0.45)
	_, err := solver.Solve(context.Background(), solve.Request{SceneID: "scene", Prefix: "prefix1"})
	var failure *solve.Failure
	if !errors.As(err, &failure) || failure.Kind != solve.FailureVision || failure.Stage != "vision" {
		t.Fatalf("error=%v", err)
	}
	transport.mu.Lock()
	verifyCount := transport.verifyCount
	actions := append([]string(nil), transport.deviceActions...)
	transport.mu.Unlock()
	if verifyCount != 0 || strings.Join(actions, ",") != "Log1,Log2,Log3" {
		t.Fatalf("verify=%d actions=%v", verifyCount, actions)
	}
	assertArtifactSet(t, artifactDirectory, "low-confidence")
}

func TestSolverVerifyNetworkErrorIsSingleAttemptAndSanitized(t *testing.T) {
	transport := newSolverTransport(t, "gap")
	transport.verifyError = errors.New("proxy password=do-not-leak")
	solver, artifactDirectory, _ := newIntegrationSolver(t, transport, 0.45)
	_, err := solver.Solve(context.Background(), solve.Request{SceneID: "scene", Prefix: "prefix1"})
	var failure *solve.Failure
	if !errors.As(err, &failure) || failure.Kind != solve.FailureNetwork || failure.Stage != "verify" || strings.Contains(err.Error(), "do-not-leak") {
		t.Fatalf("error=%v", err)
	}
	transport.mu.Lock()
	verifyCount := transport.verifyCount
	transport.mu.Unlock()
	if verifyCount != 1 {
		t.Fatalf("Verify attempts=%d", verifyCount)
	}
	assertArtifactSet(t, artifactDirectory, string(solve.FailureNetwork))
}

func TestSolverSeparatesProtocolFailuresFromNetworkFailures(t *testing.T) {
	tests := []struct {
		name  string
		stage string
		set   func(*solverTransport)
	}{
		{name: "device HTTP 200 malformed", stage: "deviceSession", set: func(value *solverTransport) { value.deviceMalformed = true }},
		{name: "Init HTTP 200 malformed", stage: "init", set: func(value *solverTransport) { value.initMalformed = true }},
		{name: "Verify HTTP 200 malformed", stage: "verify", set: func(value *solverTransport) { value.verifyMalformed = true }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transport := newSolverTransport(t, "gap")
			test.set(transport)
			solver, _, _ := newIntegrationSolver(t, transport, 0.45)
			_, err := solver.Solve(context.Background(), solve.Request{SceneID: "scene", Prefix: "prefix1"})
			var failure *solve.Failure
			if !errors.As(err, &failure) || failure.Kind != solve.FailureProtocol || failure.Stage != test.stage {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestSolverRejectsInvalidAndCancelledRequestsWithoutNetwork(t *testing.T) {
	transport := newSolverTransport(t, "gap")
	solver, _, _ := newIntegrationSolver(t, transport, 0.45)
	if _, err := solver.Solve(context.Background(), solve.Request{SceneID: "scene", Prefix: "bad-prefix"}); err == nil {
		t.Fatal("invalid prefix accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := solver.Solve(ctx, solve.Request{SceneID: "scene", Prefix: "prefix1"}); err == nil {
		t.Fatal("cancelled request accepted")
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if transport.initCount != 0 || len(transport.deviceActions) != 0 {
		t.Fatalf("invalid requests reached network: init=%d actions=%v", transport.initCount, transport.deviceActions)
	}
}

func TestSolverMapsTransportCapacityAsInternalResourceFailure(t *testing.T) {
	transport := newSolverTransport(t, "gap")
	solver, _, factory := newIntegrationSolver(t, transport, 0.45)
	factory.options.GetTransport = func(string) (http.RoundTripper, bool, error) {
		return nil, false, httpclient.ErrTransportRouteCapacity
	}

	_, err := solver.Solve(context.Background(), solve.Request{SceneID: "scene", Prefix: "prefix1", Proxy: "http://proxy.invalid"})
	var failure *solve.Failure
	if !errors.As(err, &failure) || failure.Kind != solve.FailureInternal || failure.Stage != "setup" {
		t.Fatalf("error=%v", err)
	}
}

func assertArtifactSet(t *testing.T, directory, reason string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("artifact count=%d, want 3", len(entries))
	}
	var metrics []byte
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), "-metrics.json") {
			metrics, err = os.ReadFile(filepath.Join(directory, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	text := string(metrics)
	if !strings.Contains(text, `"reason":"`+reason+`"`) {
		t.Fatalf("metrics reason mismatch: %s", text)
	}
	for _, forbidden := range []string{"fixture-security-token", "fixture-certify", "do-not-leak"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("artifact leaked %q: %s", forbidden, text)
		}
	}
}
