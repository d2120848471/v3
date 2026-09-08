package contracts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	model "github.com/d2120848471/v3/ali-slider-go/internal/application/solve"
	api "github.com/d2120848471/v3/ali-slider-go/internal/interfaces/httpapi"
)

type executor func(context.Context, model.Request) (model.Outcome, error)

func (run executor) Solve(ctx context.Context, request model.Request) (model.Outcome, error) {
	return run(ctx, request)
}

type requestFields struct {
	SceneID, Prefix, RPCKeyID, Proxy string
}

type exchange struct {
	Status  int
	Headers map[string]string
	Body    map[string]any
	Calls   int
	Request requestFields
}

// golden 来自迁移前 Handler 的实际离线响应，而不是从新实现推导的期望。
func TestHTTPCompatibility(t *testing.T) {
	actual := collectExchanges(t)
	golden, err := os.ReadFile("testdata/http.json")
	if err != nil {
		t.Fatal(err)
	}
	var expected map[string]exchange
	if err := json.Unmarshal(golden, &expected); err != nil {
		t.Fatal(err)
	}
	if len(actual) != len(expected) {
		t.Fatalf("exchange count = %d, want %d", len(actual), len(expected))
	}
	for name, want := range expected {
		if got := actual[name]; !reflect.DeepEqual(got, want) {
			gotJSON, _ := json.MarshalIndent(got, "", "  ")
			wantJSON, _ := json.MarshalIndent(want, "", "  ")
			t.Errorf("%s changed:\nwant %s\ngot %s", name, wantJSON, gotJSON)
		}
	}
}

func collectExchanges(t *testing.T) map[string]exchange {
	t.Helper()
	result := make(map[string]exchange)
	for _, test := range []struct {
		name, method, path, body, mode string
		origin                         bool
	}{
		{name: "empty", method: "POST", path: api.SolvePath},
		{name: "object", method: "POST", path: api.SolvePath, body: `{}`},
		{name: "aliases", method: "POST", path: api.SolvePath, body: `{"sceneId":"场景","Prefix":"pre1","aaduaneId":"fixture-key","Proxy":"proxy.invalid:8080"}`},
		{name: "primary-empty", method: "POST", path: api.SolvePath, body: `{"SceneId":null,"sceneId":"alias","prefix":"","Prefix":"other"}`},
		{name: "body-over-query", method: "POST", path: api.SolvePath + "?SceneId=ignored&prefix=bad-prefix", body: `{"SceneId":"body"}`},
		{name: "unknown-fields", method: "POST", path: api.SolvePath, body: `{"extra":{"x":123},"SceneId":"first","SceneId":"last"}`},
		{name: "legacy", method: "GET", path: api.SolvePath + "?SceneId=first&SceneId=last&sceneId=alias&Proxy=proxy.invalid%3A8080", body: `{"SceneId":"ignored"}`},
		{name: "legacy-utf8", method: "GET", path: api.SolvePath + "?SceneId=%FF%FE"},
		{name: "bad-query", method: "GET", path: api.SolvePath + "?SceneId=%ZZ"},
		{name: "bad-prefix", method: "POST", path: api.SolvePath, body: `{"prefix":"bad-prefix"}`},
		{name: "bad-json-type", method: "POST", path: api.SolvePath, body: `{"SceneId":123}`},
		{name: "null-body", method: "POST", path: api.SolvePath, body: `null`},
		{name: "array-body", method: "POST", path: api.SolvePath, body: `[]`},
		{name: "trailing-body", method: "POST", path: api.SolvePath, body: `{} {}`},
		{name: "business-rejection", method: "POST", path: api.SolvePath, mode: "reject"},
		{name: "invalid-error", method: "POST", path: api.SolvePath, mode: "invalid"},
		{name: "network-error", method: "POST", path: api.SolvePath, mode: "network"},
		{name: "unexpected-error", method: "POST", path: api.SolvePath, mode: "unexpected"},
		{name: "panic", method: "POST", path: api.SolvePath, mode: "panic"},
		{name: "cross-origin-post", method: "POST", path: api.SolvePath, origin: true},
		{name: "cross-origin-get", method: "GET", path: api.SolvePath, origin: true},
		{name: "health", method: "GET", path: api.HealthPath},
		{name: "unknown-route", method: "GET", path: "/missing"},
		{name: "wrong-method", method: "DELETE", path: api.SolvePath},
	} {
		var current exchange
		handler, err := api.New(api.Options{
			DefaultSceneID: "default-scene", DefaultPrefix: "default1",
			Solver: executor(func(_ context.Context, request model.Request) (model.Outcome, error) {
				current.Calls++
				current.Request = requestFields{request.SceneID, request.Prefix, request.RPCKeyID, request.Proxy}
				switch test.mode {
				case "invalid":
					return model.Outcome{}, &model.Failure{Kind: model.FailureInvalidRequest, Message: "参数无效"}
				case "network":
					return model.Outcome{}, &model.Failure{Kind: model.FailureNetwork, Message: "网络失败", Cause: errors.New("fixture-secret")}
				case "unexpected":
					return model.Outcome{}, errors.New("fixture-secret")
				case "panic":
					panic("fixture-secret")
				}
				accepted := test.mode != "reject"
				code := "T001"
				if !accepted {
					code = "F015"
				}
				return model.Outcome{
					OK: accepted, SecurityToken: "fixture-token", VerifyCode: code, VerifyResult: accepted,
					CertifyID: "fixture-certify", SceneID: request.SceneID, Proxied: request.Proxy != "",
					TimingsMS: map[string]int{"total": 37, "vision": 9},
				}, nil
			}),
		})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
		if test.origin {
			request.Header.Set("Origin", "https://outside.invalid")
			request.Header.Set("Sec-Fetch-Site", "cross-site")
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		current.Status = response.Code
		current.Headers = make(map[string]string)
		for _, header := range []string{"Content-Type", "Cache-Control", "X-Content-Type-Options", "Referrer-Policy", "Retry-After"} {
			current.Headers[header] = response.Header().Get(header)
		}
		if err := json.Unmarshal(response.Body.Bytes(), &current.Body); err != nil {
			t.Fatalf("%s: invalid JSON: %v", test.name, err)
		}
		if trace, exists := current.Body["traceId"]; exists {
			if trace == "" || fmt.Sprint(trace) != response.Header().Get("X-Trace-ID") {
				t.Fatalf("%s: trace header/body mismatch", test.name)
			}
			current.Body["traceId"] = "<generated>"
		}
		result[test.name] = current
	}
	return result
}
