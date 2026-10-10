package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/foundation/runtimekit"
	"github.com/d2120848471/v3/ali-slider-go/internal/platform/v8runtime"
)

const (
	v8HostRequestMaxBytes = 4 << 20
	v8HostHeaderMaxBytes  = 64 << 10
	v8RandomMaxBytes      = 65_536
	v8AssetCacheTTL       = 30 * time.Second
	v8AssetCacheMaxItems  = 8
)

type v8AssetCacheEntry struct {
	response  v8HostHTTPResponse
	expiresAt time.Time
}

// v8AssetCache 只保存公开 CDN 静态响应字节。挑战 Init/Verify、token、DOM 和
// VM 状态都不进入缓存；短 TTL 仅去重同一 Device engine 生命期内的重复静态资源请求。
type v8AssetCache struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]v8AssetCacheEntry
}

func newV8AssetCache() *v8AssetCache {
	return &v8AssetCache{
		now: time.Now, entries: make(map[string]v8AssetCacheEntry),
	}
}

type v8HostRequest struct {
	Op      string             `json:"op"`
	Length  *int               `json:"length,omitempty"`
	Request *v8HostHTTPRequest `json:"request,omitempty"`
}

type v8HostHTTPRequest struct {
	URL      string            `json:"url"`
	Method   string            `json:"method"`
	Headers  map[string]string `json:"headers"`
	Body     *string           `json:"body"`
	Redirect string            `json:"redirect"`
}

type v8HostHTTPResponse struct {
	Status     int               `json:"status"`
	StatusText string            `json:"statusText"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body"`
	URL        string            `json:"url"`
	Redirected bool              `json:"redirected"`
}

func newV8HostHandler(
	transport http.RoundTripper,
	entropy runtimekit.Entropy,
	networkEnabled bool,
	assetCache *v8AssetCache,
) v8runtime.HostFunc {
	return func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		return handleV8HostRequestWithCache(ctx, transport, entropy, networkEnabled, assetCache, raw)
	}
}

func handleV8HostRequest(
	ctx context.Context,
	transport http.RoundTripper,
	entropy runtimekit.Entropy,
	networkEnabled bool,
	raw json.RawMessage,
) (json.RawMessage, error) {
	return handleV8HostRequestWithCache(ctx, transport, entropy, networkEnabled, nil, raw)
}

func handleV8HostRequestWithCache(
	ctx context.Context,
	transport http.RoundTripper,
	entropy runtimekit.Entropy,
	networkEnabled bool,
	assetCache *v8AssetCache,
	raw json.RawMessage,
) (json.RawMessage, error) {
	if ctx == nil {
		return nil, errors.New("V8 host context is nil")
	}
	if len(raw) == 0 || len(raw) > v8HostRequestMaxBytes {
		return nil, errors.New("V8 host request size is invalid")
	}
	var request v8HostRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return nil, errors.New("V8 host request is invalid")
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, errors.New("V8 host request is invalid")
	}

	switch request.Op {
	case "random":
		if request.Length == nil || *request.Length < 0 || *request.Length > v8RandomMaxBytes {
			return nil, errors.New("V8 random byte count is invalid")
		}
		if request.Request != nil || entropy == nil {
			return nil, errors.New("V8 random host is unavailable")
		}
		value := make([]byte, *request.Length)
		if _, err := entropy.Read(value); err != nil {
			return nil, errors.New("V8 random source failed")
		}
		encoded, err := json.Marshal(map[string]string{
			"base64": base64.StdEncoding.EncodeToString(value),
		})
		return encoded, err
	case "http":
		if request.Length != nil || request.Request == nil {
			return nil, errors.New("V8 HTTP request is invalid")
		}
		if !networkEnabled || transport == nil {
			return nil, errors.New("V8 HTTP host is disabled")
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		requestValue := *request.Request
		var response v8HostHTTPResponse
		var err error
		if cacheKey, ok := cacheableV8AssetRequest(requestValue); ok && assetCache != nil {
			if cached, hit := assetCache.get(cacheKey); hit {
				response = cached
			} else {
				response, err = roundTripV8HostRequest(ctx, transport, requestValue)
				if err == nil {
					assetCache.put(cacheKey, response)
				}
			}
		} else {
			response, err = roundTripV8HostRequest(ctx, transport, requestValue)
		}
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(response)
		return encoded, err
	default:
		return nil, errors.New("V8 host operation is unsupported")
	}
}

func cacheableV8AssetRequest(input v8HostHTTPRequest) (string, bool) {
	method := strings.ToUpper(strings.TrimSpace(input.Method))
	if method == "" {
		method = http.MethodGet
	}
	if method != http.MethodGet || input.Body != nil || input.Redirect != "manual" {
		return "", false
	}
	parsed, err := allowedV8NetworkURL(input.URL)
	if err != nil || !strings.EqualFold(parsed.Hostname(), keyPEHost) && !strings.EqualFold(parsed.Hostname(), keyCaptchaAssetHost) {
		return "", false
	}
	assetPath := parsed.EscapedPath()
	assetSuffix := strings.ToLower(assetPath)
	if !strings.HasPrefix(assetPath, "/captcha-frontend/dynamicJS/") ||
		!strings.HasSuffix(assetSuffix, ".js") && !strings.HasSuffix(assetSuffix, ".css") {
		return "", false
	}
	headerLines := make([]string, 0, len(input.Headers))
	for name, value := range input.Headers {
		normalizedName := strings.ToLower(strings.TrimSpace(name))
		if normalizedName == "authorization" || normalizedName == "cookie" {
			return "", false
		}
		headerLines = append(headerLines, normalizedName+"\x00"+value)
	}
	sort.Strings(headerLines)
	return parsed.String() + "\n" + strings.Join(headerLines, "\n"), true
}

func (cache *v8AssetCache) get(key string) (v8HostHTTPResponse, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	now := cache.now()
	cache.evictExpiredLocked(now)
	if cached, ok := cache.entries[key]; ok {
		return cloneV8HostHTTPResponse(cached.response), true
	}
	return v8HostHTTPResponse{}, false
}

func (cache *v8AssetCache) put(key string, response v8HostHTTPResponse) {
	ttl := cacheableV8AssetResponseTTL(response)
	if ttl <= 0 {
		return
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	now := cache.now()
	cache.evictExpiredLocked(now)
	if _, exists := cache.entries[key]; !exists {
		cache.evictOneLocked()
	}
	cache.entries[key] = v8AssetCacheEntry{
		response:  cloneV8HostHTTPResponse(response),
		expiresAt: now.Add(ttl),
	}
}

func cacheableV8AssetResponseTTL(response v8HostHTTPResponse) time.Duration {
	if response.Status != http.StatusOK || strings.TrimSpace(response.Headers["set-cookie"]) != "" {
		return 0
	}
	for _, field := range strings.Split(response.Headers["vary"], ",") {
		if strings.TrimSpace(field) == "*" {
			return 0
		}
	}
	ttl := v8AssetCacheTTL
	for _, directive := range strings.Split(strings.ToLower(response.Headers["cache-control"]), ",") {
		directive = strings.TrimSpace(directive)
		name, value, hasValue := strings.Cut(directive, "=")
		switch strings.TrimSpace(name) {
		case "no-store", "no-cache", "private":
			return 0
		case "max-age":
			if !hasValue {
				return 0
			}
			seconds, err := strconv.ParseInt(strings.Trim(value, `"`), 10, 64)
			if err != nil || seconds <= 0 {
				return 0
			}
			if seconds < int64(ttl/time.Second) {
				ttl = time.Duration(seconds) * time.Second
			}
		}
	}
	return ttl
}

func cloneV8HostHTTPResponse(response v8HostHTTPResponse) v8HostHTTPResponse {
	cloned := response
	cloned.Headers = make(map[string]string, len(response.Headers))
	for name, value := range response.Headers {
		cloned.Headers[name] = value
	}
	return cloned
}

func (cache *v8AssetCache) evictExpiredLocked(now time.Time) {
	for key, entry := range cache.entries {
		if !now.Before(entry.expiresAt) {
			delete(cache.entries, key)
		}
	}
}

func (cache *v8AssetCache) evictOneLocked() {
	if len(cache.entries) < v8AssetCacheMaxItems {
		return
	}
	var oldestKey string
	var oldestExpiry time.Time
	for key, entry := range cache.entries {
		if oldestKey == "" || entry.expiresAt.Before(oldestExpiry) {
			oldestKey, oldestExpiry = key, entry.expiresAt
		}
	}
	delete(cache.entries, oldestKey)
}

func allowedV8NetworkURL(value string) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		return nil, errors.New("V8 HTTP URL is invalid")
	}
	hostname := strings.ToLower(parsed.Hostname())
	hostAllowed := hostname == keyPEHost || hostname == keyCaptchaAssetHost || strings.HasSuffix(hostname, ".aliyuncs.com")
	if parsed.Port() != "" || parsed.User != nil || !hostAllowed {
		return nil, errors.New("V8 HTTP URL is outside the allowlist")
	}
	return parsed, nil
}

func roundTripV8HostRequest(
	ctx context.Context,
	transport http.RoundTripper,
	input v8HostHTTPRequest,
) (v8HostHTTPResponse, error) {
	parsed, err := allowedV8NetworkURL(input.URL)
	if err != nil {
		return v8HostHTTPResponse{}, err
	}
	method := strings.ToUpper(strings.TrimSpace(input.Method))
	if method == "" {
		method = http.MethodGet
	}
	if input.Redirect != "" && input.Redirect != "manual" {
		return v8HostHTTPResponse{}, errors.New("V8 HTTP redirects must be handled by the JS bridge")
	}
	var body io.Reader
	if input.Body != nil {
		if len(*input.Body) > int(keyScriptMaxBytes) {
			return v8HostHTTPResponse{}, errors.New("V8 HTTP request body is too large")
		}
		body = strings.NewReader(*input.Body)
	}
	request, err := http.NewRequestWithContext(ctx, method, parsed.String(), body)
	if err != nil {
		return v8HostHTTPResponse{}, errors.New("V8 HTTP request is invalid")
	}
	if input.Body != nil {
		fields, _ := url.ParseQuery(*input.Body)
		if fields.Get("Action") == "VerifyCaptchaV2" {
			// WAF Verify 也只有一次机会，禁止 HTTP/2 通过 GetBody 透明重放。
			request.GetBody = nil
		}
	}
	headerBytes := 0
	for name, value := range input.Headers {
		headerBytes += len(name) + len(value)
		if headerBytes > v8HostHeaderMaxBytes {
			return v8HostHTTPResponse{}, errors.New("V8 HTTP headers are too large")
		}
		request.Header.Set(name, value)
	}

	response, err := transport.RoundTrip(request)
	if err != nil {
		if ctx.Err() != nil {
			return v8HostHTTPResponse{}, ctx.Err()
		}
		return v8HostHTTPResponse{}, errors.New("V8 HTTP transport failed")
	}
	if response == nil || response.Body == nil {
		return v8HostHTTPResponse{}, errors.New("V8 HTTP response is invalid")
	}
	defer response.Body.Close()
	if response.StatusCode < 100 || response.StatusCode > 999 {
		return v8HostHTTPResponse{}, errors.New("V8 HTTP response status is invalid")
	}
	limited := io.LimitReader(response.Body, keyScriptMaxBytes+1)
	payload, err := io.ReadAll(limited)
	if err != nil {
		return v8HostHTTPResponse{}, errors.New("V8 HTTP response body failed")
	}
	if int64(len(payload)) > keyScriptMaxBytes {
		return v8HostHTTPResponse{}, errors.New("V8 HTTP response body is too large")
	}
	finalURL := parsed.String()
	if response.Request != nil && response.Request.URL != nil {
		validated, validateErr := allowedV8NetworkURL(response.Request.URL.String())
		if validateErr != nil {
			return v8HostHTTPResponse{}, errors.New("V8 HTTP response URL is outside the allowlist")
		}
		finalURL = validated.String()
	}
	headers := make(map[string]string, len(response.Header))
	for name, values := range response.Header {
		headers[strings.ToLower(name)] = strings.Join(values, ", ")
	}
	statusText := http.StatusText(response.StatusCode)
	if statusText == "" {
		statusText = fmt.Sprintf("HTTP %d", response.StatusCode)
	}
	return v8HostHTTPResponse{
		Status:     response.StatusCode,
		StatusText: statusText,
		Headers:    headers,
		Body:       string(payload),
		URL:        finalURL,
		Redirected: false,
	}, nil
}
