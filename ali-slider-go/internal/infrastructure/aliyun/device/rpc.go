package device

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/d2120848471/v3/ali-slider-go/internal/domain/protocol"
	"github.com/d2120848471/v3/ali-slider-go/internal/foundation/runtimekit"
)

const (
	deviceAPIKey     = "ab034ec0643f91399eb33e062dc7fae1"
	deviceAppName    = "saf-captcha"
	deviceAppVersion = "W20220202"
	deviceField0     = "W.10054"
	devicePlatform   = "W.10001.c"
	deviceAPIVersion = "2020-10-15"
	deviceOrigin     = "http://localhost:38185"
	deviceLocation   = deviceOrigin + "/package_product/pages/PaySubmit/index"
	maxRPCBodyBytes  = 1 << 20
)

type rpcPayload struct {
	DeviceConfig string
}

// NetworkError 只标记已进入 HTTP 交通后的连接、响应读取或非 2xx 失败。
// 稳定 Error 文本不携带上游正文、token 或代理凭据。
type NetworkError struct {
	operation string
	cause     error
}

func (failure *NetworkError) Error() string { return failure.operation + " failed" }
func (failure *NetworkError) Unwrap() error { return failure.cause }
func (failure *NetworkError) NetworkFailure() bool {
	return failure != nil
}

func networkError(operation string, cause error) error {
	return &NetworkError{operation: operation, cause: cause}
}

func (session *Session) postAction(ctx context.Context, action, data string) (rpcPayload, error) {
	nonce, err := runtimekit.UUIDv4(session.client.options.Sources.Entropy)
	if err != nil {
		return rpcPayload{}, fmt.Errorf("generate device RPC %s nonce", action)
	}
	params := map[string]string{
		"AaduaneId":        session.client.secrets.DeviceRPCKeyID(),
		"Version":          deviceAPIVersion,
		"SignatureMethod":  "HMAC-SHA1",
		"SignatureVersion": "1.0",
		"Format":           "JSON",
		"Action":           action,
		"Data":             data,
		"SignatureNonce":   nonce,
	}
	signature, err := protocol.RPCV1Signature(params, session.client.secrets.DeviceRPCKeySecret())
	if err != nil {
		return rpcPayload{}, fmt.Errorf("sign device RPC %s", action)
	}
	fields := []protocol.Field{
		{Key: "AaduaneId", Value: params["AaduaneId"]},
		{Key: "Version", Value: params["Version"]},
		{Key: "SignatureMethod", Value: params["SignatureMethod"]},
		{Key: "SignatureVersion", Value: params["SignatureVersion"]},
		{Key: "Format", Value: params["Format"]},
		{Key: "Action", Value: action},
		{Key: "Data", Value: data},
		{Key: "SignatureNonce", Value: nonce},
		{Key: "Signature", Value: signature},
	}
	body, err := protocol.JSFormURLEncode(fields)
	if err != nil {
		return rpcPayload{}, fmt.Errorf("encode device RPC %s", action)
	}
	requestContext, cancel := context.WithTimeout(ctx, session.client.options.Timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, session.client.options.Endpoint, strings.NewReader(body))
	if err != nil {
		return rpcPayload{}, fmt.Errorf("build device RPC %s request", action)
	}
	// Device action 含一次性时序与会话状态；清空 GetBody，防止 net/http
	// 在已发送 body 后因 HTTP/2 流错误透明重放。ContentLength 保持不变。
	request.GetBody = nil
	for key, value := range session.client.options.Profile.BrowserHeaders(deviceOrigin+"/", deviceOrigin, "empty", "cors", true) {
		request.Header.Set(key, value)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	if session.http == nil {
		return rpcPayload{}, errors.New("device HTTP session is closed")
	}
	response, err := session.http.Do(request)
	if err != nil {
		return rpcPayload{}, networkError("device RPC "+action+" request", err)
	}
	if response == nil || response.Body == nil {
		return rpcPayload{}, networkError("device RPC "+action+" response", nil)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return rpcPayload{}, networkError("device RPC "+action+" HTTP response", nil)
	}
	limited := io.LimitReader(response.Body, maxRPCBodyBytes+1)
	responseBody, err := io.ReadAll(limited)
	if err != nil || len(responseBody) > maxRPCBodyBytes {
		return rpcPayload{}, networkError("device RPC "+action+" response read", err)
	}
	var decoded struct {
		Code         json.RawMessage `json:"Code"`
		ResultObject json.RawMessage `json:"ResultObject"`
	}
	if err := json.Unmarshal(responseBody, &decoded); err != nil {
		return rpcPayload{}, fmt.Errorf("device RPC %s returned invalid JSON (%s)", action, safeResponseMetadata(response, responseBody))
	}
	if !successCode(decoded.Code) {
		return rpcPayload{}, fmt.Errorf("device RPC %s returned protocol code %s", action, safeResponseCode(decoded.Code))
	}
	payload := rpcPayload{}
	if action == "Log1" && len(decoded.ResultObject) > 0 && string(decoded.ResultObject) != "null" {
		var resultObject struct {
			DeviceConfig string `json:"DeviceConfig"`
		}
		if err := json.Unmarshal(decoded.ResultObject, &resultObject); err != nil {
			return rpcPayload{}, errors.New("device RPC Log1 returned invalid ResultObject")
		}
		payload.DeviceConfig = resultObject.DeviceConfig
	}
	session.actions = append(session.actions, action)
	return payload, nil
}

// safeResponseMetadata 只输出诊断协议格式所需的有界元数据。
// 它不返回正文、响应头原值、Cookie、令牌或业务标识。
func safeResponseMetadata(response *http.Response, body []byte) string {
	protocolName := "unknown"
	switch response.Proto {
	case "HTTP/1.0", "HTTP/1.1", "HTTP/2.0", "HTTP/3.0":
		protocolName = response.Proto
	}
	contentType := "unknown"
	if mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type")); err == nil {
		switch mediaType {
		case "application/json", "application/problem+json", "text/html", "text/plain", "application/octet-stream":
			contentType = mediaType
		}
	}
	contentEncoding := "identity"
	if value := strings.ToLower(strings.TrimSpace(response.Header.Get("Content-Encoding"))); value != "" {
		switch value {
		case "gzip", "deflate", "br", "zstd", "identity":
			contentEncoding = value
		default:
			contentEncoding = "unknown"
		}
	}
	prefixHex := "empty"
	if len(body) > 0 {
		prefixHex = hex.EncodeToString(body[:min(len(body), 4)])
	}
	return fmt.Sprintf(
		"proto=%s contentType=%s contentEncoding=%s uncompressed=%t bodyBytes=%d prefixHex=%s",
		protocolName, contentType, contentEncoding, response.Uncompressed, len(body), prefixHex,
	)
}

func successCode(raw json.RawMessage) bool {
	value := strings.TrimSpace(string(raw))
	return value == "200" || value == `"200"`
}

func safeResponseCode(raw json.RawMessage) string {
	value := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if value == "" || len(value) > 32 {
		return "non-success"
	}
	for _, character := range []byte(value) {
		if (character < '0' || character > '9') && (character < 'A' || character > 'Z') && (character < 'a' || character > 'z') && character != '-' && character != '_' {
			return "non-success"
		}
	}
	return value
}

func (session *Session) buildLog1() (string, error) {
	innerText := strings.Join([]string{devicePlatform, deviceAppName, "scene", "captcha-front", session.client.options.Prefix, session.client.options.Region}, "#")
	inner, err := protocol.AESCBCEncryptBase64([]byte(innerText), session.client.secrets.DeviceFlagKey())
	if err != nil {
		return "", err
	}
	outer := deviceAPIKey + "#W#" + inner + "#" + deviceAppVersion + "#CLOUD#"
	return protocol.AESCBCEncryptBase64([]byte(outer), session.client.secrets.DeviceRequestKey())
}

func (session *Session) buildLog2(fingerprintCipher string, cost int) (string, error) {
	appCipher, fieldCipher, timeCipher, pairCipher, err := session.logCiphers()
	if err != nil {
		return "", err
	}
	inner := strings.Join([]string{session.config.SessionID, fingerprintCipher, appCipher, fieldCipher, "", timeCipher}, "#")
	outer := strings.Join([]string{
		deviceAPIKey, "W", pairCipher, deviceAppVersion, "CLOUD", strconv.Itoa(cost), "501",
		base64.StdEncoding.EncodeToString([]byte(inner)),
	}, "#")
	ciphertext, err := protocol.AESCBCEncryptBase64([]byte(outer), session.client.secrets.DeviceUploadKey())
	if err != nil {
		return "", errors.New("encrypt device Log2")
	}
	return ciphertext, nil
}

func (session *Session) buildLog3() (string, error) {
	appCipher, fieldCipher, markerTimeCipher, pairCipher, err := session.logCiphers()
	if err != nil {
		return "", err
	}
	markerHeader := strings.Join([]string{session.config.SessionID, "", appCipher, fieldCipher, "", markerTimeCipher}, "#")
	eventJSON, err := protocol.CompactJSON(struct {
		Mousemove    []struct{} `json:"mousemove"`
		Mouseclick   []struct{} `json:"mouseclick"`
		Keyup        []struct{} `json:"keyup"`
		ScrollTop    []struct{} `json:"scrollTop"`
		ScrollLeft   []struct{} `json:"scrollLeft"`
		PointerEvent []struct{} `json:"pointerEvent"`
		ClientType   string     `json:"clientType"`
		StartTime    int64      `json:"startTime"`
		Timestamp    string     `json:"timestamp"`
	}{
		Mousemove: []struct{}{}, Mouseclick: []struct{}{}, Keyup: []struct{}{},
		ScrollTop: []struct{}{}, ScrollLeft: []struct{}{}, PointerEvent: []struct{}{},
		ClientType: "mobile", StartTime: session.combatStartedMS, Timestamp: session.config.Timestamp,
	})
	if err != nil {
		return "", errors.New("encode device Log3 events")
	}
	eventCipher, err := protocol.AESCBCEncryptBase64([]byte(eventJSON), session.config.Key)
	if err != nil {
		return "", errors.New("encrypt device Log3 events")
	}
	combatTimeCipher, err := protocol.AESCBCEncryptBase64([]byte(strconv.FormatInt(session.client.options.Sources.Clock.Now().UnixMilli(), 10)), session.config.Key)
	if err != nil {
		return "", errors.New("encrypt device Log3 time")
	}
	combatHeader := strings.Join([]string{session.config.SessionID, eventCipher, appCipher, fieldCipher, "", combatTimeCipher}, "#")
	stage := "511#" + base64.StdEncoding.EncodeToString([]byte(markerHeader)) + "-504#" + base64.StdEncoding.EncodeToString([]byte(combatHeader))
	outer := strings.Join([]string{
		deviceAPIKey, "W", pairCipher, deviceAppVersion, "CLOUD", "",
		base64.StdEncoding.EncodeToString([]byte(stage)),
	}, "#")
	ciphertext, err := protocol.AESCBCEncryptBase64([]byte(outer), session.client.secrets.DeviceUploadKey())
	if err != nil {
		return "", errors.New("encrypt device Log3")
	}
	return ciphertext, nil
}

func (session *Session) logCiphers() (appCipher, fieldCipher, timeCipher, pairCipher string, err error) {
	appCipher, err = protocol.AESCBCEncryptBase64([]byte(deviceAppName), session.config.Key)
	if err != nil {
		return "", "", "", "", errors.New("encrypt device application marker")
	}
	fieldCipher, err = protocol.AESCBCEncryptBase64([]byte(deviceField0), session.config.Key)
	if err != nil {
		return "", "", "", "", errors.New("encrypt device field marker")
	}
	timeCipher, err = protocol.AESCBCEncryptBase64([]byte(strconv.FormatInt(session.client.options.Sources.Clock.Now().UnixMilli(), 10)), session.config.Key)
	if err != nil {
		return "", "", "", "", errors.New("encrypt device time marker")
	}
	pairCipher, err = protocol.AESCBCEncryptBase64([]byte(deviceField0+"#"+deviceAppName), session.config.Key)
	if err != nil {
		return "", "", "", "", errors.New("encrypt device pair marker")
	}
	return appCipher, fieldCipher, timeCipher, pairCipher, nil
}
