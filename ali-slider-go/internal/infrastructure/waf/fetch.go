package waf

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"

	"github.com/d2120848471/v3/ali-slider-go/internal/application/service"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/device"
)

var (
	ErrPageSource       = errors.New("WAF page fetch failed")
	ErrUnsafePageTarget = errors.New("WAF page target is not public")
	// 除 private/link-local 外，也排除保留、文档、共享地址和可映射到内网的转换段。
	nonPublicIPv4 = []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("192.88.99.0/24"),
		netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
		netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("224.0.0.0/4"), netip.MustParsePrefix("240.0.0.0/4"),
	}
	publicIPv6    = netip.MustParsePrefix("2000::/3")
	nonPublicIPv6 = []netip.Prefix{
		netip.MustParsePrefix("2001::/23"), netip.MustParsePrefix("2001:db8::/32"),
		netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("3fff::/20"),
	}
)

// FetchPage 只抓取一次页面，无 Cookie jar，不接受重定向，也不回访业务 URL。
func FetchPage(ctx context.Context, transport *http.Transport, profile device.Profile, pageURL string) ([]byte, error) {
	return fetchPage(ctx, transport, profile, pageURL, func(ctx context.Context, host string) ([]netip.Addr, error) {
		return resolvePageAddresses(ctx, transport, host)
	})
}

func fetchPage(ctx context.Context, base *http.Transport, profile device.Profile, pageURL string, lookup func(context.Context, string) ([]netip.Addr, error)) ([]byte, error) {
	if ctx == nil || base == nil || base.DialContext == nil || lookup == nil {
		return nil, ErrPageSource
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if service.ValidateWAFURL(pageURL) != nil {
		return nil, ErrUnsafePageTarget
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return nil, ErrPageSource
	}
	addresses, err := lookup(ctx, request.URL.Hostname())
	if err != nil {
		if errors.Is(err, ErrUnsafePageTarget) {
			return nil, ErrUnsafePageTarget
		}
		return nil, pageSourceFailure(ctx, "resolve target")
	}
	address, err := publicPageAddress(addresses)
	if err != nil {
		return nil, err
	}
	transport, err := pinnedPageTransport(base, request)
	if err != nil {
		return nil, pageSourceFailure(ctx, "configure page transport")
	}
	defer transport.CloseIdleConnections()
	// URL 的 IP 同时约束直连、CONNECT 和 SOCKS 的目标；原 Host 与 SNI 保持不变。
	request.Host = request.URL.Host
	request.URL.Host = net.JoinHostPort(address.String(), "443")
	for name, value := range profile.BrowserHeaders("", "", "document", "navigate", false) {
		request.Header.Set(name, value)
	}
	request.Header.Del("Referer")
	request.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	request.Header.Set("Sec-Fetch-Site", "none")
	request.Header.Set("Sec-Fetch-User", "?1")
	request.Header.Set("Upgrade-Insecure-Requests", "1")
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, pageSourceFailure(ctx, "request")
	}
	defer response.Body.Close()
	if !challengePageStatus(response.StatusCode) {
		return nil, fmt.Errorf("%w: HTTP %d", ErrPageSource, response.StatusCode)
	}
	if response.ContentLength > MaxPageBytes {
		return nil, ErrUnsupportedPage
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxPageBytes+1))
	if err != nil {
		return nil, pageSourceFailure(ctx, "read response")
	}
	if len(body) > MaxPageBytes {
		return nil, ErrUnsupportedPage
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return body, nil
}

func challengePageStatus(status int) bool {
	return (status >= 200 && status < 300) || status == http.StatusForbidden || status == http.StatusMethodNotAllowed ||
		status == http.StatusPreconditionFailed || status == http.StatusTooManyRequests
}

func pageSourceFailure(ctx context.Context, stage string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// 不保留 net/url.Error、DNS 错误或正文，防止 URL 查询及代理凭据经错误链泄露。
	return fmt.Errorf("%w: %s", ErrPageSource, stage)
}

func publicPageAddress(addresses []netip.Addr) (netip.Addr, error) {
	if len(addresses) == 0 {
		return netip.Addr{}, fmt.Errorf("%w: target has no addresses", ErrPageSource)
	}
	var selected netip.Addr
	for _, candidate := range addresses {
		address := candidate.Unmap()
		if !isPublicPageAddress(address) {
			return netip.Addr{}, ErrUnsafePageTarget
		}
		// IPv4 优先，允许现有 SOCKS4 路由使用同一安全抓取路径。
		if !selected.IsValid() || address.Is4() && !selected.Is4() {
			selected = address
		}
	}
	return selected, nil
}

func isPublicPageAddress(address netip.Addr) bool {
	if !address.IsValid() || address.Zone() != "" || !address.IsGlobalUnicast() || address.IsPrivate() {
		return false
	}
	prefixes := nonPublicIPv4
	if address.Is6() {
		if !publicIPv6.Contains(address) {
			return false
		}
		prefixes = nonPublicIPv6
	}
	for _, prefix := range prefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

func pinnedPageTransport(base *http.Transport, request *http.Request) (*http.Transport, error) {
	transport := base.Clone()
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	transport.TLSClientConfig.ServerName = request.URL.Hostname()
	// 单页传输不继承可绕开 DialContext 的自定义 TLS dial，也不建立跨请求连接复用。
	transport.DialTLS, transport.DialTLSContext = nil, nil
	transport.DisableKeepAlives = true
	if base.Proxy == nil {
		return transport, nil
	}
	proxyURL, err := base.Proxy(request)
	if err != nil {
		return nil, err
	}
	transport.Proxy = http.ProxyURL(proxyURL)
	if proxyURL == nil || proxyURL.Scheme != "https" {
		return transport, nil
	}

	// 标准 Transport 共用 TLSClientConfig。先用代理自己的 SNI 建 TLS，
	// 再走标准 HTTP CONNECT，使页面 TLS 仍能校验原域名且 CONNECT 目标固定为 IP。
	proxyCopy := *proxyURL
	port := proxyCopy.Port()
	if port == "" {
		port = "443"
	}
	proxyAddress := net.JoinHostPort(proxyCopy.Hostname(), port)
	proxyCopy.Host, proxyCopy.Scheme = proxyAddress, "http"
	transport.Proxy = http.ProxyURL(&proxyCopy)
	proxyTLSConfig := transport.TLSClientConfig.Clone()
	proxyTLSConfig.ServerName = proxyURL.Hostname()
	proxyTLSConfig.NextProtos = []string{"http/1.1"}
	dialContext := base.DialContext
	handshakeTimeout := base.TLSHandshakeTimeout
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != proxyAddress {
			return nil, ErrPageSource
		}
		connection, err := dialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		tlsConnection := tls.Client(connection, proxyTLSConfig)
		if handshakeTimeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, handshakeTimeout)
			defer cancel()
		}
		if err := tlsConnection.HandshakeContext(ctx); err != nil {
			_ = connection.Close()
			return nil, err
		}
		return tlsConnection, nil
	}
	return transport, nil
}
