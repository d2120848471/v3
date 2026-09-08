package engine

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/d2120848471/v3/ali-slider-go/internal/domain/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/pe"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/protocol"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/track"
)

func validateDeviceRuntimeOptions(options DeviceRuntimeOptions) error {
	if options.Prefix == "" || options.Region == "" || options.Timeout < time.Second || options.Timeout > 5*time.Minute {
		return errors.New("device labels or timeout are invalid")
	}
	if options.GatherCostMin < 0 || options.GatherCostMax < options.GatherCostMin || options.FirstTouchAgeMin < 1 || options.FirstTouchAgeMax < options.FirstTouchAgeMin {
		return errors.New("device timing bounds are invalid")
	}
	return options.Sources.Validate()
}

// acceptInitialStage 校验运行时初始阶段，并保存后续完成阶段必须沿用的会话材料。
func (session *DeviceRuntimeSession) acceptInitialStage(stage deviceBridgeStage) error {
	secrets, err := protocol.ResolveFrontendSecrets()
	if err != nil {
		return fmt.Errorf("%w: device protocol material", pe.ErrKeyRuntime)
	}
	config, err := protocolConfig(stage.DeviceConfig)
	if err != nil {
		return err
	}
	parsed, fields, collectorStarted, tokenTime, err := validateRuntimeToken(stage.DeviceToken, config, secrets.DeviceTokenSalt())
	if err != nil {
		return err
	}
	if len(fields) != 111 {
		return fmt.Errorf("%w: initial Node fingerprint field count", pe.ErrUnsupportedPE)
	}
	if err := validateVerifyProfile(stage.VerifyArgProfile); err != nil {
		return err
	}
	actions := stageActions(stage)
	if stage.RequestCount != 3 || !slices.Equal(actions, []string{"Log1", "Log2", "Log3"}) {
		return fmt.Errorf("%w: initial device request sequence", pe.ErrUnsupportedPE)
	}
	targetCost, err := session.normalizedGatherCost(parsed.GatherCost)
	if err != nil {
		return fmt.Errorf("%w: normalize GatherCost", pe.ErrKeyRuntime)
	}
	initToken := stage.DeviceToken
	if parsed.GatherCost != strconv.Itoa(targetCost) {
		initToken, err = protocol.BuildDeviceToken(parsed.SessionID, parsed.FingerprintCipher, strconv.Itoa(targetCost), secrets.DeviceTokenSalt())
		if err != nil {
			return fmt.Errorf("%w: normalize Init token", pe.ErrKeyRuntime)
		}
		parsed, err = protocol.ParseDeviceToken(initToken, secrets.DeviceTokenSalt())
		if err != nil {
			return fmt.Errorf("%w: check normalized Init token", pe.ErrKeyRuntime)
		}
	}
	session.secrets = secrets
	session.config = config
	session.verifyProfile = stage.VerifyArgProfile
	session.initToken = initToken
	session.initParsed = parsed
	session.initial = device.Result{
		InitToken: initToken, VerifyToken: initToken, RequestActions: actions,
		RequestCount: len(actions), FingerprintFieldCount: len(fields),
		CollectorStartedMS: collectorStarted, TokenTimeMS: tokenTime, GatherCost: targetCost,
	}
	return nil
}

func protocolConfig(value nativeDeviceConfig) (protocol.DeviceConfig, error) {
	if len([]byte(value.Key)) != 16 || value.SessionID == "" || value.Timestamp == "" || value.IP == "" {
		return protocol.DeviceConfig{}, fmt.Errorf("%w: Node DeviceConfig", pe.ErrUnsupportedPE)
	}
	return protocol.DeviceConfig{
		Key: value.Key, Switch: value.Switch, SessionID: value.SessionID, Version: value.Version,
		PluginElements: value.PluginElements, PluginResource: value.PluginResource,
		GlobalVariable: value.GlobalVariable, Timestamp: value.Timestamp, IP: value.IP,
		ExtraSegments: append([]string(nil), value.ExtraSegments...),
	}, nil
}

func validateVerifyProfile(value deviceVerifyArgProfile) error {
	for _, item := range []string{value.AccessSec, value.SessionIDSalt} {
		if len(item) < 1 || len(item) > 128 || !utf8.ValidString(item) {
			return fmt.Errorf("%w: Node Verify arg profile", pe.ErrUnsupportedPE)
		}
		for _, character := range []byte(item) {
			if character < 0x20 || character > 0x7e {
				return fmt.Errorf("%w: Node Verify arg profile", pe.ErrUnsupportedPE)
			}
		}
	}
	return nil
}

func validateRuntimeToken(token string, config protocol.DeviceConfig, salt string) (protocol.DeviceToken, []string, int64, int64, error) {
	parsed, err := protocol.ParseDeviceToken(token, salt)
	if err != nil || parsed.SessionID != config.SessionID {
		return protocol.DeviceToken{}, nil, 0, 0, fmt.Errorf("%w: Node DeviceToken container", pe.ErrUnsupportedPE)
	}
	plaintext, err := protocol.AESCBCDecryptBase64(parsed.FingerprintCipher, config.Key)
	if err != nil || !utf8.Valid(plaintext) {
		return protocol.DeviceToken{}, nil, 0, 0, fmt.Errorf("%w: Node DeviceToken fingerprint", pe.ErrUnsupportedPE)
	}
	fields := strings.Split(string(plaintext), "#")
	if len(fields) < 78 || len(fields) > 512 {
		return protocol.DeviceToken{}, nil, 0, 0, fmt.Errorf("%w: Node fingerprint field count", pe.ErrUnsupportedPE)
	}
	collectorStarted, err := strconv.ParseInt(fields[72], 10, 64)
	if err != nil || collectorStarted < 1 {
		return protocol.DeviceToken{}, nil, 0, 0, fmt.Errorf("%w: Node collector clock", pe.ErrUnsupportedPE)
	}
	tokenTime, err := strconv.ParseInt(fields[74], 10, 64)
	if err != nil || tokenTime < collectorStarted {
		return protocol.DeviceToken{}, nil, 0, 0, fmt.Errorf("%w: Node token clock", pe.ErrUnsupportedPE)
	}
	return parsed, fields, collectorStarted, tokenTime, nil
}

func stageActions(stage deviceBridgeStage) []string {
	actions := make([]string, len(stage.Requests))
	for index, request := range stage.Requests {
		actions[index] = request.Action
	}
	return actions
}

func (session *DeviceRuntimeSession) normalizedGatherCost(value string) (int, error) {
	cost, err := strconv.Atoi(value)
	if err != nil {
		return 0, err
	}
	if cost >= session.options.GatherCostMin {
		return cost, nil
	}
	width := session.options.GatherCostMax - session.options.GatherCostMin + 1
	random, err := session.options.Sources.Entropy.Uint64n(uint64(width))
	if err != nil {
		return 0, err
	}
	return session.options.GatherCostMin + int(random), nil
}

func validateRuntimeInteractions(events []device.InteractionEvent) error {
	if len(events) < 1 || len(events) > pe.MaximumTrackEvents {
		return errors.New("device interactions are outside bounds")
	}
	previous := -1.0
	for _, event := range events {
		if event.Type != "mousemove" || !event.IsTrusted || math.IsNaN(event.X) || math.IsInf(event.X, 0) || math.IsNaN(event.Y) || math.IsInf(event.Y, 0) || math.Abs(event.X) > 10_000 || math.Abs(event.Y) > 10_000 || math.IsNaN(event.TimeStamp) || math.IsInf(event.TimeStamp, 0) || event.TimeStamp < 0 || event.TimeStamp > nativeEventTimeLimitMS || event.TimeStamp < previous {
			return errors.New("device interaction is invalid")
		}
		previous = event.TimeStamp
	}
	if events[len(events)-1].TimeStamp-events[0].TimeStamp > float64(pe.MaximumTrackDurationMS) {
		return errors.New("device interaction span is invalid")
	}
	return nil
}

func validateTracelessInput(input device.TracelessInput) error {
	if utf8.RuneCountInString(input.SceneID) < 1 || utf8.RuneCountInString(input.SceneID) > 64 || input.CertifyID == "" || len(input.CertifyID) > 512 || !strings.EqualFold(strings.TrimSpace(input.CaptchaType), "TRACELESS") {
		return errors.New("traceless runtime input is invalid")
	}
	if input.StaticPath == "" || len(input.StaticPath) > 512 || strings.HasPrefix(input.StaticPath, "/") || strings.Contains(input.StaticPath, "://") {
		return errors.New("traceless static path is invalid")
	}
	for _, segment := range strings.Split(input.StaticPath, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return errors.New("traceless static path is invalid")
		}
	}
	return nil
}

func validateSlidingInput(input device.SlidingInput) error {
	if utf8.RuneCountInString(input.SceneID) < 1 || utf8.RuneCountInString(input.SceneID) > 64 || input.CertifyID == "" || len(input.CertifyID) > 512 || !isSlidingType(input.CaptchaType) {
		return errors.New("sliding runtime input is invalid")
	}
	if input.StaticPath == "" || len(input.StaticPath) > 512 || strings.HasPrefix(input.StaticPath, "/") || strings.Contains(input.StaticPath, "://") {
		return errors.New("sliding static path is invalid")
	}
	for _, segment := range strings.Split(input.StaticPath, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return errors.New("sliding static path is invalid")
		}
	}
	if input.SlideWidth < 320 || input.SlideWidth > 1024 || input.HandleWidth < 30 || input.HandleWidth >= input.SlideWidth {
		return errors.New("sliding dimensions are invalid")
	}
	return validateSlidingTrack(input.Track, input.SlideWidth-input.HandleWidth)
}

func validateSlidingTrack(events []track.Event, target int) error {
	if len(events) < 3 || len(events) > 512 || target < 1 {
		return errors.New("sliding track is invalid")
	}
	total := 0
	for index, event := range events {
		if event.DT < 0 || event.DT > 5_000 || event.X < -64 || event.X > target+64 || event.Y < -256 || event.Y > 256 || math.IsNaN(event.Force) || math.IsInf(event.Force, 0) || event.Force < 0 || event.Force > 1 || math.IsNaN(event.RadiusX) || math.IsInf(event.RadiusX, 0) || event.RadiusX <= 0 || event.RadiusX > 128 || math.IsNaN(event.RadiusY) || math.IsInf(event.RadiusY, 0) || event.RadiusY <= 0 || event.RadiusY > 128 {
			return errors.New("sliding track is invalid")
		}
		if index == 0 && (event.Type != "touchstart" || event.DT != 0 || event.X != 0) {
			return errors.New("sliding track is invalid")
		}
		if index > 0 && index < len(events)-1 && event.Type != "touchmove" {
			return errors.New("sliding track is invalid")
		}
		if index == len(events)-1 && (event.Type != "touchend" || event.X != target) {
			return errors.New("sliding track is invalid")
		}
		total += event.DT
		if total > 15_000 {
			return errors.New("sliding track is invalid")
		}
	}
	if events[len(events)-2].X != target {
		return errors.New("sliding track is invalid")
	}
	return nil
}

func (session *DeviceRuntimeSession) acceptTracelessStage(stage deviceBridgeStage, input device.TracelessInput) (device.TracelessResult, error) {
	config, err := protocolConfig(stage.DeviceConfig)
	if err != nil || stage.Stage != "traceless" || !equalProtocolConfig(config, session.config) || stage.VerifyArgProfile != session.verifyProfile {
		return device.TracelessResult{}, fmt.Errorf("%w: traceless device runtime changed", pe.ErrUnsupportedPE)
	}
	if !isTracelessType(stage.CaptchaType) || stage.SceneID != input.SceneID || stage.CertifyID != input.CertifyID {
		return device.TracelessResult{}, fmt.Errorf("%w: traceless challenge identity mismatch", pe.ErrUnsupportedPE)
	}
	parsedToken, _, _, _, err := validateRuntimeToken(stage.TracelessDeviceToken, session.config, session.secrets.DeviceTokenSalt())
	if err != nil || parsedToken.SessionID != session.initParsed.SessionID {
		return device.TracelessResult{}, fmt.Errorf("%w: traceless DeviceToken session mismatch", pe.ErrUnsupportedPE)
	}
	business, err := protocol.ParseBusinessCaptchaVerifyParam(stage.CaptchaVerifyParam)
	if err != nil {
		return device.TracelessResult{}, fmt.Errorf("%w: traceless success parameter encoding", pe.ErrUnsupportedPE)
	}
	if !business.IsSign {
		return device.TracelessResult{}, fmt.Errorf("%w: traceless success signature flag", pe.ErrUnsupportedPE)
	}
	if business.SceneID != input.SceneID {
		return device.TracelessResult{}, fmt.Errorf("%w: traceless success SceneId mismatch", pe.ErrUnsupportedPE)
	}
	if business.CertifyID != input.CertifyID {
		return device.TracelessResult{}, fmt.Errorf("%w: traceless success CertifyId mismatch", pe.ErrUnsupportedPE)
	}
	if stage.VerifyCode != "T001" || !stage.VerifyResult || stage.SecurityToken == "" || stage.SecurityToken != business.SecurityToken || stage.VerifyCertifyID != business.CertifyID {
		return device.TracelessResult{}, fmt.Errorf("%w: traceless official Verify result mismatch", pe.ErrUnsupportedPE)
	}
	if stage.RequestCount != len(stage.Requests) || len(stage.Requests) < 5 {
		return device.TracelessResult{}, fmt.Errorf("%w: traceless request count", pe.ErrUnsupportedPE)
	}
	actions := stageActions(stage)
	if !slices.Equal(actions[:3], []string{"Log1", "Log2", "Log3"}) {
		return device.TracelessResult{}, fmt.Errorf("%w: traceless initial request sequence", pe.ErrUnsupportedPE)
	}
	initIndex, verifyIndex := -1, -1
	for index, request := range stage.Requests {
		switch request.Action {
		case "InitCaptchaV3":
			if initIndex >= 0 || !containsAllStrings(request.FieldNames, "SceneId", "DeviceToken") {
				return device.TracelessResult{}, fmt.Errorf("%w: traceless Init request", pe.ErrUnsupportedPE)
			}
			initIndex = index
		case "VerifyCaptchaV3":
			if verifyIndex >= 0 || !containsAllStrings(request.FieldNames, "SceneId", "CertifyId", "CaptchaVerifyParam") {
				return device.TracelessResult{}, fmt.Errorf("%w: traceless Verify request", pe.ErrUnsupportedPE)
			}
			verifyIndex = index
		}
	}
	if initIndex < 3 || verifyIndex <= initIndex {
		return device.TracelessResult{}, fmt.Errorf("%w: traceless request order", pe.ErrUnsupportedPE)
	}
	return device.TracelessResult{
		SecurityToken:  stage.SecurityToken,
		VerifyCode:     stage.VerifyCode,
		VerifyResult:   stage.VerifyResult,
		CertifyID:      stage.VerifyCertifyID,
		RequestActions: actions,
	}, nil
}

func (session *DeviceRuntimeSession) acceptSlidingStage(stage deviceBridgeStage, input device.SlidingInput) (device.SlidingResult, error) {
	config, err := protocolConfig(stage.DeviceConfig)
	if err != nil || stage.Stage != "sliding" || !equalProtocolConfig(config, session.config) || stage.VerifyArgProfile != session.verifyProfile {
		return device.SlidingResult{}, fmt.Errorf("%w: sliding device runtime changed", pe.ErrUnsupportedPE)
	}
	if !isSlidingType(stage.CaptchaType) || stage.SceneID != input.SceneID || stage.CertifyID != input.CertifyID {
		return device.SlidingResult{}, fmt.Errorf("%w: sliding challenge identity mismatch", pe.ErrUnsupportedPE)
	}
	parsedToken, _, _, _, err := validateRuntimeToken(stage.SlidingDeviceToken, session.config, session.secrets.DeviceTokenSalt())
	if err != nil || parsedToken.SessionID != session.initParsed.SessionID {
		return device.SlidingResult{}, fmt.Errorf("%w: sliding DeviceToken session mismatch", pe.ErrUnsupportedPE)
	}
	if len(stage.CaptchaVerifyParam) <= 140 || len(stage.CaptchaVerifyParam) > 16*1024 {
		return device.SlidingResult{}, fmt.Errorf("%w: sliding success parameter length", pe.ErrUnsupportedPE)
	}
	business, err := protocol.ParseBusinessCaptchaVerifyParam(stage.CaptchaVerifyParam)
	if err != nil || !business.IsSign || business.SceneID != input.SceneID || business.CertifyID != input.CertifyID || business.SecurityToken == "" {
		return device.SlidingResult{}, fmt.Errorf("%w: sliding success parameter mismatch", pe.ErrUnsupportedPE)
	}
	if stage.VerifyCode != "T001" || !stage.VerifyResult || stage.SecurityToken == "" || stage.SecurityToken != business.SecurityToken || stage.VerifyCertifyID != business.CertifyID {
		return device.SlidingResult{}, fmt.Errorf("%w: sliding official Verify result mismatch", pe.ErrUnsupportedPE)
	}
	if stage.RequestCount != len(stage.Requests) || len(stage.Requests) < 5 {
		return device.SlidingResult{}, fmt.Errorf("%w: sliding request count", pe.ErrUnsupportedPE)
	}
	actions := stageActions(stage)
	if !slices.Equal(actions[:3], []string{"Log1", "Log2", "Log3"}) {
		return device.SlidingResult{}, fmt.Errorf("%w: sliding initial request sequence", pe.ErrUnsupportedPE)
	}
	initIndex, verifyIndex := -1, -1
	for index, request := range stage.Requests {
		switch request.Action {
		case "InitCaptchaV3":
			if initIndex >= 0 || !containsAllStrings(request.FieldNames, "SceneId", "DeviceToken") {
				return device.SlidingResult{}, fmt.Errorf("%w: sliding Init request", pe.ErrUnsupportedPE)
			}
			initIndex = index
		case "VerifyCaptchaV3":
			if verifyIndex >= 0 || !containsAllStrings(request.FieldNames, "SceneId", "CertifyId", "CaptchaVerifyParam") {
				return device.SlidingResult{}, fmt.Errorf("%w: sliding Verify request", pe.ErrUnsupportedPE)
			}
			verifyIndex = index
		}
	}
	if initIndex < 3 || verifyIndex <= initIndex {
		return device.SlidingResult{}, fmt.Errorf("%w: sliding request order", pe.ErrUnsupportedPE)
	}
	return device.SlidingResult{
		SecurityToken: stage.SecurityToken,
		VerifyCode:    stage.VerifyCode, VerifyResult: stage.VerifyResult, CertifyID: stage.VerifyCertifyID,
		RequestActions: actions,
	}, nil
}

func isTracelessType(value string) bool {
	return strings.EqualFold(strings.TrimSpace(value), "TRACELESS")
}

func isSlidingType(value string) bool {
	return strings.EqualFold(strings.TrimSpace(value), "SLIDING")
}

func containsAllStrings(values []string, required ...string) bool {
	for _, item := range required {
		if !slices.Contains(values, item) {
			return false
		}
	}
	return true
}

func (session *DeviceRuntimeSession) acceptVerifyStage(stage deviceBridgeStage, eventCount int) (device.Result, error) {
	config, err := protocolConfig(stage.DeviceConfig)
	if err != nil || !equalProtocolConfig(config, session.config) || stage.VerifyArgProfile != session.verifyProfile {
		return device.Result{}, fmt.Errorf("%w: Verify device runtime changed", pe.ErrUnsupportedPE)
	}
	if stage.GetterArgumentCount == nil || *stage.GetterArgumentCount != 1 || stage.InteractionEventCount == nil || *stage.InteractionEventCount != eventCount {
		return device.Result{}, fmt.Errorf("%w: Verify device completion count", pe.ErrUnsupportedPE)
	}
	actions := stageActions(stage)
	if stage.RequestCount != 4 || !slices.Equal(actions, []string{"Log1", "Log2", "Log3", "Log2"}) {
		return device.Result{}, fmt.Errorf("%w: Verify device request sequence", pe.ErrUnsupportedPE)
	}
	parsed, fields, collectorStarted, tokenTime, err := validateRuntimeToken(stage.VerifyDeviceToken, session.config, session.secrets.DeviceTokenSalt())
	if err != nil {
		return device.Result{}, err
	}
	if parsed.SessionID != session.initParsed.SessionID {
		return device.Result{}, fmt.Errorf("%w: Verify DeviceToken session mismatch", pe.ErrUnsupportedPE)
	}
	verifyToken := stage.VerifyDeviceToken
	if parsed.GatherCost != strconv.Itoa(session.initial.GatherCost) {
		verifyToken, err = protocol.BuildDeviceToken(parsed.SessionID, parsed.FingerprintCipher, strconv.Itoa(session.initial.GatherCost), session.secrets.DeviceTokenSalt())
		if err != nil {
			return device.Result{}, fmt.Errorf("%w: normalize Verify token", pe.ErrKeyRuntime)
		}
		if _, err := protocol.ParseDeviceToken(verifyToken, session.secrets.DeviceTokenSalt()); err != nil {
			return device.Result{}, fmt.Errorf("%w: check normalized Verify token", pe.ErrKeyRuntime)
		}
	}
	return device.Result{
		InitToken: session.initToken, VerifyToken: verifyToken, RequestActions: actions,
		RequestCount: len(actions), FingerprintFieldCount: len(fields),
		GetterArgumentCount: 1, InteractionEventCount: eventCount,
		CollectorStartedMS: collectorStarted, TokenTimeMS: tokenTime,
		GatherCost: session.initial.GatherCost,
	}, nil
}

func equalProtocolConfig(left, right protocol.DeviceConfig) bool {
	return left.Key == right.Key && left.Switch == right.Switch && left.SessionID == right.SessionID && left.Version == right.Version && left.PluginElements == right.PluginElements && left.PluginResource == right.PluginResource && left.GlobalVariable == right.GlobalVariable && left.Timestamp == right.Timestamp && left.IP == right.IP && slices.Equal(left.ExtraSegments, right.ExtraSegments)
}
