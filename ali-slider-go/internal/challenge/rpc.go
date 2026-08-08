package challenge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/protocol"
	"github.com/d2120848471/v3/ali-slider-go/internal/runtimekit"
)

const (
	captchaAPIVersion   = "2023-03-05"
	defaultOrigin       = "http://localhost:38185"
	defaultReferer      = defaultOrigin + "/"
	maxRPCResponseBytes = 2 << 20
	verifyFutureSkew    = 2 * time.Second
)

// CaptchaChallenge 是 Init 成功后的非敏感控制字段；CertifyID 不得进入日志。
type CaptchaChallenge struct {
	CertifyID       string
	ImagePath       string
	PuzzleImagePath string
	StaticPath      string
	CaptchaType     string
	InitStartedMS   int64
	InitFinishedMS  int64
}

func (CaptchaChallenge) String() string   { return "challenge.CaptchaChallenge{redacted}" }
func (CaptchaChallenge) GoString() string { return "challenge.CaptchaChallenge{redacted}" }

// VerifyResult 是服务端完成态；VerifyCode 非 T001 仍是正常 200 业务结果。
type VerifyResult struct {
	VerifyCode    string
	VerifyResult  bool
	SecurityToken string
	CertifyID     string
}

func (VerifyResult) String() string   { return "challenge.VerifyResult{redacted}" }
func (VerifyResult) GoString() string { return "challenge.VerifyResult{redacted}" }

func (r VerifyResult) Succeeded() bool {
	return r.VerifyCode == "T001" && r.VerifyResult && r.SecurityToken != ""
}

// rpcError 区分交通失败与 HTTP 2xx 内的协议/本地合同失败。
// Error 文本固定，cause 只用于 errors.Is/As，不会进入 HTTP 或普通日志。
type rpcError struct {
	network bool
	message string
	cause   error
}

func (failure *rpcError) Error() string { return failure.message }
func (failure *rpcError) Unwrap() error { return failure.cause }
func (failure *rpcError) NetworkFailure() bool {
	return failure != nil && failure.network
}

func rpcProtocolError(message string, cause error) error {
	return &rpcError{message: message, cause: cause}
}

func rpcNetworkError(message string, cause error) error {
	return &rpcError{network: true, message: message, cause: cause}
}

// RPCClient 一个实例只服务一个 CertifyId；Verify 的尝试位由 mutex 保护。
type RPCClient struct {
	client    *http.Client
	profile   device.Profile
	sources   runtimekit.Sources
	secrets   protocol.FrontendSecrets
	sceneID   string
	prefix    string
	rpcKeyID  string
	initURL   string
	verifyURL string

	stateMu         sync.Mutex
	initAttempted   bool
	issuedCertifyID string
	verifyAttempted bool
}

type RPCOptions struct {
	Transport http.RoundTripper
	Profile   device.Profile
	Sources   runtimekit.Sources
	SceneID   string
	Prefix    string
	RPCKeyID  string
}

func NewRPCClient(options RPCOptions) (*RPCClient, error) {
	if options.Transport == nil || options.SceneID == "" || options.Prefix == "" {
		return nil, errors.New("captcha RPC requires transport, SceneId and prefix")
	}
	if err := options.Sources.Validate(); err != nil {
		return nil, err
	}
	for _, value := range []byte(options.Prefix) {
		if (value < '0' || value > '9') && (value < 'A' || value > 'Z') && (value < 'a' || value > 'z') {
			return nil, errors.New("captcha prefix must be ASCII alphanumeric")
		}
	}
	secretsValue, err := protocol.ResolveFrontendSecrets()
	if err != nil {
		return nil, errors.New("resolve captcha RPC credentials")
	}
	rpcKeyID := options.RPCKeyID
	if rpcKeyID == "" {
		rpcKeyID = secretsValue.MainRPCKeyID()
	}
	return &RPCClient{
		client: &http.Client{
			Transport:     options.Transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		profile: options.Profile, sources: options.Sources, secrets: secretsValue,
		sceneID: options.SceneID, prefix: options.Prefix, rpcKeyID: rpcKeyID,
		initURL:   "https://" + options.Prefix + ".captcha-open.aliyuncs.com/",
		verifyURL: "https://" + options.Prefix + "-verify.captcha-open.aliyuncs.com/",
	}, nil
}

// Init 获取一轮新挑战；响应原文不会保留在结果或错误中。
func (client *RPCClient) Init(ctx context.Context, deviceToken string) (CaptchaChallenge, error) {
	if deviceToken == "" {
		return CaptchaChallenge{}, rpcProtocolError("Init requires a device token", nil)
	}
	client.stateMu.Lock()
	if client.initAttempted {
		client.stateMu.Unlock()
		return CaptchaChallenge{}, rpcProtocolError("Init has already been attempted", nil)
	}
	client.initAttempted = true
	client.stateMu.Unlock()
	started := client.sources.Clock.Now()
	payload, err := client.post(ctx, client.initURL, "InitCaptchaV3", []protocol.Field{
		{Key: "SceneId", Value: client.sceneID},
		{Key: "Language", Value: "cn"},
		{Key: "Mode", Value: "popup"},
		{Key: "DeviceToken", Value: deviceToken},
	})
	finished := client.sources.Clock.Now()
	if err != nil {
		return CaptchaChallenge{}, err
	}
	var response struct {
		Success     bool   `json:"Success"`
		Code        string `json:"Code"`
		CertifyID   string `json:"CertifyId"`
		Image       string `json:"Image"`
		PuzzleImage string `json:"PuzzleImage"`
		StaticPath  string `json:"StaticPath"`
		CaptchaType string `json:"CaptchaType"`
	}
	if err := decodeObject(payload, &response); err != nil {
		return CaptchaChallenge{}, rpcProtocolError("Init response schema is invalid", err)
	}
	if !response.Success || response.Code != "Success" || response.CertifyID == "" {
		return CaptchaChallenge{}, rpcProtocolError("InitCaptchaV3 returned failure", nil)
	}
	imagePath, err := ValidateAssetPath(response.Image, "Image")
	if err != nil {
		return CaptchaChallenge{}, rpcProtocolError("Init image path is invalid", err)
	}
	puzzlePath, err := ValidateAssetPath(response.PuzzleImage, "PuzzleImage")
	if err != nil {
		return CaptchaChallenge{}, rpcProtocolError("Init puzzle path is invalid", err)
	}
	staticPath, err := ValidateAssetPath(response.StaticPath, "StaticPath")
	if err != nil {
		return CaptchaChallenge{}, rpcProtocolError("Init static path is invalid", err)
	}
	client.stateMu.Lock()
	client.issuedCertifyID = response.CertifyID
	client.stateMu.Unlock()
	return CaptchaChallenge{
		CertifyID: response.CertifyID, ImagePath: imagePath, PuzzleImagePath: puzzlePath,
		StaticPath: staticPath, CaptchaType: response.CaptchaType,
		InitStartedMS: started.UnixMilli(), InitFinishedMS: finished.UnixMilli(),
	}, nil
}

// Verify 发送该 RPCClient 生命周期内唯一一次 Verify。网络错误也消耗尝试位。
func (client *RPCClient) Verify(ctx context.Context, challenge CaptchaChallenge, deviceToken, data string, verifyTimeMS int64) (VerifyResult, error) {
	if challenge.CertifyID == "" || deviceToken == "" || data == "" {
		return VerifyResult{}, rpcProtocolError("Verify fields must not be empty", nil)
	}
	if verifyTimeMS > client.sources.Clock.Now().Add(verifyFutureSkew).UnixMilli() {
		return VerifyResult{}, rpcProtocolError("VerifyTime exceeds the future-skew limit", nil)
	}
	client.stateMu.Lock()
	if client.issuedCertifyID == "" || challenge.CertifyID != client.issuedCertifyID {
		client.stateMu.Unlock()
		return VerifyResult{}, rpcProtocolError("Verify challenge was not issued by this client", nil)
	}
	if client.verifyAttempted {
		client.stateMu.Unlock()
		return VerifyResult{}, rpcProtocolError("Verify has already been attempted", nil)
	}
	client.verifyAttempted = true
	client.stateMu.Unlock()

	verifyParam, err := protocol.BuildVerifyCaptchaParam(client.sceneID, challenge.CertifyID, deviceToken, data)
	if err != nil {
		return VerifyResult{}, rpcProtocolError("build VerifyCaptchaParam", err)
	}
	payload, err := client.post(ctx, client.verifyURL, "VerifyCaptchaV3", []protocol.Field{
		{Key: "SceneId", Value: client.sceneID},
		{Key: "CertifyId", Value: challenge.CertifyID},
		{Key: "CaptchaVerifyParam", Value: verifyParam},
	})
	if err != nil {
		return VerifyResult{}, err
	}
	var response struct {
		Code   string `json:"Code"`
		Result *struct {
			VerifyCode    string `json:"VerifyCode"`
			VerifyResult  bool   `json:"VerifyResult"`
			SecurityToken string `json:"securityToken"`
			CertifyID     string `json:"certifyId"`
		} `json:"Result"`
	}
	if err := decodeObject(payload, &response); err != nil {
		return VerifyResult{}, rpcProtocolError("Verify response schema is invalid", err)
	}
	// 兼容已冻结的 Python 合同：Result 存在时以嵌套字段为权威；
	// 仅缺少 Result 时回退顶层 Code，缺少 certifyId 回退本请求的 ID。
	result := VerifyResult{VerifyCode: response.Code, CertifyID: challenge.CertifyID}
	if response.Result != nil {
		result.VerifyCode = response.Result.VerifyCode
		result.VerifyResult = response.Result.VerifyResult
		result.SecurityToken = response.Result.SecurityToken
		if response.Result.CertifyID != "" {
			result.CertifyID = response.Result.CertifyID
		}
	}
	return result, nil
}

func (client *RPCClient) post(ctx context.Context, endpoint, action string, extras []protocol.Field) ([]byte, error) {
	nonce, err := runtimekit.UUIDv4(client.sources.Entropy)
	if err != nil {
		return nil, errors.New("generate RPC nonce")
	}
	fields := []protocol.Field{
		{Key: "AaduaneId", Value: client.rpcKeyID},
		{Key: "SignatureMethod", Value: "HMAC-SHA1"},
		{Key: "SignatureVersion", Value: "1.0"},
		{Key: "Format", Value: "JSON"},
		{Key: "Timestamp", Value: client.sources.Clock.Now().UTC().Format("2006-01-02T15:04:05Z")},
		{Key: "Version", Value: captchaAPIVersion},
		{Key: "Action", Value: action},
	}
	fields = append(fields, extras...)
	fields = append(fields, protocol.Field{Key: "SignatureNonce", Value: nonce})
	params := make(map[string]string, len(fields))
	for _, field := range fields {
		params[field.Key] = field.Value
	}
	signature, err := protocol.RPCV1Signature(params, client.secrets.MainRPCKeySecret())
	if err != nil {
		return nil, errors.New("sign captcha RPC")
	}
	fields = append(fields, protocol.Field{Key: "Signature", Value: signature})
	body, err := protocol.JSFormURLEncode(fields)
	if err != nil {
		return nil, errors.New("encode captcha RPC form")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		return nil, errors.New("build captcha RPC request")
	}
	if action == "VerifyCaptchaV3" {
		// strings.Reader 会让 net/http 自动生成 GetBody。若保留它，HTTP/2
		// 可能在 GOAWAY/REFUSED_STREAM 后透明重放已经发出的 Verify。
		// 清空回卷入口但保留 ContentLength，确保网络结果未知时无法重发。
		request.GetBody = nil
	}
	for key, value := range client.profile.BrowserHeaders(defaultReferer, defaultOrigin, "empty", "cors", true) {
		request.Header.Set(key, value)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	response, err := client.client.Do(request)
	if err != nil {
		return nil, rpcNetworkError("captcha RPC request failed", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, rpcNetworkError(fmt.Sprintf("captcha RPC returned HTTP %d", response.StatusCode), nil)
	}
	content, err := io.ReadAll(io.LimitReader(response.Body, maxRPCResponseBytes+1))
	if err != nil {
		return nil, rpcNetworkError("read captcha RPC response", err)
	}
	if len(content) > maxRPCResponseBytes {
		return nil, rpcProtocolError("captcha RPC response exceeds size limit", nil)
	}
	return content, nil
}

func decodeObject(content []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(content))
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("JSON response has trailing content")
	}
	return nil
}
