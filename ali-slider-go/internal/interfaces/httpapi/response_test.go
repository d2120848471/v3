package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/d2120848471/v3/ali-slider-go/internal/application/solve"
)

func TestApplicationFailuresKeepHTTPErrorContract(t *testing.T) {
	for _, test := range []struct {
		kind   solve.FailureKind
		status int
		name   string
	}{
		{solve.FailureInvalidRequest, http.StatusBadRequest, "InvalidRequest"},
		{solve.FailureProtocol, http.StatusInternalServerError, "ProtocolError"},
		{solve.FailureNetwork, http.StatusInternalServerError, "NetworkError"},
		{solve.FailureVision, http.StatusInternalServerError, "VisionError"},
		{solve.FailureInternal, http.StatusInternalServerError, "InternalError"},
		{"future", http.StatusInternalServerError, "InternalError"},
	} {
		t.Run(test.name+string(test.kind), func(t *testing.T) {
			handler := newTestHandler(t, solverFunc(func(context.Context, solve.Request) (solve.Outcome, error) {
				return solve.Outcome{}, fmt.Errorf("wrapped: %w", &solve.Failure{Kind: test.kind, Stage: "fixture", Message: "稳定消息", Cause: errors.New("secret cause")})
			}), nil)
			response := performRequest(handler, http.MethodPost, SolvePath, `{}`)
			body := decodeObject(t, response)
			if response.Code != test.status || body["errorType"] != test.name || body["error"] != "稳定消息" || body["ok"] != false {
				t.Fatalf("status=%d body=%#v", response.Code, body)
			}
			if timings, ok := body["timingsMs"].(map[string]any); !ok || len(timings) != 0 {
				t.Fatalf("timings=%#v", body["timingsMs"])
			}
			if response.Header().Get("X-Trace-ID") != body["traceId"] || len(body) != 5 {
				t.Fatalf("error fields=%#v", body)
			}
		})
	}
	status, kind, message := solverErrorResponse(&solve.Failure{Kind: solve.FailureInternal})
	if status != http.StatusInternalServerError || kind != "InternalError" || message != "协议运行失败" {
		t.Fatalf("empty-message mapping=%d %s %s", status, kind, message)
	}
}

func TestHTTPResponseOwnsLegacyJSONAndTimingMap(t *testing.T) {
	outcome := solve.Outcome{
		OK: true, SecurityToken: "token", VerifyCode: "T001", VerifyResult: true,
		CertifyID: "certify", SceneID: "scene", Proxied: true,
		TimingsMS: map[string]int{"total": 27, "vision": 2},
	}
	responseDTO := responseFromOutcome(outcome, "trace")
	responseDTO.TimingsMS["total"] = 99
	if outcome.TimingsMS["total"] != 27 {
		t.Fatal("HTTP response shares mutable timings with executor")
	}
	handler := newTestHandler(t, solverFunc(func(context.Context, solve.Request) (solve.Outcome, error) { return outcome, nil }), nil)
	response := performRequest(handler, http.MethodPost, SolvePath, `{}`)
	body := decodeObject(t, response)
	want := map[string]any{
		"ok": true, "securityToken": "token", "VerifyCode": "T001", "VerifyResult": true,
		"certifyId": "certify", "sceneId": "scene", "proxied": true, "elapsedMs": float64(27),
		"timingsMs": map[string]any{"total": float64(27), "vision": float64(2)},
		"traceId":   response.Header().Get("X-Trace-ID"),
	}
	if !reflect.DeepEqual(body, want) {
		t.Fatalf("response=%#v want=%#v", body, want)
	}
	if responseFromOutcome(solve.Outcome{}, "trace").TimingsMS == nil {
		t.Fatal("nil timings must encode as an empty object")
	}
}
