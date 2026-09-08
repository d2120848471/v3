package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/domain/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/pe"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/protocol"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/track"
	"github.com/d2120848471/v3/ali-slider-go/internal/foundation/runtimekit"
)

type runtimeTestEntropy struct {
	value uint64
	err   error
}

func (entropy runtimeTestEntropy) Read(buffer []byte) (int, error) {
	if entropy.err != nil {
		return 0, entropy.err
	}
	clear(buffer)
	return len(buffer), nil
}

func (entropy runtimeTestEntropy) Uint64n(limit uint64) (uint64, error) {
	if entropy.err != nil {
		return 0, entropy.err
	}
	if limit == 0 {
		return 0, errors.New("zero limit")
	}
	return entropy.value % limit, nil
}

func validDeviceRuntimeOptions() DeviceRuntimeOptions {
	return DeviceRuntimeOptions{
		Prefix: "fsgtmi", Region: "cn", Timeout: 2 * time.Second,
		GatherCostMin: 50, GatherCostMax: 60, FirstTouchAgeMin: 600, FirstTouchAgeMax: 700,
		Sources: runtimekit.Sources{
			Clock:   fixedClock{now: time.UnixMilli(2_000_000_000_000)},
			Entropy: runtimeTestEntropy{value: 3},
		},
	}
}

func validRuntimeProtocolConfig() protocol.DeviceConfig {
	return protocol.DeviceConfig{
		Key: "0123456789abcdef", Switch: 1, SessionID: "runtime-session", Version: "1",
		PluginElements: "elements", PluginResource: "resource", GlobalVariable: "global",
		Timestamp: "2000000000000", IP: "127.0.0.1", ExtraSegments: []string{"extra"},
	}
}

func nativeConfigFromProtocol(config protocol.DeviceConfig) nativeDeviceConfig {
	return nativeDeviceConfig{
		Key: config.Key, Switch: config.Switch, SessionID: config.SessionID, Version: config.Version,
		PluginElements: config.PluginElements, PluginResource: config.PluginResource,
		GlobalVariable: config.GlobalVariable, Timestamp: config.Timestamp, IP: config.IP,
		ExtraSegments: append([]string(nil), config.ExtraSegments...),
	}
}

func runtimeTokenWithFields(t *testing.T, config protocol.DeviceConfig, count int, gatherCost string, collectorStarted, tokenTime int64) string {
	t.Helper()
	fields := make([]string, count)
	for index := range fields {
		fields[index] = "v"
	}
	fields[72] = stringInt64(collectorStarted)
	fields[74] = stringInt64(tokenTime)
	ciphertext, err := protocol.AESCBCEncryptBase64([]byte(strings.Join(fields, "#")), config.Key)
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := protocol.ResolveFrontendSecrets()
	if err != nil {
		t.Fatal(err)
	}
	token, err := protocol.BuildDeviceToken(config.SessionID, ciphertext, gatherCost, secrets.DeviceTokenSalt())
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func stringInt64(value int64) string {
	return strconv.FormatInt(value, 10)
}

func validInitialDeviceStage(t *testing.T, gatherCost string) deviceBridgeStage {
	t.Helper()
	config := validRuntimeProtocolConfig()
	return deviceBridgeStage{
		Stage: "init", DeviceToken: runtimeTokenWithFields(t, config, 111, gatherCost, 2_000_000_000_000, 2_000_000_000_055),
		DeviceConfig:     nativeConfigFromProtocol(config),
		VerifyArgProfile: deviceVerifyArgProfile{AccessSec: "access-sec", SessionIDSalt: "session-salt"},
		TokenSource:      "deviceCallback", RequestCount: 3,
		Requests: []deviceRequestSummary{{Action: "Log1"}, {Action: "Log2"}, {Action: "Log3"}},
	}
}

func validVerifyDeviceStage(t *testing.T, gatherCost string, eventCount int) deviceBridgeStage {
	t.Helper()
	config := validRuntimeProtocolConfig()
	getterCount := 1
	return deviceBridgeStage{
		Stage: "verify", VerifyDeviceToken: runtimeTokenWithFields(t, config, 142, gatherCost, 2_000_000_000_000, 2_000_000_000_850),
		DeviceConfig:     nativeConfigFromProtocol(config),
		VerifyArgProfile: deviceVerifyArgProfile{AccessSec: "access-sec", SessionIDSalt: "session-salt"},
		TokenSource:      "pe-getToken", GetterArgumentCount: &getterCount, InteractionEventCount: &eventCount,
		RequestCount: 4,
		Requests:     []deviceRequestSummary{{Action: "Log1"}, {Action: "Log2"}, {Action: "Log3"}, {Action: "Log2"}},
	}
}

func validTracelessDeviceStage(t *testing.T, sceneID, certifyID string) deviceBridgeStage {
	t.Helper()
	config := validRuntimeProtocolConfig()
	parameter, err := protocol.BuildBusinessCaptchaVerifyParam(certifyID, sceneID, "fixture-security-token", true)
	if err != nil {
		t.Fatal(err)
	}
	return deviceBridgeStage{
		Stage: "traceless", CaptchaType: "TRACELESS", SceneID: sceneID,
		CertifyID: certifyID, CaptchaVerifyParam: parameter,
		TracelessDeviceToken: runtimeTokenWithFields(t, config, 111, "53", 2_000_000_000_000, 2_000_000_000_900),
		VerifyCode:           "T001",
		VerifyResult:         true,
		SecurityToken:        "fixture-security-token",
		VerifyCertifyID:      certifyID,
		DeviceConfig:         nativeConfigFromProtocol(config),
		VerifyArgProfile:     deviceVerifyArgProfile{AccessSec: "access-sec", SessionIDSalt: "session-salt"},
		RequestCount:         5,
		Requests: []deviceRequestSummary{
			{Action: "Log1"}, {Action: "Log2"}, {Action: "Log3"},
			{Action: "InitCaptchaV3", FieldNames: []string{"SceneId", "DeviceToken"}},
			{Action: "VerifyCaptchaV3", FieldNames: []string{"SceneId", "CertifyId", "CaptchaVerifyParam"}},
		},
	}
}

func validSlidingTrack() []track.Event {
	return []track.Event{
		{Type: "touchstart", X: 0, Y: 1, DT: 0, Force: 0.6, RadiusX: 12, RadiusY: 11},
		{Type: "touchmove", X: 180, Y: 2, DT: 180, Force: 0.62, RadiusX: 13, RadiusY: 12},
		{Type: "touchmove", X: 370, Y: 1, DT: 220, Force: 0.58, RadiusX: 12, RadiusY: 12},
		{Type: "touchend", X: 370, Y: 1, DT: 60, Force: 0.55, RadiusX: 12, RadiusY: 11},
	}
}

func validSlidingInput(sceneID, certifyID string) device.SlidingInput {
	return device.SlidingInput{
		SceneID: sceneID, CertifyID: certifyID,
		StaticPath: "3.22.0/sg.092.fixture.js", CaptchaType: "SLIDING",
		Track: validSlidingTrack(), SlideWidth: 418, HandleWidth: 48,
	}
}

func validSlidingDeviceStage(t *testing.T, sceneID, certifyID string) deviceBridgeStage {
	t.Helper()
	config := validRuntimeProtocolConfig()
	parameter, err := protocol.BuildBusinessCaptchaVerifyParam(certifyID, sceneID, "fixture-upload-security-token", true)
	if err != nil {
		t.Fatal(err)
	}
	return deviceBridgeStage{
		Stage: "sliding", CaptchaType: "SLIDING", SceneID: sceneID,
		CertifyID: certifyID, CaptchaVerifyParam: parameter,
		SlidingDeviceToken: runtimeTokenWithFields(t, config, 111, "53", 2_000_000_000_000, 2_000_000_000_900),
		VerifyCode:         "T001",
		VerifyResult:       true,
		SecurityToken:      "fixture-upload-security-token",
		VerifyCertifyID:    certifyID,
		DeviceConfig:       nativeConfigFromProtocol(config),
		VerifyArgProfile:   deviceVerifyArgProfile{AccessSec: "access-sec", SessionIDSalt: "session-salt"},
		RequestCount:       5,
		Requests: []deviceRequestSummary{
			{Action: "Log1"}, {Action: "Log2"}, {Action: "Log3"},
			{Action: "InitCaptchaV3", FieldNames: []string{"SceneId", "DeviceToken"}},
			{Action: "VerifyCaptchaV3", FieldNames: []string{"SceneId", "CertifyId", "CaptchaVerifyParam"}},
		},
	}
}

func newAcceptedRuntimeSession(t *testing.T, gatherCost string) *DeviceRuntimeSession {
	t.Helper()
	session := &DeviceRuntimeSession{options: validDeviceRuntimeOptions()}
	if err := session.acceptInitialStage(validInitialDeviceStage(t, gatherCost)); err != nil {
		t.Fatal(err)
	}
	return session
}

func TestDeviceRuntimeStageValidationAndAccessors(t *testing.T) {
	session := newAcceptedRuntimeSession(t, "1")
	if session.initial.GatherCost != 53 {
		t.Fatalf("normalized GatherCost=%d", session.initial.GatherCost)
	}
	if token, err := session.InitToken(); err != nil || token == "" {
		t.Fatalf("InitToken=%q err=%v", token, err)
	}
	config, err := session.PEDeviceConfig()
	if err != nil || config.SessionID != "runtime-session" {
		t.Fatalf("config=%+v err=%v", config, err)
	}
	config.ExtraSegments[0] = "changed"
	if session.config.ExtraSegments[0] != "extra" {
		t.Fatal("PEDeviceConfig leaked mutable storage")
	}
	access, salt, err := session.PEVerifyArgProfile()
	if err != nil || access != "access-sec" || salt != "session-salt" {
		t.Fatalf("profile=%q/%q err=%v", access, salt, err)
	}
	age, err := session.TargetFirstTouchAgeMS()
	if err != nil || age != 603 {
		t.Fatalf("first touch age=%d err=%v", age, err)
	}
	if cached, _ := session.TargetFirstTouchAgeMS(); cached != age {
		t.Fatal("first touch age was not stable within the challenge")
	}
	result, err := session.acceptVerifyStage(validVerifyDeviceStage(t, "2", 2), 2)
	if err != nil || result.GatherCost != 53 || result.FingerprintFieldCount != 142 || result.VerifyToken == "" {
		t.Fatalf("verify result=%+v err=%v", result, err)
	}
}

func TestDeviceRuntimeRejectsInvalidStages(t *testing.T) {
	initialTests := []struct {
		name   string
		mutate func(*deviceBridgeStage)
	}{
		{"config", func(stage *deviceBridgeStage) { stage.DeviceConfig.Key = "short" }},
		{"token", func(stage *deviceBridgeStage) { stage.DeviceToken = "invalid" }},
		{"field count", func(stage *deviceBridgeStage) {
			stage.DeviceToken = runtimeTokenWithFields(t, validRuntimeProtocolConfig(), 110, "50", 1, 2)
		}},
		{"verify profile", func(stage *deviceBridgeStage) { stage.VerifyArgProfile.AccessSec = "" }},
		{"actions", func(stage *deviceBridgeStage) { stage.Requests[2].Action = "Log2" }},
	}
	for _, test := range initialTests {
		t.Run("init "+test.name, func(t *testing.T) {
			stage := validInitialDeviceStage(t, "50")
			test.mutate(&stage)
			session := &DeviceRuntimeSession{options: validDeviceRuntimeOptions()}
			if err := session.acceptInitialStage(stage); !errors.Is(err, pe.ErrUnsupportedPE) {
				t.Fatalf("error=%v", err)
			}
		})
	}

	verifyTests := []struct {
		name   string
		mutate func(*deviceBridgeStage)
	}{
		{"config", func(stage *deviceBridgeStage) { stage.DeviceConfig.IP = "other" }},
		{"count", func(stage *deviceBridgeStage) { stage.GetterArgumentCount = nil }},
		{"actions", func(stage *deviceBridgeStage) { stage.RequestCount = 3 }},
		{"token", func(stage *deviceBridgeStage) { stage.VerifyDeviceToken = "invalid" }},
	}
	for _, test := range verifyTests {
		t.Run("verify "+test.name, func(t *testing.T) {
			session := newAcceptedRuntimeSession(t, "53")
			stage := validVerifyDeviceStage(t, "53", 2)
			test.mutate(&stage)
			if _, err := session.acceptVerifyStage(stage, 2); !errors.Is(err, pe.ErrUnsupportedPE) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestDeviceRuntimeAcceptsBoundTracelessStage(t *testing.T) {
	const sceneID, certifyID = "wa3238du", "fixture-traceless-certify"
	input := device.TracelessInput{
		SceneID: sceneID, CertifyID: certifyID,
		StaticPath: "3.29.0/pe.091.fixture.js", CaptchaType: "traceless",
	}
	session := newAcceptedRuntimeSession(t, "53")
	result, err := session.acceptTracelessStage(validTracelessDeviceStage(t, sceneID, certifyID), input)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Succeeded() || result.CertifyID != certifyID || result.VerifyCode != "T001" || strings.Join(result.RequestActions, ",") != "Log1,Log2,Log3,InitCaptchaV3,VerifyCaptchaV3" {
		t.Fatalf("result=%+v", result)
	}
	for _, formatted := range []string{result.String(), result.GoString()} {
		if strings.Contains(formatted, certifyID) || strings.Contains(formatted, result.SecurityToken) {
			t.Fatalf("traceless result formatting leaked secrets: %s", formatted)
		}
	}
}

func TestDeviceRuntimeRejectsMismatchedTracelessStages(t *testing.T) {
	const sceneID, certifyID = "wa3238du", "fixture-traceless-certify"
	input := device.TracelessInput{
		SceneID: sceneID, CertifyID: certifyID,
		StaticPath: "3.29.0/pe.091.fixture.js", CaptchaType: "TRACELESS",
	}
	tests := []struct {
		name   string
		mutate func(*deviceBridgeStage)
	}{
		{"scene", func(stage *deviceBridgeStage) { stage.SceneID = "other" }},
		{"certify", func(stage *deviceBridgeStage) { stage.CertifyID = "other" }},
		{"config", func(stage *deviceBridgeStage) { stage.DeviceConfig.IP = "other" }},
		{"order", func(stage *deviceBridgeStage) {
			stage.Requests[3], stage.Requests[4] = stage.Requests[4], stage.Requests[3]
		}},
		{"fields", func(stage *deviceBridgeStage) { stage.Requests[4].FieldNames = []string{"SceneId"} }},
		{"parameter", func(stage *deviceBridgeStage) { stage.CaptchaVerifyParam = "invalid" }},
		{"official token", func(stage *deviceBridgeStage) { stage.SecurityToken = "other" }},
		{"official certify", func(stage *deviceBridgeStage) { stage.VerifyCertifyID = "other" }},
		{"official code", func(stage *deviceBridgeStage) { stage.VerifyCode = "F001" }},
		{"device token", func(stage *deviceBridgeStage) { stage.TracelessDeviceToken = "invalid" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session := newAcceptedRuntimeSession(t, "53")
			stage := validTracelessDeviceStage(t, sceneID, certifyID)
			test.mutate(&stage)
			if _, err := session.acceptTracelessStage(stage, input); !errors.Is(err, pe.ErrUnsupportedPE) {
				t.Fatalf("error=%v", err)
			}
		})
	}
	for _, invalid := range []device.TracelessInput{
		{},
		{SceneID: sceneID, CertifyID: certifyID, StaticPath: "../bad.js", CaptchaType: "TRACELESS"},
		{SceneID: sceneID, CertifyID: certifyID, StaticPath: "valid.js", CaptchaType: "PUZZLE"},
	} {
		if validateTracelessInput(invalid) == nil {
			t.Fatalf("invalid input accepted: %+v", invalid)
		}
	}
}

func TestDeviceRuntimeAcceptsBoundSlidingStage(t *testing.T) {
	const sceneID, certifyID = "159tlu75", "fixture-sliding-certify"
	input := validSlidingInput(sceneID, certifyID)
	session := newAcceptedRuntimeSession(t, "53")
	result, err := session.acceptSlidingStage(validSlidingDeviceStage(t, sceneID, certifyID), input)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Succeeded() || result.CertifyID != certifyID || result.VerifyCode != "T001" || result.SecurityToken != "fixture-upload-security-token" || strings.Join(result.RequestActions, ",") != "Log1,Log2,Log3,InitCaptchaV3,VerifyCaptchaV3" {
		t.Fatalf("result=%+v", result)
	}
	for _, formatted := range []string{result.String(), result.GoString()} {
		if strings.Contains(formatted, certifyID) || strings.Contains(formatted, result.SecurityToken) {
			t.Fatalf("sliding result formatting leaked secrets: %s", formatted)
		}
	}
}

func TestDeviceRuntimeRejectsMismatchedSlidingStages(t *testing.T) {
	const sceneID, certifyID = "159tlu75", "fixture-sliding-certify"
	input := validSlidingInput(sceneID, certifyID)
	tests := []struct {
		name   string
		mutate func(*deviceBridgeStage)
	}{
		{"scene", func(stage *deviceBridgeStage) { stage.SceneID = "other" }},
		{"certify", func(stage *deviceBridgeStage) { stage.CertifyID = "other" }},
		{"config", func(stage *deviceBridgeStage) { stage.DeviceConfig.IP = "other" }},
		{"parameter", func(stage *deviceBridgeStage) { stage.CaptchaVerifyParam = "invalid" }},
		{"verify result", func(stage *deviceBridgeStage) { stage.SecurityToken = "other" }},
		{"device token", func(stage *deviceBridgeStage) { stage.SlidingDeviceToken = "invalid" }},
		{"duplicate verify", func(stage *deviceBridgeStage) {
			stage.Requests = append(stage.Requests, deviceRequestSummary{Action: "VerifyCaptchaV3"})
			stage.RequestCount++
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session := newAcceptedRuntimeSession(t, "53")
			stage := validSlidingDeviceStage(t, sceneID, certifyID)
			test.mutate(&stage)
			if _, err := session.acceptSlidingStage(stage, input); !errors.Is(err, pe.ErrUnsupportedPE) {
				t.Fatalf("error=%v", err)
			}
		})
	}
	for _, invalid := range []device.SlidingInput{
		{},
		{SceneID: sceneID, CertifyID: certifyID, StaticPath: "../bad.js", CaptchaType: "SLIDING", Track: validSlidingTrack(), SlideWidth: 418, HandleWidth: 48},
		{SceneID: sceneID, CertifyID: certifyID, StaticPath: "valid.js", CaptchaType: "TRACELESS", Track: validSlidingTrack(), SlideWidth: 418, HandleWidth: 48},
		{SceneID: sceneID, CertifyID: certifyID, StaticPath: "valid.js", CaptchaType: "SLIDING", Track: validSlidingTrack(), SlideWidth: 48, HandleWidth: 48},
	} {
		if validateSlidingInput(invalid) == nil {
			t.Fatalf("invalid input accepted: %+v", invalid)
		}
	}
}

func TestDeviceRuntimePrimitiveValidation(t *testing.T) {
	options := validDeviceRuntimeOptions()
	if err := validateDeviceRuntimeOptions(options); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*DeviceRuntimeOptions){
		func(value *DeviceRuntimeOptions) { value.Prefix = "" },
		func(value *DeviceRuntimeOptions) { value.Timeout = 0 },
		func(value *DeviceRuntimeOptions) { value.GatherCostMax = value.GatherCostMin - 1 },
		func(value *DeviceRuntimeOptions) { value.FirstTouchAgeMin = 0 },
		func(value *DeviceRuntimeOptions) { value.Sources = runtimekit.Sources{} },
	} {
		value := options
		mutate(&value)
		if validateDeviceRuntimeOptions(value) == nil {
			t.Fatal("invalid runtime options accepted")
		}
	}
	if _, err := protocolConfig(nativeDeviceConfig{Key: "short"}); !errors.Is(err, pe.ErrUnsupportedPE) {
		t.Fatalf("invalid config: %v", err)
	}
	if config, err := protocolConfig(nativeConfigFromProtocol(validRuntimeProtocolConfig())); err != nil || config.SessionID == "" {
		t.Fatalf("valid config=%+v err=%v", config, err)
	}
	for _, profile := range []deviceVerifyArgProfile{{}, {AccessSec: "ok", SessionIDSalt: "\x01"}, {AccessSec: strings.Repeat("x", 129), SessionIDSalt: "ok"}} {
		if validateVerifyProfile(profile) == nil {
			t.Fatalf("invalid verify profile accepted: %+v", profile)
		}
	}
	config := validRuntimeProtocolConfig()
	token := runtimeTokenWithFields(t, config, 111, "53", 10, 20)
	parsed, fields, started, tokenTime, err := validateRuntimeToken(token, config, mustDeviceSalt(t))
	if err != nil || parsed.SessionID != config.SessionID || len(fields) != 111 || started != 10 || tokenTime != 20 {
		t.Fatalf("token=%+v fields=%d clocks=%d/%d err=%v", parsed, len(fields), started, tokenTime, err)
	}
	wrongConfig := config
	wrongConfig.SessionID = "other"
	if _, _, _, _, err := validateRuntimeToken(token, wrongConfig, mustDeviceSalt(t)); !errors.Is(err, pe.ErrUnsupportedPE) {
		t.Fatalf("session mismatch: %v", err)
	}
	badClock := runtimeTokenWithFields(t, config, 111, "53", 20, 10)
	if _, _, _, _, err := validateRuntimeToken(badClock, config, mustDeviceSalt(t)); !errors.Is(err, pe.ErrUnsupportedPE) {
		t.Fatalf("bad clock: %v", err)
	}
}

func mustDeviceSalt(t *testing.T) string {
	t.Helper()
	secrets, err := protocol.ResolveFrontendSecrets()
	if err != nil {
		t.Fatal(err)
	}
	return secrets.DeviceTokenSalt()
}

func TestValidateRuntimeInteractions(t *testing.T) {
	valid := []device.InteractionEvent{
		{Type: "mousemove", X: 1, Y: 2, TimeStamp: 3, IsTrusted: true},
		{Type: "mousemove", X: 2, Y: 3, TimeStamp: 4, IsTrusted: true},
	}
	if err := validateRuntimeInteractions(valid); err != nil {
		t.Fatal(err)
	}
	invalid := [][]device.InteractionEvent{
		nil,
		{{Type: "click", IsTrusted: true}},
		{{Type: "mousemove", X: 10_001, IsTrusted: true}},
		{{Type: "mousemove", TimeStamp: -1, IsTrusted: true}},
		{{Type: "mousemove", TimeStamp: 2, IsTrusted: true}, {Type: "mousemove", TimeStamp: 1, IsTrusted: true}},
		{{Type: "mousemove", TimeStamp: 0, IsTrusted: true}, {Type: "mousemove", TimeStamp: float64(pe.MaximumTrackDurationMS + 1), IsTrusted: true}},
	}
	for _, events := range invalid {
		if validateRuntimeInteractions(events) == nil {
			t.Fatalf("invalid interactions accepted: %+v", events)
		}
	}
}

func TestReadDeviceBridgeStage(t *testing.T) {
	stage := validInitialDeviceStage(t, "53")
	encoded, err := json.Marshal(stage)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(strings.NewReader("public script debug\n" + string(encoded) + "\n"))
	got, err := readDeviceBridgeStage(reader, "init")
	if err != nil || got.Stage != "init" {
		t.Fatalf("stage=%+v err=%v", got, err)
	}
	for _, text := range []string{"", "{}\n", "{\"stage\":\"init\",\"unknown\":1}\n"} {
		if _, err := readDeviceBridgeStage(bufio.NewReader(strings.NewReader(text)), "init"); !errors.Is(err, pe.ErrKeyRuntime) {
			t.Fatalf("invalid stage %q: %v", text, err)
		}
	}
}

func TestNodeProxyEnvironmentAndConnectRelay(t *testing.T) {
	if environment, enabled, relay, err := nodeProxyEnvironment("", http.DefaultTransport); err != nil || enabled || relay != nil || len(environment) != 0 {
		t.Fatalf("direct env=%v enabled=%v relay=%v err=%v", environment, enabled, relay, err)
	}
	environment, enabled, relay, err := nodeProxyEnvironment("127.0.0.1:8080", http.DefaultTransport)
	if err != nil || !enabled || relay != nil || len(environment) != 4 || !strings.Contains(environment[0], "127.0.0.1:8080") {
		t.Fatalf("HTTP env=%v enabled=%v relay=%v err=%v", environment, enabled, relay, err)
	}
	for _, proxy := range []string{"http://%", "ftp://127.0.0.1:21"} {
		if _, _, relay, err := nodeProxyEnvironment(proxy, http.DefaultTransport); err == nil || relay != nil {
			t.Fatalf("invalid proxy %q accepted", proxy)
		}
	}
	if _, _, relay, err := nodeProxyEnvironment("socks5://127.0.0.1:1080", newScriptTransport()); err == nil || relay != nil {
		t.Fatal("SOCKS proxy accepted a transport without DialContext")
	}
	transport := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("unused")
	}}
	environment, enabled, relay, err = nodeProxyEnvironment("socks5://127.0.0.1:1080", transport)
	if err != nil || !enabled || relay == nil || !strings.HasPrefix(environment[0], "HTTP_PROXY=http://127.0.0.1:") {
		t.Fatalf("SOCKS env=%v enabled=%v relay=%v err=%v", environment, enabled, relay, err)
	}
	relay.Close()
	relay.Close()
	var nilRelay *connectRelay
	nilRelay.Close()
	if _, err := startConnectRelay(nil); err == nil {
		t.Fatal("nil relay dialer accepted")
	}
}

func TestConnectRelayTunnelsOnlyAllowedHTTPSHostsAndClosesActiveConnections(t *testing.T) {
	upstreamListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	upstreamDone := make(chan struct{})
	go func() {
		defer close(upstreamDone)
		connection, acceptErr := upstreamListener.Accept()
		if acceptErr == nil {
			_, _ = io.Copy(connection, connection)
			_ = connection.Close()
		}
	}()
	relay, err := startConnectRelay(func(ctx context.Context, network, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, upstreamListener.Addr().String())
	})
	if err != nil {
		t.Fatal(err)
	}

	forbidden, err := net.Dial("tcp", relay.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(forbidden, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")
	status, _ := bufio.NewReader(forbidden).ReadString('\n')
	_ = forbidden.Close()
	if !strings.Contains(status, "403") {
		t.Fatalf("forbidden status=%q", status)
	}

	client, err := net.Dial("tcp", relay.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(client)
	_, _ = io.WriteString(client, "CONNECT x.alicdn.com:443 HTTP/1.1\r\nHost: x.alicdn.com:443\r\n\r\n")
	status, err = reader.ReadString('\n')
	if err != nil || !strings.Contains(status, "200") {
		t.Fatalf("CONNECT status=%q err=%v", status, err)
	}
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Fatal(readErr)
		}
		if line == "\r\n" {
			break
		}
	}
	if _, err := client.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, 4)
	if _, err := io.ReadFull(reader, echo); err != nil || string(echo) != "ping" {
		t.Fatalf("echo=%q err=%v", echo, err)
	}

	closed := make(chan struct{})
	go func() {
		relay.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("relay Close blocked with an active tunnel")
	}
	_ = client.Close()
	_ = upstreamListener.Close()
	select {
	case <-upstreamDone:
	case <-time.After(time.Second):
		t.Fatal("upstream connection did not close")
	}
}

func writeDeviceRuntimeStub(t *testing.T, initial, verify deviceBridgeStage) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable fixture")
	}
	initialJSON, err := json.Marshal(initial)
	if err != nil {
		t.Fatal(err)
	}
	verifyJSON, err := json.Marshal(verify)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(string(initialJSON)+string(verifyJSON), "'") {
		t.Fatal("stub output contains a shell quote")
	}
	path := filepath.Join(t.TempDir(), "node-device-stub")
	script := "#!/bin/sh\nprintf '%s\\n' '" + string(initialJSON) + "'\nIFS= read -r completion || true\nprintf '%s\\n' '" + string(verifyJSON) + "'\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOpenDeviceKeepsOneProcessThroughCompletion(t *testing.T) {
	events := []device.InteractionEvent{
		{Type: "mousemove", X: 1, Y: 2, TimeStamp: 700, IsTrusted: true},
		{Type: "mousemove", X: 2, Y: 3, TimeStamp: 750, IsTrusted: true},
	}
	stub := writeDeviceRuntimeStub(t, validInitialDeviceStage(t, "53"), validVerifyDeviceStage(t, "53", len(events)))
	resolver := newNodeKeyResolver(stub)
	resolver.sdk = cachedSDK{source: []byte("cached-sdk"), fetchedAt: time.Now()}
	session, err := resolver.OpenDevice(context.Background(), newScriptTransport(), device.Profile{}, validDeviceRuntimeOptions())
	if err != nil {
		t.Fatal(err)
	}
	tempDir := session.tempDir
	stdoutReader := session.stdoutCloser
	if _, err := session.InitToken(); err != nil {
		t.Fatal(err)
	}
	sdkSource, err := session.PESDKSource()
	if err != nil || string(sdkSource) != "cached-sdk" {
		session.Close()
		t.Fatalf("SDK source=%q err=%v", sdkSource, err)
	}
	sdkSource[0] = 'X'
	if session.sdkSource[0] == 'X' {
		session.Close()
		t.Fatal("PESDKSource leaked mutable storage")
	}
	result, err := session.Complete(context.Background(), "certify-id", events, 10)
	if err != nil {
		session.Close()
		t.Fatal(err)
	}
	if result.RequestCount != 4 || result.InteractionEventCount != len(events) || result.FingerprintFieldCount != 142 {
		t.Fatalf("unexpected completion: %+v", result)
	}
	session.Close()
	session.Close()
	if _, err := stdoutReader.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("session Close did not release stdout: %v", err)
	}
	if _, err := os.Stat(tempDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("runtime directory remains: %v", err)
	}
	if _, err := session.InitToken(); err == nil {
		t.Fatal("closed session returned Init token")
	}
	if _, err := session.PESDKSource(); err == nil {
		t.Fatal("closed session returned SDK source")
	}
}

func TestOpenDeviceKeepsOneProcessThroughTracelessCompletion(t *testing.T) {
	const sceneID, certifyID = "wa3238du", "fixture-traceless-certify"
	stub := writeDeviceRuntimeStub(t, validInitialDeviceStage(t, "53"), validTracelessDeviceStage(t, sceneID, certifyID))
	resolver := newNodeKeyResolver(stub)
	resolver.sdk = cachedSDK{source: []byte("cached-sdk"), fetchedAt: time.Now()}
	session, err := resolver.OpenDevice(context.Background(), newScriptTransport(), device.Profile{}, validDeviceRuntimeOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result, err := session.SolveTraceless(context.Background(), device.TracelessInput{
		SceneID: sceneID, CertifyID: certifyID,
		StaticPath: "3.29.0/pe.091.fixture.js", CaptchaType: "TRACELESS",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Succeeded() || result.CertifyID != certifyID {
		t.Fatalf("result=%+v", result)
	}
	if _, err := session.SolveTraceless(context.Background(), device.TracelessInput{
		SceneID: sceneID, CertifyID: certifyID, StaticPath: "valid.js", CaptchaType: "TRACELESS",
	}); err == nil {
		t.Fatal("completed session accepted a second traceless completion")
	}
}

func TestOpenDeviceKeepsOneProcessThroughSlidingCompletion(t *testing.T) {
	const sceneID, certifyID = "159tlu75", "fixture-sliding-certify"
	stub := writeDeviceRuntimeStub(t, validInitialDeviceStage(t, "53"), validSlidingDeviceStage(t, sceneID, certifyID))
	resolver := newNodeKeyResolver(stub)
	resolver.sdk = cachedSDK{source: []byte("cached-sdk"), fetchedAt: time.Now()}
	session, err := resolver.OpenDevice(context.Background(), newScriptTransport(), device.Profile{}, validDeviceRuntimeOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	// 强制末阶段在子进程退出后才读取；输出仍必须保留到会话主动关闭。
	// 旧 StdoutPipe + 并发 Wait 会提前关闭读端，此处不依赖 sleep 或调度概率。
	session.stdout = bufio.NewReaderSize(afterProcessExitReader{reader: session.stdout, exited: session.waitDone}, 32<<10)
	input := validSlidingInput(sceneID, certifyID)
	result, err := session.SolveSliding(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Succeeded() || result.CertifyID != certifyID || result.SecurityToken != "fixture-upload-security-token" {
		t.Fatalf("result=%+v", result)
	}
	if _, err := session.SolveSliding(context.Background(), input); err == nil {
		t.Fatal("completed session accepted a second sliding completion")
	}
}

type afterProcessExitReader struct {
	reader io.Reader
	exited <-chan struct{}
}

func (reader afterProcessExitReader) Read(buffer []byte) (int, error) {
	<-reader.exited
	return reader.reader.Read(buffer)
}

func TestOpenDeviceAndCompletionFailureBoundaries(t *testing.T) {
	options := validDeviceRuntimeOptions()
	transport := newScriptTransport()
	if _, err := (*KeyResolver)(nil).OpenDevice(context.Background(), transport, device.Profile{}, options); !errors.Is(err, pe.ErrKeyRuntime) {
		t.Fatalf("nil resolver: %v", err)
	}
	resolver := newNodeKeyResolver(filepath.Join(t.TempDir(), "missing-node"))
	if _, err := resolver.OpenDevice(context.Background(), nil, device.Profile{}, options); !errors.Is(err, pe.ErrKeyNetwork) {
		t.Fatalf("nil transport: %v", err)
	}
	invalid := options
	invalid.Timeout = 0
	if _, err := resolver.OpenDevice(context.Background(), transport, device.Profile{}, invalid); !errors.Is(err, pe.ErrKeyRuntime) {
		t.Fatalf("invalid options: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := resolver.OpenDevice(ctx, transport, device.Profile{}, options); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context: %v", err)
	}
	resolver.sdk = cachedSDK{source: []byte("cached-sdk"), fetchedAt: time.Now()}
	if _, err := resolver.OpenDevice(context.Background(), transport, device.Profile{}, options); !errors.Is(err, pe.ErrKeyRuntime) {
		t.Fatalf("missing Node: %v", err)
	}

	session := newAcceptedRuntimeSession(t, "53")
	var absentContext context.Context
	if _, err := session.Complete(absentContext, "certify-id", []device.InteractionEvent{{Type: "mousemove", IsTrusted: true}}, 0); err == nil {
		t.Fatal("nil completion context accepted")
	}
	session.stdin = nil
	if _, err := session.Complete(context.Background(), "certify-id", []device.InteractionEvent{{Type: "mousemove", IsTrusted: true}}, 0); err == nil {
		t.Fatal("unavailable session accepted completion")
	}
}

func TestDeviceRuntimeSessionProfileIsCloned(t *testing.T) {
	session := &DeviceRuntimeSession{profile: device.Profile{
		ProfileID: "slot-profile", Languages: []string{"zh-CN"},
	}}
	profile := session.DeviceProfile()
	profile.Languages[0] = "changed"
	if session.profile.Languages[0] != "zh-CN" || session.DeviceProfile().ProfileID != "slot-profile" {
		t.Fatalf("DeviceProfile leaked mutable state: %+v", session.profile)
	}
}
