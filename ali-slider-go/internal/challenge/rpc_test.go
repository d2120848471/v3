package challenge

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/runtimekit"
)

type fixedClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *fixedClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	value := clock.now
	clock.now = clock.now.Add(time.Millisecond)
	return value
}

func (clock *fixedClock) Sleep(context.Context, time.Duration) error { return nil }

type byteEntropy struct {
	mu   sync.Mutex
	next byte
}

func (entropy *byteEntropy) Read(buffer []byte) (int, error) {
	entropy.mu.Lock()
	defer entropy.mu.Unlock()
	for index := range buffer {
		buffer[index] = entropy.next
		entropy.next++
	}
	return len(buffer), nil
}

func (entropy *byteEntropy) Uint64n(limit uint64) (uint64, error) { return 0, nil }

func TestCaptchaRPCInitAndSingleVerify(t *testing.T) {
	var mu sync.Mutex
	actions := make([]string, 0, 2)
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		content, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		values, err := url.ParseQuery(string(content))
		if err != nil {
			return nil, err
		}
		action := values.Get("Action")
		mu.Lock()
		actions = append(actions, action)
		mu.Unlock()
		if values.Get("Signature") == "" || values.Get("Timestamp") == "" || values.Get("SignatureNonce") == "" {
			t.Errorf("missing signed fields: %s", content)
		}
		response := `{"Success":true,"Code":"Success","CertifyId":"fixture-certify","Image":"a/back.png","PuzzleImage":"a/shadow.png","StaticPath":"3.29.0/pe.091.00665af58b020d81.js","CaptchaType":"slider"}`
		if action == "VerifyCaptchaV3" {
			if request.GetBody != nil {
				t.Error("Verify request exposed a replayable GetBody")
			}
			if request.ContentLength != int64(len(content)) {
				t.Errorf("Verify ContentLength=%d, want %d", request.ContentLength, len(content))
			}
			if values.Get("CertifyId") != "fixture-certify" || !strings.Contains(values.Get("CaptchaVerifyParam"), `"deviceToken":"verify-token"`) {
				t.Errorf("invalid Verify form")
			}
			response = `{"Code":"Success","Result":{"VerifyCode":"T001","VerifyResult":true,"securityToken":"fixture-security","certifyId":"fixture-certify"}}`
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response)), Request: request}, nil
	})
	clock := &fixedClock{now: time.UnixMilli(1_700_000_000_000)}
	client, err := NewRPCClient(RPCOptions{
		Transport: transport, Profile: device.Profile{}, Sources: runtimekit.Sources{Clock: clock, Entropy: &byteEntropy{}},
		SceneID: "1ug4aptr", Prefix: "fsgtmi",
	})
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := client.Init(context.Background(), "init-token")
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Verify(context.Background(), challenge, "verify-token", "fixture-data", 1_700_000_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Succeeded() || result.CertifyID != "fixture-certify" {
		t.Fatalf("result=%+v", result)
	}
	if _, err := client.Verify(context.Background(), challenge, "verify-token", "fixture-data", 1_700_000_000_000); err == nil {
		t.Fatal("second Verify was accepted")
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(actions, ",") != "InitCaptchaV3,VerifyCaptchaV3" {
		t.Fatalf("actions=%v", actions)
	}
}

func TestVerifyNetworkErrorStillConsumesAttempt(t *testing.T) {
	var calls int
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(
				`{"Success":true,"Code":"Success","CertifyId":"fixture","Image":"a.png","PuzzleImage":"b.png","StaticPath":"s.js"}`,
			)), Request: request}, nil
		}
		return nil, errors.New("upstream detail")
	})
	client, err := NewRPCClient(RPCOptions{
		Transport: transport, Profile: device.Profile{}, Sources: runtimekit.Sources{
			Clock: &fixedClock{now: time.UnixMilli(1_700_000_000_000)}, Entropy: &byteEntropy{},
		}, SceneID: "scene", Prefix: "prefix",
	})
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := client.Init(context.Background(), "init-token")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Verify(context.Background(), challenge, "token", "data", 1_700_000_000_000); err == nil || strings.Contains(err.Error(), "upstream detail") {
		t.Fatalf("unexpected first error: %v", err)
	}
	if _, err := client.Verify(context.Background(), challenge, "token", "data", 1_700_000_000_000); err == nil {
		t.Fatal("second Verify was accepted")
	}
	if calls != 2 {
		t.Fatalf("round trips=%d", calls)
	}
}

func TestVerifyCompatibilityUsesNestedResultAndCertifyIDFallback(t *testing.T) {
	calls := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		response := `{"Success":true,"Code":"Success","CertifyId":"fixture","Image":"a.png","PuzzleImage":"b.png","StaticPath":"s.js"}`
		if calls == 2 {
			response = `{"Code":"top-level-not-authoritative","Result":{"VerifyCode":"T001","VerifyResult":true,"securityToken":"fixture-security"}}`
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response)), Request: request}, nil
	})
	client, err := NewRPCClient(RPCOptions{
		Transport: transport, Profile: device.Profile{}, Sources: runtimekit.Sources{
			Clock: &fixedClock{now: time.UnixMilli(1_700_000_000_000)}, Entropy: &byteEntropy{},
		}, SceneID: "scene", Prefix: "prefix",
	})
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := client.Init(context.Background(), "init-token")
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Verify(context.Background(), challenge, "token", "data", 1_700_000_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Succeeded() || result.CertifyID != challenge.CertifyID {
		t.Fatalf("compatibility result=%+v", result)
	}
}

func TestRPCInitAcceptsImageLessTracelessOnly(t *testing.T) {
	newClient := func(captchaType string) *RPCClient {
		t.Helper()
		client, err := NewRPCClient(RPCOptions{
			Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				response := `{"Success":true,"Code":"Success","CertifyId":"fixture-traceless","StaticPath":"3.29.0/pe.091.fixture.js","CaptchaType":"` + captchaType + `"}`
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response)), Request: request}, nil
			}),
			Sources: runtimekit.Sources{
				Clock: &fixedClock{now: time.UnixMilli(1_700_000_000_000)}, Entropy: &byteEntropy{},
			}, SceneID: "scene", Prefix: "prefix",
		})
		if err != nil {
			t.Fatal(err)
		}
		return client
	}

	challenge, err := newClient("traceless").Init(context.Background(), "init-token")
	if err != nil {
		t.Fatal(err)
	}
	if challenge.CaptchaType != "traceless" || challenge.ImagePath != "" || challenge.PuzzleImagePath != "" || challenge.StaticPath == "" {
		t.Fatalf("challenge=%+v", challenge)
	}
	if _, err := newClient("slider").Init(context.Background(), "init-token"); err == nil {
		t.Fatal("image-less slider challenge was accepted")
	}
}

func TestRPCBindsVerifyToSingleInit(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(
			`{"Success":true,"Code":"Success","CertifyId":"issued","Image":"a.png","PuzzleImage":"b.png","StaticPath":"s.js"}`,
		)), Request: request}, nil
	})
	client, err := NewRPCClient(RPCOptions{
		Transport: transport, Sources: runtimekit.Sources{
			Clock: &fixedClock{now: time.UnixMilli(1_700_000_000_000)}, Entropy: &byteEntropy{},
		}, SceneID: "scene", Prefix: "prefix",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Verify(context.Background(), CaptchaChallenge{CertifyID: "issued"}, "token", "data", 1_700_000_000_000); err == nil {
		t.Fatal("Verify without Init was accepted")
	}
	issued, err := client.Init(context.Background(), "init-token")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Init(context.Background(), "init-token"); err == nil {
		t.Fatal("second Init was accepted")
	}
	if _, err := client.Verify(context.Background(), CaptchaChallenge{CertifyID: issued.CertifyID + "-other"}, "token", "data", 1_700_000_000_000); err == nil {
		t.Fatal("Verify for another CertifyId was accepted")
	}
}

func TestRPCMalformedSuccessResponseIsProtocolNotNetwork(t *testing.T) {
	client, err := NewRPCClient(RPCOptions{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"Success":`)), Request: request}, nil
		}),
		Sources: runtimekit.Sources{Clock: &fixedClock{now: time.Now()}, Entropy: &byteEntropy{}}, SceneID: "scene", Prefix: "prefix",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Init(context.Background(), "token")
	var failure networkFailure
	if err == nil || (errors.As(err, &failure) && failure.NetworkFailure()) {
		t.Fatalf("malformed HTTP 200 was not a protocol failure: %v", err)
	}
}

func TestRPCValidation(t *testing.T) {
	sources := runtimekit.Sources{Clock: &fixedClock{now: time.Now()}, Entropy: &byteEntropy{}}
	if _, err := NewRPCClient(RPCOptions{Transport: roundTripFunc(nil), Sources: sources, SceneID: "scene", Prefix: "bad-prefix"}); err == nil {
		t.Fatal("invalid prefix accepted")
	}
	client, err := NewRPCClient(RPCOptions{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewBufferString(`{"Success":true,"Code":"Success","CertifyId":"c","Image":"../bad","PuzzleImage":"p.png","StaticPath":"s.js"}`))}, nil
	}), Sources: sources, SceneID: "scene", Prefix: "prefix"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Init(context.Background(), "token"); err == nil {
		t.Fatal("unsafe Init path accepted")
	}
}
