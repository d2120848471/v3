package protocol

import (
	"crypto/hmac"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

type DeviceConfig struct {
	Key            string
	Switch         int
	SessionID      string
	Version        string
	PluginElements string
	PluginResource string
	GlobalVariable string
	Timestamp      string
	IP             string
	ExtraSegments  []string
}

type DeviceToken struct {
	Platform          string
	SessionID         string
	FingerprintCipher string
	GatherCost        string
	Checksum          string
}

func validateTokenPart(value, label string) error {
	if value == "" || strings.Contains(value, "#") {
		return fmt.Errorf("%s must be non-empty and must not contain #", label)
	}
	return nil
}

func validDecimal(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range []byte(value) {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

// BuildDeviceToken 保留 GatherCost 文本（包括前导零）参与 MD5。
func BuildDeviceToken(sessionID, fingerprintCipher, gatherCost, salt string) (string, error) {
	if err := validateTokenPart(sessionID, "device token session"); err != nil {
		return "", err
	}
	if err := validateTokenPart(fingerprintCipher, "device token fingerprint cipher"); err != nil {
		return "", err
	}
	if !validDecimal(gatherCost) {
		return "", fmt.Errorf("GatherCost must be decimal text")
	}
	if salt == "" {
		return "", fmt.Errorf("device token salt must not be empty")
	}
	prefix := "WEB#" + sessionID + "#" + fingerprintCipher + "#" + gatherCost + "#"
	digest := md5.Sum(append([]byte(prefix), []byte(salt)...))
	text := prefix + hex.EncodeToString(digest[:])
	return base64.StdEncoding.EncodeToString([]byte(text)), nil
}

func ParseDeviceToken(token, salt string) (DeviceToken, error) {
	decoded, err := decodeBase64Strict(token)
	if err != nil {
		return DeviceToken{}, fmt.Errorf("decode device token: %w", err)
	}
	if !utf8.Valid(decoded) {
		return DeviceToken{}, fmt.Errorf("device token is not UTF-8")
	}
	parts := strings.Split(string(decoded), "#")
	if len(parts) != 5 || parts[0] != "WEB" {
		return DeviceToken{}, fmt.Errorf("device token must contain five WEB segments")
	}
	if err := validateTokenPart(parts[1], "device token session"); err != nil {
		return DeviceToken{}, err
	}
	if err := validateTokenPart(parts[2], "device token fingerprint cipher"); err != nil {
		return DeviceToken{}, err
	}
	if !validDecimal(parts[3]) {
		return DeviceToken{}, fmt.Errorf("GatherCost must be decimal text")
	}
	if len(parts[4]) != md5.Size*2 {
		return DeviceToken{}, fmt.Errorf("device token checksum must be lowercase MD5")
	}
	checksum, err := hex.DecodeString(parts[4])
	if err != nil || strings.ToLower(parts[4]) != parts[4] {
		return DeviceToken{}, fmt.Errorf("device token checksum must be lowercase MD5")
	}
	if salt == "" {
		return DeviceToken{}, fmt.Errorf("device token salt must not be empty")
	}
	prefix := strings.Join(parts[:4], "#") + "#"
	expected := md5.Sum(append([]byte(prefix), []byte(salt)...))
	if !hmac.Equal(checksum, expected[:]) {
		return DeviceToken{}, fmt.Errorf("device token MD5 mismatch")
	}
	return DeviceToken{parts[0], parts[1], parts[2], parts[3], parts[4]}, nil
}

func decodeUTF8Base64(value, label string) (string, error) {
	if value == "" {
		return "", nil
	}
	decoded, err := decodeBase64Strict(value)
	if err != nil || !utf8.Valid(decoded) {
		return "", fmt.Errorf("DeviceConfig.%s base64 is invalid", label)
	}
	return string(decoded), nil
}

// ParseDeviceConfig 解密并解析 Log1 的九段以上 DeviceConfig。
func ParseDeviceConfig(ciphertext, responseKey string) (DeviceConfig, error) {
	plaintext, err := AESCBCDecryptBase64(ciphertext, responseKey)
	if err != nil || !utf8.Valid(plaintext) {
		return DeviceConfig{}, fmt.Errorf("decrypt Log1 DeviceConfig")
	}
	parts := strings.Split(string(plaintext), "#")
	if len(parts) < 9 {
		return DeviceConfig{}, fmt.Errorf("DeviceConfig has fewer than nine segments")
	}
	key, err := decodeUTF8Base64(parts[0], "key")
	if err != nil {
		return DeviceConfig{}, err
	}
	switchText, err := decodeUTF8Base64(parts[1], "switch")
	if err != nil {
		return DeviceConfig{}, err
	}
	switchValue, err := strconv.Atoi(switchText)
	if err != nil {
		return DeviceConfig{}, fmt.Errorf("DeviceConfig.switch is invalid")
	}
	pluginElements, err := decodeUTF8Base64(parts[4], "pluginElements")
	if err != nil {
		return DeviceConfig{}, err
	}
	pluginResource, err := decodeUTF8Base64(parts[5], "pluginResource")
	if err != nil {
		return DeviceConfig{}, err
	}
	globalVariable, err := decodeUTF8Base64(parts[6], "globalVariable")
	if err != nil {
		return DeviceConfig{}, err
	}
	if len([]byte(key)) != 16 || parts[2] == "" {
		return DeviceConfig{}, fmt.Errorf("DeviceConfig key/session is invalid")
	}
	return DeviceConfig{key, switchValue, parts[2], parts[3], pluginElements, pluginResource, globalVariable, parts[7], parts[8], append([]string(nil), parts[9:]...)}, nil
}
