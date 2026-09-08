package aliyun

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/d2120848471/v3/ali-slider-go/internal/application/solve"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/pe"
)

func TestNewRoundFactoryRequiresPEKeyResolver(t *testing.T) {
	_, _, factory := newIntegrationSolver(t, newSolverTransport(t, "gap"), 0.45)
	options := factory.options
	options.PEKeys = nil
	options.GetTransport = func(string) (http.RoundTripper, bool, error) {
		t.Fatal("constructor requested a transport")
		return nil, false, nil
	}
	solver, err := NewRoundFactory(options)
	if solver != nil || err == nil || err.Error() != "solver PE key resolver is required" {
		t.Fatalf("solver=%v error=%v", solver, err)
	}
}

func TestSolverCompatibilityProfileValidationStaysInSetup(t *testing.T) {
	transport := newSolverTransport(t, "gap")
	solver, _, factory := newIntegrationSolver(t, transport, 0.45)
	factory.options.FixedProfile = &device.Profile{}
	_, err := solver.Solve(context.Background(), solve.Request{SceneID: "scene", Prefix: "prefix1"})
	var failure *solve.Failure
	if !errors.As(err, &failure) || failure.Kind != solve.FailureInternal || failure.Stage != "setup" {
		t.Fatalf("error=%v", err)
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if transport.initCount != 0 || transport.verifyCount != 0 || len(transport.deviceActions) != 0 {
		t.Fatalf("invalid profile reached network: init=%d verify=%d actions=%v", transport.initCount, transport.verifyCount, transport.deviceActions)
	}
}

func TestSolverDynamicDeviceOpenFailureStaysInDeviceSession(t *testing.T) {
	transport := newSolverTransport(t, "gap")
	solver, _, factory := newIntegrationSolver(t, transport, 0.45)
	// 动态会话自行提供画像，不能先被未使用的兼容客户端校验拦截。
	factory.options.FixedProfile = &device.Profile{}
	opened := 0
	factory.options.OpenDevice = func(context.Context, http.RoundTripper, device.Profile, solve.Request) (DeviceSession, func(), error) {
		opened++
		return nil, nil, pe.ErrKeyRuntime
	}
	_, err := solver.Solve(context.Background(), solve.Request{SceneID: "scene", Prefix: "prefix1"})
	var failure *solve.Failure
	if !errors.As(err, &failure) || failure.Kind != solve.FailureInternal || failure.Stage != "deviceSession" || !errors.Is(err, pe.ErrKeyRuntime) {
		t.Fatalf("error=%v", err)
	}
	if opened != 1 {
		t.Fatalf("device open calls=%d, want 1", opened)
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if transport.initCount != 0 || transport.verifyCount != 0 || len(transport.deviceActions) != 0 {
		t.Fatalf("failed device open reached network: init=%d verify=%d actions=%v", transport.initCount, transport.verifyCount, transport.deviceActions)
	}
}
