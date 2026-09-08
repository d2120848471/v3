package aliyun

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/d2120848471/v3/ali-slider-go/internal/application/solve"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/pe"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/protocol"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/track"
)

type buildResolverFunc func(context.Context, http.RoundTripper, device.Profile, string, pe.RuntimeInput) (pe.Result, error)

func (function buildResolverFunc) Prepare(context.Context, http.RoundTripper, device.Profile, string) error {
	return nil
}
func (function buildResolverFunc) Build(ctx context.Context, transport http.RoundTripper, profile device.Profile, path string, input pe.RuntimeInput) (pe.Result, error) {
	return function(ctx, transport, profile, path, input)
}

type contractDeviceSession struct {
	completedDeviceSession
	ageErr, configErr, profileErr, sdkErr, completeErr error
	completeResult                                     *device.Result
	completePlan                                       solve.CompletionPlan
	closed                                             int
}

func (session *contractDeviceSession) TargetFirstTouchAgeMS() (int, error) {
	return 712, session.ageErr
}
func (session *contractDeviceSession) PEDeviceConfig() (protocol.DeviceConfig, error) {
	config, _ := session.completedDeviceSession.PEDeviceConfig()
	return config, session.configErr
}
func (session *contractDeviceSession) PEVerifyArgProfile() (string, string, error) {
	return "fixture-access", "fixture-salt", session.profileErr
}
func (session *contractDeviceSession) PESDKSource() ([]byte, error) {
	return []byte("current-device-sdk"), session.sdkErr
}
func (session *contractDeviceSession) Complete(ctx context.Context, argument string, interactions []device.InteractionEvent, delay int) (device.Result, error) {
	session.completePlan = solve.CompletionPlan{GetterArgument: argument, Interactions: interactions, PostDelayMS: delay}
	if session.completeErr != nil {
		return device.Result{}, session.completeErr
	}
	if session.completeResult != nil {
		return *session.completeResult, nil
	}
	return session.completedDeviceSession.Complete(ctx, argument, interactions, delay)
}
func (session *contractDeviceSession) Close() { session.closed++ }

func runtimeResult() pe.Result {
	return pe.Result{SlidePos: 123, Data: "fixture-proof", VerifyTimeMS: 991,
		DeviceGetterPlans:      []pe.GetterPlan{{ArgumentCount: 1, Arguments: []pe.GetterArgument{{ValueType: "string", Value: "current-certify", EqualsCertifyID: true}}}},
		InteractionEvents:      []pe.InteractionEvent{{Type: "mousemove", X: 12.5, Y: 21, TimeStamp: 877.1, IsTrusted: true}},
		PostInteractionDelayMS: 3.5,
	}
}
func buildRound(session DeviceSession, resolver PEKeyResolver) *round {
	return &round{
		session: session, options: FactoryOptions{PEKeys: resolver},
		request:   solve.Request{SceneID: "current-scene", Prefix: "currentprefix"},
		initToken: "current-device-token", profile: device.Profile{ProfileID: "current-profile"},
		challenge: CaptchaChallenge{CertifyID: "current-certify", CaptchaType: "slider", StaticPath: "current/pe.js", ImagePath: "current/back.png", PuzzleImagePath: "current/shadow.png", InitStartedMS: 987},
	}
}

func TestRoundBuildBindsChallengeProfileConfigSDKAndCompletionFields(t *testing.T) {
	session := &contractDeviceSession{}
	input := solve.PuzzleInput{XPos: 145, SlidePos: 123, Track: []track.Event{{Type: "touchstart", X: 0}, {Type: "touchmove", X: 123}}, ImageWidth: 600, ImageHeight: 400, PuzzleWidth: 100, PuzzleHeight: 80, RenderedWidth: 300, HandleWidth: 40}
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("Build made an unexpected HTTP request")
		return nil, nil
	})
	calls := 0
	round := buildRound(session, buildResolverFunc(func(_ context.Context, gotTransport http.RoundTripper, profile device.Profile, path string, got pe.RuntimeInput) (pe.Result, error) {
		calls++
		if gotTransport == nil || profile.ProfileID != "current-profile" || path != "current/pe.js" {
			t.Fatalf("transport=%T profile=%s path=%s", gotTransport, profile.ProfileID, path)
		}
		if got.SceneID != "current-scene" || got.CertifyID != "current-certify" || got.DeviceToken != "current-device-token" || got.CaptchaType != "slider" || got.Image != "current/back.png" || got.PuzzleImage != "current/shadow.png" || got.DeviceConfig.Key != solverFixtureKey || got.DeviceConfig.SessionID != "fixture-session" || got.VerifyAccessSec != "fixture-access" || got.VerifySalt != "fixture-salt" || string(got.SDKSource) != "current-device-sdk" || got.InitBeginTimeMS != 987 || got.FirstTouchAgeMS != 712 || got.ExpectedXPos == nil || *got.ExpectedXPos != 145 {
			t.Fatal("runtime input lost current-round device or challenge fields")
		}
		expectedDimensions := pe.RuntimeDimensions{ImageWidth: 600, ImageHeight: 400, PuzzleWidth: 100, PuzzleHeight: 80, RenderedWidth: 300, HandleWidth: 40}
		if got.Dimensions != expectedDimensions || !reflect.DeepEqual(got.Track, input.Track) {
			t.Fatalf("dimensions=%+v track=%+v", got.Dimensions, got.Track)
		}
		return runtimeResult(), nil
	}))
	round.transport = transport
	proof, err := round.BuildPuzzle(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || proof.SlidePos != 123 || proof.Data != "fixture-proof" || proof.VerifyTimeMS != 991 || proof.Completion.GetterArgument != "current-certify" || proof.Completion.PostDelayMS != 4 || !reflect.DeepEqual(proof.Completion.Interactions, []device.InteractionEvent{{Type: "mousemove", X: 12.5, Y: 21, TimeStamp: 877.1, IsTrusted: true}}) {
		t.Fatalf("calls=%d proof=%+v", calls, proof)
	}
	token, err := round.CompleteDevice(context.Background(), proof.Completion)
	if err != nil || token != "fixture-verify-token" || !reflect.DeepEqual(session.completePlan, proof.Completion) {
		t.Fatalf("token=%q err=%v completion=%+v", token, err, session.completePlan)
	}
}

func TestRoundRejectsEveryInvalidGetterPlanBeforeComplete(t *testing.T) {
	tests := []struct {
		name, message string
		change        func(*pe.Result)
	}{
		{"missing-plan", "PE 输出合同不一致", func(r *pe.Result) { r.DeviceGetterPlans = nil }},
		{"extra-plan", "PE 输出合同不一致", func(r *pe.Result) { r.DeviceGetterPlans = append(r.DeviceGetterPlans, r.DeviceGetterPlans[0]) }},
		{"argument-count", "设备 getter 合同不一致", func(r *pe.Result) { r.DeviceGetterPlans[0].ArgumentCount = 2 }},
		{"missing-argument", "设备 getter 合同不一致", func(r *pe.Result) { r.DeviceGetterPlans[0].Arguments = nil }},
		{"extra-argument", "设备 getter 合同不一致", func(r *pe.Result) { p := &r.DeviceGetterPlans[0]; p.Arguments = append(p.Arguments, p.Arguments[0]) }},
		{"argument-type", "设备 getter 合同不一致", func(r *pe.Result) { r.DeviceGetterPlans[0].Arguments[0].ValueType = "number" }},
		{"empty-argument", "设备 getter 合同不一致", func(r *pe.Result) { r.DeviceGetterPlans[0].Arguments[0].Value = "" }},
		{"wrong-certify", "设备 getter 合同不一致", func(r *pe.Result) { r.DeviceGetterPlans[0].Arguments[0].EqualsCertifyID = false }},
		{"slide-before-getter", "PE 输出合同不一致", func(r *pe.Result) { r.SlidePos++; r.DeviceGetterPlans[0].Arguments = nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session := &contractDeviceSession{}
			round := buildRound(session, buildResolverFunc(func(context.Context, http.RoundTripper, device.Profile, string, pe.RuntimeInput) (pe.Result, error) {
				result := runtimeResult()
				test.change(&result)
				return result, nil
			}))
			_, err := round.BuildPuzzle(context.Background(), solve.PuzzleInput{SlidePos: 123})
			var dependency *solve.DependencyError
			if !errors.As(err, &dependency) || dependency.Kind != solve.FailureProtocol || dependency.Message != test.message {
				t.Fatalf("error=%v", err)
			}
			if session.completePlan.GetterArgument != "" {
				t.Fatal("invalid getter reached device Complete")
			}
		})
	}
}

func TestRoundRejectsInvalidDeviceCompletionContracts(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*device.Result)
	}{
		{"token", func(r *device.Result) { r.VerifyToken = "" }},
		{"getter-count", func(r *device.Result) { r.GetterArgumentCount = 2 }},
		{"interaction-count", func(r *device.Result) { r.InteractionEventCount = 0 }},
		{"actions", func(r *device.Result) { r.RequestActions = []string{"Log1", "Log2", "Log3"} }},
		{"action-order", func(r *device.Result) { r.RequestActions = []string{"Log1", "Log3", "Log2", "Log2"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := device.Result{VerifyToken: "verify-token", GetterArgumentCount: 1, InteractionEventCount: 1, RequestActions: []string{"Log1", "Log2", "Log3", "Log2"}}
			test.change(&result)
			round := buildRound(&contractDeviceSession{completeResult: &result}, nil)
			_, err := round.CompleteDevice(context.Background(), solve.CompletionPlan{GetterArgument: "current-certify", Interactions: []device.InteractionEvent{{Type: "mousemove"}}})
			var dependency *solve.DependencyError
			if !errors.As(err, &dependency) || dependency.Kind != solve.FailureProtocol || dependency.Message != "设备完成态合同不一致" {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestRoundBuildProviderFailuresPreserveErrorChainAndStopResolver(t *testing.T) {
	for _, provider := range []string{"age", "config", "profile", "sdk"} {
		t.Run(provider, func(t *testing.T) {
			session := &contractDeviceSession{}
			switch provider {
			case "age":
				session.ageErr = pe.ErrKeyRuntime
			case "config":
				session.configErr = pe.ErrKeyRuntime
			case "profile":
				session.profileErr = pe.ErrKeyRuntime
			case "sdk":
				session.sdkErr = pe.ErrKeyRuntime
			}
			round := buildRound(session, buildResolverFunc(func(context.Context, http.RoundTripper, device.Profile, string, pe.RuntimeInput) (pe.Result, error) {
				t.Fatal("resolver called after provider error")
				return pe.Result{}, nil
			}))
			_, err := round.BuildPuzzle(context.Background(), solve.PuzzleInput{})
			var dependency *solve.DependencyError
			if !errors.As(err, &dependency) || dependency.Kind != solve.FailureInternal || !errors.Is(err, pe.ErrKeyRuntime) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestRoundClosesPartialSessionExactlyOnce(t *testing.T) {
	transport := newSolverTransport(t, "gap")
	solver, _, factory := newIntegrationSolver(t, transport, 0.45)
	session := &contractDeviceSession{}
	factory.options.OpenDevice = func(context.Context, http.RoundTripper, device.Profile, solve.Request) (DeviceSession, func(), error) {
		return session, nil, nil
	}
	_, err := solver.Solve(context.Background(), solve.Request{SceneID: "scene", Prefix: "prefix1"})
	var failure *solve.Failure
	if !errors.As(err, &failure) || failure.Kind != solve.FailureProtocol || failure.Stage != "deviceSession" || session.closed != 1 {
		t.Fatalf("error=%v closed=%d", err, session.closed)
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if transport.initCount != 0 || transport.verifyCount != 0 || len(transport.deviceActions) != 0 {
		t.Fatal("incomplete session reached network")
	}
}

func TestRoundSDKTypesRequireSessionCapability(t *testing.T) {
	for _, kind := range []string{"TRACELESS", "SLIDING"} {
		t.Run(kind, func(t *testing.T) {
			transport := newSolverTransport(t, "gap")
			transport.captchaType = kind
			solver, _, factory := newIntegrationSolver(t, transport, 0.45)
			closed := 0
			factory.options.OpenDevice = func(context.Context, http.RoundTripper, device.Profile, solve.Request) (DeviceSession, func(), error) {
				return &completedDeviceSession{}, func() { closed++ }, nil
			}
			_, err := solver.Solve(context.Background(), solve.Request{SceneID: "scene", Prefix: "prefix1"})
			var failure *solve.Failure
			expectedStage, expectedMessage := "traceless", "设备会话不支持无痕验证码"
			if kind == "SLIDING" {
				expectedStage, expectedMessage = "sliding", "设备会话不支持拖动验证码"
			}
			if !errors.As(err, &failure) || failure.Kind != solve.FailureProtocol || failure.Stage != expectedStage || failure.Message != expectedMessage || closed != 1 {
				t.Fatalf("err=%v close=%d", err, closed)
			}
			transport.mu.Lock()
			defer transport.mu.Unlock()
			if transport.initCount != 1 || transport.verifyCount != 0 || len(transport.assetUserAgents) != 0 {
				t.Fatal("SDK branch reached puzzle or second Verify")
			}
		})
	}
}
