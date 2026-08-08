package device

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/d2120848471/v3/ali-slider-go/internal/protocol"
	"github.com/d2120848471/v3/ali-slider-go/internal/runtimekit"
)

const (
	DefaultEndpoint = "https://cloudauth-device-dualstack.cn-shanghai.aliyuncs.com"
	defaultPrefix   = "fsgtmi"
	defaultRegion   = "cn"
	maxCombatWait   = time.Second
)

// ClientOptions 是设备 RPC 的不变配置。代理已固化在注入的 Transport 中。
type ClientOptions struct {
	Endpoint         string
	Prefix           string
	Region           string
	Timeout          time.Duration
	GatherCostMin    int
	GatherCostMax    int
	FirstTouchAgeMin int
	FirstTouchAgeMax int
	Profile          Profile
	Sources          runtimekit.Sources
}

// DefaultClientOptions 返回与当前 Python 链一致的运行默认值。
func DefaultClientOptions(profile Profile, sources runtimekit.Sources) ClientOptions {
	return ClientOptions{
		Endpoint: DefaultEndpoint, Prefix: defaultPrefix, Region: defaultRegion,
		Timeout: 25 * time.Second, GatherCostMin: 180, GatherCostMax: 260,
		FirstTouchAgeMin: 650, FirstTouchAgeMax: 850, Profile: profile, Sources: sources,
	}
}

// Client 可并发打开多个独立 Session。Transport 只负责共享连接池。
type Client struct {
	options   ClientOptions
	transport http.RoundTripper
	secrets   protocol.FrontendSecrets
}

// NewClient 固定整轮 HTTP transport，防止 Init/Verify 之间意外换代理。
func NewClient(options ClientOptions, transport http.RoundTripper) (*Client, error) {
	if transport == nil {
		return nil, errors.New("device HTTP transport is required")
	}
	endpoint, err := url.Parse(options.Endpoint)
	if err != nil || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.Path != "" || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return nil, errors.New("device endpoint must be an absolute HTTP(S) origin")
	}
	if !safeDeviceLabel(options.Prefix) || !safeDeviceLabel(options.Region) {
		return nil, errors.New("device prefix and region are required")
	}
	if options.Timeout <= 0 || options.Timeout > 5*time.Minute {
		return nil, errors.New("device timeout must be within (0,5m]")
	}
	if options.GatherCostMin < 0 || options.GatherCostMax < options.GatherCostMin {
		return nil, errors.New("device gather cost range is invalid")
	}
	if options.FirstTouchAgeMin < 1 || options.FirstTouchAgeMax < options.FirstTouchAgeMin {
		return nil, errors.New("device first touch age range is invalid")
	}
	if options.Profile.UserAgent == "" || options.Profile.CanvasSeed == "" {
		return nil, errors.New("device profile is incomplete")
	}
	if err := options.Sources.Validate(); err != nil {
		return nil, err
	}
	options.Profile.UABrands = append([]UABrand(nil), options.Profile.UABrands...)
	options.Profile.UAFullVersions = append([]UABrand(nil), options.Profile.UAFullVersions...)
	options.Profile.Languages = append([]string(nil), options.Profile.Languages...)
	secrets, err := protocol.ResolveFrontendSecrets()
	if err != nil {
		return nil, errors.New("device protocol material is unavailable")
	}
	return &Client{options: options, transport: transport, secrets: secrets}, nil
}

// Result 只暴露 challenge 编排需要的合同。String/GoString 永不打印 token。
type Result struct {
	InitToken             string
	VerifyToken           string
	RequestActions        []string
	RequestCount          int
	FingerprintFieldCount int
	GetterArgumentCount   int
	InteractionEventCount int
	CollectorStartedMS    int64
	TokenTimeMS           int64
	GatherCost            int
}

func (Result) String() string   { return "device.Result{redacted}" }
func (Result) GoString() string { return "device.Result{redacted}" }

// Session 跨 Init/Verify 持有一份 DeviceConfig、指纹和 action 状态。
type Session struct {
	mu sync.Mutex

	client *Client
	http   *http.Client
	closed bool

	config             protocol.DeviceConfig
	stableFields       map[int]string
	actions            []string
	collectorStartedMS int64
	combatStartedMS    int64
	gatherCost         int
	gatherCostSet      bool
	firstTouchAgeMS    int
	firstTouchAgeSet   bool
	initial            Result
	initialized        bool
	completed          bool
}

// Open 创建独立状态并完成 Log1、Log2、Log3。
func (client *Client) Open(ctx context.Context) (*Session, error) {
	if client == nil {
		return nil, errors.New("device client is nil")
	}
	if ctx == nil {
		return nil, errors.New("device context is nil")
	}
	// Python oracle 每轮使用独立 requests.Session。Go 同样必须让
	// Log1 响应的 cookie 只在本 Device Session 内传递，不能跨挑战共享。
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, errors.New("create device cookie jar")
	}
	session := &Session{client: client, http: &http.Client{
		Transport: client.transport, Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	if err := session.initialize(ctx); err != nil {
		session.clear()
		return nil, err
	}
	return session, nil
}

func (session *Session) initialize(ctx context.Context) error {
	session.collectorStartedMS = session.client.options.Sources.Clock.Now().UnixMilli()
	log1, err := session.buildLog1()
	if err != nil {
		return errors.New("build device Log1")
	}
	payload, err := session.postAction(ctx, "Log1", log1)
	if err != nil {
		return err
	}
	if payload.DeviceConfig == "" {
		return errors.New("device Log1 response is missing DeviceConfig")
	}
	config, err := protocol.ParseDeviceConfig(payload.DeviceConfig, session.client.secrets.DeviceResponseKey())
	if err != nil {
		return errors.New("parse device Log1 response")
	}
	session.config = config
	configTime, err := strconv.ParseInt(config.Timestamp, 10, 64)
	if err != nil {
		return errors.New("device configuration timestamp is invalid")
	}
	wait := time.Duration(configTime+90-session.client.options.Sources.Clock.Now().UnixMilli()) * time.Millisecond
	waitLimit := min(session.client.options.Timeout, maxCombatWait)
	if wait > waitLimit {
		return errors.New("device configuration clock skew exceeds limit")
	}
	if err := session.client.options.Sources.Clock.Sleep(ctx, max(wait, 0)); err != nil {
		return fmt.Errorf("device initialization canceled: %w", err)
	}
	session.combatStartedMS = session.client.options.Sources.Clock.Now().UnixMilli()
	if err := session.initializeStableFields(); err != nil {
		return err
	}
	if err := session.client.options.Sources.Clock.Sleep(ctx, 44*time.Millisecond); err != nil {
		return fmt.Errorf("device token wait canceled: %w", err)
	}
	token, err := session.buildToken("")
	if err != nil {
		return err
	}
	snapshot, err := session.log2Snapshot(token.fingerprintCipher)
	if err != nil {
		return err
	}
	log2, err := session.buildLog2(snapshot, 0)
	if err != nil {
		return err
	}
	if err := session.client.options.Sources.Clock.Sleep(ctx, 6*time.Millisecond); err != nil {
		return fmt.Errorf("device Log3 wait canceled: %w", err)
	}
	log3, err := session.buildLog3()
	if err != nil {
		return err
	}
	if _, err := session.postAction(ctx, "Log2", log2); err != nil {
		return err
	}
	if _, err := session.postAction(ctx, "Log3", log3); err != nil {
		return err
	}
	session.initialized = true
	session.initial = session.result(token.token, token.token, token.timeMS, 0, 0)
	return nil
}

type builtToken struct {
	token             string
	parsed            protocol.DeviceToken
	timeMS            int64
	fingerprintCipher string
	fields            []string
}

func (session *Session) buildToken(getterArgument string) (builtToken, error) {
	timeMS := session.client.options.Sources.Clock.Now().UnixMilli()
	fields, err := session.fingerprintFields(timeMS, getterArgument)
	if err != nil {
		return builtToken{}, err
	}
	ciphertext, err := protocol.AESCBCEncryptBase64([]byte(strings.Join(fields, "#")), session.config.Key)
	if err != nil {
		return builtToken{}, errors.New("encrypt device fingerprint")
	}
	if !session.gatherCostSet {
		value, err := randomRange(session.client.options.Sources.Entropy, session.client.options.GatherCostMin, session.client.options.GatherCostMax)
		if err != nil {
			return builtToken{}, errors.New("generate device gather cost")
		}
		session.gatherCost, session.gatherCostSet = value, true
	}
	token, err := protocol.BuildDeviceToken(session.config.SessionID, ciphertext, strconv.Itoa(session.gatherCost), session.client.secrets.DeviceTokenSalt())
	if err != nil {
		return builtToken{}, errors.New("build device token")
	}
	parsed, err := protocol.ParseDeviceToken(token, session.client.secrets.DeviceTokenSalt())
	if err != nil {
		return builtToken{}, errors.New("self-check device token")
	}
	return builtToken{token: token, parsed: parsed, timeMS: timeMS, fingerprintCipher: ciphertext, fields: fields}, nil
}

func (session *Session) initializeStableFields() error {
	probe, err := sessionProbe(session.config.SessionID)
	if err != nil {
		return err
	}
	canvas, err := canvasFingerprint(session.client.options.Profile)
	if err != nil {
		return errors.New("build canvas fingerprint")
	}
	random42, err := randomAlnum(session.client.options.Sources.Entropy, 42)
	if err != nil {
		return errors.New("build stable device fingerprint")
	}
	gpu, err := gpuFingerprint(session.client.options.Profile)
	if err != nil {
		return errors.New("build GPU fingerprint")
	}
	session.stableFields = map[int]string{21: probe, 32: canvas, 73: random42, 75: "mobile", 78: gpu}
	return nil
}

func (session *Session) fingerprintFields(tokenTimeMS int64, getterArgument string) ([]string, error) {
	profile := session.client.options.Profile
	chromeVersion := valueAfter(profile.UserAgent, "Chrome/")
	if chromeVersion == "" {
		chromeVersion = profile.UAFullVersion
	}
	chromeMajor := strings.SplitN(chromeVersion, ".", 2)[0]
	chromeZeroed := chromeMajor + ".0.0.0"
	androidMajor := valueAfter(profile.UserAgent, "Android ")
	if androidMajor == "" {
		androidMajor = strings.SplitN(profile.UAPlatformVersion, ".", 2)[0]
	} else {
		androidMajor = strings.SplitN(androidMajor, ";", 2)[0]
	}
	random40, err := randomAlnum(session.client.options.Sources.Entropy, 40)
	if err != nil {
		return nil, errors.New("build rotating device fingerprint")
	}
	timing, err := session.collectorTiming(tokenTimeMS)
	if err != nil {
		return nil, err
	}
	fields := make([]string, 111)
	values := map[int]string{
		0: "W.10054", 5: profile.Platform, 6: "Chrome", 7: chromeZeroed,
		22: strconv.Itoa(profile.DeviceMemory), 34: strconv.Itoa(profile.HardwareConcurrency),
		36: profile.UAPlatform, 37: androidMajor, 42: session.config.IP, 43: timing,
		44: strconv.FormatBool(profile.Mobile), 45: strconv.FormatBool(profile.Mobile),
		47: fmt.Sprintf("%d*%d", profile.Screen.Height, profile.Screen.Width),
		49: strconv.Itoa(profile.MaxTouchPoints), 53: deviceLocation,
		63: chromeZeroed, 64: profile.UserAgent, 67: deviceAppName,
		68: strconv.Itoa(session.config.Switch), 71: random40,
		72: strconv.FormatInt(session.collectorStartedMS, 10), 74: strconv.FormatInt(tokenTimeMS, 10),
		77: getterArgument, 80: profile.AppVersion, 85: strconv.Itoa(session.config.Switch),
		86: "0", 87: session.config.Timestamp, 110: brandList(profile),
	}
	for index, value := range session.stableFields {
		values[index] = value
	}
	for index, value := range values {
		if strings.Contains(value, "#") || !utf8.ValidString(value) {
			return nil, fmt.Errorf("device fingerprint field %d is invalid", index)
		}
		fields[index] = value
	}
	return fields, nil
}

func (session *Session) collectorTiming(tokenTimeMS int64) (string, error) {
	configTime, err := strconv.ParseInt(session.config.Timestamp, 10, 64)
	if err != nil {
		return "", errors.New("device configuration timestamp is invalid")
	}
	stage11 := max(int64(0), configTime-session.collectorStartedMS)
	stage20 := stage11 + 1
	stage23 := session.combatStartedMS - session.collectorStartedMS
	if stage23 <= stage20 {
		return "", errors.New("device combat clock precedes configuration stage")
	}
	stage30, stage40 := stage23+1, stage23+5
	elapsed := tokenTimeMS - session.collectorStartedMS
	if elapsed < stage40+3 {
		return "", errors.New("device token clock precedes collection stage")
	}
	stage93, stage94 := elapsed+4, elapsed+8
	if session.initialized {
		stage93, stage94 = elapsed+2, elapsed+2
	}
	return fmt.Sprintf("10-0|11-%d|20-%d|23-%d|30-%d|40-%d|90-%d|91-%d|92-%d|93-%d|94-%d", stage11, stage20, stage23, stage30, stage40, elapsed-3, elapsed, elapsed, stage93, stage94), nil
}

func (session *Session) log2Snapshot(tokenCipher string) (string, error) {
	plaintext, err := protocol.AESCBCDecryptBase64(tokenCipher, session.config.Key)
	if err != nil || !utf8.Valid(plaintext) {
		return "", errors.New("decrypt device Log2 fingerprint snapshot")
	}
	fields := strings.Split(string(plaintext), "#")
	if len(fields) != 111 {
		return "", errors.New("device Log2 fingerprint field count is invalid")
	}
	parts := strings.Split(fields[43], "|")
	kept := parts[:0]
	for _, part := range parts {
		if !strings.HasPrefix(part, "93-") && !strings.HasPrefix(part, "94-") {
			kept = append(kept, part)
		}
	}
	fields[43] = strings.Join(kept, "|")
	ciphertext, err := protocol.AESCBCEncryptBase64([]byte(strings.Join(fields, "#")), session.config.Key)
	if err != nil {
		return "", errors.New("encrypt device Log2 fingerprint snapshot")
	}
	return ciphertext, nil
}

// InitToken 返回已完成 Log1/2/3 的首枚 token。
func (session *Session) InitToken() (string, error) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed || !session.initialized {
		return "", errors.New("device session is not initialized")
	}
	return session.initial.InitToken, nil
}

// InitialResult 返回脱离内部 slice 的初始化摘要。
func (session *Session) InitialResult() (Result, error) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed || !session.initialized {
		return Result{}, errors.New("device session is not initialized")
	}
	return cloneResult(session.initial), nil
}

// PEDeviceConfig 返回本轮 Log1 解出的配置副本，供同一挑战的动态 PE VM 使用。
// 它只在进程内传递，不得记录或跨会话缓存。
func (session *Session) PEDeviceConfig() (protocol.DeviceConfig, error) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed || !session.initialized || session.completed {
		return protocol.DeviceConfig{}, errors.New("device session cannot provide PE config")
	}
	value := session.config
	value.ExtraSegments = append([]string(nil), session.config.ExtraSegments...)
	return value, nil
}

// TargetFirstTouchAgeMS 在本会话内首次生成后保持不变。
func (session *Session) TargetFirstTouchAgeMS() (int, error) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed || !session.initialized || session.completed {
		return 0, errors.New("device session cannot provide first touch age")
	}
	if !session.firstTouchAgeSet {
		value, err := randomRange(session.client.options.Sources.Entropy, session.client.options.FirstTouchAgeMin, session.client.options.FirstTouchAgeMax)
		if err != nil {
			return 0, errors.New("generate first touch age")
		}
		session.firstTouchAgeMS, session.firstTouchAgeSet = value, true
	}
	return session.firstTouchAgeMS, nil
}

// Complete 写入 getter 实参，刷新 Verify token，并且只发一枚最终 Log2。
func (session *Session) Complete(ctx context.Context, getterArgument string, events []InteractionEvent, postInteractionDelayMS int) (Result, error) {
	if ctx == nil {
		return Result{}, errors.New("device context is nil")
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed || !session.initialized {
		return Result{}, errors.New("device session is not initialized")
	}
	if session.completed {
		return Result{}, errors.New("device session is already complete")
	}
	if getterArgument == "" {
		return Result{}, errors.New("device getter argument is required")
	}
	if err := validateInteractionEvents(events); err != nil {
		return Result{}, err
	}
	if postInteractionDelayMS < 0 || postInteractionDelayMS > 500 {
		return Result{}, errors.New("post interaction delay must be within 0..500")
	}
	token, err := session.buildToken(getterArgument)
	if err != nil {
		return Result{}, err
	}
	snapshot, err := session.log2Snapshot(token.fingerprintCipher)
	if err != nil {
		return Result{}, err
	}
	log2, err := session.buildLog2(snapshot, 0)
	if err != nil {
		return Result{}, err
	}
	// 从发包前即进入终态：网络结果不明时也不重发最终 Log2。
	session.completed = true
	if _, err := session.postAction(ctx, "Log2", log2); err != nil {
		return Result{}, err
	}
	return session.result(session.initial.InitToken, token.token, token.timeMS, 1, len(events)), nil
}

func (session *Session) result(initToken, verifyToken string, tokenTimeMS int64, getterCount, eventCount int) Result {
	actions := append([]string(nil), session.actions...)
	return Result{
		InitToken: initToken, VerifyToken: verifyToken, RequestActions: actions,
		RequestCount: len(actions), FingerprintFieldCount: 111,
		GetterArgumentCount: getterCount, InteractionEventCount: eventCount,
		CollectorStartedMS: session.collectorStartedMS, TokenTimeMS: tokenTimeMS,
		GatherCost: session.gatherCost,
	}
}

// Close 只清理本 Session 状态，不关闭 Client 共享的 transport。
func (session *Session) Close() {
	if session == nil {
		return
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	session.clear()
}

func (session *Session) clear() {
	session.closed = true
	session.http = nil
	session.config = protocol.DeviceConfig{}
	session.stableFields = nil
	session.actions = nil
	session.initial = Result{}
}

func randomRange(entropy runtimekit.Entropy, minimum, maximum int) (int, error) {
	span := uint64(maximum) - uint64(minimum) + 1
	value, err := entropy.Uint64n(span)
	if err != nil {
		return 0, err
	}
	return minimum + int(value), nil
}

func safeDeviceLabel(value string) bool {
	if value == "" || strings.Contains(value, "#") || !utf8.ValidString(value) {
		return false
	}
	for _, character := range []byte(value) {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return true
}

func valueAfter(value, marker string) string {
	index := strings.Index(value, marker)
	if index < 0 {
		return ""
	}
	value = value[index+len(marker):]
	if end := strings.IndexByte(value, ' '); end >= 0 {
		value = value[:end]
	}
	return value
}

func cloneResult(value Result) Result {
	value.RequestActions = append([]string(nil), value.RequestActions...)
	return value
}
