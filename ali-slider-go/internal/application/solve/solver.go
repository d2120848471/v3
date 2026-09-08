package solve

import (
	"bytes"
	"context"
	"errors"
	"image/png"
	"sync"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/domain/track"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/vision"
)

const (
	sliderRenderedWidth = 300
	sliderHandleWidth   = 40
	slidingTrackWidth   = 418
	slidingHandleWidth  = 48
)

type Solver struct{ options Options }

// Solve 只编排当轮阶段；Round 独占验证码和设备状态，跨调用不共享。
func (solver *Solver) Solve(parent context.Context, request Request) (outcome Outcome, resultErr error) {
	timings := newTimingMap()
	started := time.Now()
	currentStage := "request"
	artifactReason := ""
	var images Images
	var estimate vision.Estimate
	defer func() {
		timings["total"] = elapsedMilliseconds(started)
		outcome.TimingsMS = timings
		if solver.options.Recorder == nil {
			return
		}
		if artifactReason == "" && resultErr != nil {
			artifactReason = failureReason(resultErr)
		}
		if artifactReason == "" {
			return
		}
		// 失败样本沿用 best-effort 策略；记录失败不能替换实际业务结果。
		_ = solver.options.Recorder.SaveFailure(FailureSample{
			Background: images.Background, Shadow: images.Shadow,
			Reason: artifactReason, Stage: currentStage, Confidence: estimate.Confidence,
			XPos: estimate.XPos, SlidePos: estimate.SlidePos, TimingsMS: cloneTimings(timings),
		})
	}()
	if parent == nil {
		return outcome, fail(FailureInvalidRequest, currentStage, "请求 context 不能为空", nil)
	}
	if err := validateRequest(request); err != nil {
		return outcome, fail(FailureInvalidRequest, currentStage, "请求参数无效", err)
	}
	ctx, cancel := context.WithTimeout(parent, solver.options.Timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return outcome, contextFailure(currentStage, err)
	}

	currentStage = "setup"
	setupStarted := time.Now()
	round, err := solver.options.Factory.NewRound(ctx, request)
	timings[currentStage] = elapsedMilliseconds(setupStarted)
	if err != nil {
		return outcome, dependencyFailure(ctx, FailureInternal, currentStage, "求解会话初始化失败", err)
	}
	if round == nil {
		return outcome, fail(FailureInternal, currentStage, "求解会话初始化失败", nil)
	}
	outcome.SceneID, outcome.Proxied = request.SceneID, round.Proxied()
	var releaseOnce sync.Once
	releaseSession := func() {
		releaseOnce.Do(func() {
			cleanupStarted := time.Now()
			round.CloseDevice()
			timings["clientCleanup"] = elapsedMilliseconds(cleanupStarted)
		})
	}
	defer releaseSession()

	currentStage = "deviceSession"
	deviceStarted := time.Now()
	err = round.OpenDevice(ctx)
	timings[currentStage] = elapsedMilliseconds(deviceStarted)
	if err != nil {
		return outcome, dependencyFailure(ctx, FailureProtocol, currentStage, "设备会话初始化失败", err)
	}
	if err := checkStageContext(ctx, currentStage); err != nil {
		return outcome, err
	}

	currentStage = "init"
	challengeStarted := time.Now()
	challenge, err := round.Init(ctx)
	timings[currentStage] = elapsedMilliseconds(challengeStarted)
	if err != nil {
		return outcome, dependencyFailure(ctx, FailureProtocol, currentStage, "验证码初始化失败", err)
	}
	if err := checkStageContext(ctx, currentStage); err != nil {
		return outcome, err
	}

	switch challenge.Type {
	case Sliding:
		currentStage = "sliding"
		slidingStarted := time.Now()
		trackValue, trackErr := track.LoadDefault(float64(slidingTrackWidth-slidingHandleWidth), solver.options.Sources.Entropy)
		if trackErr != nil {
			timings[currentStage] = elapsedMilliseconds(slidingStarted)
			return outcome, dependencyFailure(ctx, FailureProtocol, currentStage, "拖动验证码轨迹生成失败", trackErr)
		}
		result, solveErr := round.SolveSliding(ctx, trackValue, slidingTrackWidth, slidingHandleWidth)
		timings[currentStage] = elapsedMilliseconds(slidingStarted)
		if solveErr != nil {
			return outcome, dependencyFailure(ctx, FailureProtocol, currentStage, "拖动验证码执行失败", solveErr)
		}
		if !result.Succeeded() || result.CertifyID != challenge.CertifyID {
			return outcome, fail(FailureProtocol, currentStage, "拖动验证码响应合同不一致", nil)
		}
		if err := checkStageContext(ctx, currentStage); err != nil {
			return outcome, err
		}
		releaseSession()
		outcome.setVerification(result)
		return outcome, nil
	case Traceless:
		currentStage = "traceless"
		tracelessStarted := time.Now()
		result, solveErr := round.SolveTraceless(ctx)
		timings[currentStage] = elapsedMilliseconds(tracelessStarted)
		if solveErr != nil {
			return outcome, dependencyFailure(ctx, FailureProtocol, currentStage, "无痕验证码执行失败", solveErr)
		}
		if !result.Succeeded() || result.CertifyID != challenge.CertifyID {
			return outcome, fail(FailureProtocol, currentStage, "无痕验证码响应合同不一致", nil)
		}
		if err := checkStageContext(ctx, currentStage); err != nil {
			return outcome, err
		}
		releaseSession()
		outcome.setVerification(result)
		return outcome, nil
	case Puzzle:
	default:
		return outcome, fail(FailureProtocol, currentStage, "验证码初始化失败", nil)
	}

	// 脚本准备与图片下载只依赖 Init，必须并发开始并等待两者收束。
	// 两者都失败时保持脚本准备错误优先，避免改变对上游变动的分类。
	type prepareResult struct {
		elapsed int
		err     error
	}
	preparedResults := make(chan prepareResult, 1)
	go func() {
		started := time.Now()
		prepareErr := round.PreparePuzzle(ctx)
		preparedResults <- prepareResult{elapsed: elapsedMilliseconds(started), err: prepareErr}
	}()
	currentStage = "downloadAssets"
	downloadStarted := time.Now()
	images, err = round.DownloadImages(ctx)
	timings[currentStage] = elapsedMilliseconds(downloadStarted)
	assetErr := err
	prepared := <-preparedResults
	timings["resolvePEKey"] = prepared.elapsed
	if prepared.err != nil {
		currentStage = "resolvePEKey"
		return outcome, dependencyFailure(ctx, FailureProtocol, currentStage, "动态 PE 分片解析失败", prepared.err)
	}
	if err := checkStageContext(ctx, "resolvePEKey"); err != nil {
		currentStage = "resolvePEKey"
		return outcome, err
	}
	if backgroundMS, ok := images.TimingsMS["background"]; ok {
		timings["downloadBackground"] = backgroundMS
	}
	if shadowMS, ok := images.TimingsMS["shadow"]; ok {
		timings["downloadShadow"] = shadowMS
	}
	if assetErr != nil {
		return outcome, stageFailure(ctx, FailureNetwork, currentStage, "验证码图片下载失败", assetErr)
	}
	if err := checkStageContext(ctx, currentStage); err != nil {
		return outcome, err
	}

	currentStage = "vision"
	visionStarted := time.Now()
	estimate, err = vision.Solve(ctx, images.Background, images.Shadow, vision.Limits{
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
	input, err := solver.puzzleInput(estimate, images)
	var proof PuzzleProof
	if err == nil {
		proof, err = round.BuildPuzzle(ctx, input)
	}
	timings[currentStage] = elapsedMilliseconds(buildStarted)
	if err != nil {
		return outcome, dependencyFailure(ctx, FailureProtocol, currentStage, "Verify 数据构造失败", err)
	}
	if proof.SlidePos != estimate.SlidePos {
		return outcome, fail(FailureProtocol, currentStage, "PE 输出合同不一致", nil)
	}
	if err := checkStageContext(ctx, currentStage); err != nil {
		return outcome, err
	}

	currentStage = "completeDevice"
	completeStarted := time.Now()
	token, err := round.CompleteDevice(ctx, proof.Completion)
	timings[currentStage] = elapsedMilliseconds(completeStarted)
	if err != nil {
		return outcome, dependencyFailure(ctx, FailureProtocol, currentStage, "设备 Verify token 刷新失败", err)
	}
	if token == "" {
		return outcome, fail(FailureProtocol, currentStage, "设备完成态合同不一致", nil)
	}
	if err := checkStageContext(ctx, currentStage); err != nil {
		return outcome, err
	}
	// Complete 后已无需设备环境；Verify 发起前释放，defer 覆盖所有早退路径。
	releaseSession()

	currentStage = "verify"
	verifyStarted := time.Now()
	result, err := round.Verify(ctx, VerificationInput{DeviceToken: token, Data: proof.Data, VerifyTimeMS: proof.VerifyTimeMS})
	timings[currentStage] = elapsedMilliseconds(verifyStarted)
	if err != nil {
		return outcome, dependencyFailure(ctx, FailureProtocol, currentStage, "验证码 Verify 请求失败", err)
	}
	if result.CertifyID != challenge.CertifyID || result.VerifyCode == "" {
		return outcome, fail(FailureProtocol, currentStage, "验证码 Verify 响应合同不一致", nil)
	}
	if err := checkStageContext(ctx, currentStage); err != nil {
		return outcome, err
	}
	outcome.setVerification(result)
	if !outcome.OK {
		artifactReason = "verify-rejected"
	}
	return outcome, nil
}

func (outcome *Outcome) setVerification(value Verification) {
	outcome.OK = value.Succeeded()
	outcome.SecurityToken, outcome.VerifyCode = value.SecurityToken, value.VerifyCode
	outcome.VerifyResult, outcome.CertifyID = value.VerifyResult, value.CertifyID
}

func (solver *Solver) puzzleInput(estimate vision.Estimate, images Images) (PuzzleInput, error) {
	input := PuzzleInput{XPos: estimate.XPos, SlidePos: estimate.SlidePos, RenderedWidth: sliderRenderedWidth, HandleWidth: sliderHandleWidth}
	var err error
	input.Track, err = track.LoadDefault(float64(estimate.SlidePos), solver.options.Sources.Entropy)
	if err != nil {
		return PuzzleInput{}, err
	}
	input.ImageWidth, input.ImageHeight, err = pngDimensions(images.Background)
	if err != nil {
		return PuzzleInput{}, err
	}
	input.PuzzleWidth, input.PuzzleHeight, err = pngDimensions(images.Shadow)
	if err != nil {
		return PuzzleInput{}, err
	}
	return input, nil
}

func pngDimensions(value []byte) (int, int, error) {
	config, err := png.DecodeConfig(bytes.NewReader(value))
	if err != nil || config.Width < 1 || config.Height < 1 {
		return 0, 0, errors.New("asset PNG dimensions are invalid")
	}
	return config.Width, config.Height, nil
}

func newTimingMap() map[string]int {
	result := make(map[string]int, 15)
	for _, name := range []string{
		"setup", "deviceSession", "init", "sliding", "traceless", "resolvePEKey", "downloadAssets", "downloadBackground", "downloadShadow",
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
