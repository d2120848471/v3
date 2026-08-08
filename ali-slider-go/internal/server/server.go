// Package server 提供兼容旧版 Python 服务合同的 HTTP 入口。
package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/d2120848471/v3/ali-slider-go/internal/config"
	"github.com/d2120848471/v3/ali-slider-go/pkg/slider"
)

const (
	TestPagePath    = "/"
	SolvePath       = "/api/slider"
	HealthPath      = "/health"
	OpenAPIPath     = "/openapi.json"
	maxRequestBytes = int64(64 * 1024)
)

var browserOriginProtection = http.NewCrossOriginProtection()

// Options 配置 HTTP 层。Solver 是唯一必填项；其余零值使用项目默认值。
type Options struct {
	Solver         slider.Solver
	DefaultSceneID string
	DefaultPrefix  string
	Timeout        time.Duration
	Logger         *log.Logger
}

// Handler 实现滑块 HTTP API。合法请求不做本地并发准入限制，直接进入 Solver。
type Handler struct {
	solver         slider.Solver
	defaultSceneID string
	defaultPrefix  string
	timeout        time.Duration
	logger         *log.Logger
}

// New 构造 HTTP handler。
func New(options Options) (*Handler, error) {
	if options.Solver == nil {
		return nil, errors.New("solver is required")
	}
	defaults := config.Defaults()
	if options.DefaultSceneID == "" {
		options.DefaultSceneID = defaults.SceneID
	}
	if options.DefaultPrefix == "" {
		options.DefaultPrefix = defaults.Prefix
	}
	if options.Timeout == 0 {
		options.Timeout = defaults.Timeout
	}
	if utf8.RuneCountInString(options.DefaultSceneID) > 64 {
		return nil, errors.New("default SceneId exceeds 64 characters")
	}
	if !validPrefix(options.DefaultPrefix) {
		return nil, errors.New("default prefix must contain 1..32 ASCII alphanumeric characters")
	}
	if options.Timeout <= 0 {
		return nil, errors.New("timeout must be positive")
	}
	if options.Logger == nil {
		options.Logger = log.New(io.Discard, "", 0)
	}

	return &Handler{
		solver:         options.Solver,
		defaultSceneID: options.DefaultSceneID,
		defaultPrefix:  options.DefaultPrefix,
		timeout:        options.Timeout,
		logger:         options.Logger,
	}, nil
}

// ServeHTTP 仅暴露冻结合同中的四个入口；GET 解题明确返回 404。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == TestPagePath:
		status := writeTestPage(w)
		h.logResult("", status, started)
	case r.Method == http.MethodPost && r.URL.Path == SolvePath:
		h.handleSolve(w, r, started)
	case r.Method == http.MethodGet && r.URL.Path == HealthPath:
		h.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": "ready"}, nil)
		h.logResult("", http.StatusOK, started)
	case r.Method == http.MethodGet && r.URL.Path == OpenAPIPath:
		h.writeJSON(w, http.StatusOK, openAPIDocument(), nil)
		h.logResult("", http.StatusOK, started)
	default:
		h.writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "未知路径"}, nil)
		h.logResult("", http.StatusNotFound, started)
	}
}

func (h *Handler) handleSolve(w http.ResponseWriter, r *http.Request, started time.Time) {
	traceID := newTraceID()
	headers := map[string]string{"X-Trace-ID": traceID}
	if browserOriginProtection.Check(r) != nil {
		h.writeJSON(w, http.StatusForbidden, errorBody("ApiOriginError", "不接受跨源浏览器请求", traceID), headers)
		h.logResult(traceID, http.StatusForbidden, started)
		return
	}
	request, err := h.decodeRequest(w, r)
	if err != nil {
		h.writeJSON(w, http.StatusBadRequest, errorBody("ApiRequestError", err.Error(), traceID), headers)
		h.logResult(traceID, http.StatusBadRequest, started)
		return
	}

	solveContext, cancelSolve := context.WithTimeout(r.Context(), h.timeout)
	result, solveErr := callSolver(solveContext, h.solver, request)
	cancelSolve()
	if solveErr != nil {
		status, errorType, message := solverErrorResponse(solveErr)
		body := errorBody(errorType, message, traceID)
		body["timingsMs"] = map[string]int{}
		h.writeJSON(w, status, body, headers)
		h.logResult(traceID, status, started)
		return
	}

	result.TraceID = traceID
	if result.TimingsMS == nil {
		result.TimingsMS = map[string]int{}
	}
	h.writeJSON(w, http.StatusOK, result, headers)
	h.logResult(traceID, http.StatusOK, started)
}

func callSolver(ctx context.Context, solver slider.Solver, request slider.Request) (result slider.Result, err error) {
	defer func() {
		if recover() != nil {
			result = slider.Result{}
			err = errors.New("solver panic")
		}
	}()
	return solver.Solve(ctx, request)
}

func solverErrorResponse(err error) (int, string, string) {
	var domainError *slider.Error
	if errors.As(err, &domainError) {
		message := domainError.Message
		if message == "" {
			message = "协议运行失败"
		}
		if domainError.Kind == slider.ErrorInvalidRequest {
			return http.StatusBadRequest, string(domainError.Kind), message
		}
		return http.StatusInternalServerError, string(domainError.Kind), message
	}
	return http.StatusInternalServerError, "UnhandledProtocolError", "未处理的协议运行错误"
}

func errorBody(errorType, message, traceID string) map[string]any {
	return map[string]any{
		"ok":        false,
		"errorType": errorType,
		"error":     message,
		"traceId":   traceID,
	}
}

func (h *Handler) decodeRequest(w http.ResponseWriter, r *http.Request) (slider.Request, error) {
	if r.ContentLength > maxRequestBytes {
		return slider.Request{}, fmt.Errorf("请求体必须位于 0..%d 字节", maxRequestBytes)
	}
	limited := http.MaxBytesReader(w, r.Body, maxRequestBytes)
	defer limited.Close()

	var payload map[string]json.RawMessage
	decoder := json.NewDecoder(limited)
	err := decoder.Decode(&payload)
	if errors.Is(err, io.EOF) {
		payload = map[string]json.RawMessage{}
	} else if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return slider.Request{}, fmt.Errorf("请求体必须位于 0..%d 字节", maxRequestBytes)
		}
		return slider.Request{}, errors.New("请求体不是合法 JSON")
	}
	if payload == nil {
		return slider.Request{}, errors.New("请求体必须是 JSON object")
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return slider.Request{}, err
	}

	sceneID, err := optionalAliasedText(payload, "SceneId", "sceneId", 64)
	if err != nil {
		return slider.Request{}, fmt.Errorf("SceneId %w", err)
	}
	rpcKeyID, err := optionalAliasedText(payload, "AaduaneId", "aaduaneId", 128)
	if err != nil {
		return slider.Request{}, fmt.Errorf("AaduaneId %w", err)
	}
	prefix, err := optionalAliasedText(payload, "prefix", "Prefix", 32)
	if err != nil {
		return slider.Request{}, fmt.Errorf("prefix %w", err)
	}
	if prefix != "" && !validPrefix(prefix) {
		return slider.Request{}, errors.New("prefix 只能是 ASCII 字母数字")
	}
	proxy, err := optionalAliasedText(payload, "proxy", "Proxy", 0)
	if err != nil {
		return slider.Request{}, fmt.Errorf("proxy %w", err)
	}
	proxy, err = normalizeProxy(proxy)
	if err != nil {
		return slider.Request{}, err
	}
	if sceneID == "" {
		sceneID = h.defaultSceneID
	}
	if prefix == "" {
		prefix = h.defaultPrefix
	}
	return slider.Request{SceneID: sceneID, Prefix: prefix, RPCKeyID: rpcKeyID, Proxy: proxy}, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra json.RawMessage
	if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return errors.New("请求体不是合法 JSON")
	}
	return errors.New("请求体只能包含一个 JSON object")
}

func optionalAliasedText(payload map[string]json.RawMessage, primary, alias string, maxRunes int) (string, error) {
	raw, found := payload[primary]
	if !found {
		raw, found = payload[alias]
	}
	if !found || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", errors.New("必须是字符串")
	}
	value = strings.TrimSpace(value)
	if maxRunes > 0 && utf8.RuneCountInString(value) > maxRunes {
		return "", fmt.Errorf("超过 %d 字符", maxRunes)
	}
	return value, nil
}

func validPrefix(value string) bool {
	if value == "" || len(value) > 32 {
		return false
	}
	for _, character := range value {
		if character > 127 || !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9')) {
			return false
		}
	}
	return true
}

func normalizeProxy(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if !strings.Contains(value, "://") {
		value = "http://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", errors.New("proxy 格式无效")
	}
	scheme := strings.ToLower(parsed.Scheme)
	switch scheme {
	case "http", "https", "socks4", "socks5", "socks5h":
	default:
		return "", errors.New("proxy scheme 必须是 http/https/socks4/socks5/socks5h")
	}
	if parsed.Hostname() == "" {
		return "", errors.New("proxy 缺少主机名")
	}
	if parsed.Opaque != "" || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("proxy 不能包含 path、query 或 fragment")
	}
	if port := parsed.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return "", errors.New("proxy 端口必须位于 1..65535")
		}
	}
	if parsed.User != nil {
		username := parsed.User.Username()
		password, hasPassword := parsed.User.Password()
		switch scheme {
		case "socks4":
			if hasPassword || len(username) > 255 || strings.ContainsRune(username, '\x00') {
				return "", errors.New("socks4 proxy userinfo 只能包含合法 username")
			}
		case "socks5", "socks5h":
			if !hasPassword || len(username) < 1 || len(username) > 255 || len(password) < 1 || len(password) > 255 {
				return "", errors.New("socks5 proxy userinfo 必须包含 1..255 字节的 username/password")
			}
		}
	}
	parsed.Scheme = scheme
	return parsed.String(), nil
}

func (h *Handler) writeJSON(w http.ResponseWriter, status int, body any, headers map[string]string) {
	payload, err := json.Marshal(body)
	if err != nil {
		status = http.StatusInternalServerError
		payload = []byte(`{"ok":false,"errorType":"InternalError","error":"响应编码失败"}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	setPrivateResponseHeaders(w.Header())
	for name, value := range headers {
		w.Header().Set(name, value)
	}
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

func setPrivateResponseHeaders(header http.Header) {
	header.Set("Cache-Control", "no-store")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Referrer-Policy", "no-referrer")
}

func (h *Handler) logResult(traceID string, status int, started time.Time) {
	// 不记录请求参数、错误正文或响应业务字段，避免 token、CertifyId 和原始正文落盘。
	if traceID == "" {
		h.logger.Printf("status=%d elapsedMs=%d", status, time.Since(started).Milliseconds())
		return
	}
	h.logger.Printf("traceId=%s status=%d elapsedMs=%d", traceID, status, time.Since(started).Milliseconds())
}

func newTraceID() string {
	buffer := make([]byte, 6)
	if _, err := rand.Read(buffer); err != nil {
		return fmt.Sprintf("%012x", time.Now().UnixNano()&0xffffffffffff)
	}
	return hex.EncodeToString(buffer)
}
