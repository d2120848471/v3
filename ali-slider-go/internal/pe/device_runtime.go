package pe

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/d2120848471/v3/ali-slider-go/internal/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/protocol"
	"github.com/d2120848471/v3/ali-slider-go/internal/runtimekit"
	"github.com/d2120848471/v3/ali-slider-go/internal/v8runtime"
)

const deviceBridgeLineLimit = 256 << 10

// DeviceRuntimeOptions 固定一轮 FeiLin V8 会话的边界。
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

// DeviceRuntimeSession 保持 Init 与 Verify 之间同一个公开 SDK/FeiLin VM。
type DeviceRuntimeSession struct {
	mu sync.Mutex

	command     *exec.Cmd
	stdin       io.WriteCloser
	stdout      *bufio.Reader
	waitDone    chan struct{}
	waitErr     error
	stderr      *cappedWriter
	tempDir     string
	relay       *connectRelay
	v8Engine    *v8runtime.Runtime
	v8Open      []byte
	profile     device.Profile
	deviceSlots chan struct{}

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

// DeviceProfile 返回该 V8 Device 槽位实际绑定的画像。
func (session *DeviceRuntimeSession) DeviceProfile() device.Profile {
	if session == nil {
		return device.Profile{}
	}
	return session.profile.Clone()
}

// OpenDevice 启动 challenge-worker，完成 Log1/Log2/Log3，并保留 VM 等待 PE
// getter。公开 SDK 源码复用五分钟缓存；DeviceToken 不缓存。
func (resolver *KeyResolver) OpenDevice(ctx context.Context, transport http.RoundTripper, profile device.Profile, options DeviceRuntimeOptions) (*DeviceRuntimeSession, error) {
	if resolver == nil || ctx == nil {
		return nil, fmt.Errorf("%w: device runtime is unavailable", ErrKeyRuntime)
	}
	if transport == nil {
		return nil, fmt.Errorf("%w: transport is nil", ErrKeyNetwork)
	}
	if err := validateDeviceRuntimeOptions(options); err != nil {
		return nil, fmt.Errorf("%w: invalid device runtime options", ErrKeyRuntime)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	releaseDeviceSlot, err := acquireDeviceExecutionSlot(ctx, resolver.deviceSlots)
	if err != nil {
		return nil, err
	}
	defer releaseDeviceSlot()

	sdkSource, err := resolver.sdkSource(ctx, transport, profile, resolver.now())
	if err != nil {
		return nil, err
	}
	if resolver.v8LibraryPath != "" {
		return resolver.openV8Device(ctx, transport, profile, options, sdkSource)
	}
	nodePath, err := exec.LookPath(resolver.nodeBinary)
	if err != nil {
		return nil, fmt.Errorf("%w: Node executable not found", ErrKeyRuntime)
	}
	profileJSON, err := marshalBridgeProfile(profile)
	if err != nil {
		return nil, fmt.Errorf("%w: encode device profile", ErrKeyRuntime)
	}
	temporaryDirectory, err := os.MkdirTemp("", "ali-slider-device-")
	if err != nil {
		return nil, fmt.Errorf("%w: create temporary directory", ErrKeyRuntime)
	}
	cleanupDirectory := true
	defer func() {
		if cleanupDirectory {
			_ = os.RemoveAll(temporaryDirectory)
		}
	}()
	runtimeDirectory, err := filepath.EvalSymlinks(temporaryDirectory)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve temporary directory", ErrKeyRuntime)
	}
	bridgePath := filepath.Join(runtimeDirectory, "sdk_device_bridge.mjs")
	sdkPath := filepath.Join(runtimeDirectory, "AliyunCaptcha.js")
	if err := os.WriteFile(bridgePath, sdkDeviceBridgeSource, 0o600); err != nil {
		return nil, fmt.Errorf("%w: write device bridge", ErrKeyRuntime)
	}
	if err := os.WriteFile(sdkPath, sdkSource, 0o600); err != nil {
		return nil, fmt.Errorf("%w: write public SDK", ErrKeyRuntime)
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
		return nil, fmt.Errorf("%w: open device stdin", ErrKeyRuntime)
	}
	stdoutPipe, err := command.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("%w: open device stdout", ErrKeyRuntime)
	}
	stderr := &cappedWriter{limit: keyBridgeMaxBytes}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("%w: start device bridge", ErrKeyRuntime)
	}

	session := &DeviceRuntimeSession{
		command: command, stdin: stdin, stdout: bufio.NewReaderSize(stdoutPipe, 32<<10),
		waitDone: make(chan struct{}), stderr: stderr, tempDir: runtimeDirectory, relay: relay, options: options,
		sdkSource: bytes.Clone(sdkSource), profile: profile.Clone(), deviceSlots: resolver.deviceSlots,
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

func validateDeviceRuntimeOptions(options DeviceRuntimeOptions) error {
	if options.Prefix == "" || options.Region == "" || options.Timeout < time.Second || options.Timeout > 5*time.Minute {
		return errors.New("device labels or timeout are invalid")
	}
	if options.GatherCostMin < 0 || options.GatherCostMax < options.GatherCostMin || options.FirstTouchAgeMin < 1 || options.FirstTouchAgeMax < options.FirstTouchAgeMin {
		return errors.New("device timing bounds are invalid")
	}
	return options.Sources.Validate()
}

func nodeProxyEnvironment(proxy string, transport http.RoundTripper) ([]string, bool, *connectRelay, error) {
	value := strings.TrimSpace(proxy)
	if value == "" {
		return []string{}, false, nil, nil
	}
	if !strings.Contains(value, "://") {
		value = "http://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil && parsed.User.Username() == "" {
		return nil, false, nil, fmt.Errorf("%w: invalid Node proxy", ErrKeyRuntime)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
	case "socks4", "socks5", "socks5h":
		base, ok := transport.(*http.Transport)
		if !ok || base.DialContext == nil {
			return nil, false, nil, fmt.Errorf("%w: SOCKS transport is unavailable", ErrKeyRuntime)
		}
		relay, relayErr := startConnectRelay(base.DialContext)
		if relayErr != nil {
			return nil, false, nil, fmt.Errorf("%w: start SOCKS relay", ErrKeyRuntime)
		}
		value = "http://" + relay.listener.Addr().String()
		return []string{
			"HTTP_PROXY=" + value, "HTTPS_PROXY=" + value,
			"NO_PROXY=localhost,127.0.0.1", "NODE_USE_ENV_PROXY=1",
		}, true, relay, nil
	default:
		return nil, false, nil, fmt.Errorf("%w: invalid Node proxy scheme", ErrKeyRuntime)
	}
	return []string{
		"HTTP_PROXY=" + parsed.String(), "HTTPS_PROXY=" + parsed.String(),
		"NO_PROXY=localhost,127.0.0.1", "NODE_USE_ENV_PROXY=1",
	}, true, nil, nil
}

type connectRelay struct {
	listener   net.Listener
	dial       func(context.Context, string, string) (net.Conn, error)
	ctx        context.Context
	cancel     context.CancelFunc
	acceptDone chan struct{}
	handlers   sync.WaitGroup
	closeOnce  sync.Once
	mu         sync.Mutex
	closed     bool
	active     map[net.Conn]struct{}
}

func startConnectRelay(dial func(context.Context, string, string) (net.Conn, error)) (*connectRelay, error) {
	if dial == nil {
		return nil, errors.New("relay dialer is nil")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	relay := &connectRelay{
		listener: listener, dial: dial, ctx: ctx, cancel: cancel,
		acceptDone: make(chan struct{}), active: make(map[net.Conn]struct{}),
	}
	go relay.serve()
	return relay, nil
}

func (relay *connectRelay) serve() {
	defer close(relay.acceptDone)
	for {
		connection, err := relay.listener.Accept()
		if err != nil {
			return
		}
		if !relay.register(connection) {
			continue
		}
		relay.handlers.Add(1)
		go func() {
			defer relay.handlers.Done()
			relay.handle(connection)
		}()
	}
}

func (relay *connectRelay) register(connection net.Conn) bool {
	relay.mu.Lock()
	defer relay.mu.Unlock()
	if relay.closed {
		_ = connection.Close()
		return false
	}
	relay.active[connection] = struct{}{}
	return true
}

func (relay *connectRelay) unregister(connection net.Conn) {
	relay.mu.Lock()
	delete(relay.active, connection)
	relay.mu.Unlock()
}

func (relay *connectRelay) handle(client net.Conn) {
	defer func() {
		relay.unregister(client)
		_ = client.Close()
	}()
	_ = client.SetDeadline(time.Now().Add(10 * time.Second))
	request, err := http.ReadRequest(bufio.NewReader(client))
	if err != nil {
		return
	}
	defer request.Body.Close()
	host, port, splitErr := net.SplitHostPort(request.Host)
	allowed := strings.EqualFold(host, "g.alicdn.com") || strings.HasSuffix(strings.ToLower(host), ".aliyuncs.com")
	if request.Method != http.MethodConnect || splitErr != nil || port != "443" || !allowed {
		_, _ = io.WriteString(client, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")
		return
	}
	upstream, err := relay.dial(relay.ctx, "tcp", request.Host)
	if err != nil {
		_, _ = io.WriteString(client, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}
	if !relay.register(upstream) {
		return
	}
	defer func() {
		relay.unregister(upstream)
		_ = upstream.Close()
	}()
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	_ = client.SetDeadline(time.Time{})
	copyDone := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, client)
		copyDone <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, upstream)
		copyDone <- struct{}{}
	}()
	completed := 0
	select {
	case <-copyDone:
		completed = 1
	case <-relay.ctx.Done():
	}
	_ = client.Close()
	_ = upstream.Close()
	for completed < 2 {
		<-copyDone
		completed++
	}
}

func (relay *connectRelay) Close() {
	if relay == nil {
		return
	}
	relay.closeOnce.Do(func() {
		relay.cancel()
		_ = relay.listener.Close()
		<-relay.acceptDone
		relay.mu.Lock()
		relay.closed = true
		for connection := range relay.active {
			_ = connection.Close()
		}
		relay.mu.Unlock()
		relay.handlers.Wait()
	})
}

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
			return deviceBridgeStage{}, fmt.Errorf("%w: device bridge output", ErrKeyRuntime)
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
			return deviceBridgeStage{}, fmt.Errorf("%w: decode device bridge stage: %v", ErrKeyRuntime, err)
		}
		if err := ensureJSONEOF(decoder); err != nil || stage.Stage != expected {
			return deviceBridgeStage{}, fmt.Errorf("%w: device bridge stage mismatch", ErrKeyRuntime)
		}
		return stage, nil
	}
	return deviceBridgeStage{}, fmt.Errorf("%w: device bridge output limit", ErrKeyRuntime)
}

func (session *DeviceRuntimeSession) acceptInitialStage(stage deviceBridgeStage) error {
	secrets, err := protocol.ResolveFrontendSecrets()
	if err != nil {
		return fmt.Errorf("%w: device protocol material", ErrKeyRuntime)
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
		return fmt.Errorf("%w: initial Node fingerprint field count", ErrUnsupportedPE)
	}
	if err := validateVerifyProfile(stage.VerifyArgProfile); err != nil {
		return err
	}
	actions := stageActions(stage)
	if stage.RequestCount != 3 || !slices.Equal(actions, []string{"Log1", "Log2", "Log3"}) {
		return fmt.Errorf("%w: initial device request sequence", ErrUnsupportedPE)
	}
	targetCost, err := session.normalizedGatherCost(parsed.GatherCost)
	if err != nil {
		return fmt.Errorf("%w: normalize GatherCost", ErrKeyRuntime)
	}
	initToken := stage.DeviceToken
	if parsed.GatherCost != strconv.Itoa(targetCost) {
		initToken, err = protocol.BuildDeviceToken(parsed.SessionID, parsed.FingerprintCipher, strconv.Itoa(targetCost), secrets.DeviceTokenSalt())
		if err != nil {
			return fmt.Errorf("%w: normalize Init token", ErrKeyRuntime)
		}
		parsed, err = protocol.ParseDeviceToken(initToken, secrets.DeviceTokenSalt())
		if err != nil {
			return fmt.Errorf("%w: check normalized Init token", ErrKeyRuntime)
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
		return protocol.DeviceConfig{}, fmt.Errorf("%w: Node DeviceConfig", ErrUnsupportedPE)
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
			return fmt.Errorf("%w: Node Verify arg profile", ErrUnsupportedPE)
		}
		for _, character := range []byte(item) {
			if character < 0x20 || character > 0x7e {
				return fmt.Errorf("%w: Node Verify arg profile", ErrUnsupportedPE)
			}
		}
	}
	return nil
}

func validateRuntimeToken(token string, config protocol.DeviceConfig, salt string) (protocol.DeviceToken, []string, int64, int64, error) {
	parsed, err := protocol.ParseDeviceToken(token, salt)
	if err != nil || parsed.SessionID != config.SessionID {
		return protocol.DeviceToken{}, nil, 0, 0, fmt.Errorf("%w: Node DeviceToken container", ErrUnsupportedPE)
	}
	plaintext, err := protocol.AESCBCDecryptBase64(parsed.FingerprintCipher, config.Key)
	if err != nil || !utf8.Valid(plaintext) {
		return protocol.DeviceToken{}, nil, 0, 0, fmt.Errorf("%w: Node DeviceToken fingerprint", ErrUnsupportedPE)
	}
	fields := strings.Split(string(plaintext), "#")
	if len(fields) < 78 || len(fields) > 512 {
		return protocol.DeviceToken{}, nil, 0, 0, fmt.Errorf("%w: Node fingerprint field count", ErrUnsupportedPE)
	}
	collectorStarted, err := strconv.ParseInt(fields[72], 10, 64)
	if err != nil || collectorStarted < 1 {
		return protocol.DeviceToken{}, nil, 0, 0, fmt.Errorf("%w: Node collector clock", ErrUnsupportedPE)
	}
	tokenTime, err := strconv.ParseInt(fields[74], 10, 64)
	if err != nil || tokenTime < collectorStarted {
		return protocol.DeviceToken{}, nil, 0, 0, fmt.Errorf("%w: Node token clock", ErrUnsupportedPE)
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
// 使用同一快照，避免预热会话恰好跨过五分钟刷新边界时混用两个 SDK 版本。
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
	releaseDeviceSlot, err := acquireDeviceExecutionSlot(ctx, session.deviceSlots)
	if err != nil {
		return device.Result{}, err
	}
	defer releaseDeviceSlot()
	if engine != nil {
		return session.completeV8Device(ctx, engine, payload, len(events))
	}
	if _, err := stdin.Write(payload); err != nil {
		_ = stdin.Close()
		return device.Result{}, fmt.Errorf("%w: write device completion", ErrKeyRuntime)
	}
	if err := stdin.Close(); err != nil {
		return device.Result{}, fmt.Errorf("%w: close device completion", ErrKeyRuntime)
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
		return device.Result{}, fmt.Errorf("%w: device bridge exit", ErrKeyRuntime)
	}
	return result, nil
}

// Recycle 在同一个已加载 bridge 的 V8 Isolate 内建立全新浏览器 context 和
// Device session。它只复用宿主与编译结果，不复用上一轮 session/token/挑战态。
func (session *DeviceRuntimeSession) Recycle(ctx context.Context) error {
	if ctx == nil {
		return errors.New("device runtime recycle context is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	session.mu.Lock()
	if session.closed || !session.completed || session.v8Engine == nil || len(session.v8Open) == 0 {
		session.mu.Unlock()
		return errors.New("device runtime session is not recyclable")
	}
	engine := session.v8Engine
	payload := bytes.Clone(session.v8Open)
	session.mu.Unlock()
	releaseDeviceSlot, err := acquireDeviceExecutionSlot(ctx, session.deviceSlots)
	if err != nil {
		return err
	}
	defer releaseDeviceSlot()

	result, err := engine.Call(ctx, "__aliV8DeviceOpen", payload)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: recycle V8 Device: %v", ErrKeyRuntime, err)
	}
	var stage deviceBridgeStage
	if err := decodeV8Result(result, &stage); err != nil {
		return fmt.Errorf("%w: recycle V8 Device output", ErrKeyRuntime)
	}

	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed || session.v8Engine != engine {
		return errors.New("device runtime session closed during recycle")
	}
	session.secrets = protocol.FrontendSecrets{}
	session.config = protocol.DeviceConfig{}
	session.verifyProfile = deviceVerifyArgProfile{}
	session.initToken = ""
	session.initParsed = protocol.DeviceToken{}
	session.initial = device.Result{}
	session.firstTouchAge = 0
	session.firstTouchSet = false
	if err := session.acceptInitialStage(stage); err != nil {
		return err
	}
	session.completed = false
	return nil
}

func acquireDeviceExecutionSlot(ctx context.Context, slots chan struct{}) (func(), error) {
	if slots == nil {
		return func() {}, nil
	}
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func validateRuntimeInteractions(events []device.InteractionEvent) error {
	if len(events) < 1 || len(events) > maximumTrackEvents {
		return errors.New("device interactions are outside bounds")
	}
	previous := -1.0
	for _, event := range events {
		if event.Type != "mousemove" || !event.IsTrusted || math.IsNaN(event.X) || math.IsInf(event.X, 0) || math.IsNaN(event.Y) || math.IsInf(event.Y, 0) || math.Abs(event.X) > 10_000 || math.Abs(event.Y) > 10_000 || math.IsNaN(event.TimeStamp) || math.IsInf(event.TimeStamp, 0) || event.TimeStamp < 0 || event.TimeStamp > nativeEventTimeLimitMS || event.TimeStamp < previous {
			return errors.New("device interaction is invalid")
		}
		previous = event.TimeStamp
	}
	if events[len(events)-1].TimeStamp-events[0].TimeStamp > float64(maximumTrackDurationMS) {
		return errors.New("device interaction span is invalid")
	}
	return nil
}

func (session *DeviceRuntimeSession) acceptVerifyStage(stage deviceBridgeStage, eventCount int) (device.Result, error) {
	config, err := protocolConfig(stage.DeviceConfig)
	if err != nil || !equalProtocolConfig(config, session.config) || stage.VerifyArgProfile != session.verifyProfile {
		return device.Result{}, fmt.Errorf("%w: Verify device runtime changed", ErrUnsupportedPE)
	}
	if stage.GetterArgumentCount == nil || *stage.GetterArgumentCount != 1 || stage.InteractionEventCount == nil || *stage.InteractionEventCount != eventCount {
		return device.Result{}, fmt.Errorf("%w: Verify device completion count", ErrUnsupportedPE)
	}
	actions := stageActions(stage)
	if stage.RequestCount != 4 || !slices.Equal(actions, []string{"Log1", "Log2", "Log3", "Log2"}) {
		return device.Result{}, fmt.Errorf("%w: Verify device request sequence", ErrUnsupportedPE)
	}
	parsed, fields, collectorStarted, tokenTime, err := validateRuntimeToken(stage.VerifyDeviceToken, session.config, session.secrets.DeviceTokenSalt())
	if err != nil {
		return device.Result{}, err
	}
	if parsed.SessionID != session.initParsed.SessionID {
		return device.Result{}, fmt.Errorf("%w: Verify DeviceToken session mismatch", ErrUnsupportedPE)
	}
	verifyToken := stage.VerifyDeviceToken
	if parsed.GatherCost != strconv.Itoa(session.initial.GatherCost) {
		verifyToken, err = protocol.BuildDeviceToken(parsed.SessionID, parsed.FingerprintCipher, strconv.Itoa(session.initial.GatherCost), session.secrets.DeviceTokenSalt())
		if err != nil {
			return device.Result{}, fmt.Errorf("%w: normalize Verify token", ErrKeyRuntime)
		}
		if _, err := protocol.ParseDeviceToken(verifyToken, session.secrets.DeviceTokenSalt()); err != nil {
			return device.Result{}, fmt.Errorf("%w: check normalized Verify token", ErrKeyRuntime)
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
	session.v8Open = nil
	stdin := session.stdin
	session.stdin = nil
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
