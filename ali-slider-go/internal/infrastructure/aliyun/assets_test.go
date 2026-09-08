package aliyun

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/domain/device"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestValidateAssetPath(t *testing.T) {
	for _, valid := range []string{"foo/back.png", "3.29.0/pe.abc.js", "a_b-1.png"} {
		if _, err := ValidateAssetPath(valid, "fixture"); err != nil {
			t.Fatalf("valid path %q: %v", valid, err)
		}
	}
	for _, invalid := range []string{"", "/absolute.png", "../secret", "a/../b", "https://evil.test/a"} {
		if _, err := ValidateAssetPath(invalid, "fixture"); err == nil {
			t.Fatalf("expected invalid path %q", invalid)
		}
	}
}

func TestDownloadAssetsUsesMemoryAndLimits(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Scheme != "https" || request.URL.Hostname() != assetHost {
			t.Fatalf("unexpected URL %s", request.URL)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("png-fixture")),
			Request:    request,
		}, nil
	})
	assets, err := DownloadAssets(context.Background(), transport, device.Profile{}, "http://localhost/", "a.png", "b.png", AssetLimits{MaxBytes: 32})
	if err != nil {
		t.Fatal(err)
	}
	if string(assets.Background) != "png-fixture" || string(assets.Shadow) != "png-fixture" {
		t.Fatalf("unexpected assets: %+v", assets)
	}
	if _, err := DownloadAssets(context.Background(), transport, device.Profile{}, "http://localhost/", "a.png", "b.png", AssetLimits{MaxBytes: 2}); err == nil {
		t.Fatal("expected byte limit error")
	}
}

func TestDownloadAssetsCancelsPeerAfterFirstFailure(t *testing.T) {
	peerCanceled := make(chan struct{})
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if strings.Contains(request.URL.Path, "back") {
			return &http.Response{StatusCode: http.StatusBadGateway, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("failed")), Request: request}, nil
		}
		<-request.Context().Done()
		close(peerCanceled)
		return nil, request.Context().Err()
	})
	started := time.Now()
	_, err := DownloadAssets(context.Background(), transport, device.Profile{}, "http://localhost/", "back.png", "shadow.png", AssetLimits{MaxBytes: 32})
	if err == nil || time.Since(started) > time.Second {
		t.Fatalf("fail-fast download err=%v elapsed=%s", err, time.Since(started))
	}
	select {
	case <-peerCanceled:
	case <-time.After(time.Second):
		t.Fatal("peer download did not observe cancellation")
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("returned peer cancellation instead of first failure: %v", err)
	}
}
