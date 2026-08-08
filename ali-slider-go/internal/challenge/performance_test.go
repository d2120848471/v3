package challenge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/pe"
	"github.com/d2120848471/v3/ali-slider-go/internal/runtimekit"
	"github.com/d2120848471/v3/ali-slider-go/internal/track"
	"github.com/d2120848471/v3/ali-slider-go/internal/vision"
)

func TestSolverOffline32ConcurrentSuccess(t *testing.T) {
	transport := newSolverTransport(t, "gap")
	// 该用例验证 32 路完整离线链的正确性，不承担延迟门槛。Race 插桩会在
	// GitHub hosted ARM64 上显著放大视觉耗时，因此使用独立的有界测试时限。
	solver, artifactDirectory := newIntegrationSolverWithTimeout(t, transport, 0.45, 30*time.Second)

	const concurrency = 32
	var group sync.WaitGroup
	errorsByWorker := make(chan error, concurrency)
	for worker := range concurrency {
		group.Add(1)
		go func() {
			defer group.Done()
			outcome, err := solver.Solve(context.Background(), SolveRequest{
				SceneID: fmt.Sprintf("scene-%02d", worker), Prefix: "prefix1",
			})
			if err != nil {
				errorsByWorker <- fmt.Errorf("worker %d: %w", worker, err)
				return
			}
			if !outcome.OK || outcome.VerifyCode != "T001" || !outcome.VerifyResult || outcome.SecurityToken == "" {
				errorsByWorker <- fmt.Errorf("worker %d: unsuccessful outcome", worker)
			}
		}()
	}
	group.Wait()
	close(errorsByWorker)
	for err := range errorsByWorker {
		t.Error(err)
	}

	transport.mu.Lock()
	initCount, verifyCount, deviceActionCount := transport.initCount, transport.verifyCount, len(transport.deviceActions)
	transport.mu.Unlock()
	if initCount != concurrency || verifyCount != concurrency || deviceActionCount != concurrency*4 {
		t.Fatalf("init=%d verify=%d deviceActions=%d", initCount, verifyCount, deviceActionCount)
	}
	if entries, err := os.ReadDir(artifactDirectory); err == nil && len(entries) != 0 {
		t.Fatalf("successful concurrent run wrote %d artifacts", len(entries))
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

// TestPureComputeP99 用 nearest-rank 口径测量“已下载图片 -> Vision -> Track -> PE/Data”。
// 普通 CI 不跑时钟硬断言；只有显式 ALI_SLIDER_PERF=1 才执行 200 样本。
func TestPureComputeP99(t *testing.T) {
	if os.Getenv("ALI_SLIDER_PERF") != "1" {
		t.Skip("set ALI_SLIDER_PERF=1 to run the 200-sample local performance gate")
	}
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("the <=100ms hard gate is calibrated for the declared darwin/arm64 baseline")
	}

	background, err := os.ReadFile(filepath.Join("..", "vision", "testdata", "python-edge-decoy", "gap", "back.png"))
	if err != nil {
		t.Fatal(err)
	}
	shadow, err := os.ReadFile(filepath.Join("..", "vision", "testdata", "python-edge-decoy", "gap", "shadow.png"))
	if err != nil {
		t.Fatal(err)
	}
	entropy := &byteEntropy{}
	clock := &solverClock{now: time.UnixMilli(2_000_000_000_000)}
	sources := runtimekit.Sources{Clock: clock, Entropy: entropy}
	profile, err := device.GenerateProfile(entropy)
	if err != nil {
		t.Fatal(err)
	}

	const samples = 200
	durations := make([]time.Duration, 0, samples)
	for sample := range samples {
		started := time.Now()
		estimate, solveErr := vision.Solve(context.Background(), background, shadow, vision.Limits{
			MaxBytes: 8 << 20, MaxDimension: 16_384, MaxPixels: 16 << 20,
		})
		if solveErr != nil {
			t.Fatalf("sample %d vision: %v", sample, solveErr)
		}
		trackValue, trackErr := track.LoadDefault(float64(estimate.SlidePos), entropy)
		if trackErr != nil {
			t.Fatalf("sample %d track: %v", sample, trackErr)
		}
		expectedX := estimate.XPos
		_, buildErr := (pe.Builder{Profile: profile, Sources: sources}).Build(context.Background(), pe.Input{
			SceneID: "1ug4aptr", CertifyID: fmt.Sprintf("perf-%03d", sample),
			StaticPath: "3.29.0/pe.091.00665af58b020d81.js", ExpectedXPos: &expectedX,
			InitBeginTimeMS: clock.Now().UnixMilli(), FirstTouchAgeMS: 700, Track: trackValue,
		})
		if buildErr != nil {
			t.Fatalf("sample %d PE: %v", sample, buildErr)
		}
		durations = append(durations, time.Since(started))
	}
	sort.Slice(durations, func(left, right int) bool { return durations[left] < durations[right] })
	p50, p95, p99 := durations[99], durations[189], durations[197]
	t.Logf("pure-compute samples=%d p50=%s p95=%s p99=%s max=%s", samples, p50, p95, p99, durations[len(durations)-1])
	if p99 > 100*time.Millisecond {
		t.Fatalf("pure-compute P99=%s exceeds 100ms hard gate", p99)
	}
}
