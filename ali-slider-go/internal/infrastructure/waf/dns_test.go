package waf

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

const fixtureDNSA = `{"Status":0,"TC":false,"Question":[{"name":"page.example.com.","type":1}],"Answer":[{"name":"page.example.com.","type":1,"data":"8.8.4.4"}]}`

func TestPageDNSOnlyFallsBackWhenAllLocalAnswersAreFakeIP(t *testing.T) {
	for _, test := range []struct {
		name      string
		addresses []string
		queries   int
		wantErr   error
	}{
		{"public IPv4", []string{"8.8.4.4"}, 0, nil},
		{"public dual stack", []string{"8.8.4.4", "2606:4700:4700::1111"}, 0, nil},
		{"all Fake-IP", []string{"198.18.2.199", "198.19.255.254"}, 2, nil},
		{"mapped Fake-IP", []string{"::ffff:198.18.2.199"}, 2, nil},
		{"empty answers", nil, 0, ErrPageSource},
		{"private", []string{"10.0.0.1"}, 0, ErrUnsafePageTarget},
		{"mixed Fake-IP and private", []string{"198.18.2.199", "10.0.0.1"}, 0, ErrUnsafePageTarget},
		{"mixed Fake-IP and public", []string{"198.18.2.199", "8.8.4.4"}, 0, ErrUnsafePageTarget},
		{"other benchmark", []string{"198.20.0.1", "198.18.0.1"}, 0, ErrUnsafePageTarget},
	} {
		t.Run(test.name, func(t *testing.T) {
			var local []netip.Addr
			for _, address := range test.addresses {
				local = append(local, netip.MustParseAddr(address))
			}
			queries := 0
			lookup := func(_ context.Context, host string) ([]netip.Addr, error) {
				if host != "page.example.com" {
					t.Fatal("local DNS received something other than the page hostname")
				}
				return local, nil
			}
			query := func(_ context.Context, host string, recordType int) ([]netip.Addr, error) {
				queries++
				if host != "page.example.com" || (queries == 1 && recordType != 1) || (queries == 2 && recordType != 28) {
					t.Fatal("DoH query lost hostname or address family")
				}
				if recordType == 1 {
					return []netip.Addr{netip.MustParseAddr("8.8.4.4")}, nil
				}
				return []netip.Addr{netip.MustParseAddr("2606:4700:4700::1111")}, nil
			}
			addresses, err := resolvePageAddressesWith(context.Background(), "page.example.com", lookup, query)
			if !errors.Is(err, test.wantErr) || queries != test.queries || (err != nil && len(addresses) != 0) {
				t.Fatalf("resolver fallback boundary: queries=%d addresses=%d error=%v", queries, len(addresses), err)
			}
			if err == nil {
				if _, err := publicPageAddress(addresses); err != nil {
					t.Fatalf("resolver returned a non-public address: %v", err)
				}
			}
		})
	}
}

func TestPageDNSFallbackNeverUsesPartialOrUnsafeAnswers(t *testing.T) {
	for _, test := range []struct {
		name     string
		localErr error
		a        []netip.Addr
		aaaa     []netip.Addr
		aaaaErr  error
		wantErr  error
	}{
		{"local DNS error", errors.New("fixture-private-local-error"), nil, nil, nil, ErrPageSource},
		{"empty DoH answers", nil, nil, nil, nil, ErrPageSource},
		{"private DoH IPv4", nil, []netip.Addr{netip.MustParseAddr("10.0.0.1")}, nil, nil, ErrUnsafePageTarget},
		{"private DoH IPv6", nil, []netip.Addr{netip.MustParseAddr("8.8.4.4")}, []netip.Addr{netip.MustParseAddr("fc00::1")}, nil, ErrUnsafePageTarget},
		{"Fake-IP DoH answer", nil, []netip.Addr{netip.MustParseAddr("198.18.2.199")}, nil, nil, ErrUnsafePageTarget},
		{"AAAA failure after valid A", nil, []netip.Addr{netip.MustParseAddr("8.8.4.4")}, nil, errors.New("fixture-private-upstream-error"), ErrPageSource},
	} {
		t.Run(test.name, func(t *testing.T) {
			lookup := func(context.Context, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("198.18.2.199")}, test.localErr
			}
			query := func(_ context.Context, _ string, recordType int) ([]netip.Addr, error) {
				if recordType == 1 {
					return test.a, nil
				}
				return test.aaaa, test.aaaaErr
			}
			addresses, err := resolvePageAddressesWith(context.Background(), "page.example.com", lookup, query)
			if !errors.Is(err, test.wantErr) || len(addresses) != 0 || strings.Contains(err.Error(), "fixture-private") {
				t.Fatalf("fallback failure returned unsafe/partial data or raw error: addresses=%d error=%v", len(addresses), err)
			}
		})
	}
}

func TestDoHUsesPinnedGoogleOriginAndPreservesProxyWithoutCredentials(t *testing.T) {
	for _, route := range []string{"direct", "http", "https", "socks-dialer"} {
		t.Run(route, func(t *testing.T) {
			base, observed := fixtureOriginTransport(t, route, http.StatusOK, fixtureDNSA, false, "dns.google")
			addresses, err := queryDoH(context.Background(), base, "page.example.com", 1)
			if err != nil || len(addresses) != 1 || addresses[0] != netip.MustParseAddr("8.8.4.4") {
				t.Fatalf("DoH response not accepted: addresses=%d error=%v", len(addresses), err)
			}
			observed.wait(t)
			wantDial := "8.8.8.8:443"
			wantNames := []string{"dns.google"}
			if route == "http" || route == "https" {
				wantDial = "proxy.example.com:443"
				wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("fixture-user:fixture-password"))
				if observed.connectHost != "8.8.8.8:443" || observed.proxyAuth != wantAuth {
					t.Fatal("DoH bypassed the proxy or lost its fixed destination")
				}
				if route == "https" {
					wantNames = []string{"proxy.example.com", "dns.google"}
				}
			}
			if observed.dialAddress != wantDial || observed.host != "dns.google" || fmt.Sprint(observed.serverNames) != fmt.Sprint(wantNames) {
				t.Fatalf("DoH identity or destination changed: dial=%s host=%s names=%v", observed.dialAddress, observed.host, observed.serverNames)
			}
			uri, err := url.Parse(observed.requestURI)
			if err != nil || uri.Path != "/resolve" || uri.Query().Get("name") != "page.example.com" || uri.Query().Get("type") != "A" || len(uri.Query()) != 2 {
				t.Fatalf("DoH query contains more than hostname and record type: %s", observed.requestURI)
			}
			for _, name := range []string{"Cookie", "Authorization", "Proxy-Authorization", "Referer", "Origin"} {
				if observed.headers.Get(name) != "" {
					t.Fatalf("DoH origin received unexpected header %s", name)
				}
			}
			if base.TLSClientConfig.ServerName != "" || observed.dialCount != 1 {
				t.Fatal("DoH mutated shared transport or performed extra requests")
			}
		})
	}
}

func TestDoHRejectsUnboundTruncatedUnsafeAndOversizedResponses(t *testing.T) {
	for _, test := range []struct {
		name    string
		status  int
		body    string
		chunked bool
		wantErr error
	}{
		{"DNS error status", 200, strings.Replace(fixtureDNSA, `"Status":0`, `"Status":3`, 1), false, ErrPageSource},
		{"missing DNS status", 200, strings.Replace(fixtureDNSA, `"Status":0,`, "", 1), false, ErrPageSource},
		{"truncated", 200, strings.Replace(fixtureDNSA, `"TC":false`, `"TC":true`, 1), false, ErrPageSource},
		{"missing truncated flag", 200, strings.Replace(fixtureDNSA, `"TC":false,`, "", 1), false, ErrPageSource},
		{"unbound name", 200, strings.ReplaceAll(fixtureDNSA, "page.example.com.", "other.example.com."), false, ErrPageSource},
		{"unbound type", 200, strings.Replace(fixtureDNSA, `"type":1`, `"type":28`, 1), false, ErrPageSource},
		{"missing question", 200, `{"Status":0,"TC":false,"Answer":[{"type":1,"data":"8.8.4.4"}]}`, false, ErrPageSource},
		{"multiple questions", 200, `{"Status":0,"TC":false,"Question":[{"name":"page.example.com.","type":1},{"name":"other.example.com.","type":1}],"Answer":[{"type":1,"data":"8.8.4.4"}]}`, false, ErrPageSource},
		{"non-public answer", 200, strings.Replace(fixtureDNSA, "8.8.4.4", "169.254.169.254", 1), false, ErrUnsafePageTarget},
		{"mixed public and private answers", 200, `{"Status":0,"TC":false,"Question":[{"name":"page.example.com.","type":1}],"Answer":[{"type":1,"data":"8.8.4.4"},{"type":1,"data":"10.0.0.1"}]}`, false, ErrUnsafePageTarget},
		{"other family private answer", 200, `{"Status":0,"TC":false,"Question":[{"name":"page.example.com.","type":1}],"Answer":[{"type":1,"data":"8.8.4.4"},{"type":28,"data":"fc00::1"}]}`, false, ErrUnsafePageTarget},
		{"Fake-IP answer", 200, strings.Replace(fixtureDNSA, "8.8.4.4", "198.18.2.199", 1), false, ErrUnsafePageTarget},
		{"invalid IP answer", 200, strings.Replace(fixtureDNSA, "8.8.4.4", "fixture-private-response", 1), false, ErrPageSource},
		{"wrong IP family", 200, strings.Replace(fixtureDNSA, "8.8.4.4", "2606:4700:4700::1111", 1), false, ErrPageSource},
		{"malformed response", 200, "fixture-private-response", false, ErrPageSource},
		{"trailing JSON", 200, fixtureDNSA + `{}`, false, ErrPageSource},
		{"redirect", 302, fixtureDNSA, false, ErrPageSource},
		{"HTTP failure", 500, fixtureDNSA, false, ErrPageSource},
		{"declared too large", 200, strings.Repeat("x", (64<<10)+1), false, ErrPageSource},
		{"stream too large", 200, strings.Repeat("x", (64<<10)+1), true, ErrPageSource},
	} {
		t.Run(test.name, func(t *testing.T) {
			base, observed := fixtureOriginTransport(t, "direct", test.status, test.body, test.chunked, "dns.google")
			addresses, err := queryDoH(context.Background(), base, "page.example.com", 1)
			if !errors.Is(err, test.wantErr) || len(addresses) != 0 || strings.Contains(err.Error(), "fixture-private") || strings.Contains(err.Error(), "page.example.com") {
				t.Fatalf("DoH failure returned partial or disclosed data: addresses=%d error=%v", len(addresses), err)
			}
			observed.wait(t)
			if observed.dialCount != 1 {
				t.Fatal("DoH followed a redirect or retried")
			}
		})
	}
}

func TestDoHAcceptsPublicAAAAAndCNAMEOnlyNodata(t *testing.T) {
	for _, test := range []struct {
		body string
		want int
	}{
		{`{"Status":0,"TC":false,"Question":[{"name":"PAGE.EXAMPLE.COM.","type":28}],"Answer":[{"type":28,"data":"2606:4700:4700::1111"}]}`, 1},
		{`{"Status":0,"TC":false,"Question":[{"name":"page.example.com.","type":28}],"Answer":[{"type":5,"data":"canonical.example.com."}]}`, 0},
	} {
		base, observed := fixtureOriginTransport(t, "direct", http.StatusOK, test.body, false, "dns.google")
		addresses, err := queryDoH(context.Background(), base, "page.example.com", 28)
		if err != nil || len(addresses) != test.want {
			t.Fatalf("AAAA/NODATA parsing: addresses=%d error=%v", len(addresses), err)
		}
		observed.wait(t)
		uri, err := url.Parse(observed.requestURI)
		if err != nil || uri.Query().Get("type") != "AAAA" {
			t.Fatal("AAAA query type was not preserved")
		}
	}
}
