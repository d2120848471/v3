package httpclient

import (
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// NewTransport 创建整个服务共享的连接池。每轮请求只共享连接，不共享 mutable state。
func NewTransport(proxy string, maxConnections int) (*http.Transport, bool, error) {
	if maxConnections < 1 || maxConnections > 32 {
		return nil, false, errors.New("max connections must be within 1..32")
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		// 直连必须不受 HTTP_PROXY/HTTPS_PROXY 等进程环境影响。
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          maxConnections * 6,
		MaxIdleConnsPerHost:   maxConnections,
		MaxConnsPerHost:       maxConnections,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		ExpectContinueTimeout: time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	value := strings.TrimSpace(proxy)
	if value == "" {
		return transport, false, nil
	}
	if !strings.Contains(value, "://") {
		value = "http://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" || parsed.Opaque != "" {
		return nil, false, errors.New("proxy must be an absolute URL with a host")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	switch parsed.Scheme {
	case "http", "https", "socks4", "socks5", "socks5h":
	default:
		return nil, false, errors.New("proxy scheme must be http/https/socks4/socks5/socks5h")
	}
	if parsed.Path != "" && parsed.Path != "/" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, false, errors.New("proxy URL must not contain a path, query, or fragment")
	}
	portText := parsed.Port()
	if portText != "" {
		port, portErr := strconv.Atoi(portText)
		if portErr != nil || port < 1 || port > 65_535 {
			return nil, false, errors.New("proxy port must be within 1..65535")
		}
	}
	switch parsed.Scheme {
	case "http", "https":
		transport.Proxy = http.ProxyURL(parsed)
	default:
		socksDialContext, socksErr := newSOCKSDialContext(parsed, dialer)
		if socksErr != nil {
			return nil, false, socksErr
		}
		transport.DialContext = socksDialContext
	}
	return transport, true, nil
}
