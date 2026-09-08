// Package v8runtime loads the pinned in-process V8 wrapper without CGo.
package v8runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/ebitengine/purego"
)

const (
	nativeABIVersion        = 1
	maximumNativeResultSize = 16 * 1024 * 1024
)

var (
	// ErrClosed 表示动态库或 runtime 已关闭。
	ErrClosed = errors.New("V8 runtime is closed")
	// ErrLibraryInUse 表示仍有 runtime 引用动态库。
	ErrLibraryInUse = errors.New("V8 runtime library is in use")
	// ErrTerminated 表示 V8 执行被宿主终止。
	ErrTerminated = errors.New("V8 execution terminated")
)

type dynamicLibrary interface {
	lookup(string) (uintptr, error)
	close() error
}

type nativeFunctions struct {
	abiVersion    func() uint32
	engineVersion func() unsafe.Pointer
	create        func() unsafe.Pointer
	destroy       func(unsafe.Pointer)
	terminate     func(unsafe.Pointer) int32
	setHost       func(unsafe.Pointer, uintptr, uintptr) int32
	eval          func(unsafe.Pointer, *byte, uintptr, *byte, uintptr) unsafe.Pointer
	call          func(unsafe.Pointer, *byte, uintptr, *byte, uintptr) unsafe.Pointer
	resultStatus  func(unsafe.Pointer) int32
	resultData    func(unsafe.Pointer) *byte
	resultLength  func(unsafe.Pointer) uintptr
	resultFree    func(unsafe.Pointer)
}

// Library 是已校验 ABI 的本地 V8 wrapper。
type Library struct {
	mu        sync.Mutex
	handle    dynamicLibrary
	functions nativeFunctions
	path      string
	runtimes  int
	closed    bool
}

// Runtime 持有一个状态化 V8 Isolate。所有调用串行执行。
type Runtime struct {
	mu      sync.Mutex
	library *Library
	pointer unsafe.Pointer
	hostID  uintptr
	host    *hostEntry
	closed  bool
}

// HostFunc 处理 V8 发给 Go 宿主的 JSON 请求，并返回 JSON。
type HostFunc func(context.Context, json.RawMessage) (json.RawMessage, error)

type hostEntry struct {
	mu      sync.RWMutex
	context context.Context
	handler HostFunc
}

var (
	hostSequence        atomic.Uint64
	hostRegistry        sync.Map
	hostCallbackOnce    sync.Once
	hostCallbackPointer uintptr
)

// NativeError 是 wrapper 返回的稳定 JS/V8 错误。
type NativeError struct {
	Operation string
	Message   string
}

func (failure *NativeError) Error() string {
	return fmt.Sprintf("%s: %s", failure.Operation, failure.Message)
}

// DefaultLibraryName 返回当前平台的发布文件名。
func DefaultLibraryName() string {
	switch goruntime.GOOS {
	case "windows":
		return "ali_slider_v8_runtime.dll"
	case "darwin":
		return "libali_slider_v8_runtime.dylib"
	default:
		return "libali_slider_v8_runtime.so"
	}
}

// DefaultLibraryPath 返回与当前可执行文件同目录的默认 wrapper 路径。
// 无法解析可执行文件时退回当前工作目录下的默认文件名。
func DefaultLibraryPath() string {
	executable, err := os.Executable()
	if err != nil {
		return DefaultLibraryName()
	}
	return filepath.Join(filepath.Dir(executable), DefaultLibraryName())
}

// Open 加载并校验指定 wrapper。path 必须是普通文件。
func Open(path string) (*Library, error) {
	if path == "" {
		return nil, errors.New("V8 runtime library path is empty")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve V8 runtime library: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return nil, fmt.Errorf("stat V8 runtime library: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("V8 runtime library is not a regular file")
	}

	handle, err := openDynamicLibrary(absolute)
	if err != nil {
		return nil, fmt.Errorf("load V8 runtime library: %w", err)
	}
	functions, err := bindNativeFunctions(handle)
	if err != nil {
		_ = handle.close()
		return nil, err
	}
	if version := functions.abiVersion(); version != nativeABIVersion {
		_ = handle.close()
		return nil, fmt.Errorf(
			"V8 runtime ABI mismatch: wrapper=%d go=%d",
			version,
			nativeABIVersion,
		)
	}
	return &Library{
		handle:    handle,
		functions: functions,
		path:      absolute,
	}, nil
}

func bindNativeFunctions(handle dynamicLibrary) (nativeFunctions, error) {
	var functions nativeFunctions
	bindings := []struct {
		name   string
		target any
	}{
		{"ali_slider_v8_abi_version", &functions.abiVersion},
		{"ali_slider_v8_engine_version", &functions.engineVersion},
		{"ali_slider_v8_runtime_create", &functions.create},
		{"ali_slider_v8_runtime_destroy", &functions.destroy},
		{"ali_slider_v8_runtime_terminate", &functions.terminate},
		{"ali_slider_v8_runtime_set_host", &functions.setHost},
		{"ali_slider_v8_runtime_eval", &functions.eval},
		{"ali_slider_v8_runtime_call", &functions.call},
		{"ali_slider_v8_result_status", &functions.resultStatus},
		{"ali_slider_v8_result_data", &functions.resultData},
		{"ali_slider_v8_result_length", &functions.resultLength},
		{"ali_slider_v8_result_free", &functions.resultFree},
	}
	for _, binding := range bindings {
		address, err := handle.lookup(binding.name)
		if err != nil {
			return nativeFunctions{}, fmt.Errorf(
				"resolve V8 runtime symbol %s: %w",
				binding.name,
				err,
			)
		}
		if err := registerFunction(binding.target, address); err != nil {
			return nativeFunctions{}, fmt.Errorf(
				"bind V8 runtime symbol %s: %w",
				binding.name,
				err,
			)
		}
	}
	return functions, nil
}

func registerFunction(target any, address uintptr) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("purego register panic: %v", recovered)
		}
	}()
	purego.RegisterFunc(target, address)
	return nil
}

// Path 返回实际加载的绝对路径。
func (library *Library) Path() string {
	if library == nil {
		return ""
	}
	return library.path
}

// Version 返回 wrapper 内实际运行的 V8 版本。
func (library *Library) Version() (string, error) {
	if library == nil {
		return "", ErrClosed
	}
	library.mu.Lock()
	defer library.mu.Unlock()
	if library.closed {
		return "", ErrClosed
	}
	result, err := library.copyResult("V8 version", library.functions.engineVersion())
	if err != nil {
		return "", err
	}
	var version string
	if err := json.Unmarshal(result, &version); err != nil || version == "" {
		return "", errors.New("V8 version result is invalid")
	}
	return version, nil
}

// New 创建状态化 runtime。
func (library *Library) New() (*Runtime, error) {
	return library.newRuntime(nil)
}

// NewWithHost 创建带同步 JSON 宿主回调的状态化 runtime。
func (library *Library) NewWithHost(handler HostFunc) (*Runtime, error) {
	if handler == nil {
		return nil, errors.New("V8 host handler is nil")
	}
	return library.newRuntime(handler)
}

func (library *Library) newRuntime(handler HostFunc) (*Runtime, error) {
	if library == nil {
		return nil, ErrClosed
	}
	library.mu.Lock()
	defer library.mu.Unlock()
	if library.closed {
		return nil, ErrClosed
	}
	pointer := library.functions.create()
	if pointer == nil {
		return nil, errors.New("create V8 runtime failed")
	}
	var id uintptr
	var entry *hostEntry
	if handler != nil {
		id = uintptr(hostSequence.Add(1))
		if id == 0 {
			library.functions.destroy(pointer)
			return nil, errors.New("V8 host registry exhausted")
		}
		entry = &hostEntry{handler: handler}
		hostRegistry.Store(id, entry)
		if library.functions.setHost(pointer, sharedHostCallback(), id) != 1 {
			hostRegistry.Delete(id)
			library.functions.destroy(pointer)
			return nil, errors.New("configure V8 host callback failed")
		}
	}
	library.runtimes++
	return &Runtime{
		library: library,
		pointer: pointer,
		hostID:  id,
		host:    entry,
	}, nil
}

// Close 释放动态库；仍有 Runtime 时拒绝卸载。
func (library *Library) Close() error {
	if library == nil {
		return nil
	}
	library.mu.Lock()
	defer library.mu.Unlock()
	if library.closed {
		return nil
	}
	if library.runtimes != 0 {
		return fmt.Errorf("%w: %d runtime(s)", ErrLibraryInUse, library.runtimes)
	}
	if err := library.handle.close(); err != nil {
		return fmt.Errorf("unload V8 runtime library: %w", err)
	}
	library.closed = true
	return nil
}

// Eval 在当前 Isolate 中执行 source，并返回 JSON 编码结果。
func (runtime *Runtime) Eval(
	ctx context.Context,
	source string,
	filename string,
) (json.RawMessage, error) {
	sourceBytes := []byte(source)
	filenameBytes := []byte(filename)
	return runtime.execute(ctx, "V8 eval", func(functions nativeFunctions, pointer unsafe.Pointer) unsafe.Pointer {
		result := functions.eval(
			pointer,
			firstByte(sourceBytes),
			uintptr(len(sourceBytes)),
			firstByte(filenameBytes),
			uintptr(len(filenameBytes)),
		)
		goruntime.KeepAlive(sourceBytes)
		goruntime.KeepAlive(filenameBytes)
		return result
	})
}

// Call 调用当前 Isolate 的全局函数，input 必须是 JSON。
func (runtime *Runtime) Call(
	ctx context.Context,
	functionName string,
	input json.RawMessage,
) (json.RawMessage, error) {
	nameBytes := []byte(functionName)
	inputBytes := []byte(input)
	return runtime.execute(ctx, "V8 call", func(functions nativeFunctions, pointer unsafe.Pointer) unsafe.Pointer {
		result := functions.call(
			pointer,
			firstByte(nameBytes),
			uintptr(len(nameBytes)),
			firstByte(inputBytes),
			uintptr(len(inputBytes)),
		)
		goruntime.KeepAlive(nameBytes)
		goruntime.KeepAlive(inputBytes)
		return result
	})
}

func (runtime *Runtime) execute(
	ctx context.Context,
	operation string,
	invoke func(nativeFunctions, unsafe.Pointer) unsafe.Pointer,
) (json.RawMessage, error) {
	if runtime == nil {
		return nil, ErrClosed
	}
	if ctx == nil {
		return nil, errors.New("V8 context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.closed || runtime.pointer == nil {
		return nil, ErrClosed
	}
	functions := runtime.library.functions
	pointer := runtime.pointer
	if runtime.host != nil {
		runtime.host.mu.Lock()
		runtime.host.context = ctx
		runtime.host.mu.Unlock()
		defer func() {
			runtime.host.mu.Lock()
			runtime.host.context = nil
			runtime.host.mu.Unlock()
		}()
	}
	if ctx.Done() == nil {
		return runtime.library.copyResult(operation, invoke(functions, pointer))
	}

	done := make(chan unsafe.Pointer, 1)
	go func() {
		done <- invoke(functions, pointer)
	}()
	select {
	case result := <-done:
		return runtime.library.copyResult(operation, result)
	case <-ctx.Done():
		functions.terminate(pointer)
		result := <-done
		_, _ = runtime.library.copyResult(operation, result)
		return nil, ctx.Err()
	}
}

func (library *Library) copyResult(
	operation string,
	result unsafe.Pointer,
) (json.RawMessage, error) {
	if result == nil {
		return nil, errors.New("native V8 returned a null result")
	}
	defer library.functions.resultFree(result)
	length := library.functions.resultLength(result)
	if length > maximumNativeResultSize {
		return nil, errors.New("native V8 result exceeds size limit")
	}
	data := library.functions.resultData(result)
	if length > 0 && data == nil {
		return nil, errors.New("native V8 result has a null buffer")
	}
	var copied []byte
	if length > 0 {
		copied = append([]byte(nil), unsafe.Slice(data, int(length))...)
	}
	switch status := library.functions.resultStatus(result); status {
	case 0:
		if !json.Valid(copied) {
			return nil, errors.New("native V8 success result is not JSON")
		}
		return json.RawMessage(copied), nil
	case 1:
		return nil, decodeNativeError(operation, copied)
	case 2:
		return nil, ErrTerminated
	default:
		return nil, fmt.Errorf("native V8 returned unknown status %d", status)
	}
}

func decodeNativeError(operation string, value []byte) error {
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(value, &payload); err != nil || payload.Error == "" {
		return errors.New("native V8 error result is invalid")
	}
	return &NativeError{Operation: operation, Message: payload.Error}
}

func firstByte(value []byte) *byte {
	if len(value) == 0 {
		return nil
	}
	return &value[0]
}

func sharedHostCallback() uintptr {
	hostCallbackOnce.Do(func() {
		hostCallbackPointer = purego.NewCallback(nativeHostCallback)
	})
	return hostCallbackPointer
}

func nativeHostCallback(
	userData uintptr,
	requestPointer *byte,
	requestLength uintptr,
	responsePointer *byte,
	responseCapacity uintptr,
	responseLengthPointer *uintptr,
) (status uintptr) {
	defer func() {
		if recover() != nil {
			status = 1
		}
	}()
	if requestLength > maximumNativeResultSize || responseCapacity > maximumNativeResultSize {
		return 1
	}
	if (requestLength > 0 && requestPointer == nil) || responsePointer == nil || responseLengthPointer == nil {
		return 1
	}
	loaded, ok := hostRegistry.Load(userData)
	if !ok {
		return 1
	}
	entry := loaded.(*hostEntry)
	entry.mu.RLock()
	ctx := entry.context
	handler := entry.handler
	entry.mu.RUnlock()
	if ctx == nil || handler == nil {
		return 1
	}

	var request []byte
	if requestLength > 0 {
		request = append(
			[]byte(nil),
			unsafe.Slice(requestPointer, int(requestLength))...,
		)
	}
	value, err := handler(ctx, json.RawMessage(request))
	response := marshalHostResponse(value, err)
	if len(response) > int(responseCapacity) {
		return 1
	}
	copy(
		unsafe.Slice(responsePointer, int(responseCapacity)),
		response,
	)
	*responseLengthPointer = uintptr(len(response))
	return 0
}

func marshalHostResponse(value json.RawMessage, err error) []byte {
	if err != nil {
		encoded, _ := json.Marshal(struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}{OK: false, Error: err.Error()})
		return encoded
	}
	if !json.Valid(value) {
		encoded, _ := json.Marshal(struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}{OK: false, Error: "host response is not JSON"})
		return encoded
	}
	encoded, _ := json.Marshal(struct {
		OK    bool            `json:"ok"`
		Value json.RawMessage `json:"value"`
	}{OK: true, Value: value})
	return encoded
}

// Close 结束 Isolate worker 并释放其 V8 资源。
func (runtime *Runtime) Close() error {
	if runtime == nil {
		return nil
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.closed {
		return nil
	}
	if runtime.hostID != 0 {
		runtime.library.functions.setHost(runtime.pointer, 0, 0)
		hostRegistry.Delete(runtime.hostID)
		runtime.hostID = 0
		runtime.host = nil
	}
	runtime.library.functions.destroy(runtime.pointer)
	runtime.pointer = nil
	runtime.closed = true

	runtime.library.mu.Lock()
	runtime.library.runtimes--
	runtime.library.mu.Unlock()
	return nil
}
