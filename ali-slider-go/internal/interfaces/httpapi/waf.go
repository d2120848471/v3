package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/application/service"
)

func (h *Handler) decodeWAFRequest(w http.ResponseWriter, r *http.Request) (service.WAFRequest, error) {
	payload, err := decodeJSONFields(w, r)
	if err != nil {
		return service.WAFRequest{}, err
	}
	pageURL, err := optionalAliasedText(payload, "pageUrl", "pageUrl", 0)
	if err == nil {
		err = service.ValidateWAFURL(pageURL)
	}
	if err != nil {
		return service.WAFRequest{}, fmt.Errorf("pageUrl %w", err)
	}
	proxy, err := optionalAliasedText(payload, "proxy", "Proxy", 0)
	if err != nil {
		return service.WAFRequest{}, fmt.Errorf("proxy %w", err)
	}
	proxy, err = normalizeProxy(proxy)
	if err != nil {
		return service.WAFRequest{}, err
	}
	return service.WAFRequest{PageURL: pageURL, Proxy: proxy}, nil
}

func (h *Handler) handleWAF(w http.ResponseWriter, r *http.Request, started time.Time) {
	traceID := newTraceID()
	headers := map[string]string{"X-Trace-ID": traceID}
	if checkSolveOrigin(r) != nil {
		h.writeJSON(w, http.StatusForbidden, errorBody("ApiOriginError", "不接受跨源浏览器请求", traceID), headers)
		h.logResult(traceID, http.StatusForbidden, started)
		return
	}
	request, err := h.decodeWAFRequest(w, r)
	if err != nil {
		h.writeJSON(w, http.StatusBadRequest, errorBody("ApiRequestError", err.Error(), traceID), headers)
		h.logResult(traceID, http.StatusBadRequest, started)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
	defer cancel()
	outcome, err := callWAF(ctx, h.waf, request)
	if err != nil {
		status, kind, message := wafErrorResponse(err)
		h.writeJSON(w, status, errorBody(kind, message, traceID), headers)
		h.logResult(traceID, status, started)
		return
	}
	h.writeJSON(w, http.StatusOK, wafResponseFromOutcome(outcome, traceID), headers)
	h.logResult(traceID, http.StatusOK, started)
}

func callWAF(ctx context.Context, executor service.WAFExecutor, request service.WAFRequest) (outcome service.WAFOutcome, err error) {
	defer func() {
		if recover() != nil {
			outcome = service.WAFOutcome{}
			err = &service.WAFFailure{Kind: service.WAFFailureInternal}
		}
	}()
	if executor == nil {
		return service.WAFOutcome{}, &service.WAFFailure{Kind: service.WAFFailureInternal}
	}
	return executor.SolveWAF(ctx, request)
}

func wafErrorResponse(err error) (int, string, string) {
	var failure *service.WAFFailure
	if errors.As(err, &failure) && failure != nil {
		switch failure.Kind {
		case service.WAFFailureInvalidRequest:
			return http.StatusBadRequest, "InvalidRequest", "WAF 请求参数无效"
		case service.WAFFailureSource:
			return http.StatusInternalServerError, "WAFSourceError", "WAF 页面或 SDK 获取失败"
		case service.WAFFailureUnsupported:
			return http.StatusInternalServerError, "WAFUnsupportedError", "当前 WAF 挑战不受支持"
		case service.WAFFailureRuntime:
			return http.StatusInternalServerError, "WAFRuntimeError", "WAF 运行失败"
		case service.WAFFailureCanceled:
			return http.StatusInternalServerError, "WAFCanceled", "WAF 请求已取消或超过总时限"
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return http.StatusInternalServerError, "WAFCanceled", "WAF 请求已取消或超过总时限"
	}
	return http.StatusInternalServerError, "InternalError", "WAF 验证失败"
}

type wafResponse struct {
	OK          bool              `json:"ok"`
	SceneID     string            `json:"sceneId"`
	CaptchaType string            `json:"captchaType"`
	VerifyCode  string            `json:"verifyCode"`
	UAToken     string            `json:"u_atoken"`
	UASig       string            `json:"u_asig"`
	UAHeaders   map[string]string `json:"uaHeaders"`
	Proxied     bool              `json:"proxied"`
	ElapsedMS   int64             `json:"elapsedMs"`
	TraceID     string            `json:"traceId"`
}

func wafResponseFromOutcome(outcome service.WAFOutcome, traceID string) wafResponse {
	return wafResponse{
		OK: outcome.OK, SceneID: outcome.SceneID, CaptchaType: outcome.CaptchaType,
		VerifyCode: outcome.VerifyCode,
		UAToken:    outcome.UAToken, UASig: outcome.UASig, UAHeaders: outcome.UAHeaders,
		Proxied: outcome.Proxied, ElapsedMS: outcome.Elapsed.Milliseconds(), TraceID: traceID,
	}
}
