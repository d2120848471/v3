package waf

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/domain/device"
)

func fixtureLookup(_ context.Context, host string) ([]netip.Addr, error) {
	if host != "page.example.com" {
		return nil, errors.New("unexpected fixture DNS name")
	}
	return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("2606:4700:4700::1111")}, nil
}

func fixtureProfile() device.Profile {
	return device.Profile{UserAgent: "fixture-browser", Languages: []string{"zh-CN", "zh"}, UAPlatform: "Android", Mobile: true}
}

func TestFetchPagePinsDestinationAndPreservesBrowserAndProxyContext(t *testing.T) {
	for _, route := range []string{"direct", "http", "https", "socks-dialer"} {
		t.Run(route, func(t *testing.T) {
			base, observed := fixturePageTransport(t, route, http.StatusOK, string(fixturePage(fixtureRequestInfo)), false)
			body, err := fetchPage(context.Background(), base, fixtureProfile(), "https://page.example.com/path?fixture=1", fixtureLookup)
			if err != nil || string(body) != string(fixturePage(fixtureRequestInfo)) {
				t.Fatalf("fetch fixture: matched=%t error=%v", string(body) == string(fixturePage(fixtureRequestInfo)), err)
			}
			observed.wait(t)
			wantDial := "8.8.8.8:443"
			if route == "http" || route == "https" {
				wantDial = "proxy.example.com:443"
				if observed.connectHost != "8.8.8.8:443" {
					t.Fatalf("proxy resolved the domain instead of a pinned IP: %s", observed.connectHost)
				}
				wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("fixture-user:fixture-password"))
				if observed.proxyAuth != wantAuth {
					t.Fatal("proxy authentication was lost")
				}
			}
			if observed.dialAddress != wantDial || observed.host != "page.example.com" || observed.requestURI != "/path?fixture=1" {
				t.Fatalf("page route changed: dial=%s host=%s uri=%s", observed.dialAddress, observed.host, observed.requestURI)
			}
			wantSNI := []string{"page.example.com"}
			if route == "https" {
				wantSNI = []string{"proxy.example.com", "page.example.com"}
			}
			if fmt.Sprint(observed.serverNames) != fmt.Sprint(wantSNI) {
				t.Fatalf("TLS names changed: %v", observed.serverNames)
			}
			profile := fixtureProfile()
			if observed.headers.Get("User-Agent") != profile.UserAgent || observed.headers.Get("Sec-CH-UA-Platform") != profile.SecCHUAPlatform() ||
				observed.headers.Get("Accept-Language") != profile.AcceptLanguage() {
				t.Fatal("page request did not use the provided device profile")
			}
			for _, name := range []string{"Cookie", "Authorization", "Proxy-Authorization"} {
				if observed.headers.Get(name) != "" {
					t.Fatalf("origin received sensitive header %s", name)
				}
			}
			if base.TLSClientConfig.ServerName != "" {
				t.Fatal("page TLS configuration mutated the engine transport")
			}
		})
	}
}

func TestFetchPageRejectsEveryNonPublicDNSAnswerBeforeDial(t *testing.T) {
	for _, address := range []string{
		"0.1.2.3", "10.0.0.1", "100.64.0.1", "127.0.0.1", "169.254.169.254", "172.16.0.1", "192.168.1.1",
		"192.0.0.1", "192.0.2.1", "198.18.0.1", "198.51.100.1", "203.0.113.1", "224.0.0.1", "240.0.0.1",
		"::1", "::ffff:127.0.0.1", "fe80::1", "fc00::1", "2001:db8::1", "2002:7f00:1::1", "64:ff9b::7f00:1", "3fff::1",
	} {
		t.Run(address, func(t *testing.T) {
			dialed := false
			transport := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
				dialed = true
				return nil, errors.New("fixture dial unexpectedly reached")
			}}
			lookup := func(context.Context, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr(address)}, nil
			}
			_, err := fetchPage(context.Background(), transport, fixtureProfile(), "https://page.example.com/", lookup)
			if !errors.Is(err, ErrUnsafePageTarget) || dialed {
				t.Fatalf("unsafe DNS answer not rejected before dial: dialed=%t error=%v", dialed, err)
			}
		})
	}
}

func TestFetchPageStatusAndSizeBoundaries(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusForbidden, http.StatusMethodNotAllowed, http.StatusPreconditionFailed, http.StatusTooManyRequests} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			base, observed := fixturePageTransport(t, "direct", status, string(fixturePage(fixtureRequestInfo)), false)
			body, err := fetchPage(context.Background(), base, fixtureProfile(), "https://page.example.com/", fixtureLookup)
			if err != nil || len(body) == 0 {
				t.Fatalf("challenge response rejected: status=%d error=%v", status, err)
			}
			observed.wait(t)
		})
	}
	for _, test := range []struct {
		name    string
		status  int
		body    string
		chunked bool
		wantErr error
	}{
		{"redirect", http.StatusFound, "fixture-body", false, ErrPageSource},
		{"server error", http.StatusInternalServerError, "fixture-private-body", false, ErrPageSource},
		{"unrecognized status", http.StatusNotFound, "fixture-private-body", false, ErrPageSource},
		{"declared too large", http.StatusOK, strings.Repeat("x", MaxPageBytes+1), false, ErrUnsupportedPage},
		{"stream too large", http.StatusOK, strings.Repeat("x", MaxPageBytes+1), true, ErrUnsupportedPage},
	} {
		t.Run(test.name, func(t *testing.T) {
			base, observed := fixturePageTransport(t, "direct", test.status, test.body, test.chunked)
			body, err := fetchPage(context.Background(), base, fixtureProfile(), "https://page.example.com/", fixtureLookup)
			if !errors.Is(err, test.wantErr) || len(body) != 0 {
				t.Fatalf("failure returned bytes or wrong error: bytes=%d error=%v", len(body), err)
			}
			if strings.Contains(err.Error(), "fixture-private") || strings.Contains(err.Error(), "page.example.com") {
				t.Fatal("fetch failure disclosed page body or URL")
			}
			observed.wait(t)
			if observed.dialCount != 1 {
				t.Fatalf("page fetch followed redirect or retried: calls=%d", observed.dialCount)
			}
		})
	}
}

func TestFetchPageCancellationAndDNSFailuresAreSanitized(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	base := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		t.Fatal("canceled request attempted dial")
		return nil, nil
	}}
	lookup := func(context.Context, string) ([]netip.Addr, error) {
		t.Fatal("canceled request attempted DNS")
		return nil, nil
	}
	if _, err := fetchPage(ctx, base, fixtureProfile(), "https://page.example.com/", lookup); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	for _, lookup := range []func(context.Context, string) ([]netip.Addr, error){
		func(context.Context, string) ([]netip.Addr, error) {
			return nil, errors.New("fixture-private-resolver-error")
		},
		func(context.Context, string) ([]netip.Addr, error) { return nil, nil },
	} {
		_, err := fetchPage(context.Background(), base, fixtureProfile(), "https://page.example.com/", lookup)
		if !errors.Is(err, ErrPageSource) || strings.Contains(err.Error(), "fixture-private") {
			t.Fatalf("DNS failure not sanitized: %v", err)
		}
	}
}

func TestFetchPagePreservesUnsafeResolverFailureBeforeDial(t *testing.T) {
	base := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		t.Fatal("unsafe resolver result attempted dial")
		return nil, nil
	}}
	lookup := func(context.Context, string) ([]netip.Addr, error) { return nil, ErrUnsafePageTarget }
	body, err := fetchPage(context.Background(), base, fixtureProfile(), "https://page.example.com/", lookup)
	if !errors.Is(err, ErrUnsafePageTarget) || len(body) != 0 {
		t.Fatalf("unsafe DNS failure lost category or returned data: bytes=%d error=%v", len(body), err)
	}
}

// net.Pipe 与内存证书覆盖真实 TLS、CONNECT 和正文边界，不访问 DNS 或网络。
type pageObservation struct {
	dialAddress, connectHost, proxyAuth, host, requestURI string
	headers                                               http.Header
	serverNames                                           []string
	dialCount                                             int
	done                                                  chan struct{}
	mu                                                    sync.Mutex
}

func (observed *pageObservation) wait(t *testing.T) {
	t.Helper()
	select {
	case <-observed.done:
	case <-time.After(2 * time.Second):
		t.Fatal("fixture connection did not close")
	}
}

func fixturePageTransport(t *testing.T, route string, status int, body string, chunked bool) (*http.Transport, *pageObservation) {
	return fixtureOriginTransport(t, route, status, body, chunked, "page.example.com")
}

func fixtureOriginTransport(t *testing.T, route string, status int, body string, chunked bool, hostname string) (*http.Transport, *pageObservation) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), DNSNames: []string{hostname, "proxy.example.com"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, IsCA: true, BasicConstraintsValid: true,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	observed := &pageObservation{done: make(chan struct{})}
	serverTLS := &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		GetConfigForClient: func(info *tls.ClientHelloInfo) (*tls.Config, error) {
			observed.serverNames = append(observed.serverNames, info.ServerName)
			return nil, nil
		},
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}, TLSHandshakeTimeout: time.Second}
	transport.DialContext = func(ctx context.Context, _, address string) (net.Conn, error) {
		observed.mu.Lock()
		observed.dialCount++
		observed.dialAddress = address
		count := observed.dialCount
		observed.mu.Unlock()
		if count > 1 {
			return nil, errors.New("fixture must receive exactly one connection")
		}
		client, server := net.Pipe()
		go func() {
			defer close(observed.done)
			defer server.Close()
			_ = server.SetDeadline(time.Now().Add(2 * time.Second))
			var connection net.Conn = server
			if route == "https" {
				outerTLS := tls.Server(connection, serverTLS)
				if err := outerTLS.HandshakeContext(ctx); err != nil {
					t.Errorf("fixture proxy TLS: %v", err)
					return
				}
				connection = outerTLS
			}
			if route == "http" || route == "https" {
				request, err := http.ReadRequest(bufio.NewReader(connection))
				if err != nil {
					t.Errorf("fixture CONNECT: %v", err)
					return
				}
				observed.connectHost = request.Host
				observed.proxyAuth = request.Header.Get("Proxy-Authorization")
				if request.Method != http.MethodConnect {
					t.Errorf("fixture proxy method: %s", request.Method)
					return
				}
				_, _ = io.WriteString(connection, "HTTP/1.1 200 Connection Established\r\n\r\n")
			}
			originTLS := tls.Server(connection, serverTLS)
			if err := originTLS.HandshakeContext(ctx); err != nil {
				t.Errorf("fixture origin TLS: %v", err)
				return
			}
			request, err := http.ReadRequest(bufio.NewReader(originTLS))
			if err != nil {
				t.Errorf("fixture origin request: %v", err)
				return
			}
			observed.host, observed.requestURI, observed.headers = request.Host, request.RequestURI, request.Header
			// net.Pipe 没有 TCP 缓冲；接收 close_notify，避免早退关闭与服务端写正文互相阻塞。
			go func() { _, _ = io.Copy(io.Discard, originTLS) }()
			response := &http.Response{
				StatusCode: status, ProtoMajor: 1, ProtoMinor: 1, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)), Close: true,
			}
			if status == http.StatusFound {
				response.Header.Set("Location", "https://private.example.com/fixture-secret")
			}
			if chunked {
				response.ContentLength = -1
				response.TransferEncoding = []string{"chunked"}
			}
			_ = response.Write(originTLS)
		}()
		return client, nil
	}
	if route == "http" || route == "https" {
		proxyURL, err := url.Parse(route + "://fixture-user:fixture-password@proxy.example.com:443")
		if err != nil {
			t.Fatal(err)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	t.Cleanup(transport.CloseIdleConnections)
	return transport, observed
}
