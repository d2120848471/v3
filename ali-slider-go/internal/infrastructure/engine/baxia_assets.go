package engine

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/d2120848471/v3/ali-slider-go/internal/domain/baxia"
)

const (
	baxiaAWSCURL              = "https://g.alicdn.com/??/AWSC/AWSC/awsc.js"
	baxiaSourceDefaultTimeout = 30 * time.Second
	baxiaSourceMaximumTimeout = 2 * time.Minute
	baxiaSourceCacheCapacity  = 16
	baxiaMaximumEncodedBytes  = 4 << 20
	baxiaSourceAccept         = "application/javascript, text/javascript"
)

var baxiaFireyePath = regexp.MustCompile(`^/AWSC(?:-br)?/fireyejs/[0-9]+(?:\.[0-9]+)+/fireyejs\.js$`)

type baxiaCachedScript struct {
	source       []byte
	etag         string
	lastModified string
	used         uint64
}

// BaxiaClient 为每个新会话重新确认 AWSC 和其选择的 Fireye 源码。
// Close 取消并等待构造中的会话后关闭 transport 的空闲连接；已交付的
// Session 有独立生命周期，调用方仍须分别 Close。
type BaxiaClient struct {
	transport http.RoundTripper
	timeout   time.Duration
	lifetime  context.Context
	cancel    context.CancelFunc

	mu         sync.Mutex
	closed     bool
	pending    sync.WaitGroup
	closeOnce  sync.Once
	cache      map[string]baxiaCachedScript
	cacheClock uint64

	discover   func(context.Context, baxia.Config, string, []byte) (string, error)
	newSession func(context.Context, baxia.Config) (baxia.Session, error)
}

var _ baxia.Client = (*BaxiaClient)(nil)

// NewBaxiaClient 只验证配置，不联网；成功后接管 transport 的空闲连接关闭。
// timeout 覆盖每次发现、下载和初始化，零值为 30 秒。
func NewBaxiaClient(transport http.RoundTripper, timeout time.Duration) (*BaxiaClient, error) {
	if transport == nil {
		return nil, fmt.Errorf("%w: source transport is nil", baxia.ErrConfig)
	}
	if timeout == 0 {
		timeout = baxiaSourceDefaultTimeout
	}
	if timeout < time.Millisecond || timeout > baxiaSourceMaximumTimeout {
		return nil, fmt.Errorf("%w: source timeout must be between 1 ms and 2 min", baxia.ErrConfig)
	}
	lifetime, cancel := context.WithCancel(context.Background())
	return &BaxiaClient{
		transport: transport, timeout: timeout, lifetime: lifetime, cancel: cancel,
		cache: make(map[string]baxiaCachedScript), discover: DiscoverBaxiaSDK,
		newSession: func(ctx context.Context, config baxia.Config) (baxia.Session, error) {
			return NewBaxiaSession(ctx, config)
		},
	}, nil
}

// NewSession 显式 SDKSource 保持离线；否则每次都经过 HTTP 重新确认，
// 条件请求只复用字节，不缓存发现结果或 RuntimeProfile。
func (client *BaxiaClient) NewSession(ctx context.Context, config baxia.Config) (baxia.Session, error) {
	if client == nil || client.lifetime == nil {
		return nil, baxia.ErrClosed
	}
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is nil", baxia.ErrConfig)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	client.mu.Lock()
	if client.closed {
		client.mu.Unlock()
		return nil, baxia.ErrClosed
	}
	// Add 与 closed 共用一把锁，避免 Close 的 Wait 与新构造竞争。
	client.pending.Add(1)
	client.mu.Unlock()
	defer client.pending.Done()

	runCtx, cancel := context.WithTimeout(ctx, client.timeout)
	defer cancel()
	stop := context.AfterFunc(client.lifetime, cancel)
	defer stop()
	failure := func(err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if client.lifetime.Err() != nil {
			return baxia.ErrClosed
		}
		if runCtx.Err() != nil {
			return runCtx.Err()
		}
		return err
	}
	if err := failure(nil); err != nil {
		return nil, err
	}

	var sdkURL, sdkHash, awscHash string
	if len(config.SDKSource) == 0 {
		awsc, finalURL, err := client.downloadScript(runCtx, baxiaAWSCURL)
		if err != nil {
			return nil, failure(err)
		}
		awscHash = fmt.Sprintf("%x", sha256.Sum256(awsc))
		selected, err := client.discover(runCtx, config, finalURL, awsc)
		if err != nil {
			return nil, failure(err)
		}
		selectedURL, err := parseBaxiaScriptURL(selected)
		if err != nil || !baxiaFireyePath.MatchString(selectedURL.Path) {
			return nil, failure(fmt.Errorf("%w: AWSC did not select an allowed Fireye URL", baxia.ErrSource))
		}
		config.SDKSource, sdkURL, err = client.downloadScript(runCtx, selected)
		if err != nil {
			return nil, failure(err)
		}
		sdkHash = fmt.Sprintf("%x", sha256.Sum256(config.SDKSource))
	}
	session, err := client.newSession(runCtx, config)
	if err != nil {
		if session != nil {
			_ = session.Close()
		}
		if sdkURL != "" {
			err = fmt.Errorf("%w: initialize Fireye from %s (SHA256 %s)", err, sdkURL, sdkHash)
		}
		return nil, failure(err)
	}
	if session == nil {
		return nil, failure(fmt.Errorf("%w: session constructor returned no session", baxia.ErrRuntime))
	}
	client.mu.Lock()
	err = failure(nil)
	client.mu.Unlock()
	if err != nil {
		_ = session.Close()
		return nil, err
	}
	if sdkURL == "" {
		return session, nil
	}
	return &baxiaSourceSession{Session: session, sdkURL: sdkURL, awscHash: awscHash}, nil
}

func (client *BaxiaClient) Close() error {
	if client == nil || client.lifetime == nil {
		return nil
	}
	client.closeOnce.Do(func() {
		client.mu.Lock()
		client.closed = true
		client.cancel()
		client.mu.Unlock()
		client.pending.Wait()
		if transport, ok := client.transport.(interface{ CloseIdleConnections() }); ok {
			transport.CloseIdleConnections()
		}
		client.mu.Lock()
		client.cache = nil
		client.mu.Unlock()
	})
	return nil
}

type baxiaSourceSession struct {
	baxia.Session
	sdkURL   string
	awscHash string
}

func (session *baxiaSourceSession) Token(ctx context.Context, requestURL string) (baxia.Result, error) {
	result, err := session.Session.Token(ctx, requestURL)
	if err == nil {
		result.SDKURL = session.sdkURL
		result.AWSCHash = session.awscHash
	}
	return result, err
}

func parseBaxiaScriptURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != "g.alicdn.com" || u.User != nil ||
		u.Opaque != "" || u.RawPath != "" || u.ForceQuery || strings.Contains(raw, "#") {
		return nil, fmt.Errorf("%w: script URL is outside the public AWSC/Fireye allowlist", baxia.ErrSource)
	}
	if u.Path == "/" && u.RawQuery == "?/AWSC/AWSC/awsc.js" {
		return u, nil
	}
	if u.RawQuery == "" && (u.Path == "/AWSC/AWSC/awsc.js" || baxiaFireyePath.MatchString(u.Path)) {
		return u, nil
	}
	return nil, fmt.Errorf("%w: script URL is outside the public AWSC/Fireye allowlist", baxia.ErrSource)
}

func (client *BaxiaClient) downloadScript(ctx context.Context, rawURL string) ([]byte, string, error) {
	u, err := parseBaxiaScriptURL(rawURL)
	if err != nil {
		return nil, "", err
	}
	for redirects := 0; ; redirects++ {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		cacheKey := u.String()
		cached := client.cachedScript(cacheKey)
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, cacheKey, nil)
		if err != nil {
			return nil, "", fmt.Errorf("%w: create script request: %w", baxia.ErrSource, err)
		}
		request.Header.Set("Accept", baxiaSourceAccept)
		request.Header.Set("Accept-Encoding", "gzip")
		request.Header["User-Agent"] = []string{""}
		if cached.etag != "" {
			request.Header.Set("If-None-Match", cached.etag)
		}
		if cached.lastModified != "" {
			request.Header.Set("If-Modified-Since", cached.lastModified)
		}
		response, err := client.transport.RoundTrip(request)
		if err != nil {
			if response != nil && response.Body != nil {
				_ = response.Body.Close()
			}
			if ctx.Err() != nil {
				return nil, "", ctx.Err()
			}
			return nil, "", fmt.Errorf("%w: download script: %w", baxia.ErrSource, err)
		}
		if response == nil || response.Body == nil {
			return nil, "", fmt.Errorf("%w: script response has no body", baxia.ErrSource)
		}
		switch response.StatusCode {
		case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
			http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
			_ = response.Body.Close()
			if redirects >= 3 {
				return nil, "", fmt.Errorf("%w: script redirect limit exceeded", baxia.ErrSource)
			}
			location := response.Header.Get("Location")
			next, err := url.Parse(location)
			if err != nil || location == "" || strings.Contains(location, "#") {
				return nil, "", fmt.Errorf("%w: script redirect has invalid Location", baxia.ErrSource)
			}
			u, err = parseBaxiaScriptURL(u.ResolveReference(next).String())
			if err != nil {
				return nil, "", err
			}
		case http.StatusNotModified:
			_ = response.Body.Close()
			if len(cached.source) == 0 || cached.etag == "" && cached.lastModified == "" {
				return nil, "", fmt.Errorf("%w: script returned 304 without a conditional cache entry", baxia.ErrSource)
			}
			if etag := response.Header.Get("ETag"); etag != "" {
				cached.etag = etag
			}
			if modified := response.Header.Get("Last-Modified"); modified != "" {
				cached.lastModified = modified
			}
			client.storeScript(cacheKey, cached, response.Header)
			return bytes.Clone(cached.source), cacheKey, nil
		case http.StatusOK:
			source, err := readBaxiaScript(response)
			_ = response.Body.Close()
			if ctx.Err() != nil {
				return nil, "", ctx.Err()
			}
			if err != nil {
				return nil, "", err
			}
			client.storeScript(cacheKey, baxiaCachedScript{
				source: bytes.Clone(source), etag: response.Header.Get("ETag"),
				lastModified: response.Header.Get("Last-Modified"),
			}, response.Header)
			return source, cacheKey, nil
		default:
			_ = response.Body.Close()
			return nil, "", fmt.Errorf("%w: script returned HTTP %d", baxia.ErrSource, response.StatusCode)
		}
	}
}

func (client *BaxiaClient) cachedScript(key string) baxiaCachedScript {
	client.mu.Lock()
	defer client.mu.Unlock()
	cached, ok := client.cache[key]
	if ok {
		client.cacheClock++
		cached.used = client.cacheClock
		client.cache[key] = cached
	}
	return cached
}

func (client *BaxiaClient) storeScript(key string, cached baxiaCachedScript, headers http.Header) {
	client.mu.Lock()
	defer client.mu.Unlock()
	if !baxiaCacheableHeaders(headers) || cached.etag == "" && cached.lastModified == "" {
		delete(client.cache, key)
		return
	}
	if _, exists := client.cache[key]; !exists && len(client.cache) >= baxiaSourceCacheCapacity {
		var oldest string
		for key, entry := range client.cache {
			if oldest == "" || entry.used < client.cache[oldest].used {
				oldest = key
			}
		}
		delete(client.cache, oldest)
	}
	client.cacheClock++
	cached.used = client.cacheClock
	client.cache[key] = cached
}

func baxiaCacheableHeaders(headers http.Header) bool {
	// 所有请求的这些表示选择头都固定。未知 Vary（含 *）改用完整 GET，
	// 不猜测 CDN 是否还根据设备、页面或其他信息选择了不同表示。
	for _, line := range headers.Values("Vary") {
		for _, name := range strings.Split(line, ",") {
			switch strings.ToLower(strings.TrimSpace(name)) {
			case "", "accept", "accept-encoding", "user-agent":
			default:
				return false
			}
		}
	}
	for _, line := range headers.Values("Cache-Control") {
		for _, directive := range strings.Split(line, ",") {
			name, _, _ := strings.Cut(strings.TrimSpace(directive), "=")
			if strings.EqualFold(name, "no-store") {
				return false
			}
		}
	}
	return true
}

func readBaxiaScript(response *http.Response) ([]byte, error) {
	fail := func(reason string) ([]byte, error) {
		return nil, fmt.Errorf("%w: %s", baxia.ErrSource, reason)
	}
	encoding := strings.ToLower(strings.TrimSpace(strings.Join(response.Header.Values("Content-Encoding"), ",")))
	if encoding != "" && encoding != "identity" && encoding != "gzip" {
		return fail("unsupported script Content-Encoding")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, baxiaMaximumEncodedBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read script: %w", baxia.ErrSource, err)
	}
	if len(raw) > baxiaMaximumEncodedBytes {
		return fail("encoded script exceeds 4 MiB")
	}
	source := raw
	// 某些 CDN 节点返回 gzip 实体却省略 Content-Encoding，按魔数识别。
	if encoding == "gzip" || len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b {
		reader, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("%w: open gzip script: %w", baxia.ErrSource, err)
		}
		source, err = io.ReadAll(io.LimitReader(reader, baxiaMaximumSDKBytes+1))
		_ = reader.Close()
		if err != nil {
			return nil, fmt.Errorf("%w: decode gzip script: %w", baxia.ErrSource, err)
		}
	}
	if len(source) > baxiaMaximumSDKBytes {
		return fail("decoded script exceeds 2 MiB")
	}
	if !utf8.Valid(source) {
		return fail("script is not UTF-8")
	}
	sniff := bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(source), []byte{0xef, 0xbb, 0xbf}))
	contentType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if len(sniff) == 0 || sniff[0] == '<' || contentType == "text/html" || contentType == "application/xhtml+xml" {
		return fail("script is empty or HTML")
	}
	return source, nil
}
