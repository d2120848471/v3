package aliyun

import (
	"context"
	"errors"
	"math"
	"strings"

	"github.com/d2120848471/v3/ali-slider-go/internal/application/solve"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/pe"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/track"
)

func (round *round) Proxied() bool { return round.proxied }

func (round *round) OpenDevice(ctx context.Context) error {
	var session DeviceSession
	var release func()
	var err error
	if round.options.OpenDevice != nil {
		session, release, err = round.options.OpenDevice(ctx, round.transport, round.profile, round.request)
		if err != nil {
			return runtimeError(err, true)
		}
		if session == nil || release == nil {
			if session != nil {
				session.Close()
			}
			return runtimeError(errors.New("device runtime opener returned an incomplete session"), true)
		}
	} else {
		session, err = round.deviceClient.Open(ctx)
		if err != nil {
			return runtimeError(err, true)
		}
		release = session.Close
	}
	round.session, round.release = session, release
	if provider, ok := session.(deviceProfileProvider); ok {
		profile := provider.DeviceProfile()
		if profile.ProfileID == "" {
			return dependencyError(solve.FailureProtocol, "设备会话画像不可用", nil)
		}
		round.profile = profile.Clone()
	}
	round.rpcClient, err = NewRPCClient(RPCOptions{
		Transport: round.transport, Profile: round.profile, Sources: round.options.Sources,
		SceneID: round.request.SceneID, Prefix: round.request.Prefix, RPCKeyID: round.request.RPCKeyID,
	})
	if err != nil {
		return dependencyError(solve.FailureProtocol, "验证码客户端初始化失败", err)
	}
	round.initToken, err = session.InitToken()
	if err != nil || round.initToken == "" {
		return dependencyError(solve.FailureProtocol, "设备初始化 token 不可用", err)
	}
	return nil
}

func (round *round) Init(ctx context.Context) (solve.Challenge, error) {
	challenge, err := round.rpcClient.Init(ctx, round.initToken)
	if err != nil {
		return solve.Challenge{}, networkOrProtocolError(err)
	}
	round.challenge = challenge
	kind := solve.Puzzle
	if isSlidingCaptchaType(challenge.CaptchaType) {
		kind = solve.Sliding
	}
	if isTracelessCaptchaType(challenge.CaptchaType) {
		kind = solve.Traceless
	}
	return solve.Challenge{Type: kind, CertifyID: challenge.CertifyID}, nil
}

func (round *round) PreparePuzzle(ctx context.Context) error {
	return runtimeError(round.options.PEKeys.Prepare(ctx, round.transport, round.profile, round.challenge.StaticPath), false)
}
func (round *round) DownloadImages(ctx context.Context) (solve.Images, error) {
	assets, err := DownloadAssets(ctx, round.transport, round.profile, defaultReferer, round.challenge.ImagePath, round.challenge.PuzzleImagePath, AssetLimits{MaxBytes: round.options.AssetMaxBytes})
	return solve.Images{Background: assets.Background, Shadow: assets.Shadow, TimingsMS: assets.TimingsMS}, err
}

func (round *round) BuildPuzzle(ctx context.Context, input solve.PuzzleInput) (solve.PuzzleProof, error) {
	firstTouchAge, err := round.session.TargetFirstTouchAgeMS()
	if err != nil {
		return solve.PuzzleProof{}, runtimeError(err, false)
	}
	config, err := round.session.PEDeviceConfig()
	if err != nil {
		return solve.PuzzleProof{}, runtimeError(err, false)
	}
	verifyAccessSec, verifySalt := "", ""
	if provider, ok := round.session.(peVerifyArgProfileProvider); ok {
		verifyAccessSec, verifySalt, err = provider.PEVerifyArgProfile()
		if err != nil {
			return solve.PuzzleProof{}, runtimeError(err, false)
		}
	}
	var sdk []byte
	if provider, ok := round.session.(peSDKSourceProvider); ok {
		sdk, err = provider.PESDKSource()
		if err != nil {
			return solve.PuzzleProof{}, runtimeError(err, false)
		}
	}
	expectedX := input.XPos
	challenge := round.challenge
	result, err := round.options.PEKeys.Build(ctx, round.transport, round.profile, challenge.StaticPath, pe.RuntimeInput{
		SceneID: round.request.SceneID, CertifyID: challenge.CertifyID,
		DeviceToken: round.initToken, CaptchaType: challenge.CaptchaType,
		Image: challenge.ImagePath, PuzzleImage: challenge.PuzzleImagePath,
		DeviceConfig: config, VerifyAccessSec: verifyAccessSec, VerifySalt: verifySalt,
		Dimensions: pe.RuntimeDimensions{
			ImageWidth: input.ImageWidth, ImageHeight: input.ImageHeight, PuzzleWidth: input.PuzzleWidth, PuzzleHeight: input.PuzzleHeight,
			RenderedWidth: input.RenderedWidth, HandleWidth: input.HandleWidth,
		},
		Track: input.Track, ExpectedXPos: &expectedX,
		InitBeginTimeMS: challenge.InitStartedMS, FirstTouchAgeMS: firstTouchAge, SDKSource: sdk,
	})
	if err != nil {
		return solve.PuzzleProof{}, runtimeError(err, false)
	}
	// 先报告 PE 输出整体不一致，保留同时出现位移与 getter 错误时的优先级。
	if result.SlidePos != input.SlidePos || len(result.DeviceGetterPlans) != 1 {
		return solve.PuzzleProof{}, dependencyError(solve.FailureProtocol, "PE 输出合同不一致", nil)
	}
	plan := result.DeviceGetterPlans[0]
	if plan.ArgumentCount != 1 || len(plan.Arguments) != 1 || plan.Arguments[0].ValueType != "string" || plan.Arguments[0].Value == "" || !plan.Arguments[0].EqualsCertifyID {
		return solve.PuzzleProof{}, dependencyError(solve.FailureProtocol, "设备 getter 合同不一致", nil)
	}
	interactions := make([]device.InteractionEvent, len(result.InteractionEvents))
	for index, event := range result.InteractionEvents {
		interactions[index] = device.InteractionEvent{Type: event.Type, X: event.X, Y: event.Y, TimeStamp: event.TimeStamp, IsTrusted: event.IsTrusted}
	}
	return solve.PuzzleProof{
		SlidePos: result.SlidePos, Data: result.Data, VerifyTimeMS: result.VerifyTimeMS,
		Completion: solve.CompletionPlan{GetterArgument: plan.Arguments[0].Value, Interactions: interactions, PostDelayMS: int(math.Floor(result.PostInteractionDelayMS + 0.5))},
	}, nil
}

func (round *round) CompleteDevice(ctx context.Context, plan solve.CompletionPlan) (string, error) {
	result, err := round.session.Complete(ctx, plan.GetterArgument, plan.Interactions, plan.PostDelayMS)
	if err != nil {
		return "", runtimeError(err, true)
	}
	if result.VerifyToken == "" || result.GetterArgumentCount != 1 || result.InteractionEventCount != len(plan.Interactions) || strings.Join(result.RequestActions, ",") != "Log1,Log2,Log3,Log2" {
		return "", dependencyError(solve.FailureProtocol, "设备完成态合同不一致", nil)
	}
	return result.VerifyToken, nil
}

func (round *round) SolveTraceless(ctx context.Context) (solve.Verification, error) {
	session, ok := round.session.(tracelessDeviceSession)
	if !ok {
		return solve.Verification{}, dependencyError(solve.FailureProtocol, "设备会话不支持无痕验证码", nil)
	}
	result, err := session.SolveTraceless(ctx, device.TracelessInput{
		SceneID: round.request.SceneID, CertifyID: round.challenge.CertifyID,
		StaticPath: round.challenge.StaticPath, CaptchaType: round.challenge.CaptchaType,
	})
	return solve.Verification{VerifyCode: result.VerifyCode, VerifyResult: result.VerifyResult, SecurityToken: result.SecurityToken, CertifyID: result.CertifyID}, runtimeError(err, false)
}
func (round *round) SolveSliding(ctx context.Context, events []track.Event, slideWidth, handleWidth int) (solve.Verification, error) {
	session, ok := round.session.(slidingDeviceSession)
	if !ok {
		return solve.Verification{}, dependencyError(solve.FailureProtocol, "设备会话不支持拖动验证码", nil)
	}
	result, err := session.SolveSliding(ctx, device.SlidingInput{
		SceneID: round.request.SceneID, CertifyID: round.challenge.CertifyID,
		StaticPath: round.challenge.StaticPath, CaptchaType: round.challenge.CaptchaType,
		Track: events, SlideWidth: slideWidth, HandleWidth: handleWidth,
	})
	return solve.Verification{VerifyCode: result.VerifyCode, VerifyResult: result.VerifyResult, SecurityToken: result.SecurityToken, CertifyID: result.CertifyID}, runtimeError(err, false)
}
func (round *round) Verify(ctx context.Context, input solve.VerificationInput) (solve.Verification, error) {
	result, err := round.rpcClient.Verify(ctx, round.challenge, input.DeviceToken, input.Data, input.VerifyTimeMS)
	return solve.Verification{VerifyCode: result.VerifyCode, VerifyResult: result.VerifyResult, SecurityToken: result.SecurityToken, CertifyID: result.CertifyID}, networkOrProtocolError(err)
}
func (round *round) CloseDevice() {
	if round.release != nil {
		release := round.release
		round.release = nil
		release()
	}
}

var _ solve.Round = (*round)(nil)
