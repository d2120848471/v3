package slider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/d2120848471/v3/ali-slider-go/internal/artifact"
	"github.com/d2120848471/v3/ali-slider-go/internal/challenge"
	"github.com/d2120848471/v3/ali-slider-go/internal/config"
	"github.com/d2120848471/v3/ali-slider-go/internal/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/runtimekit"
)

// ClientOptions 配置可并发复用的 Go Client。直接使用零值结构时会按
// DefaultClientOptions 补齐。如需显式传递有意义的零（如置信度 0、
// GatherCost 0..0 或关闭预热），应先调用 DefaultClientOptions 再覆盖。
type ClientOptions struct {
	DefaultSceneID string
	DefaultPrefix  string
	// MaxConcurrency 是兼容旧名：只控制每 route/host 出站连接与预热资源边界，不限制 Solve 调用。
	MaxConcurrency        int
	MaxTransportRoutes    int
	Timeout               time.Duration
	MinimumConfidence     float64
	GatherCostMin         int
	GatherCostMax         int
	FirstTouchAgeMin      int
	FirstTouchAgeMax      int
	ArtifactDir           string
	ArtifactRetention     time.Duration
	AssetMaxBytes         int64
	AssetMaxDimension     int
	AssetMaxPixels        int64
	DevicePrewarmCapacity int
	DeviceSessionMaxAge   time.Duration

	defaultsResolved bool
}

// DefaultClientOptions 返回服务与嵌入式 library 共用的生产默认值。
func DefaultClientOptions() ClientOptions {
	defaults := config.Defaults()
	return ClientOptions{
		DefaultSceneID: defaults.SceneID, DefaultPrefix: defaults.Prefix,
		MaxConcurrency: defaults.MaxConcurrency, MaxTransportRoutes: 32,
		Timeout: defaults.Timeout, MinimumConfidence: defaults.MinimumConfidence,
		GatherCostMin: defaults.GatherCostMin, GatherCostMax: defaults.GatherCostMax,
		FirstTouchAgeMin: defaults.FirstTouchAgeMin, FirstTouchAgeMax: defaults.FirstTouchAgeMax,
		ArtifactDir: defaults.ArtifactDir, ArtifactRetention: defaults.ArtifactRetention,
		AssetMaxBytes: defaults.AssetMaxBytes, AssetMaxDimension: defaults.AssetMaxDimension,
		AssetMaxPixels: 16 << 20, DevicePrewarmCapacity: defaults.DevicePrewarmCapacity,
		DeviceSessionMaxAge: challenge.DefaultDeviceSessionMaxAge,
		defaultsResolved:    true,
	}
}

type challengeSolver interface {
	Solve(context.Context, challenge.SolveRequest) (challenge.SolveOutcome, error)
}

// Client 可被多个 goroutine 并发复用。每次 Solve 仍持有独立挑战、设备与
// Verify 尝试位；共享的只有连接池、密码学熵、只读画像和可选预热池。
type Client struct {
	options    ClientOptions
	solver     challengeSolver
	transports *challenge.TransportPool
	devices    *challenge.DeviceSessionPool
	artifacts  artifact.Store

	mu        sync.Mutex
	closed    bool
	active    sync.WaitGroup
	closeOnce sync.Once
	closeDone chan struct{}
}

// NewClient 创建纯 Go Client，不发外部请求。需要启动预热时显式调用 Prime。
func NewClient(options ClientOptions) (*Client, error) {
	if !options.defaultsResolved {
		options = fillClientDefaults(options)
	}
	if err := validateClientOptions(options); err != nil {
		return nil, err
	}
	sources := runtimekit.NewSystemSources()
	transports, err := challenge.NewTransportPool(options.MaxTransportRoutes, options.MaxConcurrency)
	if err != nil {
		return nil, err
	}
	// 无论是否开启预热都先保留直连路由，避免随机代理占满路由表后阻断默认路径。
	directTransport, _, err := transports.Get("")
	if err != nil {
		transports.CloseIdleConnections()
		return nil, err
	}
	store := artifact.Store{
		Directory: options.ArtifactDir, Retention: options.ArtifactRetention,
		Entropy: sources.Entropy,
	}

	var fixedProfile *device.Profile
	var devicePool *challenge.DeviceSessionPool
	var leaseDevice challenge.DeviceLeaseFunc
	if options.DevicePrewarmCapacity > 0 {
		profile, profileErr := device.GenerateProfile(sources.Entropy)
		if profileErr != nil {
			transports.CloseIdleConnections()
			return nil, fmt.Errorf("generate fixed device profile: %w", profileErr)
		}
		fixedProfile = &profile
		deviceOptions := productionDeviceOptions(options, profile, sources)
		deviceClient, clientErr := device.NewClient(deviceOptions, directTransport)
		if clientErr != nil {
			transports.CloseIdleConnections()
			return nil, clientErr
		}
		devicePool, err = challenge.NewDeviceSessionPool(
			options.DevicePrewarmCapacity, options.DeviceSessionMaxAge,
			challenge.DevicePoolKeyFrom(deviceOptions, ""), deviceClient.Open,
		)
		if err != nil {
			transports.CloseIdleConnections()
			return nil, err
		}
		leaseDevice = func(ctx context.Context, key challenge.DevicePoolKey, client *device.Client) (*device.Session, func(), error) {
			return devicePool.Lease(ctx, key, client.Open)
		}
	}

	getTransport := func(proxy string) (http.RoundTripper, bool, error) {
		transport, proxied, getErr := transports.Get(proxy)
		return transport, proxied, getErr
	}
	engine, err := challenge.NewSolver(challenge.SolverOptions{
		Timeout: options.Timeout, MinimumConfidence: options.MinimumConfidence,
		GatherCostMin: options.GatherCostMin, GatherCostMax: options.GatherCostMax,
		FirstTouchAgeMin: options.FirstTouchAgeMin, FirstTouchAgeMax: options.FirstTouchAgeMax,
		AssetMaxBytes: options.AssetMaxBytes, AssetMaxDimension: options.AssetMaxDimension,
		AssetMaxPixels: options.AssetMaxPixels, Sources: sources, GetTransport: getTransport,
		Artifacts: &store, FixedProfile: fixedProfile, LeaseDevice: leaseDevice,
	})
	if err != nil {
		if devicePool != nil {
			devicePool.Close()
		}
		transports.CloseIdleConnections()
		return nil, err
	}
	return &Client{
		options: options, solver: engine, transports: transports, devices: devicePool,
		artifacts: store, closeDone: make(chan struct{}),
	}, nil
}

// Solve 执行一轮挑战。SceneID/Prefix 空值使用 Client 默认配置。
func (client *Client) Solve(ctx context.Context, request Request) (Result, error) {
	if client == nil || !client.begin() {
		return Result{}, &Error{Kind: ErrorInternal, Stage: "client", Message: "Client 已关闭或不可用"}
	}
	defer client.active.Done()
	if request.SceneID == "" {
		request.SceneID = client.options.DefaultSceneID
	}
	if request.Prefix == "" {
		request.Prefix = client.options.DefaultPrefix
	}
	outcome, err := client.solver.Solve(ctx, challenge.SolveRequest{
		SceneID: request.SceneID, Prefix: request.Prefix, RPCKeyID: request.RPCKeyID, Proxy: request.Proxy,
	})
	if err != nil {
		return Result{}, publicError(err)
	}
	timings := cloneTimingMap(outcome.TimingsMS)
	return Result{
		OK: outcome.OK, SecurityToken: outcome.SecurityToken,
		VerifyCode: outcome.VerifyCode, VerifyResult: outcome.VerifyResult,
		CertifyID: outcome.CertifyID, SceneID: outcome.SceneID, Proxied: outcome.Proxied,
		ElapsedMS: int64(timings["total"]), TimingsMS: timings,
	}, nil
}

// Prime 并行预热默认直连配置的设备会话。预热关闭时立即成功。
// 部分预热失败会返回错误；已成功的槽位仍保留，后续请求可正常冷建兜底。
func (client *Client) Prime(ctx context.Context) error {
	if client == nil || !client.begin() {
		return &Error{Kind: ErrorInternal, Stage: "prime", Message: "Client 已关闭或不可用"}
	}
	defer client.active.Done()
	if client.devices == nil {
		return nil
	}
	if err := client.devices.Prime(ctx); err != nil {
		return &Error{Kind: ErrorNetwork, Stage: "prime", Message: "设备会话预热未完全成功", Cause: err}
	}
	return nil
}

// PurgeArtifacts 删除超过配置保留期的受管失败样本；不触碰其他文件。
func (client *Client) PurgeArtifacts() (int, error) {
	if client == nil || !client.begin() {
		return 0, &Error{Kind: ErrorInternal, Stage: "artifacts", Message: "Client 已关闭或不可用"}
	}
	defer client.active.Done()
	return client.artifacts.Purge()
}

// Close 等待正在执行的 Solve/Prime/Purge 返回，再关闭预热会话和空闲连接。
// 多个 goroutine 可安全重复调用。
func (client *Client) Close() {
	if client == nil {
		return
	}
	client.closeOnce.Do(func() {
		client.mu.Lock()
		client.closed = true
		client.mu.Unlock()
		client.active.Wait()
		if client.devices != nil {
			client.devices.Close()
		}
		if client.transports != nil {
			client.transports.CloseIdleConnections()
		}
		close(client.closeDone)
	})
	<-client.closeDone
}

func (client *Client) begin() bool {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.closed {
		return false
	}
	client.active.Add(1)
	return true
}

func fillClientDefaults(options ClientOptions) ClientOptions {
	defaults := DefaultClientOptions()
	if options.DefaultSceneID == "" {
		options.DefaultSceneID = defaults.DefaultSceneID
	}
	if options.DefaultPrefix == "" {
		options.DefaultPrefix = defaults.DefaultPrefix
	}
	if options.MaxConcurrency == 0 {
		options.MaxConcurrency = defaults.MaxConcurrency
	}
	if options.MaxTransportRoutes == 0 {
		options.MaxTransportRoutes = defaults.MaxTransportRoutes
	}
	if options.Timeout == 0 {
		options.Timeout = defaults.Timeout
	}
	if options.MinimumConfidence == 0 {
		options.MinimumConfidence = defaults.MinimumConfidence
	}
	if options.GatherCostMin == 0 && options.GatherCostMax == 0 {
		options.GatherCostMin, options.GatherCostMax = defaults.GatherCostMin, defaults.GatherCostMax
	}
	if options.FirstTouchAgeMin == 0 && options.FirstTouchAgeMax == 0 {
		options.FirstTouchAgeMin, options.FirstTouchAgeMax = defaults.FirstTouchAgeMin, defaults.FirstTouchAgeMax
	}
	if options.ArtifactDir == "" {
		options.ArtifactDir = defaults.ArtifactDir
	}
	if options.ArtifactRetention == 0 {
		options.ArtifactRetention = defaults.ArtifactRetention
	}
	if options.AssetMaxBytes == 0 {
		options.AssetMaxBytes = defaults.AssetMaxBytes
	}
	if options.AssetMaxDimension == 0 {
		options.AssetMaxDimension = defaults.AssetMaxDimension
	}
	if options.AssetMaxPixels == 0 {
		options.AssetMaxPixels = defaults.AssetMaxPixels
	}
	if options.DevicePrewarmCapacity == 0 {
		options.DevicePrewarmCapacity = defaults.DevicePrewarmCapacity
	}
	if options.DeviceSessionMaxAge == 0 {
		options.DeviceSessionMaxAge = defaults.DeviceSessionMaxAge
	}
	return options
}

func validateClientOptions(options ClientOptions) error {
	if options.DefaultSceneID == "" || utf8.RuneCountInString(options.DefaultSceneID) > 64 {
		return errors.New("default SceneId must contain 1..64 characters")
	}
	if !asciiAlphanumeric(options.DefaultPrefix, 32) {
		return errors.New("default prefix must contain 1..32 ASCII alphanumeric characters")
	}
	if options.MaxConcurrency < 1 || options.MaxConcurrency > 32 || options.MaxTransportRoutes < 1 || options.MaxTransportRoutes > 32 {
		return errors.New("transport connections and routes must be within 1..32")
	}
	if options.Timeout <= 0 || options.Timeout > 5*time.Minute || options.MinimumConfidence < 0 || options.MinimumConfidence > 1 {
		return errors.New("timeout or confidence is outside the supported range")
	}
	if options.GatherCostMin < 0 || options.GatherCostMax < options.GatherCostMin || options.FirstTouchAgeMin < 1 || options.FirstTouchAgeMax < options.FirstTouchAgeMin {
		return errors.New("device timing range is invalid")
	}
	if options.ArtifactDir == "" || options.ArtifactRetention <= 0 {
		return errors.New("artifact directory and retention are required")
	}
	if options.AssetMaxBytes < 1 || options.AssetMaxBytes > 64<<20 || options.AssetMaxDimension < 1 || options.AssetMaxDimension > 16_384 || options.AssetMaxPixels < 1 {
		return errors.New("asset limits are invalid")
	}
	if options.DevicePrewarmCapacity < 0 || options.DevicePrewarmCapacity > options.MaxConcurrency || options.DeviceSessionMaxAge <= 0 {
		return errors.New("device prewarm settings are invalid")
	}
	return nil
}

func productionDeviceOptions(options ClientOptions, profile device.Profile, sources runtimekit.Sources) device.ClientOptions {
	result := device.DefaultClientOptions(profile, sources)
	result.Prefix = options.DefaultPrefix
	result.Timeout = options.Timeout
	result.GatherCostMin, result.GatherCostMax = options.GatherCostMin, options.GatherCostMax
	result.FirstTouchAgeMin, result.FirstTouchAgeMax = options.FirstTouchAgeMin, options.FirstTouchAgeMax
	return result
}

func publicError(err error) error {
	var failure *challenge.Failure
	if !errors.As(err, &failure) {
		return &Error{Kind: ErrorInternal, Stage: "solver", Message: "未处理的协议运行错误", Cause: err}
	}
	kind := ErrorInternal
	switch failure.Kind {
	case challenge.FailureInvalidRequest:
		kind = ErrorInvalidRequest
	case challenge.FailureProtocol:
		kind = ErrorProtocol
	case challenge.FailureNetwork:
		kind = ErrorNetwork
	case challenge.FailureVision:
		kind = ErrorVision
	}
	return &Error{Kind: kind, Stage: failure.Stage, Message: failure.Message, Cause: err}
}

func cloneTimingMap(value map[string]int) map[string]int {
	result := make(map[string]int, len(value))
	for key, duration := range value {
		result[key] = duration
	}
	return result
}

func asciiAlphanumeric(value string, maximum int) bool {
	if value == "" || len(value) > maximum {
		return false
	}
	for _, character := range []byte(value) {
		if (character < '0' || character > '9') && (character < 'A' || character > 'Z') && (character < 'a' || character > 'z') {
			return false
		}
	}
	return true
}
