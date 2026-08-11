package pe

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type v8HostEntropy struct {
	value byte
	err   error
}

func (entropy v8HostEntropy) Read(buffer []byte) (int, error) {
	if entropy.err != nil {
		return 0, entropy.err
	}
	for index := range buffer {
		buffer[index] = entropy.value + byte(index)
	}
	return len(buffer), nil
}

func (v8HostEntropy) Uint64n(uint64) (uint64, error) { return 0, nil }

type v8RoundTripFunc func(*http.Request) (*http.Response, error)

func (function v8RoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestV8HostRandom(t *testing.T) {
	result, err := handleV8HostRequest(
		context.Background(),
		nil,
		v8HostEntropy{value: 1},
		false,
		json.RawMessage(`{"op":"random","length":4}`),
	)
	if err != nil {
		t.Fatalf("handleV8HostRequest(random) error = %v", err)
	}
	var response struct {
		Base64 string `json:"base64"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if want := base64.StdEncoding.EncodeToString([]byte{1, 2, 3, 4}); response.Base64 != want {
		t.Fatalf("random base64 = %q, want %q", response.Base64, want)
	}

	for _, raw := range []string{
		`{"op":"random","length":65537}`,
		`{"op":"random","length":1,"extra":true}`,
		`{"op":"missing"}`,
	} {
		if _, err := handleV8HostRequest(context.Background(), nil, v8HostEntropy{}, false, json.RawMessage(raw)); err == nil {
			t.Fatalf("handleV8HostRequest(%s) unexpectedly succeeded", raw)
		}
	}
}

func TestV8HostHTTP(t *testing.T) {
	transport := v8RoundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("ReadAll(request.Body) error = %v", err)
		}
		if request.URL.String() != "https://g.alicdn.com/path" || request.Method != http.MethodPost || string(body) != "payload" {
			t.Fatalf("unexpected request %s %s %q", request.Method, request.URL, body)
		}
		if request.Header.Get("X-Test") != "yes" {
			t.Fatalf("X-Test = %q", request.Header.Get("X-Test"))
		}
		return &http.Response{
			StatusCode: http.StatusCreated,
			Header:     http.Header{"X-Reply": {"one", "two"}},
			Body:       io.NopCloser(strings.NewReader("ok")),
			Request:    request,
		}, nil
	})
	body := "payload"
	raw, err := json.Marshal(v8HostRequest{
		Op: "http",
		Request: &v8HostHTTPRequest{
			URL: "https://g.alicdn.com/path", Method: http.MethodPost,
			Headers: map[string]string{"X-Test": "yes"}, Body: &body, Redirect: "manual",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := handleV8HostRequest(context.Background(), transport, v8HostEntropy{}, true, raw)
	if err != nil {
		t.Fatalf("handleV8HostRequest(http) error = %v", err)
	}
	var response v8HostHTTPResponse
	if err := json.Unmarshal(result, &response); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if response.Status != http.StatusCreated || response.Body != "ok" || response.Headers["x-reply"] != "one, two" || response.Redirected {
		t.Fatalf("unexpected response %#v", response)
	}
}

func TestV8HostHTTPRejectsUnsafeRequests(t *testing.T) {
	for _, rawURL := range []string{
		"https://g.alicdn.com/path",
		"https://x.alicdn.com/captcha-frontend/dynamicJS/path.js",
		"https://prefix.captcha-open.aliyuncs.com/",
	} {
		if _, err := allowedV8NetworkURL(rawURL); err != nil {
			t.Fatalf("allowed URL %q rejected: %v", rawURL, err)
		}
	}
	transport := v8RoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("must not run")
	})
	for _, request := range []v8HostHTTPRequest{
		{URL: "http://g.alicdn.com/path", Method: "GET", Redirect: "manual"},
		{URL: "https://example.com/path", Method: "GET", Redirect: "manual"},
		{URL: "https://evil.x.alicdn.com/path", Method: "GET", Redirect: "manual"},
		{URL: "https://evil.aliyuncs.com.example/path", Method: "GET", Redirect: "manual"},
		{URL: "https://g.alicdn.com:443/path", Method: "GET", Redirect: "manual"},
		{URL: "https://user@g.alicdn.com/path", Method: "GET", Redirect: "manual"},
		{URL: "https://g.alicdn.com/path", Method: "GET", Redirect: "follow"},
	} {
		raw, err := json.Marshal(v8HostRequest{Op: "http", Request: &request})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := handleV8HostRequest(context.Background(), transport, v8HostEntropy{}, true, raw); err == nil {
			t.Fatalf("unsafe request %#v unexpectedly succeeded", request)
		}
	}

	raw := json.RawMessage(`{"op":"http","request":{"url":"https://g.alicdn.com/path","method":"GET","headers":{},"body":null,"redirect":"manual"}}`)
	if _, err := handleV8HostRequest(context.Background(), transport, v8HostEntropy{}, false, raw); err == nil {
		t.Fatal("disabled HTTP host unexpectedly succeeded")
	}
}

func TestV8HostHTTPResponseLimitAndCancellation(t *testing.T) {
	tooLarge := bytes.Repeat([]byte{'x'}, int(keyScriptMaxBytes)+1)
	transport := v8RoundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader(tooLarge)),
			Request:    request,
		}, nil
	})
	request := v8HostHTTPRequest{URL: "https://g.alicdn.com/path", Method: "GET", Redirect: "manual"}
	if _, err := roundTripV8HostRequest(context.Background(), transport, request); err == nil {
		t.Fatal("oversized response unexpectedly succeeded")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	transport = v8RoundTripFunc(func(request *http.Request) (*http.Response, error) {
		return nil, request.Context().Err()
	})
	if _, err := roundTripV8HostRequest(ctx, transport, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled response error = %v", err)
	}
}

func TestV8HostStaticAssetCache(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cache := newV8AssetCache()
	cache.now = func() time.Time { return now }
	calls := 0
	transport := v8RoundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Cache-Control": {"public, max-age=120"},
				"Content-Type":  {"application/javascript"},
			},
			Body:    io.NopCloser(strings.NewReader("asset")),
			Request: request,
		}, nil
	})
	handler := newV8HostHandler(transport, v8HostEntropy{}, true, cache)
	encode := func(rawURL, language string) json.RawMessage {
		t.Helper()
		raw, err := json.Marshal(v8HostRequest{
			Op: "http",
			Request: &v8HostHTTPRequest{
				URL: rawURL, Method: http.MethodGet,
				Headers:  map[string]string{"Accept-Language": language},
				Redirect: "manual",
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	asset := encode("https://x.alicdn.com/captcha-frontend/dynamicJS/versioned.js", "zh-CN")
	for range 2 {
		if _, err := handler(context.Background(), asset); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("cached asset requests = %d, want 1", calls)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := handler(cancelled, asset); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled cached request error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("cancelled cached request reached transport: calls=%d", calls)
	}
	if _, err := handler(context.Background(), encode("https://x.alicdn.com/captcha-frontend/dynamicJS/versioned.js", "en-US")); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("header-specific asset requests = %d, want 2", calls)
	}
	now = now.Add(v8AssetCacheTTL + time.Second)
	if _, err := handler(context.Background(), asset); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("expired asset requests = %d, want 3", calls)
	}
	api := encode("https://prefix.captcha-open.aliyuncs.com/", "zh-CN")
	for range 2 {
		if _, err := handler(context.Background(), api); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 5 {
		t.Fatalf("captcha API requests = %d, want 5", calls)
	}
}

func TestV8HostStaticAssetCachePolicy(t *testing.T) {
	for _, request := range []v8HostHTTPRequest{
		{URL: "https://prefix.captcha-open.aliyuncs.com/", Method: "GET", Redirect: "manual"},
		{URL: "https://x.alicdn.com/captcha-frontend/dynamicJS/asset.js", Method: "POST", Redirect: "manual"},
		{URL: "https://x.alicdn.com/captcha-frontend/dynamicJS/asset.js", Method: "GET", Headers: map[string]string{"Cookie": "secret"}, Redirect: "manual"},
		{URL: "https://x.alicdn.com/captcha-frontend/dynamicJS/asset.js", Method: "GET", Headers: map[string]string{"Authorization": "secret"}, Redirect: "manual"},
		{URL: "https://x.alicdn.com/not-a-captcha-asset.js", Method: "GET", Redirect: "manual"},
		{URL: "https://x.alicdn.com/captcha-frontend/dynamicJS/asset.json", Method: "GET", Redirect: "manual"},
	} {
		if _, ok := cacheableV8AssetRequest(request); ok {
			t.Fatalf("sensitive request became cacheable: %#v", request)
		}
	}
	for name, response := range map[string]v8HostHTTPResponse{
		"non-200":    {Status: http.StatusNotModified},
		"no-store":   {Status: http.StatusOK, Headers: map[string]string{"cache-control": "no-store"}},
		"private":    {Status: http.StatusOK, Headers: map[string]string{"cache-control": `private="set-cookie"`}},
		"set-cookie": {Status: http.StatusOK, Headers: map[string]string{"set-cookie": "secret"}},
		"vary-all":   {Status: http.StatusOK, Headers: map[string]string{"vary": "Accept-Encoding, *"}},
	} {
		if ttl := cacheableV8AssetResponseTTL(response); ttl != 0 {
			t.Fatalf("%s response TTL = %s, want 0", name, ttl)
		}
	}
}
