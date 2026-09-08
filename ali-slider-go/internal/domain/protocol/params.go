package protocol

import (
	"bytes"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
)

const maxBusinessCaptchaVerifyParamBytes = 16 << 10

// BusinessCaptchaVerifyParam 是 SDK success 回调交给业务端的已签名参数。
type BusinessCaptchaVerifyParam struct {
	CertifyID     string `json:"certifyId"`
	SceneID       string `json:"sceneId"`
	IsSign        bool   `json:"isSign"`
	SecurityToken string `json:"securityToken"`
}

func BuildVerifyCaptchaParam(sceneID, certifyID, deviceToken, data string) (string, error) {
	if sceneID == "" || certifyID == "" || deviceToken == "" || data == "" {
		return "", fmt.Errorf("VerifyCaptchaParam fields must not be empty")
	}
	return CompactJSON(struct {
		SceneID     string `json:"sceneId"`
		CertifyID   string `json:"certifyId"`
		DeviceToken string `json:"deviceToken"`
		Data        string `json:"data"`
	}{sceneID, certifyID, deviceToken, data})
}

func BuildBusinessCaptchaVerifyParam(certifyID, sceneID, securityToken string, isSign bool) (string, error) {
	if certifyID == "" || sceneID == "" || securityToken == "" {
		return "", fmt.Errorf("business captcha fields must not be empty")
	}
	text, err := CompactJSON(struct {
		CertifyID     string `json:"certifyId"`
		SceneID       string `json:"sceneId"`
		IsSign        bool   `json:"isSign"`
		SecurityToken string `json:"securityToken"`
	}{certifyID, sceneID, isSign, securityToken})
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString([]byte(text)), nil
}

// ParseBusinessCaptchaVerifyParam 严格解析 SDK success 回调，不接受扩展字段或空关键值。
func ParseBusinessCaptchaVerifyParam(value string) (BusinessCaptchaVerifyParam, error) {
	if value == "" || len(value) > maxBusinessCaptchaVerifyParamBytes {
		return BusinessCaptchaVerifyParam{}, fmt.Errorf("business captcha parameter is empty or too large")
	}
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(decoded) == 0 || len(decoded) > maxBusinessCaptchaVerifyParamBytes {
		return BusinessCaptchaVerifyParam{}, fmt.Errorf("business captcha parameter is not valid base64")
	}
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	decoder.DisallowUnknownFields()
	var result BusinessCaptchaVerifyParam
	if err := decoder.Decode(&result); err != nil {
		return BusinessCaptchaVerifyParam{}, fmt.Errorf("business captcha parameter is not valid JSON")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return BusinessCaptchaVerifyParam{}, fmt.Errorf("business captcha parameter has trailing content")
	}
	if result.CertifyID == "" || result.SceneID == "" || result.SecurityToken == "" {
		return BusinessCaptchaVerifyParam{}, fmt.Errorf("business captcha parameter fields must not be empty")
	}
	return result, nil
}

func BuildBusinessSignedQuery(bodyText, rawToken, salt string, lgtime int64, lgnonce string) (string, string, error) {
	if lgtime < 0 || len(lgnonce) != 32 {
		return "", "", fmt.Errorf("invalid business nonce or time")
	}
	if _, err := hex.DecodeString(lgnonce); err != nil {
		return "", "", fmt.Errorf("lgnonce must be 32 lowercase hex characters")
	}
	for _, char := range []byte(lgnonce) {
		if char >= 'A' && char <= 'F' {
			return "", "", fmt.Errorf("lgnonce must be 32 lowercase hex characters")
		}
	}
	canonical := fmt.Sprintf("lgnonce=%s&lgtime=%d", lgnonce, lgtime)
	digest := sha512.Sum512([]byte(canonical + bodyText + rawToken + salt))
	return canonical, canonical + "&lgsign=" + hex.EncodeToString(digest[:]), nil
}
