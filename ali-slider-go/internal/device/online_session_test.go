//go:build online

package device

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/runtimekit"
)

// TestOnlineDeviceSession 只验证 Device Log1/Log2/Log3，不调用 Captcha Init/Verify。
// 必须同时显式给出 online build tag 和环境开关，避免 CI 意外访问真实目标。
func TestOnlineDeviceSession(t *testing.T) {
	if os.Getenv("ALI_SLIDER_ONLINE_DEVICE") != "1" {
		t.Skip("explicit ALI_SLIDER_ONLINE_DEVICE=1 authorization is required")
	}
	sources := runtimekit.NewSystemSources()
	profile, err := GenerateProfile(sources.Entropy)
	if err != nil {
		t.Fatal("generate online device profile failed")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	client, err := NewClient(DefaultClientOptions(profile, sources), transport)
	if err != nil {
		t.Fatal("create online device client failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	session, err := client.Open(ctx)
	if err != nil {
		t.Fatalf("online device session failed: %v", err)
	}
	defer session.Close()
	result, err := session.InitialResult()
	if err != nil {
		t.Fatal("read online device result failed")
	}
	if result.RequestCount != 3 {
		t.Fatalf("online device action count=%d, want 3", result.RequestCount)
	}
	t.Log("ONLINE_DEVICE actions=3 fingerprintFields=111")
}
