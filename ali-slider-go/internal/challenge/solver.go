package challenge

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/d2120848471/v3/ali-slider-go/internal/artifact"
	"github.com/d2120848471/v3/ali-slider-go/internal/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/pe"
	"github.com/d2120848471/v3/ali-slider-go/internal/runtimekit"
	"github.com/d2120848471/v3/ali-slider-go/internal/track"
	"github.com/d2120848471/v3/ali-slider-go/internal/vision"
)

const defaultRegion = "cn"

// FailureKind 是编排层稳定、可脱敏映射的失败类别。
type FailureKind string

const (
	FailureInvalidRequest FailureKind = "invalid_request"
	FailureProtocol       FailureKind = "protocol"
	FailureNetwork        FailureKind = "network"
	FailureVision         FailureKind = "vision"
	FailureInternal       FailureKind = "internal"
)

// Failure 的 Message 只允许稳定文本；Cause 仅保留已由底层脱敏的诊断链。
type Failure struct {
	Kind    FailureKind
	Stage   string
	Message string
	Cause   error
}

func (failure *Failure) Error() string {
	if failure == nil {
		return ""
	}
	return fmt.Sprintf("%s at %s: %s", failure.Kind, failure.Stage, failure.Message)
}

func (failure *Failure) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.Cause
}

// SolveRequest 是内部单轮请求；HTTP 别名和默认值在公共层处理。
type SolveRequest struct {
	SceneID  string
	Prefix   string
	RPCKeyID string
	Proxy    string
}

func (SolveRequest) String() string   { return "challenge.SolveRequest{redacted}" }
func (SolveRequest) GoString() string { return "challenge.SolveRequest{redacted}" }

// SolveOutcome 只包含公共结果所需字段，不保留上游原始响应。
type SolveOutcome struct {
	OK            bool
	SecurityToken string
	VerifyCode    string
	VerifyResult  bool
	CertifyID     string
	SceneID       string
	Proxied       bool
	TimingsMS     map[string]int
}

func (SolveOutcome) String() string   { return "challenge.SolveOutcome{redacted}" }
func (SolveOutcome) GoString() string { return "challenge.SolveOutcome{redacted}" }

// TransportGetter 为生产连接池和完全离线的 Mock RoundTripper 提供同一入口。
type TransportGetter func(proxy string) (http.RoundTripper, bool, error)

// DeviceLeaseFunc 允许公共 Client 接入预热池。release 必须并发安全且可重复调用。
type DeviceLeaseFunc func(context.Context, DevicePoolKey, *device.Client) (*device.Session, func(), error)

// SolverOptions 固定进程级资源边界。FixedProfile 非 nil 时，HTTP、设备与 PE
// 全程共享同一份只读画像，这是设备会话预热命中的必要条件。
type SolverOptions struct {
	Timeout           time.Duration
	MinimumConfidence float64
	GatherCostMin     int
	GatherCostMax     int
	FirstTouchAgeMin  int
	FirstTouchAgeMax  int
	AssetMaxBytes     int64
	AssetMaxDimension int
	AssetMaxPixels    int64
	Sources           runtimekit.Sources
	GetTransport      TransportGetter
	Artifacts         *artifact.Store
	FixedProfile      *device.Profile
	LeaseDevice       DeviceLeaseFunc
}

// Solver 执行 Device → Init → Assets → Vision → PE → Device Complete → Verify。
type Solver struct {
	options SolverOptions
}

// NewSolver 构造无共享挑战状态的编排器；连接池、熵和可选设备池由调用方共享。
func NewSolver(options SolverOptions) (*Solver, error) {
	if options.Timeout <= 0 || options.Timeout > 5*time.Minute {
		return nil, errors.New("solver timeout must be within (0,5m]")
	}
	if options.MinimumConfidence < 0 || options.MinimumConfidence > 1 {
		return nil, errors.New("solver confidence must be within 0..1")
	}
	if options.GatherCostMin < 0 || options.GatherCostMax < options.GatherCostMin {
		return nil, errors.New("solver gather cost range is invalid")
	}
	if options.FirstTouchAgeMin < 1 || options.FirstTouchAgeMax < options.FirstTouchAgeMin {
		return nil, errors.New("solver first-touch range is invalid")
	}
	if options.AssetMaxBytes < 1 || options.AssetMaxBytes > 64<<20 || options.AssetMaxDimension < 1 || options.AssetMaxDimension > 16_384 || options.AssetMaxPixels < 1 {
		return nil, errors.New("solver asset limits are invalid")
	}
	if err := options.Sources.Validate(); err != nil {
		return nil, err
	}
	if options.GetTransport == nil {
		return nil, errors.New("solver transport getter is required")
	}
	return &Solver{options: options}, nil
}

type setupState struct {
	transport    http.RoundTripper
	profile      device.Profile
	deviceClient *device.Client
	rpcClient    *RPCClient
	proxied      bool
	deviceKey    DevicePoolKey
}

// Solve 完成一轮挑战。每次调用只创建一个 RPCClient，因此一个 CertifyId 最多
// 消耗一次 Verify；Verify 的网络结果不明也不会重发。
func (solver *Solver) Solve(parent context.Context, request SolveRequest) (outcome SolveOutcome, resultErr error) {
	timings := newTimingMap()
	started := time.Now()
	currentStage := "request"
	artifactReason := ""
	var assets Assets
	var estimate vision.Estimate

	defer func() {
		timings["total"] = elapsedMilliseconds(started)
		outcome.TimingsMS = timings
		if solver.options.Artifacts == nil {
			return
		}
		if artifactReason == "" && resultErr != nil {
			artifactReason = failureReason(resultErr)
		}
		if artifactReason == "" {
			return
		}
		_, _ = solver.options.Artifacts.SaveFailure(assets.Background, assets.Shadow, artifact.Metrics{
			Reason: artifactReason, Stage: currentStage, Confidence: estimate.Confidence,
			XPos: estimate.XPos, SlidePos: estimate.SlidePos, TimingsMS: cloneTimings(timings),
		})
	}()

	if parent == nil {
		return outcome, fail(FailureInvalidRequest, currentStage, "请求 context 不能为空", nil)
	}
	if err := validateSolveRequest(request); err != nil {
		return outcome, fail(FailureInvalidRequest, currentStage, "请求参数无效", err)
	}
	ctx, cancel := context.WithTimeout(parent, solver.options.Timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return outcome, contextFailure(currentStage, err)
	}

	currentStage = "setup"
	setupStarted := time.Now()
	setup, err := solver.setup(ctx, request)
	timings[currentStage] = elapsedMilliseconds(setupStarted)
	if err != nil {
		return outcome, err
	}
	outcome.SceneID, outcome.Proxied = request.SceneID, setup.proxied

	currentStage = "deviceSession"
	deviceStarted := time.Now()
	session, release, err := solver.openDeviceSession(ctx, setup)
	timings[currentStage] = elapsedMilliseconds(deviceStarted)
	if err != nil {
		return outcome, classifiedStageFailure(ctx, currentStage, "设备会话初始化失败", err)
	}
	if release == nil {
		session.Close()
		return outcome, fail(FailureInternal, currentStage, "设备会话释放器缺失", nil)
	}
	defer func() {
		cleanupStarted := time.Now()
		release()
		timings["clientCleanup"] = elapsedMilliseconds(cleanupStarted)
	}()
	initToken, err := session.InitToken()
	if err != nil || initToken == "" {
		return outcome, stageFailure(ctx, FailureProtocol, currentStage, "设备初始化 token 不可用", err)
	}
	if err := checkStageContext(ctx, currentStage); err != nil {
		return outcome, err
	}

	currentStage = "init"
	challengeStarted := time.Now()
	challengeValue, err := setup.rpcClient.Init(ctx, initToken)
	timings[currentStage] = elapsedMilliseconds(challengeStarted)
	if err != nil {
		return outcome, classifiedStageFailure(ctx, currentStage, "验证码初始化失败", err)
	}
	if err := checkStageContext(ctx, currentStage); err != nil {
		return outcome, err
	}

	currentStage = "downloadAssets"
	downloadStarted := time.Now()
	assets, err = DownloadAssets(ctx, setup.transport, setup.profile, defaultReferer, challengeValue.ImagePath, challengeValue.PuzzleImagePath, AssetLimits{MaxBytes: solver.options.AssetMaxBytes})
	timings[currentStage] = elapsedMilliseconds(downloadStarted)
	if backgroundMS, ok := assets.TimingsMS["background"]; ok {
		timings["downloadBackground"] = backgroundMS
	}
	if shadowMS, ok := assets.TimingsMS["shadow"]; ok {
		timings["downloadShadow"] = shadowMS
	}
	if err != nil {
		return outcome, stageFailure(ctx, FailureNetwork, currentStage, "验证码图片下载失败", err)
	}
	if err := checkStageContext(ctx, currentStage); err != nil {
		return outcome, err
	}

	currentStage = "vision"
	visionStarted := time.Now()
	estimate, err = vision.Solve(ctx, assets.Background, assets.Shadow, vision.Limits{
		MaxBytes: int(solver.options.AssetMaxBytes), MaxDimension: solver.options.AssetMaxDimension, MaxPixels: solver.options.AssetMaxPixels,
	})
	timings[currentStage] = elapsedMilliseconds(visionStarted)
	if err != nil {
		return outcome, stageFailure(ctx, FailureVision, currentStage, "缺口识别失败", err)
	}
	if estimate.Confidence < solver.options.MinimumConfidence {
		artifactReason = "low-confidence"
		return outcome, fail(FailureVision, currentStage, "缺口识别置信度不足", nil)
	}
	if err := checkStageContext(ctx, currentStage); err != nil {
		return outcome, err
	}

	currentStage = "buildVerifyData"
	buildStarted := time.Now()
	trackValue, err := track.LoadDefault(float64(estimate.SlidePos), solver.options.Sources.Entropy)
	if err == nil {
		var firstTouchAge int
		firstTouchAge, err = session.TargetFirstTouchAgeMS()
		if err == nil {
			expectedX := estimate.XPos
			var build pe.Result
			build, err = (pe.Builder{Profile: setup.profile, Sources: solver.options.Sources}).Build(ctx, pe.Input{
				SceneID: request.SceneID, CertifyID: challengeValue.CertifyID, StaticPath: challengeValue.StaticPath,
				ExpectedXPos: &expectedX, InitBeginTimeMS: challengeValue.InitStartedMS,
				FirstTouchAgeMS: firstTouchAge, Track: trackValue,
			})
			if err == nil {
				timings[currentStage] = elapsedMilliseconds(buildStarted)
				return solver.completeAndVerify(ctx, request, setup, session, challengeValue, estimate, build, timings, &currentStage, &artifactReason)
			}
		}
	}
	timings[currentStage] = elapsedMilliseconds(buildStarted)
	return outcome, stageFailure(ctx, FailureProtocol, currentStage, "Verify 数据构造失败", err)
}

func (solver *Solver) completeAndVerify(
	ctx context.Context,
	request SolveRequest,
	setup setupState,
	session *device.Session,
	challengeValue CaptchaChallenge,
	estimate vision.Estimate,
	build pe.Result,
	timings map[string]int,
	currentStage *string,
	artifactReason *string,
) (SolveOutcome, error) {
	outcome := SolveOutcome{SceneID: request.SceneID, Proxied: setup.proxied, TimingsMS: timings}
	if build.SlidePos != estimate.SlidePos || len(build.DeviceGetterPlans) != 1 {
		return outcome, fail(FailureProtocol, *currentStage, "PE 输出合同不一致", nil)
	}
	plan := build.DeviceGetterPlans[0]
	if plan.ArgumentCount != 1 || len(plan.Arguments) != 1 || plan.Arguments[0].ValueType != "string" || plan.Arguments[0].Value == "" || !plan.Arguments[0].EqualsCertifyID {
		return outcome, fail(FailureProtocol, *currentStage, "设备 getter 合同不一致", nil)
	}
	if err := checkStageContext(ctx, *currentStage); err != nil {
		return outcome, err
	}

	interactions := make([]device.InteractionEvent, len(build.InteractionEvents))
	for index, event := range build.InteractionEvents {
		interactions[index] = device.InteractionEvent{
			Type: event.Type, X: event.X, Y: event.Y, TimeStamp: event.TimeStamp, IsTrusted: event.IsTrusted,
		}
	}
	postDelay := int(math.Floor(build.PostInteractionDelayMS + 0.5))
	*currentStage = "completeDevice"
	completeStarted := time.Now()
	deviceResult, err := session.Complete(ctx, plan.Arguments[0].Value, interactions, postDelay)
	timings[*currentStage] = elapsedMilliseconds(completeStarted)
	if err != nil {
		return outcome, classifiedStageFailure(ctx, *currentStage, "设备 Verify token 刷新失败", err)
	}
	if deviceResult.VerifyToken == "" || deviceResult.GetterArgumentCount != 1 || deviceResult.InteractionEventCount != len(interactions) || strings.Join(deviceResult.RequestActions, ",") != "Log1,Log2,Log3,Log2" {
		return outcome, fail(FailureProtocol, *currentStage, "设备完成态合同不一致", nil)
	}
	if err := checkStageContext(ctx, *currentStage); err != nil {
		return outcome, err
	}

	*currentStage = "verify"
	verifyStarted := time.Now()
	verifyResult, err := setup.rpcClient.Verify(ctx, challengeValue, deviceResult.VerifyToken, build.Data, build.VerifyTimeMS)
	timings[*currentStage] = elapsedMilliseconds(verifyStarted)
	if err != nil {
		return outcome, classifiedStageFailure(ctx, *currentStage, "验证码 Verify 请求失败", err)
	}
	if verifyResult.CertifyID != challengeValue.CertifyID || verifyResult.VerifyCode == "" {
		return outcome, fail(FailureProtocol, *currentStage, "验证码 Verify 响应合同不一致", nil)
	}
	if err := checkStageContext(ctx, *currentStage); err != nil {
		return outcome, err
	}
	outcome.OK = verifyResult.Succeeded()
	outcome.SecurityToken = verifyResult.SecurityToken
	outcome.VerifyCode = verifyResult.VerifyCode
	outcome.VerifyResult = verifyResult.VerifyResult
	outcome.CertifyID = verifyResult.CertifyID
	if !outcome.OK {
		*artifactReason = "verify-rejected"
	}
	return outcome, nil
}

func (solver *Solver) setup(ctx context.Context, request SolveRequest) (setupState, error) {
	if err := ctx.Err(); err != nil {
		return setupState{}, contextFailure("setup", err)
	}
	var profile device.Profile
	if solver.options.FixedProfile != nil {
		profile = *solver.options.FixedProfile
	} else {
		var err error
		profile, err = device.GenerateProfile(solver.options.Sources.Entropy)
		if err != nil {
			return setupState{}, fail(FailureInternal, "setup", "设备画像生成失败", err)
		}
	}
	transport, proxied, err := solver.options.GetTransport(request.Proxy)
	if errors.Is(err, ErrTransportRouteCapacity) {
		return setupState{}, fail(FailureInternal, "setup", "传输路由资源已满", err)
	}
	if err != nil || transport == nil {
		return setupState{}, fail(FailureInvalidRequest, "setup", "代理配置无效或路由不可用", err)
	}
	deviceOptions := device.DefaultClientOptions(profile, solver.options.Sources)
	deviceOptions.Prefix = request.Prefix
	deviceOptions.Region = defaultRegion
	deviceOptions.Timeout = solver.options.Timeout
	deviceOptions.GatherCostMin, deviceOptions.GatherCostMax = solver.options.GatherCostMin, solver.options.GatherCostMax
	deviceOptions.FirstTouchAgeMin, deviceOptions.FirstTouchAgeMax = solver.options.FirstTouchAgeMin, solver.options.FirstTouchAgeMax
	deviceClient, err := device.NewClient(deviceOptions, transport)
	if err != nil {
		return setupState{}, fail(FailureInternal, "setup", "设备客户端初始化失败", err)
	}
	rpcClient, err := NewRPCClient(RPCOptions{
		Transport: transport, Profile: profile, Sources: solver.options.Sources,
		SceneID: request.SceneID, Prefix: request.Prefix, RPCKeyID: request.RPCKeyID,
	})
	if err != nil {
		return setupState{}, fail(FailureProtocol, "setup", "验证码客户端初始化失败", err)
	}
	return setupState{
		transport: transport, profile: profile, deviceClient: deviceClient, rpcClient: rpcClient,
		proxied: proxied, deviceKey: DevicePoolKeyFrom(deviceOptions, request.Proxy),
	}, nil
}

func (solver *Solver) openDeviceSession(ctx context.Context, setup setupState) (*device.Session, func(), error) {
	if solver.options.LeaseDevice != nil {
		return solver.options.LeaseDevice(ctx, setup.deviceKey, setup.deviceClient)
	}
	session, err := setup.deviceClient.Open(ctx)
	if err != nil {
		return nil, nil, err
	}
	return session, session.Close, nil
}

func validateSolveRequest(request SolveRequest) error {
	if request.SceneID == "" || utf8.RuneCountInString(request.SceneID) > 64 {
		return errors.New("SceneId must contain 1..64 characters")
	}
	if request.Prefix == "" || len(request.Prefix) > 32 {
		return errors.New("prefix must contain 1..32 ASCII alphanumeric characters")
	}
	for _, value := range []byte(request.Prefix) {
		if (value < '0' || value > '9') && (value < 'A' || value > 'Z') && (value < 'a' || value > 'z') {
			return errors.New("prefix must contain 1..32 ASCII alphanumeric characters")
		}
	}
	if utf8.RuneCountInString(request.RPCKeyID) > 128 || len(request.Proxy) > 4096 {
		return errors.New("request field exceeds its size limit")
	}
	return nil
}

func newTimingMap() map[string]int {
	result := make(map[string]int, 12)
	for _, name := range []string{
		"setup", "deviceSession", "init", "downloadAssets", "downloadBackground", "downloadShadow",
		"vision", "buildVerifyData", "completeDevice", "verify", "clientCleanup", "total",
	} {
		result[name] = 0
	}
	return result
}

func cloneTimings(value map[string]int) map[string]int {
	output := make(map[string]int, len(value))
	for key, duration := range value {
		output[key] = duration
	}
	return output
}

func elapsedMilliseconds(started time.Time) int {
	return max(0, int(time.Since(started)/time.Millisecond))
}

func fail(kind FailureKind, stage, message string, cause error) error {
	return &Failure{Kind: kind, Stage: stage, Message: message, Cause: cause}
}

func stageFailure(ctx context.Context, kind FailureKind, stage, message string, cause error) error {
	if ctx != nil && ctx.Err() != nil {
		return contextFailure(stage, ctx.Err())
	}
	return fail(kind, stage, message, cause)
}

type networkFailure interface {
	NetworkFailure() bool
}

// classifiedStageFailure 只把底层明确标记的 HTTP/DNS/TLS/读取失败计入网络类。
// HTTP 2xx 的 schema、本地签名/构造和设备配置解析失败都归入协议类。
func classifiedStageFailure(ctx context.Context, stage, message string, cause error) error {
	if ctx != nil && ctx.Err() != nil {
		return contextFailure(stage, ctx.Err())
	}
	var marked networkFailure
	if errors.As(cause, &marked) && marked.NetworkFailure() {
		return fail(FailureNetwork, stage, message, cause)
	}
	return fail(FailureProtocol, stage, message, cause)
}

func contextFailure(stage string, cause error) error {
	return fail(FailureNetwork, stage, "请求已取消或超过总时限", cause)
}

func checkStageContext(ctx context.Context, stage string) error {
	if err := ctx.Err(); err != nil {
		return contextFailure(stage, err)
	}
	return nil
}

func failureReason(err error) string {
	var failure *Failure
	if errors.As(err, &failure) {
		return string(failure.Kind)
	}
	return string(FailureInternal)
}
