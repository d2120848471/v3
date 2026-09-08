package protocol

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
)

// RPCV1CanonicalQuery 排除 Signature，按原始 key 排序后编码。
func RPCV1CanonicalQuery(params map[string]string) (string, error) {
	keys := make([]string, 0, len(params))
	for key := range params {
		if key != "Signature" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for index, key := range keys {
		encodedKey, err := RPCPercentEncode(key)
		if err != nil {
			return "", fmt.Errorf("encode RPC key: %w", err)
		}
		encodedValue, err := RPCPercentEncode(params[key])
		if err != nil {
			return "", fmt.Errorf("encode RPC value for %q: %w", key, err)
		}
		parts[index] = encodedKey + "=" + encodedValue
	}
	return strings.Join(parts, "&"), nil
}

// RPCV1StringToSign 构造固定 POST / 的 RPC v1 签名原文。
func RPCV1StringToSign(params map[string]string) (string, error) {
	canonical, err := RPCV1CanonicalQuery(params)
	if err != nil {
		return "", err
	}
	encoded, err := RPCPercentEncode(canonical)
	if err != nil {
		return "", err
	}
	return "POST&%2F&" + encoded, nil
}

// RPCV1Signature 返回 Base64(HMAC-SHA1(secret+"&", StringToSign))。
func RPCV1Signature(params map[string]string, secret string) (string, error) {
	if secret == "" {
		return "", fmt.Errorf("RPC secret must not be empty")
	}
	text, err := RPCV1StringToSign(params)
	if err != nil {
		return "", err
	}
	digest := hmac.New(sha1.New, []byte(secret+"&"))
	_, _ = digest.Write([]byte(text))
	return base64.StdEncoding.EncodeToString(digest.Sum(nil)), nil
}
