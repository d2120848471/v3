package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"sync"
	"testing"

	"github.com/d2120848471/v3/ali-slider-go/internal/application/solve"
)

func TestOpenAPIOperationIDsAndConcurrentReads(t *testing.T) {
	handler := newTestHandler(t, solverFunc(func(context.Context, solve.Request) (solve.Outcome, error) {
		t.Error("OpenAPI must not invoke Solver")
		return solve.Outcome{}, nil
	}), nil)
	response := performRequest(handler, http.MethodGet, OpenAPIPath, "")
	var document struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []struct{ path, method, id string }{
		{TestPagePath, "get", "getTestPage"},
		{SolvePath, "get", "solveSliderLegacy"},
		{SolvePath, "post", "solveSlider"},
		{BaxiaPath, "post", "generateBXUA"},
		{HealthPath, "get", "getHealth"},
		{OpenAPIPath, "get", "getOpenAPI"},
	} {
		if got := document.Paths[operation.path][operation.method].OperationID; got != operation.id {
			t.Errorf("%s %s operationId = %q, want %q", operation.method, operation.path, got, operation.id)
		}
	}

	const readers = 16
	var group sync.WaitGroup
	want := response.Body.String()
	for range readers {
		group.Go(func() {
			got := performRequest(handler, http.MethodGet, OpenAPIPath, "")
			if got.Code != http.StatusOK || got.Body.String() != want || got.Header().Get("Cache-Control") != "no-store" {
				t.Error("concurrent OpenAPI read changed document or response headers")
			}
		})
	}
	group.Wait()
}

func TestBaxiaOpenAPIContract(t *testing.T) {
	handler := newTestHandler(t, solverFunc(func(context.Context, solve.Request) (solve.Outcome, error) {
		t.Error("OpenAPI must not invoke Solver")
		return solve.Outcome{}, nil
	}), nil)
	response := performRequest(handler, http.MethodGet, OpenAPIPath, "")
	var document map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	methods := document["paths"].(map[string]any)[BaxiaPath].(map[string]any)
	if len(methods) != 1 || methods["post"] == nil {
		t.Fatalf("Baxia must only document POST: %#v", methods)
	}
	operation := methods["post"].(map[string]any)
	body := operation["requestBody"].(map[string]any)
	if body["required"] != true {
		t.Fatal("Baxia body must be required")
	}
	request := body["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	if !reflect.DeepEqual(request["required"], []any{"pageUrl", "requestUrl"}) {
		t.Fatalf("required request fields = %#v", request["required"])
	}
	fields := request["properties"].(map[string]any)
	for _, name := range []string{"pageUrl", "requestUrl"} {
		field := fields[name].(map[string]any)
		if field["type"] != "string" || field["format"] != "uri" || field["maxLength"] != float64(8192) {
			t.Errorf("URL schema %s = %#v", name, field)
		}
	}
	for _, name := range []string{"proxy", "Proxy"} {
		field := fields[name].(map[string]any)
		if field["type"] != "string" || field["nullable"] != true {
			t.Errorf("proxy schema %s = %#v", name, field)
		}
	}
	responses := operation["responses"].(map[string]any)
	for _, status := range []string{"200", "400", "403", "500"} {
		if responses[status] == nil {
			t.Fatalf("missing HTTP %s", status)
		}
	}
	success := responses["200"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	properties := success["properties"].(map[string]any)
	required := success["required"].([]any)
	for _, name := range []string{"ok", "bx-ua", "version", "sdkURL", "sdkSha256", "awscSha256", "profile", "uaHeaders", "proxied", "elapsedMs", "traceId"} {
		found := false
		for _, value := range required {
			found = found || value == name
		}
		if !found || properties[name] == nil {
			t.Errorf("missing required response field %s", name)
		}
	}
	if properties["VerifyCode"] != nil || properties["securityToken"] != nil {
		t.Fatal("Baxia generation must not promise a Verify result")
	}
	headers := properties["uaHeaders"].(map[string]any)["properties"].(map[string]any)
	for _, name := range []string{"User-Agent", "Accept-Language", "Sec-CH-UA", "Sec-CH-UA-Mobile", "Sec-CH-UA-Platform"} {
		if headers[name] == nil {
			t.Errorf("missing matching UA header %s", name)
		}
	}
}
