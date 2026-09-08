package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/d2120848471/v3/ali-slider-go/internal/application/solve"
)

func solverErrorResponse(err error) (int, string, string) {
	var failure *solve.Failure
	if errors.As(err, &failure) {
		message := failure.Message
		if message == "" {
			message = "协议运行失败"
		}
		if failure.Kind == solve.FailureInvalidRequest {
			return http.StatusBadRequest, "InvalidRequest", message
		}
		kind := "InternalError"
		switch failure.Kind {
		case solve.FailureProtocol:
			kind = "ProtocolError"
		case solve.FailureNetwork:
			kind = "NetworkError"
		case solve.FailureVision:
			kind = "VisionError"
		}
		return http.StatusInternalServerError, kind, message
	}
	return http.StatusInternalServerError, "UnhandledProtocolError", "未处理的协议运行错误"
}

// solveResponse 属于 HTTP 合同。应用输出不携带 JSON 标签、traceId 或派生耗时。
type solveResponse struct {
	OK            bool           `json:"ok"`
	SecurityToken string         `json:"securityToken"`
	VerifyCode    string         `json:"VerifyCode"`
	VerifyResult  bool           `json:"VerifyResult"`
	CertifyID     string         `json:"certifyId"`
	SceneID       string         `json:"sceneId"`
	Proxied       bool           `json:"proxied"`
	ElapsedMS     int64          `json:"elapsedMs"`
	TimingsMS     map[string]int `json:"timingsMs"`
	TraceID       string         `json:"traceId,omitempty"`
}

func responseFromOutcome(outcome solve.Outcome, traceID string) solveResponse {
	timings := make(map[string]int, len(outcome.TimingsMS))
	for stage, milliseconds := range outcome.TimingsMS {
		timings[stage] = milliseconds
	}
	return solveResponse{
		OK: outcome.OK, SecurityToken: outcome.SecurityToken,
		VerifyCode: outcome.VerifyCode, VerifyResult: outcome.VerifyResult,
		CertifyID: outcome.CertifyID, SceneID: outcome.SceneID, Proxied: outcome.Proxied,
		ElapsedMS: int64(timings["total"]), TimingsMS: timings, TraceID: traceID,
	}
}

func errorBody(errorType, message, traceID string) map[string]any {
	return map[string]any{
		"ok":        false,
		"errorType": errorType,
		"error":     message,
		"traceId":   traceID,
	}
}

func (h *Handler) writeJSON(w http.ResponseWriter, status int, body any, headers map[string]string) {
	payload, err := json.Marshal(body)
	if err != nil {
		status = http.StatusInternalServerError
		payload = []byte(`{"ok":false,"errorType":"InternalError","error":"响应编码失败"}`)
	}
	writeJSONBytes(w, status, payload, headers)
}

func writeJSONBytes(w http.ResponseWriter, status int, payload []byte, headers map[string]string) {
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
