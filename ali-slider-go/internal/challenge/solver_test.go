package challenge

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
	"testing"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/artifact"
	"github.com/d2120848471/v3/ali-slider-go/internal/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/pe"
	"github.com/d2120848471/v3/ali-slider-go/internal/protocol"
	"github.com/d2120848471/v3/ali-slider-go/internal/runtimekit"
)

const solverFixtureKey = "0123456789abcdef"

func TestChallengeTypesRedactFormatting(t *testing.T) {
	values := []any{
		SolveRequest{SceneID: "scene-secret", RPCKeyID: "rpc-secret", Proxy: "http://user:password@proxy.invalid"},
		SolveOutcome{SecurityToken: "token-secret", CertifyID: "certify-secret"},
		CaptchaChallenge{CertifyID: "certify-secret", ImagePath: "asset-secret"},
		VerifyResult{SecurityToken: "token-secret", CertifyID: "certify-secret"},
		DevicePoolKey{Route: "http://user:password@proxy.invalid"},
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
}

func newSolverTransport(t *testing.T, fixture string) *solverTransport {
	t.Helper()
	secrets, err := protocol.ResolveFrontendSecrets()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join("..", "vision", "testdata", "python-edge-decoy", fixture)
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
		identifier := fmt.Sprintf("fixture-certify-%04d", transport.initCount)
		transport.mu.Unlock()
		if transport.initMalformed {
			return solverResponse(request, http.StatusOK, []byte(`{"Success":`)), nil
		}
		staticPath := transport.staticPath
		if staticPath == "" {
			staticPath = "3.29.0/pe.091.00665af58b020d81.js"
		}
		return solverJSONResponse(request, map[string]any{
			"Success": true, "Code": "Success", "CertifyId": identifier,
			"Image": "fixtures/back.png", "PuzzleImage": "fixtures/shadow.png",
			"StaticPath": staticPath, "CaptchaType": "slider",
		})
	case "VerifyCaptchaV3":
		transport.mu.Lock()
		transport.verifyCount++
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

func newIntegrationSolver(t *testing.T, transport *solverTransport, minimumConfidence float64) (*Solver, string) {
	return newIntegrationSolverWithTimeout(t, transport, minimumConfidence, 5*time.Second)
}

func newIntegrationSolverWithTimeout(t *testing.T, transport *solverTransport, minimumConfidence float64, timeout time.Duration) (*Solver, string) {
	return newIntegrationSolverWithPEKeys(t, transport, minimumConfidence, timeout, nil)
}

func newIntegrationSolverWithPEKeys(t *testing.T, transport *solverTransport, minimumConfidence float64, timeout time.Duration, peKeys PEKeyResolver) (*Solver, string) {
	t.Helper()
	if peKeys == nil {
		peKeys = &staticPEKeyResolver{profile: pe.RuntimeProfile{
			ArgumentKey: "0kd8i0mclivjow32", IncludeScreenInfo: true,
		}}
	}
	artifactDirectory := filepath.Join(t.TempDir(), "artifacts")
	sources := runtimekit.Sources{Clock: transport.clock, Entropy: &byteEntropy{}}
	store := &artifact.Store{Directory: artifactDirectory, Retention: 7 * 24 * time.Hour, Entropy: sources.Entropy, Now: transport.clock.Now}
	solver, err := NewSolver(SolverOptions{
		Timeout: timeout, MinimumConfidence: minimumConfidence,
		GatherCostMin: 180, GatherCostMax: 260, FirstTouchAgeMin: 650, FirstTouchAgeMax: 850,
		AssetMaxBytes: 8 << 20, AssetMaxDimension: 16_384, AssetMaxPixels: 16 << 20,
		Sources: sources, Artifacts: store, PEKeys: peKeys,
		GetTransport: func(string) (http.RoundTripper, bool, error) { return transport, false, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return solver, artifactDirectory
}

type staticPEKeyResolver struct {
	mu      sync.Mutex
	profile pe.RuntimeProfile
	err     error
	paths   []string
}

func (resolver *staticPEKeyResolver) Resolve(_ context.Context, _ http.RoundTripper, _ device.Profile, staticPath string) (pe.RuntimeProfile, error) {
	resolver.mu.Lock()
	resolver.paths = append(resolver.paths, staticPath)
	resolver.mu.Unlock()
	return resolver.profile, resolver.err
}

func (resolver *staticPEKeyResolver) Build(_ context.Context, _ http.RoundTripper, _ device.Profile, _ string, input pe.RuntimeInput) (pe.Result, error) {
	if resolver.err != nil {
		return pe.Result{}, resolver.err
	}
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
	solver, artifactDirectory := newIntegrationSolver(t, transport, 0.45)
	outcome, err := solver.Solve(context.Background(), SolveRequest{SceneID: "1ug4aptr", Prefix: "fsgtmi"})
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

func TestSolverUsesResolvedDynamicPEKey(t *testing.T) {
	transport := newSolverTransport(t, "gap")
	transport.staticPath = "3.29.0/pe.058.77d5c01b1737016e.js"
	resolver := &staticPEKeyResolver{profile: pe.RuntimeProfile{ArgumentKey: "dmmlums5zuewlgt7"}}
	solver, _ := newIntegrationSolverWithPEKeys(t, transport, 0.45, 5*time.Second, resolver)
	outcome, err := solver.Solve(context.Background(), SolveRequest{SceneID: "scene", Prefix: "prefix1"})
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
		t.Fatalf("resolved paths=%v", paths)
	}
}

func TestSolverClassifiesPEKeyFailuresBeforeAssetsAndVerify(t *testing.T) {
	tests := []struct {
		name string
		err  error
		kind FailureKind
	}{
		{name: "network", err: pe.ErrKeyNetwork, kind: FailureNetwork},
		{name: "runtime", err: pe.ErrKeyRuntime, kind: FailureInternal},
		{name: "unsupported", err: pe.ErrUnsupportedPE, kind: FailureProtocol},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transport := newSolverTransport(t, "gap")
			transport.staticPath = "3.29.0/pe.058.77d5c01b1737016e.js"
			resolver := &staticPEKeyResolver{err: test.err}
			solver, _ := newIntegrationSolverWithPEKeys(t, transport, 0.45, 5*time.Second, resolver)
			_, err := solver.Solve(context.Background(), SolveRequest{SceneID: "scene", Prefix: "prefix1"})
			var failure *Failure
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
	solver, artifactDirectory := newIntegrationSolver(t, transport, 0.45)
	outcome, err := solver.Solve(context.Background(), SolveRequest{SceneID: "scene", Prefix: "prefix1"})
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
	solver, artifactDirectory := newIntegrationSolver(t, transport, 0.45)
	_, err := solver.Solve(context.Background(), SolveRequest{SceneID: "scene", Prefix: "prefix1"})
	var failure *Failure
	if !errors.As(err, &failure) || failure.Kind != FailureVision || failure.Stage != "vision" {
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
	solver, artifactDirectory := newIntegrationSolver(t, transport, 0.45)
	_, err := solver.Solve(context.Background(), SolveRequest{SceneID: "scene", Prefix: "prefix1"})
	var failure *Failure
	if !errors.As(err, &failure) || failure.Kind != FailureNetwork || failure.Stage != "verify" || strings.Contains(err.Error(), "do-not-leak") {
		t.Fatalf("error=%v", err)
	}
	transport.mu.Lock()
	verifyCount := transport.verifyCount
	transport.mu.Unlock()
	if verifyCount != 1 {
		t.Fatalf("Verify attempts=%d", verifyCount)
	}
	assertArtifactSet(t, artifactDirectory, string(FailureNetwork))
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
			solver, _ := newIntegrationSolver(t, transport, 0.45)
			_, err := solver.Solve(context.Background(), SolveRequest{SceneID: "scene", Prefix: "prefix1"})
			var failure *Failure
			if !errors.As(err, &failure) || failure.Kind != FailureProtocol || failure.Stage != test.stage {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestSolverRejectsInvalidAndCancelledRequestsWithoutNetwork(t *testing.T) {
	transport := newSolverTransport(t, "gap")
	solver, _ := newIntegrationSolver(t, transport, 0.45)
	if _, err := solver.Solve(context.Background(), SolveRequest{SceneID: "scene", Prefix: "bad-prefix"}); err == nil {
		t.Fatal("invalid prefix accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := solver.Solve(ctx, SolveRequest{SceneID: "scene", Prefix: "prefix1"}); err == nil {
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
	sources := runtimekit.Sources{Clock: transport.clock, Entropy: &byteEntropy{}}
	solver, err := NewSolver(SolverOptions{
		Timeout: 5 * time.Second, MinimumConfidence: 0.45,
		GatherCostMin: 180, GatherCostMax: 260, FirstTouchAgeMin: 650, FirstTouchAgeMax: 850,
		AssetMaxBytes: 8 << 20, AssetMaxDimension: 16_384, AssetMaxPixels: 16 << 20,
		Sources: sources, GetTransport: func(string) (http.RoundTripper, bool, error) {
			return nil, false, ErrTransportRouteCapacity
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = solver.Solve(context.Background(), SolveRequest{SceneID: "scene", Prefix: "prefix1", Proxy: "http://proxy.invalid"})
	var failure *Failure
	if !errors.As(err, &failure) || failure.Kind != FailureInternal || failure.Stage != "setup" {
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
