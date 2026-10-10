package waf

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

const (
	dohHost     = "dns.google"
	dohAddress  = "8.8.8.8:443"
	dohMaxBytes = 64 << 10
	dnsTypeA    = 1
	dnsTypeAAAA = 28
)

var fakeIPRange = netip.MustParsePrefix("198.18.0.0/15")

func resolvePageAddresses(ctx context.Context, transport *http.Transport, hostname string) ([]netip.Addr, error) {
	return resolvePageAddressesWith(ctx, hostname,
		func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		},
		func(ctx context.Context, host string, recordType int) ([]netip.Addr, error) {
			return queryDoH(ctx, transport, host, recordType)
		},
	)
}

func resolvePageAddressesWith(ctx context.Context, hostname string,
	lookup func(context.Context, string) ([]netip.Addr, error),
	query func(context.Context, string, int) ([]netip.Addr, error),
) ([]netip.Addr, error) {
	if ctx == nil || lookup == nil || query == nil {
		return nil, ErrPageSource
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	addresses, err := lookup(ctx, hostname)
	if err != nil {
		return nil, pageSourceFailure(ctx, "resolve target")
	}
	if !onlyFakeIPAddresses(addresses) {
		if _, err := publicPageAddress(addresses); err != nil {
			return nil, err
		}
		return addresses, nil
	}
	// TUN 的 Fake-IP 不是可访问目标。仅此完整地址段命中时向固定公共 DNS
	// 取回真实 A/AAAA；任何其他私网答案以及公共 DNS 失败均不能触发放宽。
	var resolved []netip.Addr
	for _, recordType := range []int{dnsTypeA, dnsTypeAAAA} {
		answers, err := query(ctx, hostname, recordType)
		if err != nil {
			if errors.Is(err, ErrUnsafePageTarget) {
				return nil, ErrUnsafePageTarget
			}
			return nil, pageSourceFailure(ctx, "public DNS query")
		}
		resolved = append(resolved, answers...)
	}
	if _, err := publicPageAddress(resolved); err != nil {
		return nil, err
	}
	return resolved, nil
}

func onlyFakeIPAddresses(addresses []netip.Addr) bool {
	if len(addresses) == 0 {
		return false
	}
	for _, address := range addresses {
		if address.Zone() != "" || !fakeIPRange.Contains(address.Unmap()) {
			return false
		}
	}
	return true
}

type dohResponse struct {
	Status    *int  `json:"Status"`
	Truncated *bool `json:"TC"`
	Question  []struct {
		Name string `json:"name"`
		Type int    `json:"type"`
	} `json:"Question"`
	Answer []struct {
		Type int    `json:"type"`
		Data string `json:"data"`
	} `json:"Answer"`
}

func queryDoH(ctx context.Context, base *http.Transport, hostname string, recordType int) ([]netip.Addr, error) {
	if ctx == nil || base == nil || base.DialContext == nil {
		return nil, ErrPageSource
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	typeName := "A"
	switch recordType {
	case dnsTypeA:
	case dnsTypeAAAA:
		typeName = "AAAA"
	default:
		return nil, ErrPageSource
	}
	endpoint := &url.URL{
		Scheme: "https", Host: dohHost, Path: "/resolve",
		RawQuery: url.Values{"name": {hostname}, "type": {typeName}}.Encode(),
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, ErrPageSource
	}
	transport, err := pinnedPageTransport(base, request)
	if err != nil {
		return nil, pageSourceFailure(ctx, "configure public DNS transport")
	}
	defer transport.CloseIdleConnections()
	// 固定 DNS 服务自身的地址，不经本机解析，避免 Fake-IP 回退递归。
	request.Host, request.URL.Host = dohHost, dohAddress
	request.Header.Set("Accept", "application/dns-json")
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, pageSourceFailure(ctx, "public DNS request")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength > dohMaxBytes {
		return nil, ErrPageSource
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, dohMaxBytes+1))
	if err != nil {
		return nil, pageSourceFailure(ctx, "read public DNS response")
	}
	if len(body) > dohMaxBytes {
		return nil, ErrPageSource
	}
	var result dohResponse
	if json.Unmarshal(body, &result) != nil || result.Status == nil || *result.Status != 0 ||
		result.Truncated == nil || *result.Truncated || len(result.Question) != 1 ||
		result.Question[0].Type != recordType || canonicalDNSName(result.Question[0].Name) != canonicalDNSName(hostname) {
		return nil, ErrPageSource
	}
	var addresses []netip.Addr
	for _, answer := range result.Answer {
		if answer.Type != dnsTypeA && answer.Type != dnsTypeAAAA {
			continue
		}
		address, err := netip.ParseAddr(answer.Data)
		if err != nil || (answer.Type == dnsTypeA && !address.Is4()) || (answer.Type == dnsTypeAAAA && (!address.Is6() || address.Is4In6())) {
			return nil, ErrPageSource
		}
		if !isPublicPageAddress(address) {
			return nil, ErrUnsafePageTarget
		}
		addresses = append(addresses, address)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return addresses, nil
}

func canonicalDNSName(hostname string) string {
	return strings.ToLower(strings.TrimSuffix(hostname, "."))
}
