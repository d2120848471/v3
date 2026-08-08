package protocol

import (
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

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
