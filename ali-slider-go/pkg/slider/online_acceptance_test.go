//go:build online

package slider_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/pkg/slider"
)

type onlineAcceptanceSummary struct {
	Attempts         int            `json:"attempts"`
	Concurrency      int            `json:"concurrency"`
	Success          int            `json:"success"`
	BusinessFailure  int            `json:"businessFailure"`
	ErrorsByKind     map[string]int `json:"errorsByKind"`
	SuccessRate      float64        `json:"successRate"`
	WallP50MS        int64          `json:"wallP50Ms"`
	WallP95MS        int64          `json:"wallP95Ms"`
	WallP99MS        int64          `json:"wallP99Ms"`
	WallMaxMS        int64          `json:"wallMaxMs"`
	SuccessWallP95MS int64          `json:"successWallP95Ms"`
	UniqueVerifyRule string         `json:"uniqueVerifyRule"`
	RetryPolicy      string         `json:"retryPolicy"`
}

// TestOnlineAcceptance 仅在显式 online build tag 和环境开关同时存在时访问真实目标。
// 每个 job 只调用一次 Solve；本测试不含任何重试分支。
func TestOnlineAcceptance(t *testing.T) {
	if os.Getenv("ALI_SLIDER_ONLINE") != "1" {
		t.Skip("explicit ALI_SLIDER_ONLINE=1 authorization is required")
	}
	attempts := acceptanceInteger(t, "ALI_SLIDER_ONLINE_COUNT", 0)
	concurrency := acceptanceInteger(t, "ALI_SLIDER_ONLINE_CONCURRENCY", 0)
	if attempts < 1 || attempts > 200 {
		t.Fatalf("ALI_SLIDER_ONLINE_COUNT must be within 1..200")
	}
	if concurrency < 1 || concurrency > 32 || concurrency > attempts {
		t.Fatalf("ALI_SLIDER_ONLINE_CONCURRENCY must be within 1..min(32,count)")
	}
	if attempts == 200 && concurrency != 32 {
		t.Fatalf("formal 200-attempt acceptance requires concurrency=32")
	}
	artifactDirectory := os.Getenv("ALI_SLIDER_ONLINE_ARTIFACT_DIR")
	if artifactDirectory == "" {
		t.Fatal("ALI_SLIDER_ONLINE_ARTIFACT_DIR is required")
	}

	options := slider.DefaultClientOptions()
	options.MaxConcurrency = concurrency
	options.DevicePrewarmCapacity = concurrency
	options.ArtifactDir = artifactDirectory
	if sceneID := os.Getenv("ALI_SLIDER_ONLINE_SCENE_ID"); sceneID != "" {
		options.DefaultSceneID = sceneID
	}
	if prefix := os.Getenv("ALI_SLIDER_ONLINE_PREFIX"); prefix != "" {
		options.DefaultPrefix = prefix
	}
	client, err := slider.NewClient(options)
	if err != nil {
		t.Fatal("create online Client failed")
	}
	defer client.Close()
	primeContext, cancelPrime := context.WithTimeout(context.Background(), options.Timeout)
	err = client.Prime(primeContext)
	cancelPrime()
	if err != nil {
		var domainError *slider.Error
		if errors.As(err, &domainError) && domainError.Cause != nil {
			t.Fatalf("device prewarm failed before challenge dispatch: kind=%s cause=%v", domainError.Kind, domainError.Cause)
		}
		t.Fatal("device prewarm failed before challenge dispatch")
	}

	type attemptResult struct {
		success  bool
		business bool
		kind     string
		wall     time.Duration
	}
	jobs := make(chan int)
	results := make(chan attemptResult, attempts)
	var workers sync.WaitGroup
	for range concurrency {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range jobs {
				started := time.Now()
				result, solveErr := client.Solve(context.Background(), slider.Request{
					RPCKeyID: os.Getenv("ALI_SLIDER_ONLINE_RPC_KEY_ID"),
					Proxy:    os.Getenv("ALI_SLIDER_ONLINE_PROXY"),
				})
				item := attemptResult{wall: time.Since(started)}
				if solveErr != nil {
					var domainError *slider.Error
					if errors.As(solveErr, &domainError) {
						item.kind = string(domainError.Kind)
					} else {
						item.kind = "UnhandledError"
					}
				} else if result.OK && result.VerifyCode == "T001" && result.VerifyResult && result.SecurityToken != "" {
					item.success = true
				} else {
					item.business = true
				}
				results <- item
			}
		}()
	}
	go func() {
		for job := range attempts {
			jobs <- job
		}
		close(jobs)
		workers.Wait()
		close(results)
	}()

	summary := onlineAcceptanceSummary{
		Attempts: attempts, Concurrency: concurrency, ErrorsByKind: make(map[string]int),
		UniqueVerifyRule: "one Solve, one RPCClient, one issued CertifyId, at most one Verify attempt",
		RetryPolicy:      "no application retry; network-unknown Verify is final",
	}
	allDurations := make([]time.Duration, 0, attempts)
	successDurations := make([]time.Duration, 0, attempts)
	for item := range results {
		allDurations = append(allDurations, item.wall)
		switch {
		case item.success:
			summary.Success++
			successDurations = append(successDurations, item.wall)
		case item.business:
			summary.BusinessFailure++
		default:
			summary.ErrorsByKind[item.kind]++
		}
	}
	summary.SuccessRate = float64(summary.Success) / float64(summary.Attempts)
	summary.WallP50MS = nearestRankMilliseconds(allDurations, 0.50)
	summary.WallP95MS = nearestRankMilliseconds(allDurations, 0.95)
	summary.WallP99MS = nearestRankMilliseconds(allDurations, 0.99)
	summary.WallMaxMS = nearestRankMilliseconds(allDurations, 1.00)
	summary.SuccessWallP95MS = nearestRankMilliseconds(successDurations, 0.95)
	encoded, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("ONLINE_ACCEPTANCE %s", encoded)
	classified := summary.Success + summary.BusinessFailure
	for _, count := range summary.ErrorsByKind {
		classified += count
	}
	if classified != attempts {
		t.Fatalf("online result classification=%d, want attempts=%d", classified, attempts)
	}
	if attempts == 200 && summary.Success < 190 {
		t.Errorf("online strict success=%d/200, want at least 190/200", summary.Success)
	}
	if attempts == 200 && summary.WallP95MS > 1000 {
		t.Errorf("online Client wall P95=%dms, want at most 1000ms", summary.WallP95MS)
	}
}

func acceptanceInteger(t *testing.T, name string, fallback int) int {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		t.Fatalf("%s must be an integer", name)
	}
	return parsed
}

func nearestRankMilliseconds(values []time.Duration, percentile float64) int64 {
	if len(values) == 0 {
		return 0
	}
	ordered := append([]time.Duration(nil), values...)
	sort.Slice(ordered, func(left, right int) bool { return ordered[left] < ordered[right] })
	rank := int(float64(len(ordered))*percentile + 0.999999999)
	rank = max(1, min(rank, len(ordered)))
	return ordered[rank-1].Milliseconds()
}
