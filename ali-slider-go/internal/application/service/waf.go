package service

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"time"
)

const MaxWAFURLBytes = 8192

type WAFRequest struct {
	PageURL, Proxy string
}

func (WAFRequest) String() string   { return "service.WAFRequest{redacted}" }
func (WAFRequest) GoString() string { return "service.WAFRequest{redacted}" }

// WAFOutcome 只包含本轮 WAF 验证结果；签名不表示后续业务请求已获放行。
type WAFOutcome struct {
	OK                   bool
	SceneID, CaptchaType string
	VerifyCode           string
	UAToken, UASig       string
	UAHeaders            map[string]string
	Proxied              bool
	Elapsed              time.Duration
}

func (WAFOutcome) String() string   { return "service.WAFOutcome{redacted}" }
func (WAFOutcome) GoString() string { return "service.WAFOutcome{redacted}" }

type WAFExecutor interface {
	SolveWAF(context.Context, WAFRequest) (WAFOutcome, error)
}

type WAFFailureKind string

const (
	WAFFailureInvalidRequest WAFFailureKind = "invalid_request"
	WAFFailureSource         WAFFailureKind = "source"
	WAFFailureUnsupported    WAFFailureKind = "unsupported"
	WAFFailureRuntime        WAFFailureKind = "runtime"
	WAFFailureCanceled       WAFFailureKind = "canceled"
	WAFFailureInternal       WAFFailureKind = "internal"
)

// WAFFailure 保留 errors.Is 链，错误文本不含页面、签名、代理或上游正文。
type WAFFailure struct {
	Kind  WAFFailureKind
	Cause error
}

func (failure *WAFFailure) Error() string { return "WAF execution failed" }
func (failure *WAFFailure) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.Cause
}

var errInvalidWAFURL = errors.New("必须是 1..8192 字节、无凭据和 fragment、使用 443 端口的公网域名 HTTPS URL")

// ValidateWAFURL 仅做静态输入检查，不解析 DNS 或发请求；下载层仍需约束实际解析地址。
func ValidateWAFURL(value string) error {
	if value == "" || len(value) > MaxWAFURLBytes || strings.TrimSpace(value) != value || strings.Contains(value, "#") {
		return errInvalidWAFURL
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Opaque != "" || parsed.User != nil || parsed.Hostname() == "" {
		return errInvalidWAFURL
	}
	host := parsed.Hostname()
	// 精确比较 authority，排除空端口、非规范端口、IPv6 和转义主机等歧义。
	if parsed.Host != host && parsed.Host != host+":443" {
		return errInvalidWAFURL
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if net.ParseIP(host) != nil || !publicWAFHostname(host) {
		return errInvalidWAFURL
	}
	return nil
}

func publicWAFHostname(host string) bool {
	if len(host) > 253 {
		return false
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if len(label) < 1 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if !((character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '-') {
				return false
			}
		}
	}
	tld := labels[len(labels)-1]
	if !strings.ContainsAny(tld, "abcdefghijklmnopqrstuvwxyz") {
		return false
	}
	switch tld {
	case "localhost", "local", "internal", "lan", "home", "test", "invalid", "example", "onion":
		return false
	}
	return true
}
