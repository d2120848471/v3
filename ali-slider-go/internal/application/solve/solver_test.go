package solve

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/domain/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/track"
	"github.com/d2120848471/v3/ali-slider-go/internal/foundation/runtimekit"
)

type factoryFunc func(context.Context, Request) (Round, error)

func (function factoryFunc) NewRound(ctx context.Context, request Request) (Round, error) {
	return function(ctx, request)
}

type recorderFunc func(FailureSample) error

func (function recorderFunc) SaveFailure(sample FailureSample) error { return function(sample) }

type fakeRound struct {
	mu                        sync.Mutex
	calls                     []string
	kind                      ChallengeType
	images                    Images
	open                      func(context.Context) error
	init                      func(context.Context) (Challenge, error)
	prepare                   func(context.Context) error
	download                  func(context.Context) (Images, error)
	build                     func(context.Context, PuzzleInput) (PuzzleProof, error)
	complete                  func(context.Context, CompletionPlan) (string, error)
	sdk                       func(context.Context) (Verification, error)
	verify                    func(context.Context, VerificationInput) (Verification, error)
	slidingTrack              []track.Event
	slidingWidth, handleWidth int
}

func (round *fakeRound) record(name string) {
	round.mu.Lock()
	defer round.mu.Unlock()
	round.calls = append(round.calls, name)
}
func (round *fakeRound) snapshot() []string {
	round.mu.Lock()
	defer round.mu.Unlock()
	return append([]string(nil), round.calls...)
}
func (round *fakeRound) Proxied() bool { return true }
func (round *fakeRound) OpenDevice(ctx context.Context) error {
	round.record("open")
	if round.open != nil {
		return round.open(ctx)
	}
	return nil
}
func (round *fakeRound) Init(ctx context.Context) (Challenge, error) {
	round.record("init")
	if round.init != nil {
		return round.init(ctx)
	}
	return Challenge{Type: round.kind, CertifyID: "certify-fixture"}, nil
}
func (round *fakeRound) PreparePuzzle(ctx context.Context) error {
	round.record("prepare")
	if round.prepare != nil {
		return round.prepare(ctx)
	}
	return nil
}
func (round *fakeRound) DownloadImages(ctx context.Context) (Images, error) {
	round.record("download")
	if round.download != nil {
		return round.download(ctx)
	}
	return round.images, nil
}
func (round *fakeRound) BuildPuzzle(ctx context.Context, input PuzzleInput) (PuzzleProof, error) {
	round.record("build")
	if round.build != nil {
		return round.build(ctx, input)
	}
	return validProof(input), nil
}
func validProof(input PuzzleInput) PuzzleProof {
	return PuzzleProof{SlidePos: input.SlidePos, Data: "proof", VerifyTimeMS: 42,
		Completion: CompletionPlan{GetterArgument: "certify-fixture", Interactions: []device.InteractionEvent{{Type: "mousemove", X: 12, Y: 4, TimeStamp: 55, IsTrusted: true}}, PostDelayMS: 7},
	}
}
func (round *fakeRound) CompleteDevice(ctx context.Context, plan CompletionPlan) (string, error) {
	round.record("complete")
	if round.complete != nil {
		return round.complete(ctx, plan)
	}
	return "verify-device-token", nil
}
func (round *fakeRound) SolveTraceless(ctx context.Context) (Verification, error) {
	round.record("traceless")
	return round.sdkResult(ctx)
}
func (round *fakeRound) SolveSliding(ctx context.Context, events []track.Event, width, handleWidth int) (Verification, error) {
	round.record("sliding")
	round.slidingTrack, round.slidingWidth, round.handleWidth = events, width, handleWidth
	return round.sdkResult(ctx)
}
func (round *fakeRound) sdkResult(ctx context.Context) (Verification, error) {
	if round.sdk != nil {
		return round.sdk(ctx)
	}
	return validVerification(), nil
}
func validVerification() Verification {
	return Verification{VerifyCode: "T001", VerifyResult: true, SecurityToken: "security-token", CertifyID: "certify-fixture"}
}
func (round *fakeRound) Verify(ctx context.Context, input VerificationInput) (Verification, error) {
	round.record("verify")
	if round.verify != nil {
		return round.verify(ctx, input)
	}
	return validVerification(), nil
}
func (round *fakeRound) CloseDevice() { round.record("close") }

func fixtureImages(t *testing.T, name string) Images {
	t.Helper()
	root := filepath.Join("..", "..", "domain", "vision", "testdata", "python-edge-decoy", name)
	background, err := os.ReadFile(filepath.Join(root, "back.png"))
	if err != nil {
		t.Fatal(err)
	}
	shadow, err := os.ReadFile(filepath.Join(root, "shadow.png"))
	if err != nil {
		t.Fatal(err)
	}
	return Images{Background: background, Shadow: shadow, TimingsMS: map[string]int{"background": 3, "shadow": 7}}
}
func testOptions(factory RoundFactory) Options {
	return Options{Timeout: 5 * time.Second, MinimumConfidence: 0.45,
		AssetMaxBytes: 8 << 20, AssetMaxDimension: 16_384, AssetMaxPixels: 16 << 20,
		Sources: runtimekit.NewSystemSources(), Factory: factory,
	}
}
func newTestSolver(t *testing.T, round *fakeRound, recorder FailureRecorder) *Solver {
	t.Helper()
	options := testOptions(factoryFunc(func(context.Context, Request) (Round, error) { return round, nil }))
	options.Recorder = recorder
	solver, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	return solver
}
func testRequest() Request { return Request{SceneID: "scene", Prefix: "prefix1"} }
func assertFailure(t *testing.T, err error, kind FailureKind, stage, message string) {
	t.Helper()
	var failure *Failure
	if !errors.As(err, &failure) || failure.Kind != kind || failure.Stage != stage || failure.Message != message {
		t.Fatalf("error=%v; want %s at %s: %s", err, kind, stage, message)
	}
}
func assertTimingKeys(t *testing.T, value map[string]int) {
	t.Helper()
	names := []string{"setup", "deviceSession", "init", "sliding", "traceless", "resolvePEKey", "downloadAssets", "downloadBackground", "downloadShadow", "vision", "buildVerifyData", "completeDevice", "verify", "clientCleanup", "total"}
	if len(value) != len(names) {
		t.Fatalf("timing keys=%v", value)
	}
	for _, name := range names {
		if duration, ok := value[name]; !ok || duration < 0 {
			t.Fatalf("invalid timing %q: %v", name, value)
		}
	}
}
func countCall(calls []string, name string) int {
	count := 0
	for _, call := range calls {
		if call == name {
			count++
		}
	}
	return count
}

func TestPuzzleCompletesAndReleasesBeforeSingleVerify(t *testing.T) {
	round := &fakeRound{kind: Puzzle, images: fixtureImages(t, "gap")}
	round.build = func(_ context.Context, input PuzzleInput) (PuzzleProof, error) {
		if input.ImageWidth < 1 || input.ImageHeight < 1 || input.PuzzleWidth < 1 || input.PuzzleHeight < 1 || input.RenderedWidth != 300 || input.HandleWidth != 40 || len(input.Track) < 3 || input.Track[len(input.Track)-1].X != input.SlidePos {
			t.Fatalf("puzzle input=%+v", input)
		}
		return validProof(input), nil
	}
	round.complete = func(_ context.Context, plan CompletionPlan) (string, error) {
		if !reflect.DeepEqual(plan, validProof(PuzzleInput{}).Completion) {
			t.Fatalf("completion plan=%+v", plan)
		}
		return "verify-device-token", nil
	}
	round.verify = func(_ context.Context, input VerificationInput) (Verification, error) {
		calls := round.snapshot()
		if countCall(calls, "close") != 1 || countCall(calls, "complete") != 1 {
			t.Fatalf("at Verify: %v", calls)
		}
		if input != (VerificationInput{DeviceToken: "verify-device-token", Data: "proof", VerifyTimeMS: 42}) {
			t.Fatalf("verification input=%+v", input)
		}
		return validVerification(), nil
	}
	solver := newTestSolver(t, round, recorderFunc(func(FailureSample) error { t.Fatal("success recorded a failure"); return nil }))
	outcome, err := solver.Solve(context.Background(), testRequest())
	if err != nil || !outcome.OK || !outcome.Proxied || outcome.SceneID != "scene" || outcome.SecurityToken != "security-token" || outcome.CertifyID != "certify-fixture" {
		t.Fatalf("outcome=%+v error=%v", outcome, err)
	}
	calls := round.snapshot()
	if countCall(calls, "close") != 1 || countCall(calls, "verify") != 1 {
		t.Fatalf("calls=%v", calls)
	}
	assertTimingKeys(t, outcome.TimingsMS)
	if outcome.TimingsMS["downloadBackground"] != 3 || outcome.TimingsMS["downloadShadow"] != 7 {
		t.Fatalf("asset timings=%v", outcome.TimingsMS)
	}
}

func TestSDKBranchesSkipPuzzleAndRPCVerify(t *testing.T) {
	for _, kind := range []ChallengeType{Sliding, Traceless} {
		t.Run(string(kind), func(t *testing.T) {
			round := &fakeRound{kind: kind}
			outcome, err := newTestSolver(t, round, nil).Solve(context.Background(), testRequest())
			if err != nil || !outcome.OK {
				t.Fatalf("outcome=%+v error=%v", outcome, err)
			}
			expected := []string{"open", "init", string(kind), "close"}
			if !reflect.DeepEqual(round.snapshot(), expected) {
				t.Fatalf("calls=%v", round.snapshot())
			}
			if kind == Sliding && (round.slidingWidth != 418 || round.handleWidth != 48 || len(round.slidingTrack) < 3 || round.slidingTrack[len(round.slidingTrack)-1].X != 370) {
				t.Fatalf("sliding dimensions/track=%+v", round)
			}
			assertTimingKeys(t, outcome.TimingsMS)
		})
	}
}

func TestSDKBusinessRejectionAndCertifyMismatchAreProtocolFailures(t *testing.T) {
	for _, kind := range []ChallengeType{Sliding, Traceless} {
		for _, mismatch := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/mismatch=%t", kind, mismatch), func(t *testing.T) {
				round := &fakeRound{kind: kind, sdk: func(context.Context) (Verification, error) {
					result := validVerification()
					if mismatch {
						result.CertifyID = "other"
					} else {
						result.VerifyCode, result.VerifyResult = "F015", false
					}
					return result, nil
				}}
				_, err := newTestSolver(t, round, nil).Solve(context.Background(), testRequest())
				message := "无痕验证码响应合同不一致"
				if kind == Sliding {
					message = "拖动验证码响应合同不一致"
				}
				assertFailure(t, err, FailureProtocol, string(kind), message)
				if countCall(round.snapshot(), "close") != 1 || countCall(round.snapshot(), "verify") != 0 {
					t.Fatalf("calls=%v", round.snapshot())
				}
			})
		}
	}
}

func TestPrepareAndDownloadStartConcurrentlyAndPrepareErrorWins(t *testing.T) {
	prepareStarted, downloadStarted := make(chan struct{}), make(chan struct{})
	prepareCause, downloadCause := errors.New("prepare sentinel"), errors.New("download sentinel")
	round := &fakeRound{kind: Puzzle}
	round.prepare = func(ctx context.Context) error {
		close(prepareStarted)
		select {
		case <-downloadStarted:
			return &DependencyError{Kind: FailureInternal, Cause: prepareCause}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	round.download = func(ctx context.Context) (Images, error) {
		close(downloadStarted)
		select {
		case <-prepareStarted:
			return Images{}, downloadCause
		case <-ctx.Done():
			return Images{}, ctx.Err()
		}
	}
	_, err := newTestSolver(t, round, nil).Solve(context.Background(), testRequest())
	assertFailure(t, err, FailureInternal, "resolvePEKey", "动态 PE 分片解析失败")
	if !errors.Is(err, prepareCause) || errors.Is(err, downloadCause) {
		t.Fatalf("error chain=%v", err)
	}
	if countCall(round.snapshot(), "close") != 1 || countCall(round.snapshot(), "build") != 0 {
		t.Fatalf("calls=%v", round.snapshot())
	}
}

func TestCancellationWaitsForPrepareAndDownloadAndReleases(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	both := make(chan struct{})
	var started, finished atomic.Int32
	waitForCancellation := func(ctx context.Context) error {
		if started.Add(1) == 2 {
			close(both)
		}
		<-ctx.Done()
		finished.Add(1)
		return ctx.Err()
	}
	round := &fakeRound{kind: Puzzle, prepare: waitForCancellation,
		download: func(ctx context.Context) (Images, error) { return Images{}, waitForCancellation(ctx) },
	}
	go func() { <-both; cancel() }()
	_, err := newTestSolver(t, round, nil).Solve(ctx, testRequest())
	assertFailure(t, err, FailureNetwork, "resolvePEKey", "请求已取消或超过总时限")
	if !errors.Is(err, context.Canceled) || finished.Load() != 2 || countCall(round.snapshot(), "close") != 1 {
		t.Fatalf("err=%v finished=%d calls=%v", err, finished.Load(), round.snapshot())
	}
}

func TestDependencyClassificationAndEarlyRelease(t *testing.T) {
	cause := errors.New("sensitive implementation detail")
	tests := []struct {
		name, stage, message string
		kind                 FailureKind
		change               func(*fakeRound)
	}{
		{"open", "deviceSession", "设备会话初始化失败", FailureInternal, func(r *fakeRound) {
			r.open = func(context.Context) error { return &DependencyError{Kind: FailureInternal, Cause: cause} }
		}},
		{"init", "init", "验证码初始化失败", FailureNetwork, func(r *fakeRound) {
			r.init = func(context.Context) (Challenge, error) {
				return Challenge{}, &DependencyError{Kind: FailureNetwork, Cause: cause}
			}
		}},
		{"prepare", "resolvePEKey", "动态 PE 分片解析失败", FailureProtocol, func(r *fakeRound) { r.prepare = func(context.Context) error { return cause } }},
		{"download", "downloadAssets", "验证码图片下载失败", FailureNetwork, func(r *fakeRound) { r.download = func(context.Context) (Images, error) { return Images{}, cause } }},
		{"build", "buildVerifyData", "Verify 数据构造失败", FailureInternal, func(r *fakeRound) {
			r.build = func(context.Context, PuzzleInput) (PuzzleProof, error) {
				return PuzzleProof{}, &DependencyError{Kind: FailureInternal, Cause: cause}
			}
		}},
		{"getter", "buildVerifyData", "设备 getter 合同不一致", FailureProtocol, func(r *fakeRound) {
			r.build = func(context.Context, PuzzleInput) (PuzzleProof, error) {
				return PuzzleProof{}, &DependencyError{Kind: FailureProtocol, Message: "设备 getter 合同不一致", Cause: cause}
			}
		}},
		{"complete", "completeDevice", "设备 Verify token 刷新失败", FailureNetwork, func(r *fakeRound) {
			r.complete = func(context.Context, CompletionPlan) (string, error) {
				return "", &DependencyError{Kind: FailureNetwork, Cause: cause}
			}
		}},
		{"verify", "verify", "验证码 Verify 请求失败", FailureNetwork, func(r *fakeRound) {
			r.verify = func(context.Context, VerificationInput) (Verification, error) {
				return Verification{}, &DependencyError{Kind: FailureNetwork, Cause: cause}
			}
		}},
	}
	images := fixtureImages(t, "gap")
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			round := &fakeRound{kind: Puzzle, images: images}
			test.change(round)
			outcome, err := newTestSolver(t, round, nil).Solve(context.Background(), testRequest())
			assertFailure(t, err, test.kind, test.stage, test.message)
			if !errors.Is(err, cause) || strings.Contains(err.Error(), cause.Error()) {
				t.Fatalf("cause not preserved or exposed: %v", err)
			}
			calls := round.snapshot()
			if countCall(calls, "close") != 1 || countCall(calls, "verify") > 1 {
				t.Fatalf("calls=%v", calls)
			}
			assertTimingKeys(t, outcome.TimingsMS)
		})
	}
}

func TestCancellationAfterSuccessfulDependencyStopsNextStage(t *testing.T) {
	for _, phase := range []string{"open", "init", "build", "complete", "verify", "sliding", "traceless"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			round := &fakeRound{kind: Puzzle, images: fixtureImages(t, "gap")}
			stage, next := phase, ""
			switch phase {
			case "open":
				stage, next = "deviceSession", "init"
				round.open = func(context.Context) error { cancel(); return nil }
			case "init":
				next = "prepare"
				round.init = func(context.Context) (Challenge, error) {
					cancel()
					return Challenge{Type: Puzzle, CertifyID: "certify-fixture"}, nil
				}
			case "build":
				stage, next = "buildVerifyData", "complete"
				round.build = func(_ context.Context, input PuzzleInput) (PuzzleProof, error) {
					cancel()
					return validProof(input), nil
				}
			case "complete":
				stage, next = "completeDevice", "verify"
				round.complete = func(context.Context, CompletionPlan) (string, error) { cancel(); return "verify-device-token", nil }
			case "verify":
				round.verify = func(context.Context, VerificationInput) (Verification, error) {
					cancel()
					return validVerification(), nil
				}
			default:
				round.kind = ChallengeType(phase)
				round.sdk = func(context.Context) (Verification, error) { cancel(); return validVerification(), nil }
			}
			_, err := newTestSolver(t, round, nil).Solve(ctx, testRequest())
			assertFailure(t, err, FailureNetwork, stage, "请求已取消或超过总时限")
			if !errors.Is(err, context.Canceled) || countCall(round.snapshot(), "close") != 1 || next != "" && countCall(round.snapshot(), next) != 0 {
				t.Fatalf("err=%v calls=%v", err, round.snapshot())
			}
		})
	}
}

func TestPuzzleContractsStopBeforeUnsafeNextStage(t *testing.T) {
	tests := []struct {
		name, stage, message string
		change               func(*fakeRound)
		forbidden            string
	}{
		{"slide-position", "buildVerifyData", "PE 输出合同不一致", func(r *fakeRound) {
			r.build = func(_ context.Context, input PuzzleInput) (PuzzleProof, error) {
				proof := validProof(input)
				proof.SlidePos++
				return proof, nil
			}
		}, "complete"},
		{"empty-token", "completeDevice", "设备完成态合同不一致", func(r *fakeRound) {
			r.complete = func(context.Context, CompletionPlan) (string, error) { return "", nil }
		}, "verify"},
		{"wrong-certify", "verify", "验证码 Verify 响应合同不一致", func(r *fakeRound) {
			r.verify = func(context.Context, VerificationInput) (Verification, error) {
				v := validVerification()
				v.CertifyID = "other"
				return v, nil
			}
		}, ""},
		{"missing-code", "verify", "验证码 Verify 响应合同不一致", func(r *fakeRound) {
			r.verify = func(context.Context, VerificationInput) (Verification, error) {
				v := validVerification()
				v.VerifyCode = ""
				return v, nil
			}
		}, ""},
		{"unknown-type", "init", "验证码初始化失败", func(r *fakeRound) { r.kind = "unsupported" }, "prepare"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			round := &fakeRound{kind: Puzzle, images: fixtureImages(t, "gap")}
			test.change(round)
			_, err := newTestSolver(t, round, nil).Solve(context.Background(), testRequest())
			assertFailure(t, err, FailureProtocol, test.stage, test.message)
			if countCall(round.snapshot(), "close") != 1 || test.forbidden != "" && countCall(round.snapshot(), test.forbidden) != 0 {
				t.Fatalf("calls=%v", round.snapshot())
			}
		})
	}
}

func TestRejectedPuzzleAndLowConfidenceRecordFailureWithoutChangingResult(t *testing.T) {
	for _, lowConfidence := range []bool{false, true} {
		t.Run(fmt.Sprintf("low-confidence=%t", lowConfidence), func(t *testing.T) {
			fixture, reason := "gap", "verify-rejected"
			if lowConfidence {
				fixture, reason = "no-gap", "low-confidence"
			}
			round := &fakeRound{kind: Puzzle, images: fixtureImages(t, fixture)}
			round.verify = func(context.Context, VerificationInput) (Verification, error) {
				return Verification{VerifyCode: "F015", CertifyID: "certify-fixture"}, nil
			}
			count := 0
			recorder := recorderFunc(func(sample FailureSample) error {
				count++
				if sample.Reason != reason || len(sample.Background) == 0 || len(sample.Shadow) == 0 {
					t.Fatalf("sample=%+v", sample)
				}
				assertTimingKeys(t, sample.TimingsMS)
				sample.TimingsMS["total"] = -1
				return errors.New("disk full")
			})
			outcome, err := newTestSolver(t, round, recorder).Solve(context.Background(), testRequest())
			if lowConfidence {
				assertFailure(t, err, FailureVision, "vision", "缺口识别置信度不足")
			} else if err != nil || outcome.OK || outcome.VerifyCode != "F015" {
				t.Fatalf("outcome=%+v err=%v", outcome, err)
			}
			if count != 1 || outcome.TimingsMS["total"] < 0 || countCall(round.snapshot(), "close") != 1 {
				t.Fatalf("record count=%d outcome=%+v calls=%v", count, outcome, round.snapshot())
			}
		})
	}
}

func TestInvalidAndCanceledRequestDoesNotCreateRound(t *testing.T) {
	calls := 0
	options := testOptions(factoryFunc(func(context.Context, Request) (Round, error) {
		calls++
		return nil, errors.New("unexpected factory call")
	}))
	solver, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, test := range []struct {
		ctx     context.Context
		request Request
		kind    FailureKind
		message string
	}{
		{nil, testRequest(), FailureInvalidRequest, "请求 context 不能为空"},
		{context.Background(), Request{SceneID: "scene", Prefix: "bad-prefix"}, FailureInvalidRequest, "请求参数无效"},
		{canceled, testRequest(), FailureNetwork, "请求已取消或超过总时限"},
	} {
		outcome, err := solver.Solve(test.ctx, test.request)
		assertFailure(t, err, test.kind, "request", test.message)
		assertTimingKeys(t, outcome.TimingsMS)
	}
	if calls != 0 {
		t.Fatalf("factory calls=%d", calls)
	}
}

func TestNewValidatesApplicationOptionsWithoutOpeningRound(t *testing.T) {
	factory := factoryFunc(func(context.Context, Request) (Round, error) { t.Fatal("constructor called factory"); return nil, nil })
	for _, change := range []func(*Options){
		func(o *Options) { o.Timeout = 0 }, func(o *Options) { o.Timeout = 6 * time.Minute },
		func(o *Options) { o.MinimumConfidence = -1 }, func(o *Options) { o.MinimumConfidence = 2 },
		func(o *Options) { o.AssetMaxBytes = 0 }, func(o *Options) { o.AssetMaxBytes = 65 << 20 },
		func(o *Options) { o.AssetMaxDimension = 0 }, func(o *Options) { o.AssetMaxDimension = 16_385 },
		func(o *Options) { o.AssetMaxPixels = 0 }, func(o *Options) { o.Sources.Clock = nil },
		func(o *Options) { o.Sources.Entropy = nil }, func(o *Options) { o.Factory = nil },
	} {
		options := testOptions(factory)
		change(&options)
		if solver, err := New(options); solver != nil || err == nil {
			t.Fatalf("solver=%v err=%v", solver, err)
		}
	}
	if _, err := New(testOptions(factory)); err != nil {
		t.Fatal(err)
	}
}
