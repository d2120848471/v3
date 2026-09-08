package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/d2120848471/v3/ali-slider-go/internal/domain/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/pe"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/track"
)

const deviceBridgeLineLimit = 256 << 10

// 以下运行时消息结构由内嵌 V8 与可选 Node oracle 共用。
type deviceVerifyArgProfile struct {
	AccessSec     string `json:"accessSec"`
	SessionIDSalt string `json:"sessionIdSalt"`
}

type deviceRequestSummary struct {
	Host        string   `json:"host"`
	Action      string   `json:"action"`
	FieldNames  []string `json:"fieldNames"`
	HeaderNames []string `json:"headerNames"`
}

type deviceBridgeStage struct {
	Stage                 string                 `json:"stage"`
	DeviceToken           string                 `json:"deviceToken"`
	VerifyDeviceToken     string                 `json:"verifyDeviceToken"`
	CaptchaType           string                 `json:"captchaType"`
	SceneID               string                 `json:"sceneId"`
	CertifyID             string                 `json:"certifyId"`
	CaptchaVerifyParam    string                 `json:"captchaVerifyParam"`
	TracelessDeviceToken  string                 `json:"tracelessDeviceToken"`
	SlidingDeviceToken    string                 `json:"slidingDeviceToken"`
	VerifyCode            string                 `json:"verifyCode"`
	VerifyResult          bool                   `json:"verifyResult"`
	SecurityToken         string                 `json:"securityToken"`
	VerifyCertifyID       string                 `json:"verifyCertifyId"`
	DeviceConfig          nativeDeviceConfig     `json:"deviceConfig"`
	VerifyArgProfile      deviceVerifyArgProfile `json:"verifyArgProfile"`
	TokenSource           string                 `json:"tokenSource"`
	GetterArgumentCount   *int                   `json:"getterArgumentCount"`
	InteractionEventCount *int                   `json:"interactionEventCount"`
	RequestCount          int                    `json:"requestCount"`
	Requests              []deviceRequestSummary `json:"requests"`
}

type deviceCompletion struct {
	Complete               bool                      `json:"complete"`
	GetterArguments        []string                  `json:"getterArguments"`
	InteractionEvents      []device.InteractionEvent `json:"interactionEvents"`
	PostInteractionDelayMS int                       `json:"postInteractionDelayMs"`
}

type tracelessCompletion struct {
	Complete    bool   `json:"complete"`
	Mode        string `json:"mode"`
	SceneID     string `json:"sceneId"`
	CertifyID   string `json:"certifyId"`
	StaticPath  string `json:"staticPath"`
	CaptchaType string `json:"captchaType"`
	DeviceToken string `json:"deviceToken"`
}

type slidingCompletion struct {
	Complete    bool          `json:"complete"`
	Mode        string        `json:"mode"`
	SceneID     string        `json:"sceneId"`
	CertifyID   string        `json:"certifyId"`
	StaticPath  string        `json:"staticPath"`
	CaptchaType string        `json:"captchaType"`
	DeviceToken string        `json:"deviceToken"`
	Track       []track.Event `json:"track"`
	SlideWidth  int           `json:"slideWidth"`
	HandleWidth int           `json:"handleWidth"`
}

// OpenDevice 启动本轮 SDK/FeiLin 会话，完成 Log1/Log2/Log3 并保留 VM。
// 生产 resolver 使用内嵌 V8；Node oracle 使用 challenge-worker 子进程。
// 公开 SDK 源码复用五分钟缓存；DeviceToken 不缓存。
func (resolver *KeyResolver) OpenDevice(ctx context.Context, transport http.RoundTripper, profile device.Profile, options DeviceRuntimeOptions) (*DeviceRuntimeSession, error) {
	if resolver == nil || ctx == nil {
		return nil, fmt.Errorf("%w: device runtime is unavailable", pe.ErrKeyRuntime)
	}
	if transport == nil {
		return nil, fmt.Errorf("%w: transport is nil", pe.ErrKeyNetwork)
	}
	if err := validateDeviceRuntimeOptions(options); err != nil {
		return nil, fmt.Errorf("%w: invalid device runtime options", pe.ErrKeyRuntime)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sdkSource, err := resolver.sdkSource(ctx, transport, profile, resolver.now())
	if err != nil {
		return nil, err
	}
	if resolver.v8LibraryPath != "" {
		return resolver.openV8Device(ctx, transport, profile, options, sdkSource)
	}
	nodePath, err := exec.LookPath(resolver.nodeBinary)
	if err != nil {
		return nil, fmt.Errorf("%w: Node executable not found", pe.ErrKeyRuntime)
	}
	profileJSON, err := marshalBridgeProfile(profile)
	if err != nil {
		return nil, fmt.Errorf("%w: encode device profile", pe.ErrKeyRuntime)
	}
	temporaryDirectory, err := os.MkdirTemp("", "ali-slider-device-")
	if err != nil {
		return nil, fmt.Errorf("%w: create temporary directory", pe.ErrKeyRuntime)
	}
	cleanupDirectory := true
	defer func() {
		if cleanupDirectory {
			_ = os.RemoveAll(temporaryDirectory)
		}
	}()
	runtimeDirectory, err := filepath.EvalSymlinks(temporaryDirectory)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve temporary directory", pe.ErrKeyRuntime)
	}
	bridgePath := filepath.Join(runtimeDirectory, "sdk_device_bridge.mjs")
	sdkPath := filepath.Join(runtimeDirectory, "AliyunCaptcha.js")
	if err := os.WriteFile(bridgePath, sdkDeviceBridgeSource, 0o600); err != nil {
		return nil, fmt.Errorf("%w: write device bridge", pe.ErrKeyRuntime)
	}
	if err := os.WriteFile(sdkPath, sdkSource, 0o600); err != nil {
		return nil, fmt.Errorf("%w: write public SDK", pe.ErrKeyRuntime)
	}

	arguments := []string{
		bridgePath, "--mode", "challenge-worker", "--sdk", sdkPath,
		"--prefix", options.Prefix, "--region", options.Region,
		"--timeout-ms", fmt.Sprint(options.Timeout.Milliseconds()),
		"--device-profile", base64.StdEncoding.EncodeToString(profileJSON),
	}
	childEnvironment, useEnvironmentProxy, relay, err := nodeProxyEnvironment(options.Proxy, transport)
	if err != nil {
		return nil, err
	}
	if relay != nil {
		defer func() {
			if cleanupDirectory {
				relay.Close()
			}
		}()
	}
	if useEnvironmentProxy {
		arguments = append([]string{"--use-env-proxy"}, arguments...)
	}
	command := exec.Command(nodePath, arguments...)
	command.Dir = runtimeDirectory
	command.Env = childEnvironment
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("%w: open device stdin", pe.ErrKeyRuntime)
	}
	// Wait 会关闭 StdoutPipe 的读端，可能抢在末阶段消费前丢弃已写出的输出。
	// 显式持有管道，让子进程退出与会话读端的释放分别由各自所有者负责。
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("%w: open device stdout", pe.ErrKeyRuntime)
	}
	command.Stdout = stdoutWriter
	stderr := &cappedWriter{limit: keyBridgeMaxBytes}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
		return nil, fmt.Errorf("%w: start device bridge", pe.ErrKeyRuntime)
	}
	_ = stdoutWriter.Close()

	session := &DeviceRuntimeSession{
		command: command, stdin: stdin, stdout: bufio.NewReaderSize(stdoutReader, 32<<10), stdoutCloser: stdoutReader,
		waitDone: make(chan struct{}), stderr: stderr, tempDir: runtimeDirectory, relay: relay, options: options,
		sdkSource: bytes.Clone(sdkSource), profile: profile.Clone(),
	}
	cleanupDirectory = false
	go func() {
		waitErr := command.Wait()
		session.mu.Lock()
		session.waitErr = waitErr
		session.mu.Unlock()
		close(session.waitDone)
	}()

	stage, err := session.readStage(ctx, "init")
	if err != nil {
		session.Close()
		return nil, err
	}
	if err := session.acceptInitialStage(stage); err != nil {
		session.Close()
		return nil, err
	}
	return session, nil
}

// readStage 读取 Node oracle 的阶段输出，并在取消时终止对应子进程。
func (session *DeviceRuntimeSession) readStage(ctx context.Context, expected string) (deviceBridgeStage, error) {
	type stageResult struct {
		stage deviceBridgeStage
		err   error
	}
	result := make(chan stageResult, 1)
	go func() {
		stage, err := readDeviceBridgeStage(session.stdout, expected)
		result <- stageResult{stage: stage, err: err}
	}()
	select {
	case <-ctx.Done():
		_ = session.command.Process.Kill()
		return deviceBridgeStage{}, ctx.Err()
	case item := <-result:
		if item.err != nil {
			return deviceBridgeStage{}, item.err
		}
		return item.stage, nil
	}
}

func readDeviceBridgeStage(reader *bufio.Reader, expected string) (deviceBridgeStage, error) {
	total := 0
	for total <= 4*deviceBridgeLineLimit {
		line, err := reader.ReadBytes('\n')
		total += len(line)
		if err != nil || len(line) > deviceBridgeLineLimit {
			return deviceBridgeStage{}, fmt.Errorf("%w: device bridge output", pe.ErrKeyRuntime)
		}
		trimmed := bytes.TrimSpace(line)
		// 公开混淆脚本偶尔会向 stdout 打印非 JSON 调试行；和历史运行时一样，
		// 只接受桥自己输出的 object 行，其余内容不记录也不外传。
		if len(trimmed) < 2 || trimmed[0] != '{' {
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(trimmed))
		decoder.DisallowUnknownFields()
		var stage deviceBridgeStage
		if err := decoder.Decode(&stage); err != nil {
			return deviceBridgeStage{}, fmt.Errorf("%w: decode device bridge stage: %v", pe.ErrKeyRuntime, err)
		}
		if err := ensureJSONEOF(decoder); err != nil || stage.Stage != expected {
			return deviceBridgeStage{}, fmt.Errorf("%w: device bridge stage mismatch", pe.ErrKeyRuntime)
		}
		return stage, nil
	}
	return deviceBridgeStage{}, fmt.Errorf("%w: device bridge output limit", pe.ErrKeyRuntime)
}
