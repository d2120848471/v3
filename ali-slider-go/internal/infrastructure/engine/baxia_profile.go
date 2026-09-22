package engine

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"github.com/d2120848471/v3/ali-slider-go/internal/domain/baxia"
)

type baxiaAlphabetObservation struct {
	Alphabet   string `json:"alphabet"`
	Characters string `json:"characters"`
}

type baxiaProfileStage struct {
	Version      int                        `json:"version"`
	Token        string                     `json:"token"`
	Observations []baxiaAlphabetObservation `json:"observations"`
}

// recoverBaxiaProfile 只恢复最终容器的 base64 编码表。解码后仍是 SDK
// 的不透明二进制数据，不能据此声称恢复了内部加密算法或密钥。
func recoverBaxiaProfile(stage baxiaProfileStage, sdkHash string) (baxia.RuntimeProfile, error) {
	fail := func(reason string) (baxia.RuntimeProfile, error) {
		return baxia.RuntimeProfile{}, fmt.Errorf("%w: %s", baxia.ErrUnsupportedSDK, reason)
	}
	if stage.Version < 1 {
		return fail("getVersion must return a positive integer")
	}
	prefix := strconv.Itoa(stage.Version) + "!"
	if !strings.HasPrefix(stage.Token, prefix) || len(stage.Token) <= len(prefix) || len(stage.Token) > baxiaMaximumTokenSize {
		return fail("getter sample does not match the SDK version prefix")
	}
	payload := strings.TrimPrefix(stage.Token, prefix)
	matched := make(map[string]struct{})
	var recovered baxia.RuntimeProfile
	for _, observation := range stage.Observations {
		if observation.Characters != payload {
			continue
		}
		alphabet, ok := normalizeBaxiaAlphabet(observation.Alphabet)
		if !ok {
			continue
		}
		profile := baxia.RuntimeProfile{Version: stage.Version, Prefix: prefix, Alphabet: alphabet, SDKHash: sdkHash}
		if err := validateBaxiaToken(baxia.Result{Version: stage.Version, Token: stage.Token}, profile); err == nil {
			matched[alphabet] = struct{}{}
			recovered = profile
		}
	}
	if len(matched) != 1 {
		return fail(fmt.Sprintf("getter sample identifies %d outer encoding alphabets; expected exactly one", len(matched)))
	}
	return recovered, nil
}

func validateBaxiaToken(result baxia.Result, profile baxia.RuntimeProfile) error {
	if profile.Version < 1 || profile.Prefix != strconv.Itoa(profile.Version)+"!" || result.Version != profile.Version {
		return fmt.Errorf("%w: token version differs from the session profile", baxia.ErrUnsupportedSDK)
	}
	alphabet, ok := normalizeBaxiaAlphabet(profile.Alphabet)
	if !ok || alphabet != profile.Alphabet {
		return fmt.Errorf("%w: session alphabet is invalid", baxia.ErrUnsupportedSDK)
	}
	if len(result.Token) <= len(profile.Prefix) || len(result.Token) > baxiaMaximumTokenSize || !strings.HasPrefix(result.Token, profile.Prefix) {
		return fmt.Errorf("%w: token prefix or size is invalid", baxia.ErrToken)
	}
	payload := result.Token[len(profile.Prefix):]
	// Strict 检查填充位，重编码检查规范形式；显式拒绝 Go decoder 会忽略的换行。
	encoding := base64.NewEncoding(alphabet[:64]).Strict()
	decoded, err := encoding.DecodeString(payload)
	if err != nil || len(decoded) == 0 || strings.ContainsAny(payload, "\r\n") || encoding.EncodeToString(decoded) != payload {
		return fmt.Errorf("%w: payload does not match the recovered outer encoding", baxia.ErrToken)
	}
	return nil
}

// 当前可核验的容器为 64 个 base64 字符的排列，使用 '=' 填充。
// 其他字符集合或填充方案发生变化时显式拒绝，不推测新的编码规则。
func normalizeBaxiaAlphabet(value string) (string, bool) {
	if len(value) == 65 && value[64] == '=' {
		value = value[:64]
	}
	if len(value) != 64 {
		return "", false
	}
	var seen [128]bool
	for i := range value {
		c := value[i]
		if c >= 128 || seen[c] || !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/') {
			return "", false
		}
		seen[c] = true
	}
	return value + "=", true
}
