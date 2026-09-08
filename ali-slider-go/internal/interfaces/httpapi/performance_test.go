package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/d2120848471/v3/ali-slider-go/internal/application/solve"
)

// BenchmarkHTTP 测量真实 Handler 的解析和响应开销；Solver 固定离线结果，
// 因此数字不包含 V8、图像计算或上游网络，不代表完整 Solve 延迟。
func BenchmarkHTTP(b *testing.B) {
	handler, err := New(Options{Solver: solverFunc(func(_ context.Context, request solve.Request) (solve.Outcome, error) {
		return solve.Outcome{OK: true, SceneID: request.SceneID, TimingsMS: map[string]int{}}, nil
	})})
	if err != nil {
		b.Fatal(err)
	}
	for _, test := range []struct {
		name, method, path, body string
	}{
		{"PostSolve", http.MethodPost, SolvePath, `{"SceneId":"scene1","prefix":"prefix1","proxy":"proxy.invalid:8080"}`},
		{"LegacyQuery", http.MethodGet, SolvePath + "?SceneId=scene1&prefix=prefix1&proxy=proxy.invalid%3A8080", ""},
		{"OpenAPI", http.MethodGet, OpenAPIPath, ""},
	} {
		b.Run(test.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if response.Code != http.StatusOK {
					b.Fatalf("status = %d", response.Code)
				}
			}
		})
	}
}
