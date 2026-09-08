package v8runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"
)

type fakeDynamicLibrary struct {
	lookupError error
	closeError  error
	closeCalls  int
}

func (library *fakeDynamicLibrary) lookup(string) (uintptr, error) {
	if library.lookupError != nil {
		return 0, library.lookupError
	}
	return 1, nil
}

func (library *fakeDynamicLibrary) close() error {
	library.closeCalls++
	return library.closeError
}

type fakeNativeResult struct {
	status int32
	data   []byte
}

type fakeNativeState struct {
	destroyCalls int
	setHostCalls int
	freeCalls    int
	terminated   int
}

func newFakeResult(status int32, data string) unsafe.Pointer {
	return unsafe.Pointer(&fakeNativeResult{status: status, data: []byte(data)})
}

func newFakeLibrary() (*Library, *fakeNativeState) {
	state := &fakeNativeState{}
	functions := nativeFunctions{
		engineVersion: func() unsafe.Pointer {
			return newFakeResult(0, `"149.4.0"`)
		},
		create: func() unsafe.Pointer {
			return unsafe.Pointer(new(byte))
		},
		destroy: func(unsafe.Pointer) {
			state.destroyCalls++
		},
		terminate: func(unsafe.Pointer) int32 {
			state.terminated++
			return 1
		},
		setHost: func(unsafe.Pointer, uintptr, uintptr) int32 {
			state.setHostCalls++
			return 1
		},
		eval: func(unsafe.Pointer, *byte, uintptr, *byte, uintptr) unsafe.Pointer {
			return newFakeResult(0, `{"kind":"eval"}`)
		},
		call: func(unsafe.Pointer, *byte, uintptr, *byte, uintptr) unsafe.Pointer {
			return newFakeResult(0, `{"kind":"call"}`)
		},
		resultStatus: func(result unsafe.Pointer) int32 {
			return (*fakeNativeResult)(result).status
		},
		resultData: func(result unsafe.Pointer) *byte {
			return firstByte((*fakeNativeResult)(result).data)
		},
		resultLength: func(result unsafe.Pointer) uintptr {
			return uintptr(len((*fakeNativeResult)(result).data))
		},
		resultFree: func(unsafe.Pointer) {
			state.freeCalls++
		},
	}
	return &Library{
		handle:    &fakeDynamicLibrary{},
		functions: functions,
		path:      "/fake/libali_slider_v8_runtime.so",
	}, state
}

func TestDefaultLibraryName(t *testing.T) {
	want := map[string]string{
		"darwin":  "libali_slider_v8_runtime.dylib",
		"linux":   "libali_slider_v8_runtime.so",
		"windows": "ali_slider_v8_runtime.dll",
	}[runtime.GOOS]
	if want == "" {
		want = "libali_slider_v8_runtime.so"
	}
	if got := DefaultLibraryName(); got != want {
		t.Fatalf("DefaultLibraryName() = %q, want %q", got, want)
	}
}

func TestOpenRejectsMissingAndDirectory(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Fatal("Open(empty) unexpectedly succeeded")
	}
	if _, err := Open(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("Open(missing) unexpectedly succeeded")
	}
	if _, err := Open(t.TempDir()); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("Open(directory) error = %v", err)
	}
	invalidLibrary := filepath.Join(t.TempDir(), DefaultLibraryName())
	if err := os.WriteFile(invalidLibrary, []byte("not a dynamic library"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(invalidLibrary); err == nil || !strings.Contains(err.Error(), "load V8 runtime library") {
		t.Fatalf("Open(invalid library) error = %v", err)
	}
}

func TestDecodeNativeError(t *testing.T) {
	err := decodeNativeError("test", []byte(`{"error":"boom"}`))
	var native *NativeError
	if !errors.As(err, &native) || native.Operation != "test" || native.Message != "boom" {
		t.Fatalf("decodeNativeError() = %#v", err)
	}
	if err := decodeNativeError("test", []byte(`{"error":""}`)); err == nil {
		t.Fatal("decodeNativeError(empty) unexpectedly succeeded")
	}
}

func TestFakeLibraryAndRuntimeLifecycle(t *testing.T) {
	library, state := newFakeLibrary()
	if got := library.Path(); got != "/fake/libali_slider_v8_runtime.so" {
		t.Fatalf("Path() = %q", got)
	}
	if got := (*Library)(nil).Path(); got != "" {
		t.Fatalf("nil Path() = %q", got)
	}
	version, err := library.Version()
	if err != nil || version != "149.4.0" {
		t.Fatalf("Version() = %q, %v", version, err)
	}
	engine, err := library.NewWithHost(func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`true`), nil
	})
	if err != nil {
		t.Fatalf("NewWithHost() error = %v", err)
	}
	if err := library.Close(); !errors.Is(err, ErrLibraryInUse) {
		t.Fatalf("Close(in use) error = %v", err)
	}
	if result, err := engine.Eval(context.Background(), "1", ""); err != nil || string(result) != `{"kind":"eval"}` {
		t.Fatalf("Eval() = %s, %v", result, err)
	}
	if result, err := engine.Call(context.Background(), "call", json.RawMessage(`{}`)); err != nil || string(result) != `{"kind":"call"}` {
		t.Fatalf("Call() = %s, %v", result, err)
	}
	if err := engine.Close(); err != nil {
		t.Fatalf("Runtime.Close() error = %v", err)
	}
	if err := engine.Close(); err != nil {
		t.Fatalf("Runtime.Close(second) error = %v", err)
	}
	if _, err := engine.Eval(context.Background(), "1", "closed.js"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Eval(closed) error = %v", err)
	}
	if state.destroyCalls != 1 || state.setHostCalls != 2 || state.freeCalls != 3 {
		t.Fatalf("native call counts = %+v", state)
	}
	if err := library.Close(); err != nil {
		t.Fatalf("Library.Close() error = %v", err)
	}
	if err := library.Close(); err != nil {
		t.Fatalf("Library.Close(second) error = %v", err)
	}
	if _, err := library.Version(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Version(closed) error = %v", err)
	}
	if _, err := library.New(); !errors.Is(err, ErrClosed) {
		t.Fatalf("New(closed) error = %v", err)
	}
}

func TestFakeRuntimeConstructionFailures(t *testing.T) {
	if _, err := (*Library)(nil).Version(); !errors.Is(err, ErrClosed) {
		t.Fatalf("nil Version() error = %v", err)
	}
	if _, err := (*Library)(nil).New(); !errors.Is(err, ErrClosed) {
		t.Fatalf("nil New() error = %v", err)
	}
	if err := (*Library)(nil).Close(); err != nil {
		t.Fatalf("nil Library.Close() error = %v", err)
	}
	if _, err := (&Library{}).NewWithHost(nil); err == nil {
		t.Fatal("NewWithHost(nil) unexpectedly succeeded")
	}

	library, _ := newFakeLibrary()
	library.functions.create = func() unsafe.Pointer { return nil }
	if _, err := library.New(); err == nil || !strings.Contains(err.Error(), "create V8 runtime failed") {
		t.Fatalf("New(create failure) error = %v", err)
	}

	library, state := newFakeLibrary()
	library.functions.setHost = func(unsafe.Pointer, uintptr, uintptr) int32 { return 0 }
	if _, err := library.NewWithHost(func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`true`), nil
	}); err == nil || !strings.Contains(err.Error(), "configure V8 host callback failed") {
		t.Fatalf("NewWithHost(set host failure) error = %v", err)
	}
	if state.destroyCalls != 1 {
		t.Fatalf("destroy calls = %d", state.destroyCalls)
	}

	library, state = newFakeLibrary()
	oldSequence := hostSequence.Load()
	hostSequence.Store(math.MaxUint64)
	_, err := library.NewWithHost(func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`true`), nil
	})
	hostSequence.Store(oldSequence)
	if err == nil || !strings.Contains(err.Error(), "host registry exhausted") {
		t.Fatalf("NewWithHost(registry exhausted) error = %v", err)
	}
	if state.destroyCalls != 1 {
		t.Fatalf("registry exhausted destroy calls = %d", state.destroyCalls)
	}
}

func TestLibraryCloseAndVersionFailures(t *testing.T) {
	library, _ := newFakeLibrary()
	library.functions.engineVersion = func() unsafe.Pointer { return newFakeResult(0, `null`) }
	if _, err := library.Version(); err == nil || !strings.Contains(err.Error(), "version result is invalid") {
		t.Fatalf("Version(invalid) error = %v", err)
	}

	handle := &fakeDynamicLibrary{closeError: errors.New("busy")}
	library, _ = newFakeLibrary()
	library.handle = handle
	if err := library.Close(); err == nil || !strings.Contains(err.Error(), "unload V8 runtime library") {
		t.Fatalf("Close(error) = %v", err)
	}
	if handle.closeCalls != 1 || library.closed {
		t.Fatalf("Close(error) state = calls %d closed %t", handle.closeCalls, library.closed)
	}
}

func TestRuntimeExecutionValidationAndCancellation(t *testing.T) {
	if _, err := (*Runtime)(nil).Eval(context.Background(), "1", "nil.js"); !errors.Is(err, ErrClosed) {
		t.Fatalf("nil Eval() error = %v", err)
	}
	if err := (*Runtime)(nil).Close(); err != nil {
		t.Fatalf("nil Runtime.Close() error = %v", err)
	}
	library, _ := newFakeLibrary()
	engine, err := library.New()
	if err != nil {
		t.Fatal(err)
	}
	//lint:ignore SA1012 此处专门验证导出边界拒绝 nil context。
	if _, err := engine.Eval(nil, "1", "nil-context.js"); err == nil || !strings.Contains(err.Error(), "context is nil") {
		t.Fatalf("Eval(nil context) error = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := engine.Call(canceled, "call", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Call(canceled) error = %v", err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}

	library, state := newFakeLibrary()
	released := make(chan struct{})
	var releaseOnce sync.Once
	library.functions.eval = func(unsafe.Pointer, *byte, uintptr, *byte, uintptr) unsafe.Pointer {
		<-released
		return newFakeResult(2, "")
	}
	library.functions.terminate = func(unsafe.Pointer) int32 {
		state.terminated++
		releaseOnce.Do(func() { close(released) })
		return 1
	}
	engine, err = library.New()
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer stop()
	if _, err := engine.Eval(ctx, "while (true) {}", "timeout.js"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Eval(timeout) error = %v", err)
	}
	if state.terminated != 1 {
		t.Fatalf("terminate calls = %d", state.terminated)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCopyResultValidation(t *testing.T) {
	library, state := newFakeLibrary()
	if _, err := library.copyResult("test", nil); err == nil || !strings.Contains(err.Error(), "null result") {
		t.Fatalf("copyResult(nil) error = %v", err)
	}

	tests := []struct {
		name   string
		status int32
		data   string
		check  func(error) bool
	}{
		{"success", 0, `{"ok":true}`, func(err error) bool { return err == nil }},
		{"invalid success", 0, `{`, func(err error) bool { return strings.Contains(fmt.Sprint(err), "not JSON") }},
		{"native error", 1, `{"error":"boom"}`, func(err error) bool {
			var native *NativeError
			return errors.As(err, &native) && native.Error() == "test: boom"
		}},
		{"invalid native error", 1, `{}`, func(err error) bool { return strings.Contains(fmt.Sprint(err), "error result is invalid") }},
		{"terminated", 2, `null`, func(err error) bool { return errors.Is(err, ErrTerminated) }},
		{"unknown", 9, `null`, func(err error) bool { return strings.Contains(fmt.Sprint(err), "unknown status 9") }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := library.copyResult("test", newFakeResult(test.status, test.data))
			if !test.check(err) {
				t.Fatalf("copyResult() = %s, %v", result, err)
			}
		})
	}

	library.functions.resultLength = func(unsafe.Pointer) uintptr { return maximumNativeResultSize + 1 }
	if _, err := library.copyResult("test", newFakeResult(0, "")); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("copyResult(oversize) error = %v", err)
	}
	library.functions.resultLength = func(unsafe.Pointer) uintptr { return 1 }
	library.functions.resultData = func(unsafe.Pointer) *byte { return nil }
	if _, err := library.copyResult("test", newFakeResult(0, "x")); err == nil || !strings.Contains(err.Error(), "null buffer") {
		t.Fatalf("copyResult(null buffer) error = %v", err)
	}
	if state.freeCalls != len(tests)+2 {
		t.Fatalf("result free calls = %d", state.freeCalls)
	}
	if firstByte(nil) != nil || firstByte([]byte{7}) == nil {
		t.Fatal("firstByte boundary mismatch")
	}
}

func TestNativeHostCallbackValidationAndResponses(t *testing.T) {
	response := make([]byte, 256)
	var responseLength uintptr
	if status := nativeHostCallback(0, nil, maximumNativeResultSize+1, &response[0], uintptr(len(response)), &responseLength); status != 1 {
		t.Fatalf("oversize request status = %d", status)
	}
	if status := nativeHostCallback(0, nil, 0, &response[0], maximumNativeResultSize+1, &responseLength); status != 1 {
		t.Fatalf("oversize response status = %d", status)
	}
	if status := nativeHostCallback(0, nil, 1, &response[0], uintptr(len(response)), &responseLength); status != 1 {
		t.Fatalf("nil request status = %d", status)
	}
	if status := nativeHostCallback(0, nil, 0, nil, uintptr(len(response)), &responseLength); status != 1 {
		t.Fatalf("nil response status = %d", status)
	}
	if status := nativeHostCallback(0, nil, 0, &response[0], uintptr(len(response)), nil); status != 1 {
		t.Fatalf("nil response length status = %d", status)
	}
	if status := nativeHostCallback(0, nil, 0, &response[0], uintptr(len(response)), &responseLength); status != 1 {
		t.Fatalf("missing registry status = %d", status)
	}

	call := func(handler HostFunc, request string, capacity int) (uintptr, string) {
		t.Helper()
		id := uintptr(hostSequence.Add(1))
		entry := &hostEntry{context: context.Background(), handler: handler}
		hostRegistry.Store(id, entry)
		defer hostRegistry.Delete(id)
		buffer := make([]byte, max(capacity, 1))
		var length uintptr
		requestBytes := []byte(request)
		status := nativeHostCallback(
			id,
			firstByte(requestBytes),
			uintptr(len(requestBytes)),
			&buffer[0],
			uintptr(capacity),
			&length,
		)
		return status, string(buffer[:min(int(length), len(buffer))])
	}

	status, body := call(func(_ context.Context, request json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"echo":` + string(request) + `}`), nil
	}, `{"value":9}`, len(response))
	if status != 0 || body != `{"ok":true,"value":{"echo":{"value":9}}}` {
		t.Fatalf("host success = %d %s", status, body)
	}
	status, body = call(func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return nil, errors.New("host failed")
	}, "", len(response))
	if status != 0 || !strings.Contains(body, `"error":"host failed"`) {
		t.Fatalf("host error = %d %s", status, body)
	}
	status, body = call(func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{`), nil
	}, "", len(response))
	if status != 0 || !strings.Contains(body, "host response is not JSON") {
		t.Fatalf("invalid host JSON = %d %s", status, body)
	}
	if status, _ := call(func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"large":true}`), nil
	}, "", 1); status != 1 {
		t.Fatalf("small response status = %d", status)
	}
	if status, _ := call(func(context.Context, json.RawMessage) (json.RawMessage, error) {
		panic("boom")
	}, "", len(response)); status != 1 {
		t.Fatalf("panic status = %d", status)
	}

	id := uintptr(hostSequence.Add(1))
	hostRegistry.Store(id, &hostEntry{})
	defer hostRegistry.Delete(id)
	if status := nativeHostCallback(id, nil, 0, &response[0], uintptr(len(response)), &responseLength); status != 1 {
		t.Fatalf("empty host entry status = %d", status)
	}
}

func TestBindingFailureIsReported(t *testing.T) {
	handle := &fakeDynamicLibrary{lookupError: errors.New("missing")}
	if _, err := bindNativeFunctions(handle); err == nil || !strings.Contains(err.Error(), "resolve V8 runtime symbol") {
		t.Fatalf("bindNativeFunctions() error = %v", err)
	}
	if err := registerFunction(42, 1); err == nil || !strings.Contains(err.Error(), "purego register panic") {
		t.Fatalf("registerFunction() error = %v", err)
	}
	if path := DefaultLibraryPath(); filepath.Base(path) != DefaultLibraryName() {
		t.Fatalf("DefaultLibraryPath() = %q", path)
	}
}

func TestNativeLibraryIntegration(t *testing.T) {
	path := os.Getenv("ALI_SLIDER_V8_TEST_LIBRARY")
	if path == "" {
		t.Skip("ALI_SLIDER_V8_TEST_LIBRARY is not set")
	}
	library, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() {
		if err := library.Close(); err != nil {
			t.Errorf("Library.Close() error = %v", err)
		}
	}()
	version, err := library.Version()
	if err != nil || version == "" {
		t.Fatalf("Version() = %q, %v", version, err)
	}

	engine, err := library.NewWithHost(func(ctx context.Context, request json.RawMessage) (json.RawMessage, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return json.RawMessage(`{"echo":` + string(request) + `}`), nil
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := library.Close(); !errors.Is(err, ErrLibraryInUse) {
		t.Fatalf("Close(in use) error = %v", err)
	}
	bootstrap, err := os.ReadFile(filepath.Join("..", "..", "infrastructure", "engine", "runtime", "v8_host_bootstrap.js"))
	if err != nil {
		t.Fatalf("ReadFile(bootstrap) error = %v", err)
	}
	if result, err := engine.Eval(context.Background(), string(bootstrap), "v8-host-bootstrap.js"); err != nil || string(result) != "true" {
		t.Fatalf("Eval(bootstrap) = %s, %v", result, err)
	}
	hostResult, err := engine.Eval(
		context.Background(),
		`JSON.parse(__aliNativeHostCall(JSON.stringify({value: 9})))`,
		"host-callback.js",
	)
	if err != nil || string(hostResult) != `{"ok":true,"value":{"echo":{"value":9}}}` {
		t.Fatalf("Eval(host callback) = %s, %v", hostResult, err)
	}
	if _, err := engine.Eval(
		context.Background(),
		`const child = __aliNodeCompat.vm.createContext({});
		__aliNodeCompat.vm.runInContext("while (true) {}", child, {timeout: 20});`,
		"child-timeout.js",
	); err == nil || !strings.Contains(err.Error(), "child context execution timed out") {
		t.Fatalf("Eval(child timeout) error = %v", err)
	}
	if recovered, err := engine.Eval(context.Background(), "20 + 22", "child-timeout-recovered.js"); err != nil || string(recovered) != "42" {
		t.Fatalf("Eval(child timeout recovered) = %s, %v", recovered, err)
	}
	if _, err := engine.Eval(
		context.Background(),
		`globalThis.delayed = ({value}) => new Promise((resolve) => setTimeout(() => resolve({value}), 5)); true`,
		"timer-callback.js",
	); err != nil {
		t.Fatalf("Eval(timer setup) error = %v", err)
	}
	timerResult, err := engine.Call(context.Background(), "delayed", json.RawMessage(`{"value":11}`))
	if err != nil || string(timerResult) != `{"value":11}` {
		t.Fatalf("Call(delayed) = %s, %v", timerResult, err)
	}

	result, err := engine.Eval(context.Background(), `
		globalThis.total = 1;
		globalThis.add = async ({value}) => ({total: total += value});
		({ready: true});
	`, "go-v8-spike.js")
	if err != nil || string(result) != `{"ready":true}` {
		t.Fatalf("Eval() = %s, %v", result, err)
	}
	for _, test := range []struct {
		input json.RawMessage
		want  string
	}{
		{json.RawMessage(`{"value":2}`), `{"total":3}`},
		{json.RawMessage(`{"value":4}`), `{"total":7}`},
	} {
		result, err = engine.Call(context.Background(), "add", test.input)
		if err != nil || string(result) != test.want {
			t.Fatalf("Call(%s) = %s, %v", test.input, result, err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, err := engine.Eval(ctx, "while (true) {}", "timeout.js"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Eval(timeout) error = %v", err)
	}
	result, err = engine.Eval(context.Background(), "6 * 7", "recovered.js")
	if err != nil || string(result) != "42" {
		t.Fatalf("Eval(recovered) = %s, %v", result, err)
	}
	if err := engine.Close(); err != nil {
		t.Fatalf("Runtime.Close() error = %v", err)
	}
	if _, err := engine.Eval(context.Background(), "1", "closed.js"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Eval(closed) error = %v", err)
	}
}
