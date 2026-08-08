package challenge

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/protocol"
)

type poolRuntimeSession struct{ closes *atomic.Int32 }

func (*poolRuntimeSession) InitToken() (string, error) { return "token", nil }
func (*poolRuntimeSession) PEDeviceConfig() (protocol.DeviceConfig, error) {
	return protocol.DeviceConfig{Key: "0123456789abcdef", SessionID: "session"}, nil
}
func (*poolRuntimeSession) TargetFirstTouchAgeMS() (int, error) { return 700, nil }
func (*poolRuntimeSession) Complete(context.Context, string, []device.InteractionEvent, int) (device.Result, error) {
	return device.Result{}, nil
}
func (session *poolRuntimeSession) Close() { session.closes.Add(1) }

type poolTestOpener struct {
	calls     atomic.Int32
	active    atomic.Int32
	maxActive atomic.Int32
	started   chan int
	gate      <-chan struct{}
	result    func(int) (*device.Session, error)
}

func newPoolTestOpener() *poolTestOpener {
	return &poolTestOpener{started: make(chan int, 256)}
}

func (opener *poolTestOpener) Open(ctx context.Context) (*device.Session, error) {
	call := int(opener.calls.Add(1))
	active := opener.active.Add(1)
	for {
		current := opener.maxActive.Load()
		if active <= current || opener.maxActive.CompareAndSwap(current, active) {
			break
		}
	}
	defer opener.active.Add(-1)
	opener.started <- call
	if opener.gate != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-opener.gate:
		}
	}
	if opener.result != nil {
		return opener.result(call)
	}
	return new(device.Session), nil
}

func poolTestKey(route, profile string) DevicePoolKey {
	return DevicePoolKey{
		Endpoint: "https://device.invalid", Prefix: "fsgtmi", Region: "cn",
		Route: route, ProfileID: profile, Timeout: time.Second,
		GatherCostMin: 180, GatherCostMax: 260, FirstTouchAgeMin: 650, FirstTouchAgeMax: 850,
	}
}

func poolCounts(pool *DeviceSessionPool) (ready, pending, leased int, closed bool) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	return len(pool.ready), pool.pending, len(pool.leased), pool.closed
}

func poolWaiters(pool *DeviceSessionPool) int {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	return pool.waiters
}

func waitForPool(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", description)
		}
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
}

func TestDevicePoolKeyAndConstructorValidation(t *testing.T) {
	options := device.ClientOptions{
		Endpoint: "https://device.invalid", Prefix: "p", Region: "r", Timeout: 3 * time.Second,
		GatherCostMin: 1, GatherCostMax: 2, FirstTouchAgeMin: 3, FirstTouchAgeMax: 4,
		Profile: device.Profile{ProfileID: "profile-id"},
	}
	key := DevicePoolKeyFrom(options, "socks5h://proxy.invalid:1080")
	if key.Endpoint != options.Endpoint || key.Prefix != "p" || key.Region != "r" || key.Route != "socks5h://proxy.invalid:1080" ||
		key.ProfileID != "profile-id" || key.Timeout != 3*time.Second || key.GatherCostMin != 1 || key.GatherCostMax != 2 ||
		key.FirstTouchAgeMin != 3 || key.FirstTouchAgeMax != 4 {
		t.Fatalf("pool key omitted configuration: %#v", key)
	}

	opener := newPoolTestOpener()
	valid := poolTestKey("", "profile")
	invalidTimeout := valid
	invalidTimeout.Timeout = 0
	invalidGather := valid
	invalidGather.GatherCostMin, invalidGather.GatherCostMax = 2, 1
	invalidFirstTouch := valid
	invalidFirstTouch.FirstTouchAgeMin = 0
	for _, test := range []struct {
		name     string
		capacity int
		maxAge   time.Duration
		key      DevicePoolKey
		open     DeviceSessionOpener
	}{
		{name: "negative capacity", capacity: -1, key: valid, open: opener.Open},
		{name: "large capacity", capacity: 33, key: valid, open: opener.Open},
		{name: "negative age", capacity: 1, maxAge: -1, key: valid, open: opener.Open},
		{name: "incomplete key", capacity: 1, key: DevicePoolKey{}, open: opener.Open},
		{name: "invalid timeout", capacity: 1, key: invalidTimeout, open: opener.Open},
		{name: "invalid gather range", capacity: 1, key: invalidGather, open: opener.Open},
		{name: "invalid first touch range", capacity: 1, key: invalidFirstTouch, open: opener.Open},
		{name: "nil opener", capacity: 1, key: valid},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewDeviceSessionPool(test.capacity, test.maxAge, test.key, test.open); err == nil {
				t.Fatal("invalid pool configuration was accepted")
			}
		})
	}

	disabled, err := NewDeviceSessionPool(0, 0, DevicePoolKey{}, nil)
	if err != nil || disabled.maxAge != DefaultDeviceSessionMaxAge {
		t.Fatalf("disabled/default pool: age=%v err=%v", disabled.maxAge, err)
	}
	defer disabled.Close()
	if err := disabled.Prime(context.Background()); err != nil || opener.calls.Load() != 0 {
		t.Fatalf("disabled Prime opened a session: calls=%d err=%v", opener.calls.Load(), err)
	}
}

func TestDeviceSessionPoolPrimeIsParallelAndBounded(t *testing.T) {
	gate := make(chan struct{})
	opener := newPoolTestOpener()
	opener.gate = gate
	pool, err := NewDeviceSessionPool(4, time.Second, poolTestKey("", "fixed"), opener.Open)
	if err != nil {
		t.Fatal(err)
	}
	var closed atomic.Int32
	pool.closeSession = func(session *device.Session) {
		closed.Add(1)
		session.Close()
	}

	result := make(chan error, 1)
	go func() { result <- pool.Prime(context.Background()) }()
	for range 4 {
		<-opener.started
	}
	if opener.calls.Load() != 4 || opener.maxActive.Load() != 4 {
		t.Fatalf("Prime was not parallel: calls=%d max-active=%d", opener.calls.Load(), opener.maxActive.Load())
	}
	close(gate)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	ready, pending, leased, _ := poolCounts(pool)
	if ready != 4 || pending != 0 || leased != 0 {
		t.Fatalf("unexpected stock: ready=%d pending=%d leased=%d", ready, pending, leased)
	}
	if err := pool.Prime(context.Background()); err != nil || opener.calls.Load() != 4 {
		t.Fatalf("full pool opened beyond capacity: calls=%d err=%v", opener.calls.Load(), err)
	}
	pool.Close()
	if closed.Load() != 4 {
		t.Fatalf("Close cleaned %d sessions, want 4", closed.Load())
	}
}

func TestDeviceSessionPoolLeaseSeparatesKeysAndRefillsOnConsumption(t *testing.T) {
	fixed := newPoolTestOpener()
	key := poolTestKey("", "fixed-profile")
	pool, err := NewDeviceSessionPool(2, time.Second, key, fixed.Open)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := pool.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}

	cold := newPoolTestOpener()
	mismatch := key
	mismatch.Route = "http://proxy.invalid:8080"
	session, release, err := pool.Lease(context.Background(), mismatch, cold.Open)
	if err != nil {
		t.Fatal(err)
	}
	if session == nil {
		t.Fatal("mismatched cold Lease returned nil session")
	}
	release()
	ready, _, _, _ := poolCounts(pool)
	if cold.calls.Load() != 1 || fixed.calls.Load() != 2 || ready != 2 {
		t.Fatalf("mismatched key touched stock: cold=%d fixed=%d ready=%d", cold.calls.Load(), fixed.calls.Load(), ready)
	}

	session, release, err = pool.Lease(context.Background(), key, cold.Open)
	if err != nil {
		t.Fatal(err)
	}
	if session == nil {
		t.Fatal("matching pooled Lease returned nil session")
	}
	ready, pending, leased, _ := poolCounts(pool)
	if fixed.calls.Load() != 2 || ready != 1 || pending != 0 || leased != 1 {
		t.Fatalf("lease refilled before release: calls=%d ready=%d pending=%d leased=%d", fixed.calls.Load(), ready, pending, leased)
	}
	release()
	waitForPool(t, "one-for-one refill", func() bool {
		ready, pending, leased, _ := poolCounts(pool)
		return fixed.calls.Load() == 3 && ready == 2 && pending == 0 && leased == 0
	})
	if cold.calls.Load() != 1 {
		t.Fatalf("stocked matching lease used cold opener %d times", cold.calls.Load())
	}

	pool.Close()
	session, release, err = pool.Lease(context.Background(), key, cold.Open)
	if err != nil {
		t.Fatal(err)
	}
	if session == nil {
		t.Fatal("post-Close cold Lease returned nil session")
	}
	release()
	if cold.calls.Load() != 2 || fixed.calls.Load() != 3 {
		t.Fatalf("closed pool did not stay cold: cold=%d fixed=%d", cold.calls.Load(), fixed.calls.Load())
	}
}

func TestDeviceSessionPoolReleaseBoundsLeasedAndRefillWork(t *testing.T) {
	opener := newPoolTestOpener()
	key := poolTestKey("", "fixed")
	pool, err := NewDeviceSessionPool(4, time.Second, key, opener.Open)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := pool.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}

	// 补货故意卡住，便于在并发任务完成前核对总量不变式。
	refillGate := make(chan struct{})
	opener.gate = refillGate
	releases := make([]func(), 0, 4)
	for range 4 {
		session, release, leaseErr := pool.Lease(context.Background(), key, opener.Open)
		if leaseErr != nil || session == nil {
			t.Fatalf("Lease: session=%v err=%v", session, leaseErr)
		}
		releases = append(releases, release)
	}
	ready, pending, leased, _ := poolCounts(pool)
	if ready != 0 || pending != 0 || leased != 4 || opener.calls.Load() != 4 {
		t.Fatalf("leased sessions refilled early: ready=%d pending=%d leased=%d calls=%d", ready, pending, leased, opener.calls.Load())
	}

	for index, release := range releases {
		release()
		release() // release 必须幂等，不能重复补货。
		ready, pending, leased, _ = poolCounts(pool)
		if total := ready + pending + leased; total > 4 {
			t.Fatalf("release %d exceeded capacity: ready=%d pending=%d leased=%d", index, ready, pending, leased)
		}
	}
	waitForPool(t, "bounded blocked refills", func() bool {
		ready, pending, leased, _ := poolCounts(pool)
		return opener.calls.Load() == 8 && ready == 0 && pending == 4 && leased == 0
	})
	if opener.maxActive.Load() > 4 {
		t.Fatalf("refill work exceeded capacity: max-active=%d", opener.maxActive.Load())
	}
	close(refillGate)
	waitForPool(t, "refill completion", func() bool {
		ready, pending, leased, _ := poolCounts(pool)
		return ready == 4 && pending == 0 && leased == 0
	})
}

func TestDeviceSessionPoolCloseOwnsOutstandingLease(t *testing.T) {
	opener := newPoolTestOpener()
	key := poolTestKey("", "fixed")
	pool, err := NewDeviceSessionPool(1, time.Second, key, opener.Open)
	if err != nil {
		t.Fatal(err)
	}
	var closed atomic.Int32
	pool.closeSession = func(session *device.Session) {
		closed.Add(1)
		session.Close()
	}
	if err := pool.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, release, err := pool.Lease(context.Background(), key, opener.Open)
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	release()
	release()
	ready, pending, leased, isClosed := poolCounts(pool)
	if !isClosed || ready != 0 || pending != 0 || leased != 0 || closed.Load() != 1 || opener.calls.Load() != 1 {
		t.Fatalf("outstanding lease cleanup: closed=%v ready=%d pending=%d leased=%d closes=%d calls=%d", isClosed, ready, pending, leased, closed.Load(), opener.calls.Load())
	}
}

func TestDeviceSessionPoolCloseWaitsForConcurrentRelease(t *testing.T) {
	opener := newPoolTestOpener()
	key := poolTestKey("", "fixed")
	pool, err := NewDeviceSessionPool(1, time.Second, key, opener.Open)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, release, err := pool.Lease(context.Background(), key, opener.Open)
	if err != nil {
		t.Fatal(err)
	}
	closing := make(chan struct{})
	allowClose := make(chan struct{})
	pool.closeSession = func(session *device.Session) {
		close(closing)
		<-allowClose
		session.Close()
	}
	released := make(chan struct{})
	go func() {
		release()
		close(released)
	}()
	<-closing
	poolClosed := make(chan struct{})
	go func() {
		pool.Close()
		close(poolClosed)
	}()
	waitForPool(t, "Close entering shutdown", func() bool {
		_, _, _, closed := poolCounts(pool)
		return closed
	})
	select {
	case <-poolClosed:
		t.Fatal("Close returned before concurrent release closed its session")
	default:
	}
	close(allowClose)
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("release did not finish")
	}
	select {
	case <-poolClosed:
	case <-time.After(time.Second):
		t.Fatal("Close did not finish after release")
	}
}

func TestDeviceSessionPoolExpiredIdleColdLeaseRestoresCapacity(t *testing.T) {
	fixed := newPoolTestOpener()
	key := poolTestKey("", "fixed")
	pool, err := NewDeviceSessionPool(2, 10*time.Second, key, fixed.Open)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var nowUnixNano atomic.Int64
	base := time.Unix(1_700_000_000, 0)
	nowUnixNano.Store(base.UnixNano())
	pool.now = func() time.Time { return time.Unix(0, nowUnixNano.Load()) }
	var closed atomic.Int32
	pool.closeSession = func(session *device.Session) {
		closed.Add(1)
		session.Close()
	}
	if err := pool.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	nowUnixNano.Store(base.Add(10 * time.Second).UnixNano())
	cold := newPoolTestOpener()
	session, release, err := pool.Lease(context.Background(), key, cold.Open)
	if err != nil {
		t.Fatal(err)
	}
	if session == nil {
		t.Fatal("expired cold Lease returned nil session")
	}
	waitForPool(t, "first expired recovery refill", func() bool { return fixed.calls.Load() == 3 })
	ready, pending, leased, _ := poolCounts(pool)
	if total := ready + pending + leased; total != 2 || leased != 1 {
		t.Fatalf("cold Lease recovery reservations: ready=%d pending=%d leased=%d", ready, pending, leased)
	}
	release()
	waitForPool(t, "expired stock recovery", func() bool {
		ready, pending, leased, _ := poolCounts(pool)
		return fixed.calls.Load() == 4 && ready == 2 && pending == 0 && leased == 0
	})
	ready, pending, leased, _ = poolCounts(pool)
	if closed.Load() != 3 || cold.calls.Load() != 1 || fixed.calls.Load() != 4 || ready != 2 || pending != 0 || leased != 0 {
		t.Fatalf("expired recovery: closed=%d cold=%d fixed=%d ready=%d pending=%d leased=%d", closed.Load(), cold.calls.Load(), fixed.calls.Load(), ready, pending, leased)
	}
	for range 20 {
		runtime.Gosched()
	}
	if fixed.calls.Load() != 4 {
		t.Fatalf("expired recovery entered a retry loop: calls=%d", fixed.calls.Load())
	}
}

func TestDeviceSessionPoolCloseCancelsExpiredColdRecovery(t *testing.T) {
	fixed := newPoolTestOpener()
	key := poolTestKey("", "fixed")
	pool, err := NewDeviceSessionPool(2, 10*time.Second, key, fixed.Open)
	if err != nil {
		t.Fatal(err)
	}
	var nowUnixNano atomic.Int64
	base := time.Unix(1_700_000_000, 0)
	nowUnixNano.Store(base.UnixNano())
	pool.now = func() time.Time { return time.Unix(0, nowUnixNano.Load()) }
	if err := pool.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	nowUnixNano.Store(base.Add(10 * time.Second).UnixNano())

	// 使本次冷建和一个并行补货都处于进行中，再与 Close 竞态。
	refillGate := make(chan struct{})
	fixed.gate = refillGate
	cold := newPoolTestOpener()
	coldGate := make(chan struct{})
	cold.gate = coldGate
	leaseResult := make(chan error, 1)
	go func() {
		_, _, leaseErr := pool.Lease(context.Background(), key, cold.Open)
		leaseResult <- leaseErr
	}()
	<-cold.started
	waitForPool(t, "expired refill start", func() bool { return fixed.calls.Load() == 3 })
	ready, pending, leased, _ := poolCounts(pool)
	if total := ready + pending + leased; total != 2 {
		t.Fatalf("recovery reservations violated capacity: ready=%d pending=%d leased=%d", ready, pending, leased)
	}

	closed := make(chan struct{})
	go func() {
		pool.Close()
		close(closed)
	}()
	select {
	case err := <-leaseResult:
		if !errors.Is(err, ErrDeviceSessionPoolClosed) {
			t.Fatalf("cold Lease after Close=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel reserved cold Lease")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not wait for recovery tasks")
	}
	ready, pending, leased, isClosed := poolCounts(pool)
	if !isClosed || ready != 0 || pending != 0 || leased != 0 {
		t.Fatalf("recovery Close state: closed=%v ready=%d pending=%d leased=%d", isClosed, ready, pending, leased)
	}
}

func TestDeviceSessionPoolCloseBetweenReservedOpenAndCommit(t *testing.T) {
	for _, test := range []struct {
		name          string
		cancelCaller  bool
		expectedError error
	}{
		{name: "pool close", expectedError: ErrDeviceSessionPoolClosed},
		{name: "caller cancellation wins", cancelCaller: true, expectedError: context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			opener := newPoolTestOpener()
			key := poolTestKey("", "fixed")
			pool, err := NewDeviceSessionPool(1, time.Second, key, opener.Open)
			if err != nil {
				t.Fatal(err)
			}
			commitReached := make(chan struct{})
			allowCommit := make(chan struct{})
			pool.beforeReservedCommit = func() {
				close(commitReached)
				<-allowCommit
			}
			var sessionCloses atomic.Int32
			pool.closeSession = func(session *device.Session) {
				sessionCloses.Add(1)
				session.Close()
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			type leaseOutcome struct {
				session *device.Session
				release func()
				err     error
			}
			leaseResult := make(chan leaseOutcome, 1)
			go func() {
				session, release, leaseErr := pool.Lease(ctx, key, opener.Open)
				leaseResult <- leaseOutcome{session: session, release: release, err: leaseErr}
			}()
			<-commitReached
			if test.cancelCaller {
				cancel()
			}
			poolClosed := make(chan struct{})
			go func() {
				pool.Close()
				close(poolClosed)
			}()
			waitForPool(t, "reserved commit Close", func() bool {
				_, _, _, closed := poolCounts(pool)
				return closed
			})
			close(allowCommit)

			select {
			case outcome := <-leaseResult:
				if !errors.Is(outcome.err, test.expectedError) {
					t.Fatalf("Lease error=%v, want %v", outcome.err, test.expectedError)
				}
				if outcome.session != nil || outcome.release != nil {
					t.Fatalf("closed pool returned unmanaged session=%v release=%v", outcome.session, outcome.release != nil)
				}
			case <-time.After(time.Second):
				t.Fatal("reserved Lease did not finish")
			}
			select {
			case <-poolClosed:
			case <-time.After(time.Second):
				t.Fatal("Close did not finish after reserved commit")
			}
			ready, pending, leased, closed := poolCounts(pool)
			if !closed || ready != 0 || pending != 0 || leased != 0 || sessionCloses.Load() != 1 {
				t.Fatalf("commit race cleanup: closed=%v ready=%d pending=%d leased=%d closes=%d", closed, ready, pending, leased, sessionCloses.Load())
			}
		})
	}
}

func TestDeviceSessionPoolExpiredColdFailureWaitsForDemandBeforeRecovery(t *testing.T) {
	fixed := newPoolTestOpener()
	key := poolTestKey("", "fixed")
	pool, err := NewDeviceSessionPool(2, 10*time.Second, key, fixed.Open)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var nowUnixNano atomic.Int64
	base := time.Unix(1_700_000_000, 0)
	nowUnixNano.Store(base.UnixNano())
	pool.now = func() time.Time { return time.Unix(0, nowUnixNano.Load()) }
	if err := pool.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	nowUnixNano.Store(base.Add(10 * time.Second).UnixNano())

	sentinel := errors.New("cold open failed")
	cold := newPoolTestOpener()
	cold.result = func(int) (*device.Session, error) { return nil, sentinel }
	if _, _, err := pool.Lease(context.Background(), key, cold.Open); !errors.Is(err, sentinel) {
		t.Fatalf("cold Lease failure=%v", err)
	}
	waitForPool(t, "failed cold Lease batch completion", func() bool {
		ready, pending, leased, _ := poolCounts(pool)
		return fixed.calls.Load() == 3 && ready == 1 && pending == 0 && leased == 0
	})
	for range 20 {
		runtime.Gosched()
	}
	if fixed.calls.Load() != 3 || cold.calls.Load() != 1 {
		t.Fatalf("failed cold recovery retried: fixed=%d cold=%d", fixed.calls.Load(), cold.calls.Load())
	}

	// 新需求消费剩余 ready 后，才恢复上一批失败留下的缺口。
	session, release, err := pool.Lease(context.Background(), key, fixed.Open)
	if err != nil || session == nil {
		t.Fatalf("demand recovery Lease: session=%v err=%v", session, err)
	}
	release()
	waitForPool(t, "demand-triggered recovery", func() bool {
		ready, pending, leased, _ := poolCounts(pool)
		return fixed.calls.Load() == 5 && ready == 2 && pending == 0 && leased == 0
	})
}

func TestDeviceSessionPoolThirtyTwoLeasesWaitForSamePendingBatch(t *testing.T) {
	gate := make(chan struct{})
	fixed := newPoolTestOpener()
	fixed.gate = gate
	key := poolTestKey("", "fixed")
	pool, err := NewDeviceSessionPool(32, time.Second, key, fixed.Open)
	if err != nil {
		t.Fatal(err)
	}
	primeResult := make(chan error, 1)
	go func() { primeResult <- pool.Prime(context.Background()) }()
	for range 32 {
		<-fixed.started
	}

	cold := newPoolTestOpener()
	type leaseOutcome struct {
		session *device.Session
		release func()
		err     error
	}
	leaseResults := make(chan leaseOutcome, 32)
	start := make(chan struct{})
	for range 32 {
		go func() {
			<-start
			session, release, leaseErr := pool.Lease(context.Background(), key, cold.Open)
			leaseResults <- leaseOutcome{session: session, release: release, err: leaseErr}
		}()
	}
	close(start)
	waitForPool(t, "32 pending waiters", func() bool { return poolWaiters(pool) == 32 })
	if cold.calls.Load() != 0 {
		t.Fatalf("pending waiters cold-opened %d sessions", cold.calls.Load())
	}
	close(gate)
	if err := <-primeResult; err != nil {
		t.Fatal(err)
	}

	releases := make([]func(), 0, 32)
	for range 32 {
		select {
		case outcome := <-leaseResults:
			if outcome.err != nil || outcome.session == nil || outcome.release == nil {
				t.Fatalf("pending waiter result: session=%v release=%v err=%v", outcome.session, outcome.release != nil, outcome.err)
			}
			releases = append(releases, outcome.release)
		case <-time.After(2 * time.Second):
			t.Fatal("pending waiter did not receive a prepared session")
		}
	}
	ready, pending, leased, _ := poolCounts(pool)
	if cold.calls.Load() != 0 || fixed.calls.Load() != 32 || poolWaiters(pool) != 0 || ready != 0 || pending != 0 || leased != 32 {
		t.Fatalf("shared pending batch: cold=%d fixed=%d waiters=%d ready=%d pending=%d leased=%d", cold.calls.Load(), fixed.calls.Load(), poolWaiters(pool), ready, pending, leased)
	}
	pool.Close()
	for _, release := range releases {
		release()
	}
}

func TestDeviceSessionPoolPendingFailureAllowsOneReservedCold(t *testing.T) {
	gate := make(chan struct{})
	sentinel := errors.New("pending open failed")
	fixed := newPoolTestOpener()
	fixed.gate = gate
	fixed.result = func(int) (*device.Session, error) { return nil, sentinel }
	key := poolTestKey("", "fixed")
	pool, err := NewDeviceSessionPool(1, time.Second, key, fixed.Open)
	if err != nil {
		t.Fatal(err)
	}
	primeResult := make(chan error, 1)
	go func() { primeResult <- pool.Prime(context.Background()) }()
	<-fixed.started
	cold := newPoolTestOpener()
	leaseResult := make(chan struct {
		session *device.Session
		release func()
		err     error
	}, 1)
	go func() {
		session, release, leaseErr := pool.Lease(context.Background(), key, cold.Open)
		leaseResult <- struct {
			session *device.Session
			release func()
			err     error
		}{session, release, leaseErr}
	}()
	waitForPool(t, "failed pending waiter", func() bool { return poolWaiters(pool) == 1 })
	if cold.calls.Load() != 0 {
		t.Fatalf("Lease cold-opened before pending completed: %d", cold.calls.Load())
	}
	close(gate)
	if err := <-primeResult; !errors.Is(err, sentinel) {
		t.Fatalf("Prime failure=%v", err)
	}
	var outcome = <-leaseResult
	if outcome.err != nil || outcome.session == nil || outcome.release == nil {
		t.Fatalf("reserved cold result: session=%v release=%v err=%v", outcome.session, outcome.release != nil, outcome.err)
	}
	if fixed.calls.Load() != 1 || cold.calls.Load() != 1 {
		t.Fatalf("pending failure recovery calls: fixed=%d cold=%d", fixed.calls.Load(), cold.calls.Load())
	}
	pool.Close()
	outcome.release()
}

func TestDeviceSessionPoolPendingWaitCancellationAndClose(t *testing.T) {
	t.Run("caller cancellation", func(t *testing.T) {
		gate := make(chan struct{})
		fixed := newPoolTestOpener()
		fixed.gate = gate
		key := poolTestKey("", "fixed")
		pool, err := NewDeviceSessionPool(1, time.Second, key, fixed.Open)
		if err != nil {
			t.Fatal(err)
		}
		primeResult := make(chan error, 1)
		go func() { primeResult <- pool.Prime(context.Background()) }()
		<-fixed.started
		cold := newPoolTestOpener()
		ctx, cancel := context.WithCancel(context.Background())
		leaseResult := make(chan error, 1)
		go func() {
			_, _, leaseErr := pool.Lease(ctx, key, cold.Open)
			leaseResult <- leaseErr
		}()
		waitForPool(t, "cancelable pending waiter", func() bool { return poolWaiters(pool) == 1 })
		cancel()
		if err := <-leaseResult; !errors.Is(err, context.Canceled) {
			t.Fatalf("pending wait cancellation=%v", err)
		}
		if cold.calls.Load() != 0 || poolWaiters(pool) != 0 {
			t.Fatalf("canceled waiter: cold=%d waiters=%d", cold.calls.Load(), poolWaiters(pool))
		}
		pool.Close()
		if err := <-primeResult; !errors.Is(err, ErrDeviceSessionPoolClosed) {
			t.Fatalf("Prime after Close=%v", err)
		}
	})

	t.Run("pool Close", func(t *testing.T) {
		gate := make(chan struct{})
		fixed := newPoolTestOpener()
		fixed.gate = gate
		key := poolTestKey("", "fixed")
		pool, err := NewDeviceSessionPool(1, time.Second, key, fixed.Open)
		if err != nil {
			t.Fatal(err)
		}
		primeResult := make(chan error, 1)
		go func() { primeResult <- pool.Prime(context.Background()) }()
		<-fixed.started
		cold := newPoolTestOpener()
		leaseResult := make(chan error, 1)
		go func() {
			_, _, leaseErr := pool.Lease(context.Background(), key, cold.Open)
			leaseResult <- leaseErr
		}()
		waitForPool(t, "closable pending waiter", func() bool { return poolWaiters(pool) == 1 })
		pool.Close()
		if err := <-leaseResult; !errors.Is(err, ErrDeviceSessionPoolClosed) {
			t.Fatalf("pending wait Close=%v", err)
		}
		if err := <-primeResult; !errors.Is(err, ErrDeviceSessionPoolClosed) {
			t.Fatalf("Prime Close=%v", err)
		}
		if cold.calls.Load() != 0 {
			t.Fatalf("closed pending waiter cold-opened %d sessions", cold.calls.Load())
		}
		waitForPool(t, "closed waiter cleanup", func() bool { return poolWaiters(pool) == 0 })
	})
}

func TestDeviceSessionPoolCancellationAndFailures(t *testing.T) {
	t.Run("Prime cancellation", func(t *testing.T) {
		gate := make(chan struct{})
		opener := newPoolTestOpener()
		opener.gate = gate
		pool, err := NewDeviceSessionPool(3, time.Second, poolTestKey("", "fixed"), opener.Open)
		if err != nil {
			t.Fatal(err)
		}
		defer pool.Close()
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() { result <- pool.Prime(ctx) }()
		for range 3 {
			<-opener.started
		}
		cancel()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("Prime cancellation=%v", err)
		}
		waitForPool(t, "canceled Prime cleanup", func() bool {
			ready, pending, leased, _ := poolCounts(pool)
			return ready == 0 && pending == 0 && leased == 0
		})
	})

	t.Run("canceled Prime result channel does not block Close", func(t *testing.T) {
		gate := make(chan struct{})
		started := make(chan struct{}, 2)
		opener := func(context.Context) (*device.Session, error) {
			started <- struct{}{}
			<-gate
			return new(device.Session), nil
		}
		pool, err := NewDeviceSessionPool(2, time.Second, poolTestKey("", "fixed"), opener)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() { result <- pool.Prime(ctx) }()
		<-started
		<-started
		cancel()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("Prime cancellation=%v", err)
		}
		close(gate)
		closed := make(chan struct{})
		go func() {
			pool.Close()
			close(closed)
		}()
		select {
		case <-closed:
		case <-time.After(time.Second):
			t.Fatal("Close blocked on abandoned Prime result delivery")
		}
	})

	t.Run("cold Lease cancellation", func(t *testing.T) {
		gate := make(chan struct{})
		opener := newPoolTestOpener()
		opener.gate = gate
		pool, err := NewDeviceSessionPool(0, 0, DevicePoolKey{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer pool.Close()
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() {
			_, _, leaseErr := pool.Lease(ctx, poolTestKey("", "cold"), opener.Open)
			result <- leaseErr
		}()
		<-opener.started
		cancel()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("Lease cancellation=%v", err)
		}
	})

	t.Run("open failures", func(t *testing.T) {
		sentinel := errors.New("offline failure")
		opener := newPoolTestOpener()
		opener.result = func(call int) (*device.Session, error) {
			if call == 1 {
				return nil, sentinel
			}
			if call == 2 {
				return nil, nil
			}
			return new(device.Session), nil
		}
		pool, err := NewDeviceSessionPool(3, time.Second, poolTestKey("", "fixed"), opener.Open)
		if err != nil {
			t.Fatal(err)
		}
		defer pool.Close()
		err = pool.Prime(context.Background())
		if !errors.Is(err, sentinel) || err == nil {
			t.Fatalf("Prime failure=%v", err)
		}
		ready, pending, leased, _ := poolCounts(pool)
		if ready != 1 || pending != 0 || leased != 0 {
			t.Fatalf("partial failure stock: ready=%d pending=%d leased=%d", ready, pending, leased)
		}
	})

	t.Run("failed refill is not retried", func(t *testing.T) {
		sentinel := errors.New("refill failure")
		opener := newPoolTestOpener()
		opener.result = func(call int) (*device.Session, error) {
			if call > 1 {
				return nil, sentinel
			}
			return new(device.Session), nil
		}
		key := poolTestKey("", "fixed")
		pool, err := NewDeviceSessionPool(1, time.Second, key, opener.Open)
		if err != nil {
			t.Fatal(err)
		}
		defer pool.Close()
		if err := pool.Prime(context.Background()); err != nil {
			t.Fatal(err)
		}
		session, release, err := pool.Lease(context.Background(), key, opener.Open)
		if err != nil {
			t.Fatal(err)
		}
		if session == nil {
			t.Fatal("matching Lease returned nil session")
		}
		release()
		waitForPool(t, "failed refill", func() bool {
			_, pending, _, _ := poolCounts(pool)
			return opener.calls.Load() == 2 && pending == 0
		})
		for range 20 {
			runtime.Gosched()
		}
		ready, _, _, _ := poolCounts(pool)
		if opener.calls.Load() != 2 || ready != 0 {
			t.Fatalf("failed refill looped: calls=%d ready=%d", opener.calls.Load(), ready)
		}
	})
}

func TestDeviceSessionPoolConcurrentCloseAndLease(t *testing.T) {
	key := poolTestKey("", "fixed")
	opener := newPoolTestOpener()
	pool, err := NewDeviceSessionPool(8, time.Second, key, opener.Open)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	var workers sync.WaitGroup
	errorsSeen := make(chan error, 128)
	for range 96 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			session, release, leaseErr := pool.Lease(context.Background(), key, opener.Open)
			if leaseErr != nil {
				// 调用已进入 reservation 后与 Close 竞态时，不返回未纳管会话。
				if errors.Is(leaseErr, ErrDeviceSessionPoolClosed) {
					return
				}
				errorsSeen <- leaseErr
				return
			}
			if session == nil {
				errorsSeen <- errors.New("concurrent Lease returned nil session")
				return
			}
			release()
		}()
	}
	for range 16 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			pool.Close()
		}()
	}
	close(start)
	workers.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		t.Errorf("concurrent Lease: %v", err)
	}
	ready, pending, leased, closed := poolCounts(pool)
	if !closed || ready != 0 || pending != 0 || leased != 0 {
		t.Fatalf("closed pool state: closed=%v ready=%d pending=%d leased=%d", closed, ready, pending, leased)
	}
	if err := pool.Prime(context.Background()); !errors.Is(err, ErrDeviceSessionPoolClosed) {
		t.Fatalf("Prime after Close=%v", err)
	}

	cold := newPoolTestOpener()
	session, release, err := pool.Lease(context.Background(), key, cold.Open)
	if err != nil {
		t.Fatal(err)
	}
	if session == nil {
		t.Fatal("post-Close cold Lease returned nil session")
	}
	release()
	if cold.calls.Load() != 1 {
		t.Fatalf("Lease after Close did not use cold opener: %d", cold.calls.Load())
	}
}

func TestNilDeviceSessionPoolFallsBackToColdOpen(t *testing.T) {
	var pool *DeviceSessionPool
	opener := newPoolTestOpener()
	session, release, err := pool.Lease(context.Background(), poolTestKey("", "cold"), opener.Open)
	if err != nil {
		t.Fatal(err)
	}
	if session == nil {
		t.Fatal("nil pool cold Lease returned nil session")
	}
	release()
	pool.Close()
	if opener.calls.Load() != 1 {
		t.Fatalf("nil pool cold calls=%d", opener.calls.Load())
	}
}

func TestRuntimeDeviceSessionPoolPreservesInterfaceSessionLifecycle(t *testing.T) {
	key := poolTestKey("", "runtime-profile")
	var opens atomic.Int32
	var closes atomic.Int32
	opener := func(context.Context) (DeviceSession, error) {
		opens.Add(1)
		return &poolRuntimeSession{closes: &closes}, nil
	}
	pool, err := NewRuntimeDeviceSessionPool(1, time.Second, key, opener)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Prime(context.Background()); err != nil {
		pool.Close()
		t.Fatal(err)
	}
	session, release, err := pool.LeaseRuntime(context.Background(), key, func(context.Context) (DeviceSession, error) {
		return nil, errors.New("unexpected cold open")
	})
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	if _, ok := session.(*poolRuntimeSession); !ok {
		pool.Close()
		t.Fatalf("runtime session type=%T", session)
	}
	release()
	release()
	waitForPool(t, "runtime refill", func() bool {
		ready, pending, leased, _ := poolCounts(pool)
		return ready == 1 && pending == 0 && leased == 0 && opens.Load() == 2
	})
	pool.Close()
	if closes.Load() != 2 {
		t.Fatalf("runtime session closes=%d, want 2", closes.Load())
	}
}
