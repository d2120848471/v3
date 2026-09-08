package protocol

import (
	"errors"
	"fmt"
	"sync"
	"unicode/utf8"
)

const frontendAccessKey = "FqJB6iRNVYdEGpwb"

var frontendCiphertexts = [...]string{
	"MQECT2fv9RPHHlaKkKNWZP0vS4BJe7J1XY8H7v7s27M=",
	"EYKxcWbB1W70XKzyYLOt12LQ9EFXzV0OvPKeOPWhABg=",
	"NLAoqT6K03oLbQXW2VS3zA==",
	"tK5X1r3E65gcLeL+GmivL6RuHUa/6AyrEEc+S59NPns=",
	"kvqU4wg9JqUyoXqN1pdq9XZ2gkf1oPpJCVdG8Hvkkq4=",
	"8KmHIQsc5+LZJA7uYex3WaHdkjgCtS6epbG/bc9xss0=",
	"9NhnQQ+LRrKCkAuxwZaUWGBtSzaFtFlNb/ksJCrCgrM=",
	"k+1RW0cz3iDi2RAbC/c3QKzTPiVwNmNO1910DXe6Gas=",
	"+fR9tYzlKFr07pEbumd7+KnO3xLOkphCS+qKUbJiMfA=",
	"xLLw/t15vkI7QQBX/1scBbcb9fKx+ymxF0tJ3ds42B0=",
}

// FrontendSecrets 保存公开前端静态密文的进程内解密结果。
// 字段故意不导出；String/GoString 始终脱敏，调用方必须显式调用单项 getter。
type FrontendSecrets struct {
	mainRPCKeyID, mainRPCKeySecret                 string
	deviceTokenSalt                                string
	deviceRPCKeyID, deviceRPCKeySecret             string
	deviceRequestKey, deviceResponseKey            string
	deviceFlagKey, deviceUploadKey, devicePreIDKey string
}

func (FrontendSecrets) String() string   { return "FrontendSecrets{redacted}" }
func (FrontendSecrets) GoString() string { return "protocol.FrontendSecrets{redacted}" }

func (s FrontendSecrets) MainRPCKeyID() string       { return s.mainRPCKeyID }
func (s FrontendSecrets) MainRPCKeySecret() string   { return s.mainRPCKeySecret }
func (s FrontendSecrets) DeviceTokenSalt() string    { return s.deviceTokenSalt }
func (s FrontendSecrets) DeviceRPCKeyID() string     { return s.deviceRPCKeyID }
func (s FrontendSecrets) DeviceRPCKeySecret() string { return s.deviceRPCKeySecret }
func (s FrontendSecrets) DeviceRequestKey() string   { return s.deviceRequestKey }
func (s FrontendSecrets) DeviceResponseKey() string  { return s.deviceResponseKey }
func (s FrontendSecrets) DeviceFlagKey() string      { return s.deviceFlagKey }
func (s FrontendSecrets) DeviceUploadKey() string    { return s.deviceUploadKey }
func (s FrontendSecrets) DevicePreIDKey() string     { return s.devicePreIDKey }

var (
	resolveSecretsOnce sync.Once
	resolvedSecrets    FrontendSecrets
	resolveSecretsErr  error
)

// ResolveFrontendSecrets 每进程只解密一次。错误只描述位置，不包含密文或明文。
func ResolveFrontendSecrets() (FrontendSecrets, error) {
	resolveSecretsOnce.Do(func() {
		resolvedSecrets, resolveSecretsErr = decodeFrontendSecrets(frontendCiphertexts[:])
	})
	if resolveSecretsErr != nil {
		return FrontendSecrets{}, resolveSecretsErr
	}
	if resolvedSecrets.mainRPCKeyID == "" {
		return FrontendSecrets{}, errors.New("public frontend secrets are unavailable")
	}
	return resolvedSecrets, nil
}

func decodeFrontendSecrets(ciphertexts []string) (FrontendSecrets, error) {
	if len(ciphertexts) != len(frontendCiphertexts) {
		return FrontendSecrets{}, errors.New("public frontend secret count is invalid")
	}
	plain := make([]string, len(ciphertexts))
	for index, ciphertext := range ciphertexts {
		decoded, err := AESCBCDecryptBase64(ciphertext, frontendAccessKey)
		if err != nil || !utf8.Valid(decoded) {
			return FrontendSecrets{}, fmt.Errorf("resolve public frontend secret %d", index)
		}
		plain[index] = string(decoded)
	}
	result := FrontendSecrets{
		mainRPCKeyID: plain[0], mainRPCKeySecret: plain[1], deviceTokenSalt: plain[2],
		deviceRPCKeyID: plain[3], deviceRPCKeySecret: plain[4], deviceRequestKey: plain[5],
		deviceResponseKey: plain[6], deviceFlagKey: plain[7], deviceUploadKey: plain[8], devicePreIDKey: plain[9],
	}
	for index, value := range plain {
		if value == "" {
			return FrontendSecrets{}, fmt.Errorf("public frontend secret %d is empty", index)
		}
	}
	return result, nil
}
