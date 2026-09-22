package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/application/service"
)

func (h *Handler) decodeBaxiaRequest(w http.ResponseWriter, r *http.Request) (service.BaxiaRequest, error) {
	payload, err := decodeJSONFields(w, r)
	if err != nil {
		return service.BaxiaRequest{}, err
	}
	readURL := func(name string) (string, error) {
		value, err := optionalAliasedText(payload, name, name, 0)
		if err == nil {
			err = service.ValidateBaxiaURL(value)
		}
		if err != nil {
			return "", fmt.Errorf("%s %w", name, err)
		}
		return value, nil
	}
	pageURL, err := readURL("pageUrl")
	if err != nil {
		return service.BaxiaRequest{}, err
	}
	requestURL, err := readURL("requestUrl")
	if err != nil {
		return service.BaxiaRequest{}, err
	}
	proxy, err := optionalAliasedText(payload, "proxy", "Proxy", 0)
	if err != nil {
		return service.BaxiaRequest{}, fmt.Errorf("proxy %w", err)
	}
	proxy, err = normalizeProxy(proxy)
	if err != nil {
		return service.BaxiaRequest{}, err
	}
	return service.BaxiaRequest{PageURL: pageURL, RequestURL: requestURL, Proxy: proxy}, nil
}

func (h *Handler) handleBaxia(w http.ResponseWriter, r *http.Request, started time.Time) {
	traceID := newTraceID()
	headers := map[string]string{"X-Trace-ID": traceID}
	if checkSolveOrigin(r) != nil {
		h.writeJSON(w, http.StatusForbidden, errorBody("ApiOriginError", "不接受跨源浏览器请求", traceID), headers)
		h.logResult(traceID, http.StatusForbidden, started)
		return
	}
	request, err := h.decodeBaxiaRequest(w, r)
	if err != nil {
		h.writeJSON(w, http.StatusBadRequest, errorBody("ApiRequestError", err.Error(), traceID), headers)
		h.logResult(traceID, http.StatusBadRequest, started)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
	defer cancel()
	outcome, err := callBaxia(ctx, h.baxia, request)
	if err != nil {
		status, kind, message := baxiaErrorResponse(err)
		h.writeJSON(w, status, errorBody(kind, message, traceID), headers)
		h.logResult(traceID, status, started)
		return
	}
	h.writeJSON(w, http.StatusOK, baxiaResponseFromOutcome(outcome, traceID), headers)
	h.logResult(traceID, http.StatusOK, started)
}

func callBaxia(ctx context.Context, executor service.BaxiaExecutor, request service.BaxiaRequest) (outcome service.BaxiaOutcome, err error) {
	defer func() {
		if recover() != nil {
			outcome = service.BaxiaOutcome{}
			err = &service.BaxiaFailure{Kind: service.BaxiaFailureInternal}
		}
	}()
	if executor == nil {
		return service.BaxiaOutcome{}, &service.BaxiaFailure{Kind: service.BaxiaFailureInternal}
	}
	return executor.GenerateBaxia(ctx, request)
}

func baxiaErrorResponse(err error) (int, string, string) {
	var failure *service.BaxiaFailure
	if errors.As(err, &failure) {
		switch failure.Kind {
		case service.BaxiaFailureInvalidRequest:
			return http.StatusBadRequest, "InvalidRequest", "Baxia 请求参数无效"
		case service.BaxiaFailureSource:
			return http.StatusInternalServerError, "BaxiaSourceError", "Baxia SDK 获取失败"
		case service.BaxiaFailureUnsupportedSDK:
			return http.StatusInternalServerError, "BaxiaUnsupportedSDKError", "当前 Baxia SDK 不受支持"
		case service.BaxiaFailureRuntime:
			return http.StatusInternalServerError, "BaxiaRuntimeError", "Baxia 运行失败"
		case service.BaxiaFailureCanceled:
			return http.StatusInternalServerError, "BaxiaCanceled", "Baxia 请求已取消或超过总时限"
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return http.StatusInternalServerError, "BaxiaCanceled", "Baxia 请求已取消或超过总时限"
	}
	return http.StatusInternalServerError, "InternalError", "Baxia 生成失败"
}

type baxiaProfileResponse struct {
	Version  int    `json:"version"`
	Prefix   string `json:"prefix"`
	Alphabet string `json:"alphabet"`
	SDKHash  string `json:"sdkHash"`
}

type baxiaResponse struct {
	OK        bool                 `json:"ok"`
	BXUA      string               `json:"bx-ua"`
	Version   int                  `json:"version"`
	SDKURL    string               `json:"sdkURL"`
	SDKHash   string               `json:"sdkSha256"`
	AWSCHash  string               `json:"awscSha256"`
	Profile   baxiaProfileResponse `json:"profile"`
	UAHeaders map[string]string    `json:"uaHeaders"`
	Proxied   bool                 `json:"proxied"`
	ElapsedMS int64                `json:"elapsedMs"`
	TraceID   string               `json:"traceId"`
}

func baxiaResponseFromOutcome(outcome service.BaxiaOutcome, traceID string) baxiaResponse {
	result := outcome.Result
	return baxiaResponse{
		OK: true, BXUA: result.Token, Version: result.Version,
		SDKURL: result.SDKURL, SDKHash: result.SDKHash, AWSCHash: result.AWSCHash,
		Profile: baxiaProfileResponse{
			Version: result.Profile.Version, Prefix: result.Profile.Prefix,
			Alphabet: result.Profile.Alphabet, SDKHash: result.Profile.SDKHash,
		},
		UAHeaders: outcome.UAHeaders, Proxied: outcome.Proxied,
		ElapsedMS: outcome.Elapsed.Milliseconds(), TraceID: traceID,
	}
}
