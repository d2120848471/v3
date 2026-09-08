// Package service 管理可复用求解服务的默认请求值和共享资源生命周期。
package service

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/application/solve"
)

const (
	DefaultSceneID = "1ug4aptr"
	DefaultPrefix  = "fsgtmi"
	DefaultTimeout = 25 * time.Second
)

// Executor 只执行一轮请求，不持有服务的关闭协调状态。
type Executor interface {
	Solve(context.Context, solve.Request) (solve.Outcome, error)
}

// Resources 由组装层实现；服务只协调操作顺序，不了解 V8、网络和文件存储。
type Resources interface {
	CheckRuntime() error
	PurgeArtifacts() (int, error)
	Close()
}

type Options struct {
	Executor       Executor
	Resources      Resources
	DefaultSceneID string
	DefaultPrefix  string
}

// Service 可并发复用。Close 拒绝新工作，等待所有在途操作结束后释放资源。
type Service struct {
	executor       Executor
	resources      Resources
	defaultSceneID string
	defaultPrefix  string
	mu             sync.Mutex
	closed         bool
	active         sync.WaitGroup
	closeOnce      sync.Once
}

func New(options Options) (*Service, error) {
	if options.Executor == nil {
		return nil, errors.New("executor is required")
	}
	if options.Resources == nil {
		return nil, errors.New("resources are required")
	}
	if options.DefaultSceneID == "" {
		options.DefaultSceneID = DefaultSceneID
	}
	if options.DefaultPrefix == "" {
		options.DefaultPrefix = DefaultPrefix
	}
	return &Service{
		executor: options.Executor, resources: options.Resources,
		defaultSceneID: options.DefaultSceneID, defaultPrefix: options.DefaultPrefix,
	}, nil
}

func (service *Service) Solve(ctx context.Context, request solve.Request) (solve.Outcome, error) {
	if !service.begin() {
		return solve.Outcome{}, unavailable("client")
	}
	defer service.active.Done()
	if request.SceneID == "" {
		request.SceneID = service.defaultSceneID
	}
	if request.Prefix == "" {
		request.Prefix = service.defaultPrefix
	}
	return service.executor.Solve(ctx, request)
}

// Prime 保留旧入口的生命周期语义，不访问上游，也不检查传入的 context。
func (service *Service) Prime(_ context.Context) error {
	if !service.begin() {
		return unavailable("prime")
	}
	defer service.active.Done()
	return nil
}

func (service *Service) CheckRuntime() error {
	if !service.begin() {
		return unavailable("runtime")
	}
	defer service.active.Done()
	if err := service.resources.CheckRuntime(); err != nil {
		return &solve.Failure{Kind: solve.FailureInternal, Stage: "runtime", Message: "V8 runtime 校验失败", Cause: err}
	}
	return nil
}

func (service *Service) PurgeArtifacts() (int, error) {
	if !service.begin() {
		return 0, unavailable("artifacts")
	}
	defer service.active.Done()
	return service.resources.PurgeArtifacts()
}

func (service *Service) Close() {
	if service == nil {
		return
	}
	// sync.Once 也使并发调用者等待首次关闭完成，无需第二个完成通道。
	service.closeOnce.Do(func() {
		service.mu.Lock()
		service.closed = true
		service.mu.Unlock()
		service.active.Wait()
		if service.resources != nil {
			service.resources.Close()
		}
	})
}

func (service *Service) begin() bool {
	if service == nil {
		return false
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.closed || service.executor == nil || service.resources == nil {
		return false
	}
	// 与 closed 状态共用锁，确保 Close 开始等待后不会再 Add。
	service.active.Add(1)
	return true
}

func unavailable(stage string) error {
	return &solve.Failure{Kind: solve.FailureInternal, Stage: stage, Message: "Client 已关闭或不可用"}
}
