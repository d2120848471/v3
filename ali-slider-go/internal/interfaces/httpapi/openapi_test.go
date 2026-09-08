package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
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
