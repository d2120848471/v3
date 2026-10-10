// Package waf 描述 WAF/ESA 单轮挑战与验证码 SDK 之间的合同。
package waf

import (
	"errors"
	"fmt"
	"strings"
)

type Challenge struct {
	Type       string `json:"type"`
	SceneID    string `json:"sceneId"`
	UserID     string `json:"userId"`
	UserUserID string `json:"userUserId"`
	TraceID    string `json:"traceid"`
	Token      string `json:"token"`
	Region     string `json:"region"`
	Language   string `json:"language,omitempty"`
}

func (Challenge) String() string   { return "waf.Challenge{redacted}" }
func (Challenge) GoString() string { return "waf.Challenge{redacted}" }

// Validate 统一页面解析与 SDK 运行时的挑战边界；错误只含字段名和规则，不含字段值。
func (challenge Challenge) Validate() error {
	if challenge.Type != "GET" {
		return errors.New("type must be GET")
	}
	if challenge.Region != "cn" && challenge.Region != "sgp" {
		return errors.New("region must be cn or sgp")
	}
	for _, field := range []struct{ name, value string }{
		{"sceneId", challenge.SceneID},
		{"userId", challenge.UserID},
		{"userUserId", challenge.UserUserID},
		{"traceid", challenge.TraceID},
		{"token", challenge.Token},
	} {
		if len(field.value) < 1 || len(field.value) > 4096 || strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("%s must contain 1..4096 bytes and non-whitespace text", field.name)
		}
	}
	if len(challenge.Language) > 64 {
		return errors.New("language must contain at most 64 bytes")
	}
	return nil
}

type Result struct {
	Signature    string `json:"signature"`
	CertifyID    string `json:"certifyId"`
	CaptchaType  string `json:"captchaType"`
	VerifyCode   string `json:"verifyCode"`
	VerifyResult bool   `json:"verifyResult"`
}

func (Result) String() string   { return "waf.Result{redacted}" }
func (Result) GoString() string { return "waf.Result{redacted}" }
