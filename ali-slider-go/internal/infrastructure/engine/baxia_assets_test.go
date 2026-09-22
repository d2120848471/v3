package engine

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/domain/baxia"
)

const (
	baxiaAssetSDKOne = "https://g.alicdn.com/AWSC/fireyejs/1.231.69/fireyejs.js"
	baxiaAssetSDKTwo = "https://g.alicdn.com/AWSC-br/fireyejs/1.232.1/fireyejs.js"
)

type baxiaAssetTransport struct {
	roundTrip func(*http.Request) (*http.Response, error)
	onClose   func()
	closed    atomic.Int32
}

func (transport *baxiaAssetTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport.roundTrip(request)
}

func (transport *baxiaAssetTransport) CloseIdleConnections() {
	transport.closed.Add(1)
	if transport.onClose != nil {
		transport.onClose()
	}
}

type baxiaAssetSession struct {
	profile baxia.RuntimeProfile
	closed  atomic.Int32
}

func (session *baxiaAssetSession) Token(ctx context.Context, _ string) (baxia.Result, error) {
	if err := ctx.Err(); err != nil {
		return baxia.Result{}, err
	}
	if session.closed.Load() != 0 {
		return baxia.Result{}, baxia.ErrClosed
	}
	return baxia.Result{
		Token: session.profile.Prefix + "fixture", Version: session.profile.Version,
		SDKHash: session.profile.SDKHash, Profile: session.profile,
	}, nil
}

func (session *baxiaAssetSession) Profile() baxia.RuntimeProfile { return session.profile }
func (session *baxiaAssetSession) Close() error                  { session.closed.Add(1); return nil }

func baxiaAssetNewSession(_ context.Context, config baxia.Config) (baxia.Session, error) {
	return &baxiaAssetSession{profile: baxia.RuntimeProfile{
		Version: len(config.SDKSource), Prefix: string(config.SDKSource),
		SDKHash: fmt.Sprintf("%x", sha256.Sum256(config.SDKSource)),
	}}, nil
}

func baxiaAssetResponse(status int, source string, headers http.Header) *http.Response {
	if headers == nil {
		headers = make(http.Header)
	}
	return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(source))}
}

func baxiaAssetClient(t *testing.T, transport *baxiaAssetTransport) *BaxiaClient {
	t.Helper()
	client, err := NewBaxiaClient(transport, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client.discover = func(context.Context, baxia.Config, string, []byte) (string, error) {
		return baxiaAssetSDKOne, nil
	}
	client.newSession = baxiaAssetNewSession
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func baxiaAssetAssertHeaders(t *testing.T, request *http.Request) {
	t.Helper()
	if request.Method != http.MethodGet || request.Header.Get("Accept") != baxiaSourceAccept ||
		request.Header.Get("Accept-Encoding") != "gzip" || request.Body != nil {
		t.Errorf("unexpected public script request: %s, %v", request.Method, request.Header)
	}
	for key := range request.Header {
		switch key {
		case "Accept", "Accept-Encoding", "User-Agent", "If-None-Match", "If-Modified-Since":
		default:
			t.Errorf("unexpected request header %q", key)
		}
	}
	if values, ok := request.Header["User-Agent"]; !ok || len(values) != 1 || values[0] != "" {
		t.Errorf("default User-Agent was not disabled: %v", values)
	}
}

func TestBaxiaClientConstructorAndOfflineSession(t *testing.T) {
	transport := &baxiaAssetTransport{roundTrip: func(*http.Request) (*http.Response, error) {
		t.Fatal("offline construction accessed the network")
		return nil, nil
	}}
	for _, timeout := range []time.Duration{-time.Second, time.Microsecond, 2*time.Minute + time.Nanosecond} {
		if _, err := NewBaxiaClient(transport, timeout); !errors.Is(err, baxia.ErrConfig) {
			t.Errorf("invalid timeout %s = %v", timeout, err)
		}
	}
	if _, err := NewBaxiaClient(nil, 0); !errors.Is(err, baxia.ErrConfig) {
		t.Fatalf("nil transport = %v", err)
	}
	client, err := NewBaxiaClient(transport, 0)
	if err != nil || client.timeout != 30*time.Second || transport.closed.Load() != 0 {
		t.Fatalf("default constructor = %+v, %v", client, err)
	}
	defer client.Close()
	client.discover = func(context.Context, baxia.Config, string, []byte) (string, error) {
		t.Fatal("explicit source invoked discovery")
		return "", nil
	}
	client.newSession = baxiaAssetNewSession
	session, err := client.NewSession(context.Background(), baxia.Config{SDKSource: []byte("explicit SDK")})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil || transport.closed.Load() != 1 {
		t.Fatalf("idempotent Close = %v, count %d", err, transport.closed.Load())
	}
	result, err := session.Token(context.Background(), "https://example.com/")
	if err != nil || result.SDKURL != "" || result.AWSCHash != "" {
		t.Fatalf("independent offline session = %+v, %v", result, err)
	}
	if _, err := client.NewSession(context.Background(), baxia.Config{}); !errors.Is(err, baxia.ErrClosed) {
		t.Fatalf("construction after Close = %v", err)
	}
	for _, timeout := range []time.Duration{time.Millisecond, 2 * time.Minute} {
		bounded, err := NewBaxiaClient(transport, timeout)
		if err != nil {
			t.Fatalf("valid timeout %s = %v", timeout, err)
		}
		_ = bounded.Close()
	}
}

func TestBaxiaClientRefreshesAWSCAndSDKWithoutChangingExistingSessions(t *testing.T) {
	for _, sameURL := range []bool{false, true} {
		t.Run(fmt.Sprintf("sameURL=%t", sameURL), func(t *testing.T) {
			generation, requests, discoveries, sessions := 1, 0, 0, 0
			transport := &baxiaAssetTransport{roundTrip: func(request *http.Request) (*http.Response, error) {
				baxiaAssetAssertHeaders(t, request)
				requests++
				if request.Header.Get("If-None-Match") != "" || request.Header.Get("If-Modified-Since") != "" {
					t.Fatal("response without validators produced a conditional request")
				}
				if request.URL.String() == baxiaAWSCURL {
					return baxiaAssetResponse(http.StatusOK, fmt.Sprintf("awsc%d", generation), nil), nil
				}
				return baxiaAssetResponse(http.StatusOK, strings.Repeat("sdk", generation), nil), nil
			}}
			client := baxiaAssetClient(t, transport)
			client.discover = func(_ context.Context, _ baxia.Config, sourceURL string, source []byte) (string, error) {
				discoveries++
				if sourceURL != baxiaAWSCURL || string(source) != fmt.Sprintf("awsc%d", generation) {
					t.Fatalf("discovery source = %s, %q", sourceURL, source)
				}
				if generation == 2 && !sameURL {
					return baxiaAssetSDKTwo, nil
				}
				return baxiaAssetSDKOne, nil
			}
			client.newSession = func(ctx context.Context, config baxia.Config) (baxia.Session, error) {
				sessions++
				return baxiaAssetNewSession(ctx, config)
			}
			first, err := client.NewSession(context.Background(), baxia.Config{})
			if err != nil {
				t.Fatal(err)
			}
			defer first.Close()
			oldProfile := first.Profile()
			generation = 2
			second, err := client.NewSession(context.Background(), baxia.Config{})
			if err != nil {
				t.Fatal(err)
			}
			defer second.Close()
			if requests != 4 || discoveries != 2 || sessions != 2 {
				t.Fatalf("per-session confirmation counts = %d, %d, %d", requests, discoveries, sessions)
			}
			_ = client.Close()
			oldResult, err := first.Token(context.Background(), "https://example.com/")
			if err != nil {
				t.Fatal(err)
			}
			newResult, err := second.Token(context.Background(), "https://example.com/")
			wantURL := baxiaAssetSDKTwo
			if sameURL {
				wantURL = baxiaAssetSDKOne
			}
			if err != nil || oldResult.SDKURL != baxiaAssetSDKOne || newResult.SDKURL != wantURL ||
				oldResult.SDKHash == newResult.SDKHash || oldResult.AWSCHash == newResult.AWSCHash ||
				first.Profile() != oldProfile || second.Profile() == oldProfile {
				t.Fatalf("source snapshots = %+v / %+v, %v", oldResult, newResult, err)
			}
			if oldResult.AWSCHash != fmt.Sprintf("%x", sha256.Sum256([]byte("awsc1"))) {
				t.Fatal("AWSC metadata did not hash downloaded bytes")
			}
		})
	}
}

func TestBaxiaClientRevalidatesCachedBytesAndResamples(t *testing.T) {
	requests := make(map[string]int)
	discoveries, sessions := 0, 0
	const modified = "Mon, 21 Sep 2026 00:00:00 GMT"
	transport := &baxiaAssetTransport{roundTrip: func(request *http.Request) (*http.Response, error) {
		baxiaAssetAssertHeaders(t, request)
		key := request.URL.String()
		requests[key]++
		if requests[key] == 1 {
			return baxiaAssetResponse(http.StatusOK, "original "+key, http.Header{
				"Etag": {`"v1"`}, "Last-Modified": {modified}, "Vary": {"Accept-Encoding"},
			}), nil
		}
		wantETag := fmt.Sprintf(`"v%d"`, requests[key]-1)
		if request.Header.Get("If-None-Match") != wantETag || request.Header.Get("If-Modified-Since") != modified {
			t.Fatalf("conditional headers = %v, want %s", request.Header, wantETag)
		}
		return baxiaAssetResponse(http.StatusNotModified, "", http.Header{
			"Etag": {fmt.Sprintf(`"v%d"`, requests[key])},
		}), nil
	}}
	client := baxiaAssetClient(t, transport)
	client.discover = func(_ context.Context, _ baxia.Config, _ string, source []byte) (string, error) {
		discoveries++
		if string(source) != "original "+baxiaAWSCURL {
			t.Fatal("discovery source was corrupted across cache reads")
		}
		source[0] = 'x'
		return baxiaAssetSDKOne, nil
	}
	client.newSession = func(ctx context.Context, config baxia.Config) (baxia.Session, error) {
		sessions++
		if string(config.SDKSource) != "original "+baxiaAssetSDKOne {
			t.Fatal("SDK cache bytes were mutated by an earlier session")
		}
		result, err := baxiaAssetNewSession(ctx, config)
		config.SDKSource[0] = 'x'
		return result, err
	}
	var hash string
	for range 3 {
		session, err := client.NewSession(context.Background(), baxia.Config{})
		if err != nil {
			t.Fatal(err)
		}
		if hash != "" && session.Profile().SDKHash != hash {
			t.Fatal("304 changed the script identity")
		}
		hash = session.Profile().SDKHash
		_ = session.Close()
	}
	if requests[baxiaAWSCURL] != 3 || requests[baxiaAssetSDKOne] != 3 || discoveries != 3 || sessions != 3 {
		t.Fatalf("confirmation/sampling counts = %v, %d, %d", requests, discoveries, sessions)
	}
}

func TestBaxiaClientNeverFallsBackAfterRefreshFailure(t *testing.T) {
	for _, stage := range []string{"awsc", "discovery", "sdk", "invalid sdk", "unsupported sdk"} {
		t.Run(stage, func(t *testing.T) {
			fail, builds := false, 0
			transport := &baxiaAssetTransport{roundTrip: func(request *http.Request) (*http.Response, error) {
				isAWSC := request.URL.String() == baxiaAWSCURL
				if fail && (stage == "awsc" && isAWSC || stage == "sdk" && !isAWSC) {
					return baxiaAssetResponse(http.StatusBadGateway, "CDN unavailable", nil), nil
				}
				if fail && stage == "invalid sdk" && !isAWSC {
					return baxiaAssetResponse(http.StatusOK, "<!doctype html><title>error</title>", nil), nil
				}
				if fail && request.Header.Get("If-None-Match") != "" {
					return baxiaAssetResponse(http.StatusNotModified, "", nil), nil
				}
				return baxiaAssetResponse(http.StatusOK, "fixture script", http.Header{"Etag": {`"v1"`}}), nil
			}}
			client := baxiaAssetClient(t, transport)
			client.discover = func(context.Context, baxia.Config, string, []byte) (string, error) {
				if fail && stage == "discovery" {
					return "", baxia.ErrUnsupportedSDK
				}
				return baxiaAssetSDKOne, nil
			}
			client.newSession = func(ctx context.Context, config baxia.Config) (baxia.Session, error) {
				builds++
				if fail && stage == "unsupported sdk" {
					return nil, baxia.ErrUnsupportedSDK
				}
				return baxiaAssetNewSession(ctx, config)
			}
			session, err := client.NewSession(context.Background(), baxia.Config{})
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			fail = true
			want := baxia.ErrSource
			if stage == "discovery" || stage == "unsupported sdk" {
				want = baxia.ErrUnsupportedSDK
			}
			next, err := client.NewSession(context.Background(), baxia.Config{})
			if next != nil || !errors.Is(err, want) {
				t.Fatalf("refresh failure = %v, %v", next, err)
			}
			if stage == "unsupported sdk" && (!strings.Contains(err.Error(), baxiaAssetSDKOne) ||
				!strings.Contains(err.Error(), fmt.Sprintf("%x", sha256.Sum256([]byte("fixture script")))) ||
				strings.Contains(err.Error(), "fixture script")) {
				t.Fatalf("initialization error lacks source identity or exposes script text: %v", err)
			}
			wantBuilds := 1
			if stage == "unsupported sdk" {
				wantBuilds = 2
			}
			if builds != wantBuilds {
				t.Fatalf("failed refresh unexpectedly created a session: %d", builds)
			}
			if _, err := session.Token(context.Background(), "https://example.com/"); err != nil {
				t.Fatalf("refresh failure damaged an existing session: %v", err)
			}
		})
	}
}

func TestBaxiaSourceURLAllowlist(t *testing.T) {
	for _, raw := range []string{baxiaAWSCURL, "https://g.alicdn.com/AWSC/AWSC/awsc.js", baxiaAssetSDKOne, baxiaAssetSDKTwo} {
		if _, err := parseBaxiaScriptURL(raw); err != nil {
			t.Errorf("allowed URL %s: %v", raw, err)
		}
	}
	for _, raw := range []string{
		"http://g.alicdn.com/AWSC/AWSC/awsc.js",
		"https://evil.example/AWSC/AWSC/awsc.js",
		"https://g.alicdn.com.evil.example/AWSC/AWSC/awsc.js",
		"https://g.alicdn.com:443/AWSC/AWSC/awsc.js",
		"https://user:password@g.alicdn.com/AWSC/AWSC/awsc.js",
		baxiaAWSCURL + "&other=1", baxiaAWSCURL + "#", baxiaAssetSDKOne + "#part",
		baxiaAssetSDKOne + "?", baxiaAssetSDKOne + "?version=1", baxiaAssetSDKOne + "/extra",
		"https://g.alicdn.com/AWSC/fireyejs/latest/fireyejs.js",
		"https://g.alicdn.com/AWSC/fireyejs/1.2-beta/fireyejs.js",
		"https://g.alicdn.com/AWSC%2ffireyejs/1.2.3/fireyejs.js",
		"https://g.alicdn.com/AWSC/../AWSC/AWSC/awsc.js",
		"https://g.alicdn.com/api/login", "https://g.alicdn.com/?/AWSC/AWSC/awsc.js",
		"//g.alicdn.com/AWSC/AWSC/awsc.js", "://invalid",
	} {
		if _, err := parseBaxiaScriptURL(raw); !errors.Is(err, baxia.ErrSource) {
			t.Errorf("disallowed URL %q: %v", raw, err)
		}
	}
}

func TestBaxiaSourceRedirectsValidateEveryHopAndIsolateValidators(t *testing.T) {
	initial := true
	transport := &baxiaAssetTransport{roundTrip: func(request *http.Request) (*http.Response, error) {
		baxiaAssetAssertHeaders(t, request)
		if request.URL.String() == baxiaAssetSDKOne {
			if initial {
				return baxiaAssetResponse(http.StatusOK, "old SDK", http.Header{"Etag": {`"one"`}}), nil
			}
			if request.Header.Get("If-None-Match") != `"one"` {
				t.Fatal("original URL lost its validator")
			}
			return baxiaAssetResponse(http.StatusFound, "", http.Header{"Location": {baxiaAssetSDKTwo}}), nil
		}
		if request.URL.String() != baxiaAssetSDKTwo || request.Header.Get("If-None-Match") != "" {
			t.Fatalf("redirect leaked a different representation validator: %s, %v", request.URL, request.Header)
		}
		return baxiaAssetResponse(http.StatusOK, "new SDK", nil), nil
	}}
	client := baxiaAssetClient(t, transport)
	if _, _, err := client.downloadScript(context.Background(), baxiaAssetSDKOne); err != nil {
		t.Fatal(err)
	}
	initial = false
	source, finalURL, err := client.downloadScript(context.Background(), baxiaAssetSDKOne)
	if err != nil || string(source) != "new SDK" || finalURL != baxiaAssetSDKTwo {
		t.Fatalf("redirect result = %q, %s, %v", source, finalURL, err)
	}
	for _, location := range []string{"https://evil.example/script.js", "/api/login", "http://g.alicdn.com/AWSC/AWSC/awsc.js", "#", baxiaAssetSDKTwo + "?x=1"} {
		t.Run(location, func(t *testing.T) {
			calls := 0
			client := baxiaAssetClient(t, &baxiaAssetTransport{roundTrip: func(*http.Request) (*http.Response, error) {
				calls++
				return baxiaAssetResponse(http.StatusFound, "", http.Header{"Location": {location}}), nil
			}})
			if _, _, err := client.downloadScript(context.Background(), baxiaAWSCURL); !errors.Is(err, baxia.ErrSource) || calls != 1 {
				t.Fatalf("rejected redirect = %v after %d requests", err, calls)
			}
		})
	}
	for _, redirects := range []int{3, 4} {
		t.Run(fmt.Sprintf("redirects=%d", redirects), func(t *testing.T) {
			calls := 0
			client := baxiaAssetClient(t, &baxiaAssetTransport{roundTrip: func(*http.Request) (*http.Response, error) {
				calls++
				if calls <= redirects {
					return baxiaAssetResponse(http.StatusTemporaryRedirect, "", http.Header{"Location": {baxiaAssetSDKOne}}), nil
				}
				return baxiaAssetResponse(http.StatusOK, "SDK", nil), nil
			}})
			_, _, err := client.downloadScript(context.Background(), baxiaAWSCURL)
			if redirects == 3 && err != nil || redirects == 4 && !errors.Is(err, baxia.ErrSource) || calls != 4 {
				t.Fatalf("redirect bound = %d calls, %v", calls, err)
			}
		})
	}
}

func TestBaxiaClientDiscoveryUsesFinalAWSCURL(t *testing.T) {
	const alias = "https://g.alicdn.com/AWSC/AWSC/awsc.js"
	client := baxiaAssetClient(t, &baxiaAssetTransport{roundTrip: func(request *http.Request) (*http.Response, error) {
		if request.URL.String() == baxiaAWSCURL {
			return baxiaAssetResponse(http.StatusMovedPermanently, "", http.Header{"Location": {"/AWSC/AWSC/awsc.js"}}), nil
		}
		return baxiaAssetResponse(http.StatusOK, "fixture script", nil), nil
	}})
	client.discover = func(_ context.Context, _ baxia.Config, awscURL string, _ []byte) (string, error) {
		if awscURL != alias {
			t.Fatalf("discovery source URL = %s", awscURL)
		}
		return baxiaAssetSDKOne, nil
	}
	session, err := client.NewSession(context.Background(), baxia.Config{})
	if err != nil {
		t.Fatal(err)
	}
	_ = session.Close()
	for _, selected := range []string{baxiaAWSCURL, "https://example.com/sdk.js"} {
		client.discover = func(context.Context, baxia.Config, string, []byte) (string, error) { return selected, nil }
		if _, err := client.NewSession(context.Background(), baxia.Config{}); !errors.Is(err, baxia.ErrSource) {
			t.Fatalf("non-Fireye discovery URL = %v", err)
		}
	}
}

func TestBaxiaSourceCacheRespectsRepresentationAndCapacity(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers http.Header
		cached  bool
	}{
		{"fixed Vary", http.Header{"Etag": {`"v1"`}, "Vary": {"Accept, Accept-Encoding, User-Agent"}}, true},
		{"Last-Modified", http.Header{"Last-Modified": {"Mon, 21 Sep 2026 00:00:00 GMT"}}, true},
		{"no validators", nil, false},
		{"Vary wildcard", http.Header{"Etag": {`"v1"`}, "Vary": {"*"}}, false},
		{"unknown Vary", http.Header{"Etag": {`"v1"`}, "Vary": {"Accept-Language"}}, false},
		{"conditional Vary", http.Header{"Etag": {`"v1"`}, "Vary": {"If-None-Match"}}, false},
		{"no-store", http.Header{"Etag": {`"v1"`}, "Cache-Control": {"max-age=100, no-store"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := baxiaAssetClient(t, &baxiaAssetTransport{roundTrip: func(request *http.Request) (*http.Response, error) {
				calls++
				conditional := request.Header.Get("If-None-Match") != "" || request.Header.Get("If-Modified-Since") != ""
				if conditional != (calls > 1 && tc.cached) {
					t.Fatalf("cache policy produced wrong conditional request: %v", request.Header)
				}
				return baxiaAssetResponse(http.StatusOK, "fixture script", tc.headers), nil
			}})
			for range 2 {
				if _, _, err := client.downloadScript(context.Background(), baxiaAssetSDKOne); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	conditional := false
	client := baxiaAssetClient(t, &baxiaAssetTransport{roundTrip: func(request *http.Request) (*http.Response, error) {
		conditional = request.Header.Get("If-None-Match") != ""
		return baxiaAssetResponse(http.StatusOK, "fixture script", http.Header{"Etag": {`"v1"`}}), nil
	}})
	for i := range baxiaSourceCacheCapacity + 4 {
		url := fmt.Sprintf("https://g.alicdn.com/AWSC/fireyejs/1.231.%d/fireyejs.js", i)
		if _, _, err := client.downloadScript(context.Background(), url); err != nil {
			t.Fatal(err)
		}
	}
	if len(client.cache) != baxiaSourceCacheCapacity {
		t.Fatalf("cache entries = %d", len(client.cache))
	}
	if _, _, err := client.downloadScript(context.Background(), "https://g.alicdn.com/AWSC/fireyejs/1.231.0/fireyejs.js"); err != nil || conditional {
		t.Fatalf("evicted entry reused a validator: %t, %v", conditional, err)
	}
}

func TestBaxiaSourceRejectsUnsolicited304AndTransportFailures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response *http.Response
		err      error
	}{
		{"unsolicited 304", baxiaAssetResponse(http.StatusNotModified, "", nil), nil},
		{"nil response", nil, nil},
		{"missing body", &http.Response{StatusCode: http.StatusOK}, nil},
		{"network error", nil, errors.New("fixture network error")},
		{"HTTP error", baxiaAssetResponse(http.StatusForbidden, "blocked", nil), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := baxiaAssetClient(t, &baxiaAssetTransport{roundTrip: func(*http.Request) (*http.Response, error) { return tc.response, tc.err }})
			if _, _, err := client.downloadScript(context.Background(), baxiaAWSCURL); !errors.Is(err, baxia.ErrSource) {
				t.Fatalf("invalid response error = %v", err)
			}
		})
	}
}

func baxiaAssetGzip(t *testing.T, source []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write(source); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestBaxiaSourceDecodesGzipAndRejectsInvalidOrOversizedScripts(t *testing.T) {
	source := []byte("// fixture\nvar script = true;")
	compressed := baxiaAssetGzip(t, source)
	exactLimit := bytes.Repeat([]byte("a"), baxiaMaximumSDKBytes)
	overLimit := bytes.Repeat([]byte("a"), baxiaMaximumSDKBytes+1)
	for _, tc := range []struct {
		name     string
		body     []byte
		headers  http.Header
		want     []byte
		wantFail bool
	}{
		{"plain", source, nil, source, false},
		{"declared gzip", compressed, http.Header{"Content-Encoding": {"gzip"}}, source, false},
		{"missing gzip header", compressed, nil, source, false},
		{"identity with gzip magic", compressed, http.Header{"Content-Encoding": {"identity"}}, source, false},
		{"exact plain limit", exactLimit, nil, exactLimit, false},
		{"exact decoded limit", baxiaAssetGzip(t, exactLimit), nil, exactLimit, false},
		{"invalid encoding", source, http.Header{"Content-Encoding": {"br"}}, nil, true},
		{"multiple encodings", compressed, http.Header{"Content-Encoding": {"gzip", "br"}}, nil, true},
		{"truncated gzip", compressed[:len(compressed)-3], nil, nil, true},
		{"false gzip header", source, http.Header{"Content-Encoding": {"gzip"}}, nil, true},
		{"non UTF-8", []byte{0xff, 0xfe}, nil, nil, true},
		{"HTML body", []byte(" \xef\xbb\xbf\n<!doctype html><html>error</html>"), nil, nil, true},
		{"HTML type", source, http.Header{"Content-Type": {"text/html; charset=utf-8"}}, nil, true},
		{"empty", []byte(" \n\t"), nil, nil, true},
		{"plain over limit", overLimit, nil, nil, true},
		{"decoded over limit", baxiaAssetGzip(t, overLimit), nil, nil, true},
		{"encoded over limit", bytes.Repeat([]byte("a"), baxiaMaximumEncodedBytes+1), nil, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := baxiaAssetResponse(http.StatusOK, string(tc.body), tc.headers)
			defer response.Body.Close()
			got, err := readBaxiaScript(response)
			if tc.wantFail {
				if !errors.Is(err, baxia.ErrSource) {
					t.Fatalf("invalid script = %v", err)
				}
			} else if err != nil || !bytes.Equal(got, tc.want) {
				t.Fatalf("script decode: len=%d, error=%v", len(got), err)
			}
		})
	}
}

func TestBaxiaClientCancellationAndCloseWaitForConstruction(t *testing.T) {
	for _, stage := range []string{"download", "discovery", "session"} {
		t.Run(stage, func(t *testing.T) {
			entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			block := func(ctx context.Context) error {
				close(entered)
				<-ctx.Done()
				close(canceled)
				<-release
				return ctx.Err()
			}
			transport := &baxiaAssetTransport{roundTrip: func(request *http.Request) (*http.Response, error) {
				if stage == "download" {
					return nil, block(request.Context())
				}
				return baxiaAssetResponse(http.StatusOK, "fixture script", nil), nil
			}}
			client := baxiaAssetClient(t, transport)
			if stage == "discovery" {
				client.discover = func(ctx context.Context, _ baxia.Config, _ string, _ []byte) (string, error) { return "", block(ctx) }
			}
			var abandoned *baxiaAssetSession
			if stage == "session" {
				abandoned = &baxiaAssetSession{}
				client.newSession = func(ctx context.Context, _ baxia.Config) (baxia.Session, error) {
					_ = block(ctx)
					return abandoned, nil
				}
			}
			result := make(chan error, 1)
			go func() { _, err := client.NewSession(context.Background(), baxia.Config{}); result <- err }()
			<-entered
			closed := make(chan struct{})
			go func() { _ = client.Close(); close(closed) }()
			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatal("Close did not cancel construction")
			}
			if transport.closed.Load() != 0 {
				t.Fatal("transport closed before construction exited")
			}
			select {
			case <-closed:
				t.Fatal("Close did not wait for construction")
			default:
			}
			close(release)
			select {
			case err := <-result:
				if !errors.Is(err, baxia.ErrClosed) {
					t.Fatalf("interrupted construction = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("construction did not exit")
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("Close deadlocked")
			}
			if transport.closed.Load() != 1 || abandoned != nil && abandoned.closed.Load() != 1 {
				t.Fatal("abandoned construction resources were not closed once")
			}
		})
	}
}

func TestBaxiaClientPreservesCallerAndTotalTimeoutErrors(t *testing.T) {
	for _, caller := range []bool{false, true} {
		t.Run(fmt.Sprint("caller=", caller), func(t *testing.T) {
			entered := make(chan struct{})
			client := baxiaAssetClient(t, &baxiaAssetTransport{roundTrip: func(request *http.Request) (*http.Response, error) {
				close(entered)
				<-request.Context().Done()
				return nil, request.Context().Err()
			}})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := context.DeadlineExceeded
			if !caller {
				client.timeout = 10 * time.Millisecond
			} else {
				want = context.Canceled
			}
			result := make(chan error, 1)
			go func() { _, err := client.NewSession(ctx, baxia.Config{}); result <- err }()
			<-entered
			if caller {
				cancel()
			}
			select {
			case err := <-result:
				if err != want {
					t.Fatalf("context error = %v, want original %v", err, want)
				}
			case <-time.After(time.Second):
				t.Fatal("context cancellation was ignored")
			}
			//lint:ignore SA1012 此处故意传 nil，验证边界拒绝空 context。
			if _, err := client.NewSession(nil, baxia.Config{}); !errors.Is(err, baxia.ErrConfig) {
				t.Fatalf("nil context = %v", err)
			}
			cancel()
			if _, err := client.NewSession(ctx, baxia.Config{}); err != context.Canceled {
				t.Fatalf("pre-canceled context = %v", err)
			}
		})
	}
}

func TestBaxiaClientConcurrentSessionsEachConfirmAndSample(t *testing.T) {
	const count = 24
	var awscRequests, sdkRequests, constructions atomic.Int32
	transport := &baxiaAssetTransport{roundTrip: func(request *http.Request) (*http.Response, error) {
		baxiaAssetAssertHeaders(t, request)
		if request.URL.String() == baxiaAWSCURL {
			awscRequests.Add(1)
		} else {
			sdkRequests.Add(1)
		}
		if request.Header.Get("If-None-Match") != "" {
			return baxiaAssetResponse(http.StatusNotModified, "", nil), nil
		}
		return baxiaAssetResponse(http.StatusOK, "fixture script", http.Header{"Etag": {`"v1"`}}), nil
	}}
	client := baxiaAssetClient(t, transport)
	client.newSession = func(ctx context.Context, config baxia.Config) (baxia.Session, error) {
		constructions.Add(1)
		return baxiaAssetNewSession(ctx, config)
	}
	var group sync.WaitGroup
	for range count {
		group.Go(func() {
			session, err := client.NewSession(context.Background(), baxia.Config{})
			if err != nil {
				t.Errorf("concurrent construction: %v", err)
				return
			}
			_ = session.Close()
		})
	}
	group.Wait()
	if awscRequests.Load() != count || sdkRequests.Load() != count || constructions.Load() != count {
		t.Fatalf("concurrent per-session counts = %d, %d, %d", awscRequests.Load(), sdkRequests.Load(), constructions.Load())
	}
	for range 4 {
		group.Go(func() { _ = client.Close() })
	}
	group.Wait()
	if transport.closed.Load() != 1 {
		t.Fatal("concurrent Close did not release transport exactly once")
	}
}
