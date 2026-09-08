package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"sync"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/domain/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/pe"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/protocol"
	"github.com/d2120848471/v3/ali-slider-go/internal/foundation/runtimekit"
	"github.com/d2120848471/v3/ali-slider-go/internal/platform/v8runtime"
)

// DeviceRuntimeOptions 固定一轮 SDK/FeiLin 会话的设备时序与传输边界。
type DeviceRuntimeOptions struct {
	Prefix           string
	Region           string
	Proxy            string
	Timeout          time.Duration
	GatherCostMin    int
	GatherCostMax    int
	FirstTouchAgeMin int
	FirstTouchAgeMax int
	Sources          runtimekit.Sources
}

// DeviceRuntimeSession 保持 Init 与 Verify 之间同一个公开 SDK/FeiLin VM。
// 生产使用内嵌 V8；Node 进程字段仅用于可选 oracle 路径。
type DeviceRuntimeSession struct {
	mu sync.Mutex

	command      *exec.Cmd
	stdin        io.WriteCloser
	stdout       *bufio.Reader
	stdoutCloser io.ReadCloser
	waitDone     chan struct{}
	waitErr      error
	stderr       *cappedWriter
	tempDir      string
	relay        *connectRelay
	v8Engine     *v8runtime.Runtime
	profile      device.Profile

	options       DeviceRuntimeOptions
	sdkSource     []byte
	secrets       protocol.FrontendSecrets
	config        protocol.DeviceConfig
	verifyProfile deviceVerifyArgProfile
	initToken     string
	initParsed    protocol.DeviceToken
	initial       device.Result
	firstTouchAge int
	firstTouchSet bool
	completed     bool
	closed        bool
}

// DeviceProfile 返回本轮 Device 会话实际绑定的画像。
func (session *DeviceRuntimeSession) DeviceProfile() device.Profile {
	if session == nil {
		return device.Profile{}
	}
	return session.profile.Clone()
}

func (session *DeviceRuntimeSession) InitToken() (string, error) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed || session.initToken == "" {
		return "", errors.New("device runtime session is not initialized")
	}
	return session.initToken, nil
}

func (session *DeviceRuntimeSession) PEDeviceConfig() (protocol.DeviceConfig, error) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed || session.completed || session.config.SessionID == "" {
		return protocol.DeviceConfig{}, errors.New("device runtime cannot provide PE config")
	}
	value := session.config
	value.ExtraSegments = append([]string(nil), value.ExtraSegments...)
	return value, nil
}

// PEVerifyArgProfile 返回同一 SDK VM 交给动态 PE 的两项运行参数。
func (session *DeviceRuntimeSession) PEVerifyArgProfile() (string, string, error) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed || session.completed || session.verifyProfile.AccessSec == "" || session.verifyProfile.SessionIDSalt == "" {
		return "", "", errors.New("device runtime cannot provide Verify arg profile")
	}
	return session.verifyProfile.AccessSec, session.verifyProfile.SessionIDSalt, nil
}

// PESDKSource 返回创建本轮 Device VM 时使用的公开 SDK 快照。动态 PE 必须
// 使用同一快照，避免本轮处理恰好跨过五分钟刷新边界时混用两个 SDK 版本。
func (session *DeviceRuntimeSession) PESDKSource() ([]byte, error) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed || session.completed || len(session.sdkSource) == 0 {
		return nil, errors.New("device runtime cannot provide SDK source")
	}
	return bytes.Clone(session.sdkSource), nil
}

func (session *DeviceRuntimeSession) TargetFirstTouchAgeMS() (int, error) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed || session.completed {
		return 0, errors.New("device runtime cannot provide first touch age")
	}
	if !session.firstTouchSet {
		width := session.options.FirstTouchAgeMax - session.options.FirstTouchAgeMin + 1
		value, err := session.options.Sources.Entropy.Uint64n(uint64(width))
		if err != nil {
			return 0, errors.New("generate first touch age")
		}
		session.firstTouchAge = session.options.FirstTouchAgeMin + int(value)
		session.firstTouchSet = true
	}
	return session.firstTouchAge, nil
}

func (session *DeviceRuntimeSession) Complete(ctx context.Context, getterArgument string, events []device.InteractionEvent, postInteractionDelayMS int) (device.Result, error) {
	if ctx == nil {
		return device.Result{}, errors.New("device runtime context is nil")
	}
	session.mu.Lock()
	if session.closed || session.completed || session.stdin == nil && session.v8Engine == nil {
		session.mu.Unlock()
		return device.Result{}, errors.New("device runtime session is unavailable")
	}
	if getterArgument == "" || len(getterArgument) > 512 || postInteractionDelayMS < 0 || postInteractionDelayMS > 500 {
		session.mu.Unlock()
		return device.Result{}, errors.New("device runtime completion is invalid")
	}
	if err := validateRuntimeInteractions(events); err != nil {
		session.mu.Unlock()
		return device.Result{}, err
	}
	payload, err := json.Marshal(deviceCompletion{
		Complete: true, GetterArguments: []string{getterArgument},
		InteractionEvents:      append([]device.InteractionEvent(nil), events...),
		PostInteractionDelayMS: postInteractionDelayMS,
	})
	if err != nil {
		session.mu.Unlock()
		return device.Result{}, errors.New("encode device runtime completion")
	}
	engine := session.v8Engine
	stdin := session.stdin
	if engine == nil {
		session.stdin = nil
	}
	session.completed = true
	session.mu.Unlock()
	if engine != nil {
		return session.completeV8Device(ctx, engine, payload, len(events))
	}
	if _, err := stdin.Write(payload); err != nil {
		_ = stdin.Close()
		return device.Result{}, fmt.Errorf("%w: write device completion", pe.ErrKeyRuntime)
	}
	if err := stdin.Close(); err != nil {
		return device.Result{}, fmt.Errorf("%w: close device completion", pe.ErrKeyRuntime)
	}
	stage, err := session.readStage(ctx, "verify")
	if err != nil {
		return device.Result{}, err
	}
	result, err := session.acceptVerifyStage(stage, len(events))
	if err != nil {
		return device.Result{}, err
	}
	select {
	case <-ctx.Done():
		_ = session.command.Process.Kill()
		return device.Result{}, ctx.Err()
	case <-session.waitDone:
	}
	session.mu.Lock()
	waitErr := session.waitErr
	stderrEmpty := session.stderr.buffer.Len() == 0
	session.mu.Unlock()
	if waitErr != nil || !stderrEmpty {
		return device.Result{}, fmt.Errorf("%w: device bridge exit", pe.ErrKeyRuntime)
	}
	return result, nil
}

// SolveTraceless 让同一 SDK/FeiLin VM 完成已经初始化的 TRACELESS 挑战。
func (session *DeviceRuntimeSession) SolveTraceless(ctx context.Context, input device.TracelessInput) (device.TracelessResult, error) {
	if ctx == nil {
		return device.TracelessResult{}, errors.New("device runtime context is nil")
	}
	if err := validateTracelessInput(input); err != nil {
		return device.TracelessResult{}, err
	}
	session.mu.Lock()
	if session.closed || session.completed || session.stdin == nil && session.v8Engine == nil || session.initToken == "" {
		session.mu.Unlock()
		return device.TracelessResult{}, errors.New("device runtime session is unavailable")
	}
	payload, err := json.Marshal(tracelessCompletion{
		Complete: true, Mode: "traceless", SceneID: input.SceneID, CertifyID: input.CertifyID,
		StaticPath: input.StaticPath, CaptchaType: "TRACELESS", DeviceToken: session.initToken,
	})
	if err != nil {
		session.mu.Unlock()
		return device.TracelessResult{}, errors.New("encode traceless runtime completion")
	}
	engine := session.v8Engine
	stdin := session.stdin
	if engine == nil {
		session.stdin = nil
	}
	session.completed = true
	session.mu.Unlock()
	if engine != nil {
		return session.completeV8Traceless(ctx, engine, payload, input)
	}
	if _, err := stdin.Write(payload); err != nil {
		_ = stdin.Close()
		return device.TracelessResult{}, fmt.Errorf("%w: write traceless completion", pe.ErrKeyRuntime)
	}
	if err := stdin.Close(); err != nil {
		return device.TracelessResult{}, fmt.Errorf("%w: close traceless completion", pe.ErrKeyRuntime)
	}
	stage, err := session.readStage(ctx, "traceless")
	if err != nil {
		return device.TracelessResult{}, err
	}
	result, err := session.acceptTracelessStage(stage, input)
	if err != nil {
		return device.TracelessResult{}, err
	}
	select {
	case <-ctx.Done():
		_ = session.command.Process.Kill()
		return device.TracelessResult{}, ctx.Err()
	case <-session.waitDone:
	}
	session.mu.Lock()
	waitErr := session.waitErr
	stderrEmpty := session.stderr.buffer.Len() == 0
	session.mu.Unlock()
	if waitErr != nil || !stderrEmpty {
		return device.TracelessResult{}, fmt.Errorf("%w: device bridge exit", pe.ErrKeyRuntime)
	}
	return result, nil
}

// SolveSliding 在同一 SDK/FeiLin VM 中渲染官方 SLIDING 并回放拖动轨迹。
func (session *DeviceRuntimeSession) SolveSliding(ctx context.Context, input device.SlidingInput) (device.SlidingResult, error) {
	if ctx == nil {
		return device.SlidingResult{}, errors.New("device runtime context is nil")
	}
	if err := validateSlidingInput(input); err != nil {
		return device.SlidingResult{}, err
	}
	session.mu.Lock()
	if session.closed || session.completed || session.stdin == nil && session.v8Engine == nil || session.initToken == "" {
		session.mu.Unlock()
		return device.SlidingResult{}, errors.New("device runtime session is unavailable")
	}
	payload, err := json.Marshal(slidingCompletion{
		Complete: true, Mode: "sliding", SceneID: input.SceneID, CertifyID: input.CertifyID,
		StaticPath: input.StaticPath, CaptchaType: "SLIDING", DeviceToken: session.initToken,
		Track: slices.Clone(input.Track), SlideWidth: input.SlideWidth, HandleWidth: input.HandleWidth,
	})
	if err != nil {
		session.mu.Unlock()
		return device.SlidingResult{}, errors.New("encode sliding runtime completion")
	}
	engine := session.v8Engine
	stdin := session.stdin
	if engine == nil {
		session.stdin = nil
	}
	session.completed = true
	session.mu.Unlock()
	if engine != nil {
		return session.completeV8Sliding(ctx, engine, payload, input)
	}
	if _, err := stdin.Write(payload); err != nil {
		_ = stdin.Close()
		return device.SlidingResult{}, fmt.Errorf("%w: write sliding completion", pe.ErrKeyRuntime)
	}
	if err := stdin.Close(); err != nil {
		return device.SlidingResult{}, fmt.Errorf("%w: close sliding completion", pe.ErrKeyRuntime)
	}
	stage, err := session.readStage(ctx, "sliding")
	if err != nil {
		return device.SlidingResult{}, err
	}
	result, err := session.acceptSlidingStage(stage, input)
	if err != nil {
		return device.SlidingResult{}, err
	}
	select {
	case <-ctx.Done():
		_ = session.command.Process.Kill()
		return device.SlidingResult{}, ctx.Err()
	case <-session.waitDone:
	}
	session.mu.Lock()
	waitErr := session.waitErr
	stderrEmpty := session.stderr.buffer.Len() == 0
	session.mu.Unlock()
	if waitErr != nil || !stderrEmpty {
		return device.SlidingResult{}, fmt.Errorf("%w: device bridge exit", pe.ErrKeyRuntime)
	}
	return result, nil
}

func (session *DeviceRuntimeSession) Close() {
	if session == nil {
		return
	}
	session.mu.Lock()
	if session.closed {
		session.mu.Unlock()
		return
	}
	session.closed = true
	engine := session.v8Engine
	session.v8Engine = nil
	stdin := session.stdin
	session.stdin = nil
	stdout := session.stdoutCloser
	session.stdoutCloser = nil
	command := session.command
	waitDone := session.waitDone
	tempDir := session.tempDir
	session.tempDir = ""
	relay := session.relay
	session.relay = nil
	session.sdkSource = nil
	session.mu.Unlock()
	if engine != nil {
		_ = engine.Close()
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	if stdout != nil {
		_ = stdout.Close()
	}
	if command != nil {
		select {
		case <-waitDone:
		default:
			_ = command.Process.Kill()
			<-waitDone
		}
	}
	if tempDir != "" {
		_ = os.RemoveAll(tempDir)
	}
	if relay != nil {
		relay.Close()
	}
}
