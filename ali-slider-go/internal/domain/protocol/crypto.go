package protocol

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"fmt"
	"strings"
)

const DefaultDeviceConfigIV = "0123456789ABCDEF"

func decodeBase64Strict(value string) ([]byte, error) {
	if strings.ContainsAny(value, "\r\n") {
		return nil, fmt.Errorf("base64 contains line breaks")
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("invalid standard base64: %w", err)
	}
	return decoded, nil
}

// AESCBCEncryptBase64 执行 UTF-8/原始字节→PKCS7→AES-128-CBC→标准 Base64。
func AESCBCEncryptBase64(plaintext []byte, key string) (string, error) {
	return AESCBCEncryptBase64WithIV(plaintext, []byte(key), []byte(DefaultDeviceConfigIV))
}

func AESCBCEncryptBase64WithIV(plaintext, key, iv []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil || len(key) != aes.BlockSize {
		return "", fmt.Errorf("AES-128 key must be exactly 16 bytes")
	}
	if len(iv) != aes.BlockSize {
		return "", fmt.Errorf("CBC IV must be exactly 16 bytes")
	}
	padding := aes.BlockSize - len(plaintext)%aes.BlockSize
	padded := make([]byte, len(plaintext)+padding)
	copy(padded, plaintext)
	for index := len(plaintext); index < len(padded); index++ {
		padded[index] = byte(padding)
	}
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(padded, padded)
	return base64.StdEncoding.EncodeToString(padded), nil
}

// AESCBCDecryptBase64 执行严格 Base64→AES-128-CBC→PKCS7 校验。
func AESCBCDecryptBase64(encrypted, key string) ([]byte, error) {
	return AESCBCDecryptBase64WithIV(encrypted, []byte(key), []byte(DefaultDeviceConfigIV))
}

func AESCBCDecryptBase64WithIV(encrypted string, key, iv []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil || len(key) != aes.BlockSize {
		return nil, fmt.Errorf("AES-128 key must be exactly 16 bytes")
	}
	if len(iv) != aes.BlockSize {
		return nil, fmt.Errorf("CBC IV must be exactly 16 bytes")
	}
	ciphertext, err := decodeBase64Strict(encrypted)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("AES ciphertext length must be a non-zero block multiple")
	}
	plaintext := append([]byte(nil), ciphertext...)
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plaintext, plaintext)
	padding := int(plaintext[len(plaintext)-1])
	if padding < 1 || padding > aes.BlockSize || padding > len(plaintext) {
		return nil, fmt.Errorf("invalid PKCS7 padding")
	}
	var mismatch byte
	for _, value := range plaintext[len(plaintext)-padding:] {
		mismatch |= value ^ byte(padding)
	}
	if mismatch != 0 {
		return nil, fmt.Errorf("invalid PKCS7 padding")
	}
	return plaintext[:len(plaintext)-padding], nil
}
