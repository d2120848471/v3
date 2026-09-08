package slider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/application/service"
	"github.com/d2120848471/v3/ali-slider-go/internal/application/solve"
	"github.com/d2120848471/v3/ali-slider-go/internal/bootstrap/config"
)

func TestPublicTypesRedactFormatting(t *testing.T) {
	request := Request{SceneID: "scene-secret", RPCKeyID: "rpc-secret", Proxy: "http://user:password@proxy.invalid"}
	result := Result{SecurityToken: "token-secret", CertifyID: "certify-secret"}
	for _, formatted := range []string{fmt.Sprint(request), fmt.Sprintf("%#v", request), fmt.Sprint(result), fmt.Sprintf("%#v", result)} {
		for _, secret := range []string{"scene-secret", "rpc-secret", "password", "token-secret", "certify-secret"} {
			if strings.Contains(formatted, secret) {
				t.Fatalf("formatted public type leaked %q: %s", secret, formatted)
			}
		}
	}
}

func TestDefaultOptionsPreserveExplicitZeroValues(t *testing.T) {
	options := DefaultClientOptions()
	options.MinimumConfidence = 0
	options.GatherCostMin, options.GatherCostMax = 0, 0
	options.DevicePrewarmCapacity = 0
	options.DeviceSessionReserve = 0
	options.ArtifactDir = t.TempDir()
	client, err := NewClient(options)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if client.options.MinimumConfidence != 0 || client.options.GatherCostMin != 0 || client.options.GatherCostMax != 0 || client.options.DevicePrewarmCapacity != 0 || client.options.DeviceSessionReserve != 0 {
		t.Fatalf("explicit zeros were overwritten: %+v", client.options)
	}
}

func TestZeroOptionsDisableDevicePrewarm(t *testing.T) {
	client, err := NewClient(ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if client.options.DevicePrewarmCapacity != 0 || client.options.DeviceSessionReserve != 0 {
		t.Fatalf("zero options prewarm capacity=%d reserve=%d", client.options.DevicePrewarmCapacity, client.options.DeviceSessionReserve)
	}
	if err := client.Prime(context.Background()); err != nil {
		t.Fatalf("disabled Prime returned error: %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.Prime(canceled); err != nil {
		t.Fatalf("compatibility Prime should stay a no-op: %v", err)
	}
}

type fakeExecutor struct {
	mu        sync.Mutex
	request   solve.Request
	outcome   solve.Outcome
	err       error
	entered   chan struct{}
	release   chan struct{}
	enterOnce sync.Once
}

func (solver *fakeExecutor) Solve(ctx context.Context, request solve.Request) (solve.Outcome, error) {
	solver.mu.Lock()
	solver.request = request
	solver.mu.Unlock()
	if solver.entered != nil {
		solver.enterOnce.Do(func() { close(solver.entered) })
	}
	if solver.release != nil {
		select {
		case <-solver.release:
		case <-ctx.Done():
			return solve.Outcome{}, ctx.Err()
		}
	}
	return solver.outcome, solver.err
}

func newFakeClient(t *testing.T, executor service.Executor) *Client {
	t.Helper()
	options := DefaultClientOptions()
	options.DevicePrewarmCapacity = 0
	application, err := service.New(service.Options{
		Executor: executor, Resources: &fakeResources{},
		DefaultSceneID: options.DefaultSceneID, DefaultPrefix: options.DefaultPrefix,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &Client{options: options, service: application}
}

func TestClientSolveMapsDefaultsAndResult(t *testing.T) {
	engine := &fakeExecutor{outcome: solve.Outcome{
		OK: true, SecurityToken: "secret", VerifyCode: "T001", VerifyResult: true,
		CertifyID: "certify", SceneID: "1ug4aptr", Proxied: true,
		TimingsMS: map[string]int{"total": 37, "vision": 9},
	}}
	client := newFakeClient(t, engine)
	defer client.Close()
	result, err := client.Solve(context.Background(), Request{RPCKeyID: "key", Proxy: "http://proxy.invalid:80"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.ElapsedMS != 37 || result.VerifyCode != "T001" || !result.Proxied {
		t.Fatalf("result=%+v", result)
	}
	engine.mu.Lock()
	received := engine.request
	engine.mu.Unlock()
	if received.SceneID != client.options.DefaultSceneID || received.Prefix != client.options.DefaultPrefix || received.RPCKeyID != "key" || received.Proxy == "" {
		t.Fatalf("request=%+v", received)
	}
	result.TimingsMS["total"] = 99
	if engine.outcome.TimingsMS["total"] != 37 {
		t.Fatal("public result shared mutable timings with engine")
	}
}

func TestClientMapsStableFailureKinds(t *testing.T) {
	tests := []struct {
		failure solve.FailureKind
		want    ErrorKind
	}{
		{solve.FailureInvalidRequest, ErrorInvalidRequest},
		{solve.FailureProtocol, ErrorProtocol},
		{solve.FailureNetwork, ErrorNetwork},
		{solve.FailureVision, ErrorVision},
		{solve.FailureInternal, ErrorInternal},
	}
	for _, test := range tests {
		t.Run(string(test.failure), func(t *testing.T) {
			client := newFakeClient(t, &fakeExecutor{err: &solve.Failure{
				Kind: test.failure, Stage: "fixture", Message: "稳定消息", Cause: errors.New("hidden cause"),
			}})
			defer client.Close()
			_, err := client.Solve(context.Background(), Request{})
			var public *Error
			if !errors.As(err, &public) || public.Kind != test.want || public.Stage != "fixture" || public.Message != "稳定消息" {
				t.Fatalf("error=%#v", err)
			}
		})
	}
}

func TestCloseWaitsForActiveSolveAndRejectsNewWork(t *testing.T) {
	engine := &fakeExecutor{
		outcome: solve.Outcome{TimingsMS: map[string]int{"total": 1}},
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	client := newFakeClient(t, engine)
	solveDone := make(chan error, 1)
	go func() {
		_, err := client.Solve(context.Background(), Request{})
		solveDone <- err
	}()
	<-engine.entered
	closeDone := make(chan struct{})
	go func() {
		client.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
		t.Fatal("Close returned while Solve was active")
	case <-time.After(20 * time.Millisecond):
	}
	close(engine.release)
	if err := <-solveDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("Close did not finish")
	}
	if _, err := client.Solve(context.Background(), Request{}); err == nil {
		t.Fatal("closed Client accepted Solve")
	}
	client.Close()
}

func TestNewClientValidationAndArtifactPurge(t *testing.T) {
	invalid := DefaultClientOptions()
	invalid.DefaultPrefix = "bad-prefix"
	if _, err := NewClient(invalid); err == nil {
		t.Fatal("invalid options accepted")
	}
	invalid = DefaultClientOptions()
	invalid.DeviceSessionReserve = config.MaxDeviceSessionReserve + 1
	if _, err := NewClient(invalid); err == nil {
		t.Fatal("invalid device reserve accepted")
	}
	invalid = DefaultClientOptions()
	invalid.DevicePrewarmCapacity = config.MaxDevicePrewarmCapacity + 1
	if _, err := NewClient(invalid); err == nil {
		t.Fatal("invalid device prewarm accepted")
	}
	invalid = DefaultClientOptions()
	invalid.DeviceSessionReserve = 1
	if _, err := NewClient(invalid); err == nil {
		t.Fatal("device prewarm plus reserve overflow accepted")
	}
	invalid = DefaultClientOptions()
	invalid.DevicePrewarmCapacity = 0
	invalid.DeviceSessionReserve = 1
	if _, err := NewClient(invalid); err == nil {
		t.Fatal("device reserve without prewarm accepted")
	}
	resources := &fakeResources{removed: 1}
	application, err := service.New(service.Options{Executor: &fakeExecutor{}, Resources: resources})
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{service: application}
	removed, err := client.PurgeArtifacts()
	if err != nil || removed != 1 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
	client.Close()
}

type fakeResources struct {
	removed    int
	runtimeErr error
	purgeErr   error
}

func (resources *fakeResources) CheckRuntime() error { return resources.runtimeErr }
func (resources *fakeResources) PurgeArtifacts() (int, error) {
	return resources.removed, resources.purgeErr
}
func (*fakeResources) Close() {}
