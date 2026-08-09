//go:build online

package slider

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

	"github.com/d2120848471/v3/ali-slider-go/internal/config"
)

type onlineAcceptanceSummary struct {
	Attempts               int              `json:"attempts"`
	Concurrency            int              `json:"concurrency"`
	ClientMaxConcurrency   int              `json:"clientMaxConcurrency"`
	DevicePrewarmCapacity  int              `json:"devicePrewarmCapacity"`
	DeviceSessionReserve   int              `json:"deviceSessionReserve"`
	Success                int              `json:"success"`
	BusinessFailure        int              `json:"businessFailure"`
	ErrorsByKind           map[string]int   `json:"errorsByKind"`
	ErrorsByStage          map[string]int   `json:"errorsByStage"`
	BusinessCodes          map[string]int   `json:"businessCodes"`
	SuccessRate            float64          `json:"successRate"`
	WallMeanMS             int64            `json:"wallMeanMs"`
	WallP50MS              int64            `json:"wallP50Ms"`
	WallP95MS              int64            `json:"wallP95Ms"`
	WallP99MS              int64            `json:"wallP99Ms"`
	WallMaxMS              int64            `json:"wallMaxMs"`
	SuccessWallMeanMS      int64            `json:"successWallMeanMs"`
	SuccessWallP95MS       int64            `json:"successWallP95Ms"`
	FirstHalfWallMeanMS    int64            `json:"firstHalfWallMeanMs"`
	SecondHalfWallMeanMS   int64            `json:"secondHalfWallMeanMs"`
	SecondHalfWallP95MS    int64            `json:"secondHalfWallP95Ms"`
	SecondHalfSuccess      int              `json:"secondHalfSuccess"`
	ProfilePrepareFast     int              `json:"profilePrepareFast"`
	ProfilePrepareSlow     int              `json:"profilePrepareSlow"`
	ProfileSourcePaths     int              `json:"profileSourcePaths"`
	ProfileSampledPaths    int              `json:"profileSampledPaths"`
	ProfileCompatiblePaths int              `json:"profileCompatiblePaths"`
	ProfileFailedPaths     int              `json:"profileFailedPaths"`
	TimingSamples          map[string]int   `json:"timingSamples"`
	TimingMeanMS           map[string]int64 `json:"timingMeanMs"`
	TimingP95MS            map[string]int64 `json:"timingP95Ms"`
	SecondHalfTimingMeanMS map[string]int64 `json:"secondHalfTimingMeanMs"`
	UniqueVerifyRule       string           `json:"uniqueVerifyRule"`
	RetryPolicy            string           `json:"retryPolicy"`
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
	prewarm := acceptanceInteger(t, "ALI_SLIDER_ONLINE_PREWARM", min(concurrency, config.MaxDevicePrewarmCapacity))
	reserve := acceptanceInteger(t, "ALI_SLIDER_ONLINE_DEVICE_RESERVE", 0)
	if prewarm < 0 || prewarm > min(concurrency, config.MaxDevicePrewarmCapacity) {
		t.Fatalf("ALI_SLIDER_ONLINE_PREWARM must be within 0..min(concurrency,%d)", config.MaxDevicePrewarmCapacity)
	}
	if reserve < 0 || reserve > config.MaxDeviceSessionReserve || prewarm == 0 && reserve != 0 || prewarm+reserve > config.MaxDevicePrewarmCapacity {
		t.Fatalf("ALI_SLIDER_ONLINE_DEVICE_RESERVE must be valid and total live capacity must not exceed %d", config.MaxDevicePrewarmCapacity)
	}
	clientMaxConcurrency := max(concurrency, prewarm)
	artifactDirectory := os.Getenv("ALI_SLIDER_ONLINE_ARTIFACT_DIR")
	if artifactDirectory == "" {
		t.Fatal("ALI_SLIDER_ONLINE_ARTIFACT_DIR is required")
	}

	options := DefaultClientOptions()
	options.MaxConcurrency = clientMaxConcurrency
	options.DevicePrewarmCapacity = prewarm
	options.DeviceSessionReserve = reserve
	options.ArtifactDir = artifactDirectory
	if libraryPath := os.Getenv("ALI_SLIDER_V8_LIBRARY"); libraryPath != "" {
		options.V8RuntimeLibrary = libraryPath
	}
	if sceneID := os.Getenv("ALI_SLIDER_ONLINE_SCENE_ID"); sceneID != "" {
		options.DefaultSceneID = sceneID
	}
	if prefix := os.Getenv("ALI_SLIDER_ONLINE_PREFIX"); prefix != "" {
		options.DefaultPrefix = prefix
	}
	client, err := NewClient(options)
	if err != nil {
		t.Fatal("create online Client failed")
	}
	defer client.Close()
	primeContext, cancelPrime := context.WithTimeout(context.Background(), options.Timeout)
	err = client.Prime(primeContext)
	cancelPrime()
	if err != nil {
		var domainError *Error
		if errors.As(err, &domainError) && domainError.Cause != nil {
			t.Fatalf("device prewarm failed before challenge dispatch: kind=%s cause=%v", domainError.Kind, domainError.Cause)
		}
		t.Fatal("device prewarm failed before challenge dispatch")
	}

	type attemptResult struct {
		index      int
		success    bool
		business   bool
		kind       string
		stage      string
		verifyCode string
		wall       time.Duration
		timings    map[string]int
	}
	jobs := make(chan int)
	results := make(chan attemptResult, attempts)
	var workers sync.WaitGroup
	for range concurrency {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for job := range jobs {
				started := time.Now()
				result, solveErr := client.Solve(context.Background(), Request{
					RPCKeyID: os.Getenv("ALI_SLIDER_ONLINE_RPC_KEY_ID"),
					Proxy:    os.Getenv("ALI_SLIDER_ONLINE_PROXY"),
				})
				item := attemptResult{index: job, wall: time.Since(started)}
				if solveErr != nil {
					var domainError *Error
					if errors.As(solveErr, &domainError) {
						item.kind = string(domainError.Kind)
						item.stage = domainError.Stage
					} else {
						item.kind = "UnhandledError"
					}
				} else {
					item.timings = result.TimingsMS
					if result.OK && result.VerifyCode == "T001" && result.VerifyResult && result.SecurityToken != "" {
						item.success = true
					} else {
						item.business = true
						item.verifyCode = result.VerifyCode
						if item.verifyCode == "" {
							item.verifyCode = "<empty>"
						}
					}
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
		Attempts: attempts, Concurrency: concurrency,
		ClientMaxConcurrency: clientMaxConcurrency, DevicePrewarmCapacity: prewarm, DeviceSessionReserve: reserve,
		ErrorsByKind: make(map[string]int), ErrorsByStage: make(map[string]int), BusinessCodes: make(map[string]int),
		TimingSamples: make(map[string]int), TimingMeanMS: make(map[string]int64), TimingP95MS: make(map[string]int64),
		SecondHalfTimingMeanMS: make(map[string]int64),
		UniqueVerifyRule:       "one Solve, one RPCClient, one issued CertifyId, at most one Verify attempt",
		RetryPolicy:            "no application retry; network-unknown Verify is final",
	}
	allDurations := make([]time.Duration, 0, attempts)
	successDurations := make([]time.Duration, 0, attempts)
	firstHalfDurations := make([]time.Duration, 0, attempts/2)
	secondHalfDurations := make([]time.Duration, 0, attempts-attempts/2)
	timingDurations := make(map[string][]time.Duration)
	secondHalfTimingDurations := make(map[string][]time.Duration)
	for item := range results {
		allDurations = append(allDurations, item.wall)
		secondHalf := item.index >= attempts/2
		if secondHalf {
			secondHalfDurations = append(secondHalfDurations, item.wall)
		} else {
			firstHalfDurations = append(firstHalfDurations, item.wall)
		}
		for stage, milliseconds := range item.timings {
			if milliseconds < 0 {
				continue
			}
			timingDurations[stage] = append(timingDurations[stage], time.Duration(milliseconds)*time.Millisecond)
			if secondHalf {
				secondHalfTimingDurations[stage] = append(secondHalfTimingDurations[stage], time.Duration(milliseconds)*time.Millisecond)
			}
		}
		if prepareMS, ok := item.timings["resolvePEKey"]; ok {
			if prepareMS <= 25 {
				summary.ProfilePrepareFast++
			} else {
				summary.ProfilePrepareSlow++
			}
		}
		switch {
		case item.success:
			summary.Success++
			if secondHalf {
				summary.SecondHalfSuccess++
			}
			successDurations = append(successDurations, item.wall)
		case item.business:
			summary.BusinessFailure++
			summary.BusinessCodes[item.verifyCode]++
		default:
			summary.ErrorsByKind[item.kind]++
			stage := item.stage
			if stage == "" {
				stage = "<unknown>"
			}
			summary.ErrorsByStage[stage]++
		}
	}
	summary.SuccessRate = float64(summary.Success) / float64(summary.Attempts)
	summary.WallMeanMS = meanMilliseconds(allDurations)
	summary.WallP50MS = nearestRankMilliseconds(allDurations, 0.50)
	summary.WallP95MS = nearestRankMilliseconds(allDurations, 0.95)
	summary.WallP99MS = nearestRankMilliseconds(allDurations, 0.99)
	summary.WallMaxMS = nearestRankMilliseconds(allDurations, 1.00)
	summary.SuccessWallMeanMS = meanMilliseconds(successDurations)
	summary.SuccessWallP95MS = nearestRankMilliseconds(successDurations, 0.95)
	summary.FirstHalfWallMeanMS = meanMilliseconds(firstHalfDurations)
	summary.SecondHalfWallMeanMS = meanMilliseconds(secondHalfDurations)
	summary.SecondHalfWallP95MS = nearestRankMilliseconds(secondHalfDurations, 0.95)
	for stage, values := range timingDurations {
		summary.TimingSamples[stage] = len(values)
		summary.TimingMeanMS[stage] = meanMilliseconds(values)
		summary.TimingP95MS[stage] = nearestRankMilliseconds(values, 0.95)
	}
	for stage, values := range secondHalfTimingDurations {
		summary.SecondHalfTimingMeanMS[stage] = meanMilliseconds(values)
	}
	profileStats := client.peRuntime.ProfileCacheStats()
	summary.ProfileSourcePaths = profileStats.SourcePaths
	summary.ProfileSampledPaths = profileStats.Sampled
	summary.ProfileCompatiblePaths = profileStats.Compatible
	summary.ProfileFailedPaths = profileStats.Failed
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

func meanMilliseconds(values []time.Duration) int64 {
	if len(values) == 0 {
		return 0
	}
	var total time.Duration
	for _, value := range values {
		total += value
	}
	return (total / time.Duration(len(values))).Milliseconds()
}
