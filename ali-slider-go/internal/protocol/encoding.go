// Package protocol 实现验证码链路中可离线验证的字节级协议。
package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Field 表示一个不可重排的 form 字段。
type Field struct {
	Key   string
	Value string
}

// CompactJSON 输出不转义 HTML、不带空白和换行的 UTF-8 JSON。
func CompactJSON(value any) (string, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "", fmt.Errorf("encode compact JSON: %w", err)
	}
	return strings.TrimSuffix(buffer.String(), "\n"), nil
}

// JSEncodeURIComponent 复制 JavaScript encodeURIComponent 的保留字符集。
func JSEncodeURIComponent(value string) (string, error) {
	if !utf8.ValidString(value) {
		return "", fmt.Errorf("encodeURIComponent input is not UTF-8")
	}
	const hex = "0123456789ABCDEF"
	var output strings.Builder
	output.Grow(len(value))
	for _, char := range []byte(value) {
		if isURIComponentSafe(char) {
			output.WriteByte(char)
			continue
		}
		output.WriteByte('%')
		output.WriteByte(hex[char>>4])
		output.WriteByte(hex[char&15])
	}
	return output.String(), nil
}

func isURIComponentSafe(char byte) bool {
	return char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
		char >= '0' && char <= '9' || strings.ContainsRune("-_.!~*'()", rune(char))
}

// RPCPercentEncode 保留前端三次非正则 replace 的“只替换首个”语义。
func RPCPercentEncode(value string) (string, error) {
	encoded, err := JSEncodeURIComponent(value)
	if err != nil {
		return "", err
	}
	encoded = strings.Replace(encoded, "+", "%20", 1)
	encoded = strings.Replace(encoded, "*", "%2A", 1)
	encoded = strings.Replace(encoded, "%7E", "~", 1)
	return encoded, nil
}

// JSFormURLEncode 按输入顺序构造 form body，空格编码为 %20。
func JSFormURLEncode(fields []Field) (string, error) {
	parts := make([]string, len(fields))
	for index, field := range fields {
		key, err := JSEncodeURIComponent(field.Key)
		if err != nil {
			return "", fmt.Errorf("encode form key %d: %w", index, err)
		}
		value, err := JSEncodeURIComponent(field.Value)
		if err != nil {
			return "", fmt.Errorf("encode form value %d: %w", index, err)
		}
		parts[index] = key + "=" + value
	}
	return strings.Join(parts, "&"), nil
}
