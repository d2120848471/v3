package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/interfaces/httpapi"
)

func TestServeInjectsWAFWithConfiguredTimeoutWithoutNetworkOrLibraryLoad(t *testing.T) {
	address, _, stop := startBaxiaTestServer(t, time.Nanosecond)
	response, err := (&http.Client{Timeout: time.Second}).Post(
		address+httpapi.WAFPath, "application/json", strings.NewReader(`{"pageUrl":"https://page.example.com/"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	stop()
	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusInternalServerError || result["errorType"] != "WAFCanceled" {
		t.Fatalf("production WAF not injected with configured timeout: status=%d body=%s", response.StatusCode, body)
	}
}
