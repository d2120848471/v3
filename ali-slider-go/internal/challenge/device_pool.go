package challenge

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/device"
)

const (
	// DefaultDeviceSessionMaxAge 限制预热会话的空闲时间，避免使用即将过期的设备配置。
	DefaultDeviceSessionMaxAge = 20 * time.Second
	maxDeviceSessionCapacity   = 32
)

// ErrDeviceSessionPoolClosed 表示已关闭的池不再接受 Prime。
var ErrDeviceSessionPoolClosed = errors.New("device session pool is closed")

// DevicePoolKey 是一个可比较的完整设备会话身份。路由、画像或时序任一不同均不得复用。
type DevicePoolKey struct {
	Endpoint         string
	Prefix           string
	Region           string
	Route            string
	ProfileID        string
	Timeout          time.Duration
	GatherCostMin    int
	GatherCostMax    int
	FirstTouchAgeMin int
	FirstTouchAgeMax int
}

// String/GoString 不打印 Route，其中可能含代理用户名和口令。
func (DevicePoolKey) String() string   { return "challenge.DevicePoolKey{redacted}" }
func (DevicePoolKey) GoString() string { return "challenge.DevicePoolKey{redacted}" }

// DevicePoolKeyFrom 从创建 device.Client 的同一份配置生成池键。
// route 必须是传输层实际使用的归一化代理路由，空字符串表示直连。
func DevicePoolKeyFrom(options device.ClientOptions, route string) DevicePoolKey {
	return DevicePoolKey{
		Endpoint: options.Endpoint, Prefix: options.Prefix, Region: options.Region,
		Route: route, ProfileID: options.Profile.ProfileID, Timeout: options.Timeout,
		GatherCostMin: options.GatherCostMin, GatherCostMax: options.GatherCostMax,
		FirstTouchAgeMin: options.FirstTouchAgeMin, FirstTouchAgeMax: options.FirstTouchAgeMax,
	}
}

func (key DevicePoolKey) validate() error {
	if key.Endpoint == "" || key.Prefix == "" || key.Region == "" || key.ProfileID == "" {
		return errors.New("device pool key identity is incomplete")
	}
	if key.Timeout <= 0 {
		return errors.New("device pool key timeout must be positive")
	}
	if key.GatherCostMin < 0 || key.GatherCostMax < key.GatherCostMin {
		return errors.New("device pool key gather cost range is invalid")
	}
	if key.FirstTouchAgeMin < 1 || key.FirstTouchAgeMax < key.FirstTouchAgeMin {
		return errors.New("device pool key first touch age range is invalid")
	}
	return nil
}

// DeviceSessionOpener 与 (*device.Client).Open 的方法值完全一致，调用方无需额外适配层。
type DeviceSessionOpener func(context.Context) (*device.Session, error)

// RuntimeDeviceSessionOpener 支持保持动态 SDK VM 的设备会话。
type RuntimeDeviceSessionOpener func(context.Context) (DeviceSession, error)

type preparedDeviceSession struct {
	session   DeviceSession
	createdAt time.Time
}

// DeviceSessionPool 持有一组已完成 Log1/2/3 的一次性设备会话。
//
// 池跟踪尚未 release 的所有会话；Lease 成功后，调用方必须延迟调用 release。
// 作为 opener 传入的函数必须遵守 context 取消。
type DeviceSessionPool struct {
	mu       sync.Mutex
	capacity int
	maxAge   time.Duration
	key      DevicePoolKey
	open     RuntimeDeviceSessionOpener
	ready    []preparedDeviceSession
	leased   map[DeviceSession]struct{}
	pending  int
	waiters  int
	closed   bool
	notify   chan struct{}

	lifecycle context.Context
	cancel    context.CancelFunc
	tasks     sync.WaitGroup
	closeOnce sync.Once

	now                 func() time.Time
	closeSession        func(*device.Session)
	closeRuntimeSession func(DeviceSession)
	// beforeReservedCommit 仅用于精确验证“冷建完成→提交池状态”的 Close 窗口。
	beforeReservedCommit func()
}

// NewDeviceSessionPool 创建固定 key 的有界池。capacity=0 显式禁用预热；
// maxAge=0 使用 20s 默认值。启用时可直接传入 client.Open。
func NewDeviceSessionPool(capacity int, maxAge time.Duration, key DevicePoolKey, open DeviceSessionOpener) (*DeviceSessionPool, error) {
	var runtimeOpen RuntimeDeviceSessionOpener
	if open != nil {
		runtimeOpen = func(ctx context.Context) (DeviceSession, error) {
			session, err := open(ctx)
			if session == nil {
				return nil, err
			}
			return session, err
		}
	}
	return newRuntimeDeviceSessionPool(capacity, maxAge, key, runtimeOpen)
}

// NewRuntimeDeviceSessionPool 创建可保存动态 SDK VM 会话的同语义预热池。
func NewRuntimeDeviceSessionPool(capacity int, maxAge time.Duration, key DevicePoolKey, open RuntimeDeviceSessionOpener) (*DeviceSessionPool, error) {
	return newRuntimeDeviceSessionPool(capacity, maxAge, key, open)
}

func newRuntimeDeviceSessionPool(capacity int, maxAge time.Duration, key DevicePoolKey, open RuntimeDeviceSessionOpener) (*DeviceSessionPool, error) {
	if capacity < 0 || capacity > maxDeviceSessionCapacity {
		return nil, fmt.Errorf("device session capacity must be within 0..%d", maxDeviceSessionCapacity)
	}
	if maxAge < 0 {
		return nil, errors.New("device session max age must not be negative")
	}
	if maxAge == 0 {
		maxAge = DefaultDeviceSessionMaxAge
	}
	if capacity > 0 {
		if err := key.validate(); err != nil {
			return nil, err
		}
		if open == nil {
			return nil, errors.New("device session opener is required")
		}
	}
	lifecycle, cancel := context.WithCancel(context.Background())
	return &DeviceSessionPool{
		capacity: capacity, maxAge: maxAge, key: key, open: open,
		ready: make([]preparedDeviceSession, 0, capacity), leased: make(map[DeviceSession]struct{}, capacity),
		notify: make(chan struct{}), lifecycle: lifecycle, cancel: cancel,
		now: time.Now, closeSession: func(session *device.Session) { session.Close() },
		closeRuntimeSession: func(session DeviceSession) { session.Close() },
	}, nil
}

// Prime 并行补足至容量。已就绪和正在构建的会话都会计入上限。
// 部分构建失败不会销毁同批已成功的会话，后续 Prime 可再次补齐。
func (pool *DeviceSessionPool) Prime(ctx context.Context) error {
	if pool == nil {
		return errors.New("device session pool is nil")
	}
	if ctx == nil {
		return errors.New("device session context is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	pool.mu.Lock()
	if pool.closed {
		pool.mu.Unlock()
		return ErrDeviceSessionPoolClosed
	}
	missing := pool.capacity - len(pool.ready) - len(pool.leased) - pool.pending
	if missing <= 0 {
		pool.mu.Unlock()
		return nil
	}
	pool.pending += missing
	pool.tasks.Add(missing)
	pool.broadcastLocked()
	opener := pool.open
	poolLifecycle := pool.lifecycle
	pool.mu.Unlock()

	primeContext, cancel := linkedDevicePoolContext(ctx, poolLifecycle)
	defer cancel()
	results := make(chan error, missing)
	for range missing {
		go pool.runOpen(primeContext, opener, results)
	}

	var failures []error
	for range missing {
		select {
		case err := <-results:
			if err != nil {
				failures = append(failures, err)
			}
		case <-ctx.Done():
			return ctx.Err()
		case <-poolLifecycle.Done():
			return ErrDeviceSessionPoolClosed
		}
	}
	return errors.Join(failures...)
}

// Lease 仅在 key 完全相等时租用新鲜预热会话。禁用、已关闭或
// key 不等时通过本次请求的 opener 冷建，因此不会跨代理/画像复用。
// 命中 key 但 ready 为空且 pending>0 时，Lease 在请求 context 内等待这批
// 构建完成，避免与投机补货同时再冷建一整批。
// 调用方必须且只需调用一次 release；它先关闭消费的会话，再按需异步补一个。
func (pool *DeviceSessionPool) Lease(ctx context.Context, key DevicePoolKey, open DeviceSessionOpener) (*device.Session, func(), error) {
	if open == nil {
		return nil, nil, errors.New("device session opener is required")
	}
	runtimeOpen := func(ctx context.Context) (DeviceSession, error) {
		session, err := open(ctx)
		if session == nil {
			return nil, err
		}
		return session, err
	}
	session, release, err := pool.LeaseRuntime(ctx, key, runtimeOpen)
	if err != nil || session == nil {
		return nil, release, err
	}
	goSession, ok := session.(*device.Session)
	if !ok {
		release()
		return nil, nil, errors.New("device session pool returned an incompatible session")
	}
	return goSession, release, nil
}

// LeaseRuntime 与 Lease 共享同一套容量、过期、等待和补货规则。
func (pool *DeviceSessionPool) LeaseRuntime(ctx context.Context, key DevicePoolKey, open RuntimeDeviceSessionOpener) (DeviceSession, func(), error) {
	if ctx == nil {
		return nil, nil, errors.New("device session context is nil")
	}
	if open == nil {
		return nil, nil, errors.New("device session opener is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if pool == nil {
		return leaseColdDeviceSession(ctx, open)
	}

	waited := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		pool.mu.Lock()
		if pool.capacity == 0 || key != pool.key {
			pool.mu.Unlock()
			return leaseColdDeviceSession(ctx, open)
		}
		if pool.closed {
			pool.mu.Unlock()
			if waited {
				if err := ctx.Err(); err != nil {
					return nil, nil, err
				}
				return nil, nil, ErrDeviceSessionPoolClosed
			}
			return leaseColdDeviceSession(ctx, open)
		}

		now := pool.now()
		expired := make([]DeviceSession, 0)
		stateChanged := false
		for len(pool.ready) > 0 {
			entry := pool.ready[0]
			pool.ready[0] = preparedDeviceSession{}
			pool.ready = pool.ready[1:]
			stateChanged = true
			if !now.Before(entry.createdAt.Add(pool.maxAge)) {
				expired = append(expired, entry.session)
				continue
			}
			pool.leased[entry.session] = struct{}{}
			// 如果本次同时清理了过期库存，立即有界恢复这部分缺口。
			// 未过期的 ready 只是转为 leased，总量不变。
			pool.refillToCapacityLocked()
			pool.broadcastLocked()
			pool.mu.Unlock()
			pool.closeSessions(expired)
			return entry.session, pool.pooledRelease(entry.session), nil
		}
		if stateChanged {
			pool.broadcastLocked()
		}

		if pool.pending > 0 {
			notify := pool.notify
			lifecycle := pool.lifecycle
			pool.waiters++
			pool.mu.Unlock()
			pool.closeSessions(expired)
			waited = true
			var waitErr error
			select {
			case <-ctx.Done():
				waitErr = ctx.Err()
			case <-lifecycle.Done():
				if err := ctx.Err(); err != nil {
					waitErr = err
				} else {
					waitErr = ErrDeviceSessionPoolClosed
				}
			case <-notify:
			}
			pool.mu.Lock()
			pool.waiters--
			pool.mu.Unlock()
			if waitErr != nil {
				return nil, nil, waitErr
			}
			continue
		}

		// 只有当前批次全部完成且仍无 ready 时，才由第一个抢到锁的
		// Lease 预留一个冷建槽位。它随后填满 pending，其他 Lease 会继续等待。
		reserved := len(pool.ready)+len(pool.leased)+pool.pending < pool.capacity
		if reserved {
			pool.pending++
			pool.tasks.Add(1)
			pool.broadcastLocked()
			pool.refillToCapacityLocked()
		}
		poolLifecycle := pool.lifecycle
		pool.mu.Unlock()
		pool.closeSessions(expired)
		if reserved {
			return pool.leaseReservedCold(ctx, poolLifecycle, open)
		}
		return leaseColdDeviceSession(ctx, open)
	}
}

// Close 可并发重复调用。它取消补货，清理所有未租出会话，
// 并等待已启动的后台任务退出。之后 Lease 仍可按冷建路径工作。
func (pool *DeviceSessionPool) Close() {
	if pool == nil {
		return
	}
	pool.closeOnce.Do(func() {
		pool.mu.Lock()
		pool.closed = true
		pool.cancel()
		ready := pool.ready
		pool.ready = nil
		leased := pool.leased
		pool.leased = nil
		pool.broadcastLocked()
		pool.mu.Unlock()

		for _, entry := range ready {
			pool.closeOne(entry.session)
		}
		for session := range leased {
			pool.closeOne(session)
		}
		pool.tasks.Wait()
	})
}

func (pool *DeviceSessionPool) startRefillLocked() {
	if pool.closed || pool.capacity == 0 || len(pool.ready)+len(pool.leased)+pool.pending >= pool.capacity {
		return
	}
	pool.pending++
	pool.tasks.Add(1)
	pool.broadcastLocked()
	opener := pool.open
	ctx, cancel := context.WithTimeout(pool.lifecycle, pool.key.Timeout)
	go func() {
		defer cancel()
		pool.runOpen(ctx, opener, nil)
	}()
}

// refillToCapacityLocked 只为当前可见缺口启动一批任务。单个任务失败后
// 不会在这里自行重试，因此无定时器、无无限补货循环。
func (pool *DeviceSessionPool) refillToCapacityLocked() {
	for !pool.closed && len(pool.ready)+len(pool.leased)+pool.pending < pool.capacity {
		pool.startRefillLocked()
	}
}

func (pool *DeviceSessionPool) runOpen(ctx context.Context, open RuntimeDeviceSessionOpener, result chan<- error) {
	defer pool.tasks.Done()
	session, err := open(ctx)
	if err == nil && session == nil {
		err = errors.New("device session opener returned nil")
	}
	if err == nil {
		err = ctx.Err()
	}

	pool.mu.Lock()
	pool.pending--
	store := err == nil && !pool.closed && len(pool.ready)+len(pool.leased) < pool.capacity
	if store {
		pool.ready = append(pool.ready, preparedDeviceSession{session: session, createdAt: pool.now()})
	}
	pool.broadcastLocked()
	pool.mu.Unlock()

	if !store && session != nil {
		pool.closeOne(session)
	}
	if result != nil {
		result <- err
	}
}

func (pool *DeviceSessionPool) leaseReservedCold(ctx, lifecycle context.Context, open RuntimeDeviceSessionOpener) (DeviceSession, func(), error) {
	leaseContext, cancel := linkedDevicePoolContext(ctx, lifecycle)
	session, err := open(leaseContext)
	if err == nil && session == nil {
		err = errors.New("device session opener returned nil")
	}
	if err == nil {
		err = leaseContext.Err()
	}
	cancel()
	if pool.beforeReservedCommit != nil {
		pool.beforeReservedCommit()
	}

	pool.mu.Lock()
	pool.pending--
	closed := pool.closed
	callerContextErr := ctx.Err()
	if err == nil && callerContextErr != nil {
		err = callerContextErr
	}
	store := err == nil && !closed && len(pool.ready)+len(pool.leased) < pool.capacity
	if store {
		pool.leased[session] = struct{}{}
	}
	// 本批预留冷建失败时只广播 pending 归零，不自行重试。
	// 若仍有等待需求，只会有下一个抢到锁的 Lease 预留单个冷建。
	pool.broadcastLocked()
	pool.mu.Unlock()
	pool.tasks.Done()

	if closed {
		if session != nil {
			pool.closeOne(session)
		}
		if callerContextErr != nil {
			return nil, nil, callerContextErr
		}
		return nil, nil, ErrDeviceSessionPoolClosed
	}
	if err != nil {
		if session != nil {
			pool.closeOne(session)
		}
		return nil, nil, err
	}
	if store {
		return session, pool.pooledRelease(session), nil
	}
	var once sync.Once
	return session, func() { once.Do(session.Close) }, nil
}

func (pool *DeviceSessionPool) closeOne(session DeviceSession) {
	if goSession, ok := session.(*device.Session); ok {
		pool.closeSession(goSession)
		return
	}
	pool.closeRuntimeSession(session)
}

func (pool *DeviceSessionPool) closeSessions(sessions []DeviceSession) {
	for _, session := range sessions {
		pool.closeOne(session)
	}
}

func leaseColdDeviceSession(ctx context.Context, open RuntimeDeviceSessionOpener) (DeviceSession, func(), error) {
	session, err := open(ctx)
	if err != nil {
		if session != nil {
			session.Close()
		}
		return nil, nil, err
	}
	if session == nil {
		return nil, nil, errors.New("device session opener returned nil")
	}
	if err := ctx.Err(); err != nil {
		session.Close()
		return nil, nil, err
	}
	var once sync.Once
	return session, func() { once.Do(session.Close) }, nil
}

func (pool *DeviceSessionPool) pooledRelease(session DeviceSession) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			pool.mu.Lock()
			_, owned := pool.leased[session]
			if owned {
				delete(pool.leased, session)
				// 与 Close 在同一把锁下登记，保证 Close 不会在会话真正关闭前返回。
				pool.tasks.Add(1)
				pool.broadcastLocked()
			}
			pool.mu.Unlock()
			if !owned {
				return
			}
			defer pool.tasks.Done()
			pool.closeOne(session)
			pool.mu.Lock()
			pool.refillToCapacityLocked()
			pool.mu.Unlock()
		})
	}
}

// broadcastLocked 在池状态世代变化时关闭旧通知通道并创建新通道。
// 观察者在同一把锁下检查状态并捕获通道，因此不会丢失唤醒。
func (pool *DeviceSessionPool) broadcastLocked() {
	close(pool.notify)
	pool.notify = make(chan struct{})
}

func linkedDevicePoolContext(primary, lifecycle context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(primary)
	stop := context.AfterFunc(lifecycle, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}
