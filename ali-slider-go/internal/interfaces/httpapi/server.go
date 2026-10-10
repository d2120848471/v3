// Package httpapi 将应用求解合同适配为 HTTP API。
package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/d2120848471/v3/ali-slider-go/internal/application/service"
	"github.com/d2120848471/v3/ali-slider-go/internal/application/solve"
)

const (
	TestPagePath = "/"
	SolvePath    = "/api/slider"
	BaxiaPath    = "/api/bxua"
	WAFPath      = "/api/waf"
	HealthPath   = "/health"
	OpenAPIPath  = "/openapi.json"
	// MaxRequestBytes 是 POST body 与 legacy GET raw query 的共享上限。
	MaxRequestBytes = int64(64 * 1024)
)

var browserOriginProtection = http.NewCrossOriginProtection()

// Options 配置 HTTP 层。Solver 是唯一必填项；其余零值使用项目默认值。
type Options struct {
	Solver         service.Executor
	Baxia          service.BaxiaExecutor
	WAF            service.WAFExecutor
	DefaultSceneID string
	DefaultPrefix  string
	Timeout        time.Duration
	Logger         *log.Logger
}

// Handler 实现滑块 HTTP API。合法请求不做本地并发准入限制，直接进入 Solver。
type Handler struct {
	solver         service.Executor
	baxia          service.BaxiaExecutor
	waf            service.WAFExecutor
	defaultSceneID string
	defaultPrefix  string
	timeout        time.Duration
	logger         *log.Logger
	openAPI        []byte
}

// New 构造 HTTP handler。
func New(options Options) (*Handler, error) {
	if options.Solver == nil {
		return nil, errors.New("solver is required")
	}
	if options.DefaultSceneID == "" {
		options.DefaultSceneID = service.DefaultSceneID
	}
	if options.DefaultPrefix == "" {
		options.DefaultPrefix = service.DefaultPrefix
	}
	if options.Timeout == 0 {
		options.Timeout = service.DefaultTimeout
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

	// 合同与请求无关，在构造时编码一次，避免每次读取重建嵌套 map。
	document, err := json.Marshal(openAPIDocument())
	if err != nil {
		return nil, fmt.Errorf("encode OpenAPI document: %w", err)
	}

	return &Handler{
		solver:         options.Solver,
		baxia:          options.Baxia,
		waf:            options.WAF,
		defaultSceneID: options.DefaultSceneID,
		defaultPrefix:  options.DefaultPrefix,
		timeout:        options.Timeout,
		logger:         options.Logger,
		openAPI:        document,
	}, nil
}

// ServeHTTP 仅暴露约定路径；Solve 兼容旧 GET query，Baxia 和 WAF 只接受 POST JSON。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == TestPagePath:
		status := writeTestPage(w)
		h.logResult("", status, started)
	case (r.Method == http.MethodGet || r.Method == http.MethodPost) && r.URL.Path == SolvePath:
		h.handleSolve(w, r, started)
	case r.Method == http.MethodPost && r.URL.Path == BaxiaPath:
		h.handleBaxia(w, r, started)
	case r.Method == http.MethodPost && r.URL.Path == WAFPath:
		h.handleWAF(w, r, started)
	case r.Method == http.MethodGet && r.URL.Path == HealthPath:
		h.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": "ready"}, nil)
		h.logResult("", http.StatusOK, started)
	case r.Method == http.MethodGet && r.URL.Path == OpenAPIPath:
		writeJSONBytes(w, http.StatusOK, h.openAPI, nil)
		h.logResult("", http.StatusOK, started)
	default:
		h.writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "未知路径"}, nil)
		h.logResult("", http.StatusNotFound, started)
	}
}

func (h *Handler) handleSolve(w http.ResponseWriter, r *http.Request, started time.Time) {
	traceID := newTraceID()
	headers := map[string]string{"X-Trace-ID": traceID}
	if checkSolveOrigin(r) != nil {
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

	h.writeJSON(w, http.StatusOK, responseFromOutcome(result, traceID), headers)
	h.logResult(traceID, http.StatusOK, started)
}

func checkSolveOrigin(r *http.Request) error {
	if r.Method != http.MethodGet {
		return browserOriginProtection.Check(r)
	}
	// 旧 GET 会发起真实上游求解，不是安全读操作。仅在同源检查副本中将它
	// 视为 POST，从而复用 Go 标准库边界；原请求的路由和 query 语义不变。
	originProbe := new(http.Request)
	*originProbe = *r
	originProbe.Method = http.MethodPost
	return browserOriginProtection.Check(originProbe)
}

func callSolver(ctx context.Context, solver service.Executor, request solve.Request) (result solve.Outcome, err error) {
	defer func() {
		if recover() != nil {
			result = solve.Outcome{}
			err = errors.New("solver panic")
		}
	}()
	return solver.Solve(ctx, request)
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
