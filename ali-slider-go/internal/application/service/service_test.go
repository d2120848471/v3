package service

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/application/solve"
)

type executorFunc func(context.Context, solve.Request) (solve.Outcome, error)

func (function executorFunc) Solve(ctx context.Context, request solve.Request) (solve.Outcome, error) {
	return function(ctx, request)
}

type resourceFixture struct {
	check  func() error
	purge  func() (int, error)
	close  func()
	closes atomic.Int32
}

func (fixture *resourceFixture) CheckRuntime() error {
	if fixture.check != nil {
		return fixture.check()
	}
	return nil
}
func (fixture *resourceFixture) PurgeArtifacts() (int, error) {
	if fixture.purge != nil {
		return fixture.purge()
	}
	return 0, nil
}
func (fixture *resourceFixture) Close() {
	fixture.closes.Add(1)
	if fixture.close != nil {
		fixture.close()
	}
}

func newServiceFixture(t *testing.T, executor Executor, resources Resources) *Service {
	t.Helper()
	application, err := New(Options{Executor: executor, Resources: resources})
	if err != nil {
		t.Fatal(err)
	}
	return application
}

func emptyExecutor(context.Context, solve.Request) (solve.Outcome, error) {
	return solve.Outcome{}, nil
}

func TestNewRequiresExecutorAndResources(t *testing.T) {
	for _, options := range []Options{{}, {Executor: executorFunc(emptyExecutor)}, {Resources: &resourceFixture{}}} {
		if _, err := New(options); err == nil {
			t.Fatal("missing dependency accepted")
		}
	}
}

func TestCloseWaitsForEveryInflightOperation(t *testing.T) {
	for _, operation := range []string{"solve", "runtime", "purge"} {
		t.Run(operation, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			block := func() { close(entered); <-release }
			resources := &resourceFixture{}
			executor := executorFunc(emptyExecutor)
			var call func(*Service) error
			switch operation {
			case "solve":
				executor = func(context.Context, solve.Request) (solve.Outcome, error) { block(); return solve.Outcome{}, nil }
				call = func(application *Service) error {
					_, err := application.Solve(context.Background(), solve.Request{})
					return err
				}
			case "runtime":
				resources.check = func() error { block(); return nil }
				call = func(application *Service) error { return application.CheckRuntime() }
			case "purge":
				resources.purge = func() (int, error) { block(); return 2, nil }
				call = func(application *Service) error { _, err := application.PurgeArtifacts(); return err }
			}
			application := newServiceFixture(t, executor, resources)
			workDone := make(chan error, 1)
			go func() { workDone <- call(application) }()
			<-entered
			closeDone := make(chan struct{})
			go func() { application.Close(); close(closeDone) }()
			awaitClosing(t, application)
			assertUnavailable(t, application)
			if resources.closes.Load() != 0 {
				t.Fatal("resources closed while work was active")
			}
			select {
			case <-closeDone:
				t.Fatal("Close returned while work was active")
			default:
			}
			unblock()
			select {
			case err := <-workDone:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("work did not finish")
			}
			select {
			case <-closeDone:
			case <-time.After(time.Second):
				t.Fatal("Close did not finish")
			}
			if got := resources.closes.Load(); got != 1 {
				t.Fatalf("resources closed %d times", got)
			}
		})
	}
}

func TestConcurrentCloseWaitsForResources(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	resources := &resourceFixture{close: func() { close(entered); <-release }}
	application := newServiceFixture(t, executorFunc(emptyExecutor), resources)
	const callers = 8
	finished := make(chan struct{}, callers)
	for range callers {
		go func() { application.Close(); finished <- struct{}{} }()
	}
	<-entered
	select {
	case <-finished:
		t.Fatal("Close returned before resources finished closing")
	default:
	}
	unblock()
	for range callers {
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Fatal("concurrent Close did not finish")
		}
	}
	if got := resources.closes.Load(); got != 1 {
		t.Fatalf("resources closed %d times", got)
	}
}

func TestNilZeroAndClosedServiceRejectWork(t *testing.T) {
	closed := newServiceFixture(t, executorFunc(emptyExecutor), &resourceFixture{})
	closed.Close()
	for _, application := range []*Service{nil, {}, closed} {
		assertUnavailable(t, application)
		application.Close()
	}
}

func TestResourceFailuresPreserveCauseAndCounts(t *testing.T) {
	cause := errors.New("resource failure")
	application := newServiceFixture(t, executorFunc(emptyExecutor), &resourceFixture{
		check: func() error { return cause },
		purge: func() (int, error) { return 3, cause },
	})
	defer application.Close()
	var failure *solve.Failure
	err := application.CheckRuntime()
	if !errors.As(err, &failure) || failure.Kind != solve.FailureInternal || failure.Stage != "runtime" || failure.Message != "V8 runtime 校验失败" || !errors.Is(err, cause) {
		t.Fatalf("runtime error=%v", err)
	}
	if removed, err := application.PurgeArtifacts(); removed != 3 || err != cause {
		t.Fatalf("purge removed=%d error=%v", removed, err)
	}
}

func TestExecutorPanicReleasesInflightWork(t *testing.T) {
	resources := &resourceFixture{}
	application := newServiceFixture(t, executorFunc(func(context.Context, solve.Request) (solve.Outcome, error) {
		panic("fixture")
	}), resources)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("executor panic was swallowed")
			}
		}()
		_, _ = application.Solve(context.Background(), solve.Request{})
	}()
	done := make(chan struct{})
	go func() { application.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("panicked Solve leaked inflight work")
	}
}

func awaitClosing(t *testing.T, application *Service) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		application.mu.Lock()
		closed := application.closed
		application.mu.Unlock()
		if closed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("Close did not begin")
		}
		runtime.Gosched()
	}
}

func assertUnavailable(t *testing.T, application *Service) {
	t.Helper()
	_, solveErr := application.Solve(context.Background(), solve.Request{})
	_, purgeErr := application.PurgeArtifacts()
	for stage, err := range map[string]error{
		"client": solveErr, "prime": application.Prime(context.Background()),
		"runtime": application.CheckRuntime(), "artifacts": purgeErr,
	} {
		var failure *solve.Failure
		if !errors.As(err, &failure) || failure.Kind != solve.FailureInternal || failure.Stage != stage || failure.Message != "Client 已关闭或不可用" {
			t.Errorf("stage=%s error=%v", stage, err)
		}
	}
}
