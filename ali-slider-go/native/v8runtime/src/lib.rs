//! 供 Go 进程内加载的最小 V8 C ABI。
//!
//! V8 Isolate 只在创建它的 Rust worker 线程上使用。Go 可以从任意
//! goroutine 调用导出函数，命令会被串行转交给该线程。

use std::collections::{HashMap, HashSet, VecDeque};
use std::convert::TryFrom;
use std::ffi::c_void;
use std::panic::{AssertUnwindSafe, catch_unwind};
use std::slice;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::{Arc, Condvar, Mutex, Once, OnceLock, mpsc};
use std::thread::{self, JoinHandle};
use std::time::{Duration, Instant};

const ABI_VERSION: u32 = 1;
const MAX_SOURCE_BYTES: usize = 16 * 1024 * 1024;
const MAX_NAME_BYTES: usize = 4 * 1024;
const MAX_JSON_BYTES: usize = 4 * 1024 * 1024;
const MAX_HOST_RESULT_BYTES: usize = 4 * 1024 * 1024;
const DEFAULT_VM_SCRIPT_TIMEOUT_MS: u64 = 1_000;
const MAX_VM_SCRIPT_TIMEOUT_MS: u64 = 10_000;
const MAX_ISOLATE_HEAP_BYTES: usize = 512 * 1024 * 1024;
const HEAP_TERMINATION_GRACE_BYTES: usize = 128 * 1024 * 1024;
const MAX_SCRIPT_CODE_CACHE_ENTRIES: usize = 64;
const MAX_SCRIPT_CODE_CACHE_BYTES: usize = 64 * 1024 * 1024;

static INITIALIZE_V8: Once = Once::new();
static SCRIPT_CODE_CACHE: OnceLock<ScriptCodeCacheStore> = OnceLock::new();

struct ScriptCodeCacheStore {
    cache: Mutex<ScriptCodeCache>,
    ready: Condvar,
}

struct ScriptCodeCache {
    entries: HashMap<String, Vec<u8>>,
    order: VecDeque<String>,
    compiling: HashSet<String>,
    bytes: usize,
}

enum ScriptCacheLookup<'a> {
    Cached(Vec<u8>),
    Compile(ScriptCompileReservation<'a>),
}

struct ScriptCompileReservation<'a> {
    store: &'a ScriptCodeCacheStore,
    key: String,
    active: bool,
}

impl ScriptCodeCache {
    fn new() -> Self {
        Self {
            entries: HashMap::new(),
            order: VecDeque::new(),
            compiling: HashSet::new(),
            bytes: 0,
        }
    }

    fn get(&mut self, key: &str) -> Option<Vec<u8>> {
        let value = self.entries.get(key)?.clone();
        self.order.retain(|candidate| candidate != key);
        self.order.push_back(key.to_owned());
        Some(value)
    }

    fn remove(&mut self, key: &str) {
        if let Some(value) = self.entries.remove(key) {
            self.bytes = self.bytes.saturating_sub(key.len() + value.len());
        }
        self.order.retain(|candidate| candidate != key);
    }

    fn insert(&mut self, key: String, value: Vec<u8>) {
        let footprint = key.len().saturating_add(value.len());
        if footprint > MAX_SCRIPT_CODE_CACHE_BYTES {
            return;
        }
        self.remove(&key);
        while self.entries.len() >= MAX_SCRIPT_CODE_CACHE_ENTRIES
            || self.bytes.saturating_add(footprint) > MAX_SCRIPT_CODE_CACHE_BYTES
        {
            let Some(oldest) = self.order.pop_front() else {
                break;
            };
            if let Some(old_value) = self.entries.remove(&oldest) {
                self.bytes = self
                    .bytes
                    .saturating_sub(oldest.len().saturating_add(old_value.len()));
            }
        }
        self.bytes = self.bytes.saturating_add(footprint);
        self.order.push_back(key.clone());
        self.entries.insert(key, value);
    }
}

impl ScriptCodeCacheStore {
    fn new() -> Self {
        Self {
            cache: Mutex::new(ScriptCodeCache::new()),
            ready: Condvar::new(),
        }
    }

    // 同一精确源码只允许一个编译者；不同源码在离开
    // 这个短临界区后可以并发编译，避免冷 PE 分片全局串行。
    fn lookup(&self, key: &str) -> ScriptCacheLookup<'_> {
        let mut cache = self
            .cache
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        loop {
            if let Some(value) = cache.get(key) {
                return ScriptCacheLookup::Cached(value);
            }
            if cache.compiling.insert(key.to_owned()) {
                return ScriptCacheLookup::Compile(ScriptCompileReservation {
                    store: self,
                    key: key.to_owned(),
                    active: true,
                });
            }
            cache = self
                .ready
                .wait(cache)
                .unwrap_or_else(|poisoned| poisoned.into_inner());
        }
    }

    fn remove(&self, key: &str) {
        self.cache
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner())
            .remove(key);
    }
}

impl ScriptCompileReservation<'_> {
    fn finish(mut self, value: Option<Vec<u8>>) {
        self.release(value);
    }

    fn release(&mut self, value: Option<Vec<u8>>) {
        if !self.active {
            return;
        }
        let mut cache = self
            .store
            .cache
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        if let Some(value) = value {
            cache.insert(self.key.clone(), value);
        }
        cache.compiling.remove(&self.key);
        self.active = false;
        drop(cache);
        self.store.ready.notify_all();
    }
}

impl Drop for ScriptCompileReservation<'_> {
    fn drop(&mut self) {
        // 编译失败或 panic 也必须唤醒同源码等待者；
        // 等待者会成为新的唯一编译者。
        self.release(None);
    }
}

fn script_code_cache() -> &'static ScriptCodeCacheStore {
    SCRIPT_CODE_CACHE.get_or_init(ScriptCodeCacheStore::new)
}

macro_rules! caught_message {
    ($scope:ident) => {{
        let exception = $scope.exception();
        match exception {
            Some(value) => value.to_rust_string_lossy($scope),
            None => "JavaScript execution failed".to_owned(),
        }
    }};
}

macro_rules! execution_error {
    ($scope:ident) => {{
        if $scope.has_terminated() {
            $scope.cancel_terminate_execution();
            RuntimeResult::terminated()
        } else {
            RuntimeResult::error(caught_message!($scope))
        }
    }};
}

#[derive(Debug)]
struct RuntimeResult {
    status: i32,
    data: Vec<u8>,
}

impl RuntimeResult {
    fn success(value: impl Into<Vec<u8>>) -> Self {
        Self {
            status: 0,
            data: value.into(),
        }
    }

    fn error(message: impl AsRef<str>) -> Self {
        let escaped = json_quote(message.as_ref());
        Self {
            status: 1,
            data: format!(r#"{{"error":{escaped}}}"#).into_bytes(),
        }
    }

    fn terminated() -> Self {
        Self {
            status: 2,
            data: br#"{"error":"JavaScript execution terminated"}"#.to_vec(),
        }
    }
}

enum Command {
    Eval {
        source: String,
        filename: String,
        reply: mpsc::SyncSender<RuntimeResult>,
    },
    Call {
        function_name: String,
        input_json: String,
        reply: mpsc::SyncSender<RuntimeResult>,
    },
    Shutdown,
}

struct RuntimeHandle {
    sender: mpsc::Sender<Command>,
    isolate: v8::IsolateHandle,
    host: Arc<HostBridge>,
    worker: Option<JoinHandle<()>>,
}

struct HostBridge {
    callback: AtomicUsize,
    user_data: AtomicUsize,
    cancelled: AtomicBool,
}

impl HostBridge {
    fn new() -> Self {
        Self {
            callback: AtomicUsize::new(0),
            user_data: AtomicUsize::new(0),
            cancelled: AtomicBool::new(false),
        }
    }

    fn call(&self, request: &str) -> Result<String, String> {
        let callback = self.callback.load(Ordering::Acquire);
        if callback == 0 {
            return Err("V8 host callback is not configured".to_owned());
        }
        type HostCallback =
            unsafe extern "C" fn(usize, *const u8, usize, *mut u8, usize, *mut usize) -> usize;
        // SAFETY: Go registers a function pointer with this exact stable C ABI.
        let callback: HostCallback = unsafe { std::mem::transmute(callback) };
        let mut output = vec![0_u8; MAX_HOST_RESULT_BYTES];
        let mut output_length = 0_usize;
        // SAFETY: Both buffers remain valid for the entire callback, and the callback
        // may write at most output.len() bytes before setting output_length.
        let status = unsafe {
            callback(
                self.user_data.load(Ordering::Acquire),
                request.as_ptr(),
                request.len(),
                output.as_mut_ptr(),
                output.len(),
                &mut output_length,
            )
        };
        if status != 0 {
            return Err("V8 host callback failed".to_owned());
        }
        if output_length > output.len() {
            return Err("V8 host callback result exceeds buffer".to_owned());
        }
        output.truncate(output_length);
        String::from_utf8(output).map_err(|_| "V8 host callback result is not UTF-8".to_owned())
    }
}

impl Drop for RuntimeHandle {
    fn drop(&mut self) {
        let _ = self.sender.send(Command::Shutdown);
        if let Some(worker) = self.worker.take() {
            let _ = worker.join();
        }
    }
}

struct Engine {
    isolate: v8::OwnedIsolate,
    context: v8::Global<v8::Context>,
    heap_limit: Box<HeapLimitState>,
    execution: Arc<ExecutionState>,
    poisoned: bool,
}

struct HeapLimitState {
    isolate: v8::IsolateHandle,
    reached: AtomicBool,
    terminator_stop: Arc<AtomicBool>,
}

struct ExecutionState {
    child_timed_out: AtomicBool,
}

struct ExecutionDeadline {
    cancel: Option<mpsc::Sender<()>>,
    worker: Option<JoinHandle<()>>,
}

impl ExecutionDeadline {
    fn start(
        isolate: v8::IsolateHandle,
        timeout: Duration,
        execution: Arc<ExecutionState>,
    ) -> Self {
        let (cancel, receiver) = mpsc::channel();
        let worker = thread::Builder::new()
            .name("ali-slider-v8-deadline".to_owned())
            .spawn(move || {
                if matches!(
                    receiver.recv_timeout(timeout),
                    Err(mpsc::RecvTimeoutError::Timeout)
                ) {
                    execution.child_timed_out.store(true, Ordering::Release);
                    isolate.terminate_execution();
                }
            })
            .expect("create V8 execution deadline worker");
        Self {
            cancel: Some(cancel),
            worker: Some(worker),
        }
    }
}

impl Drop for ExecutionDeadline {
    fn drop(&mut self) {
        if let Some(cancel) = self.cancel.take() {
            let _ = cancel.send(());
        }
        if let Some(worker) = self.worker.take() {
            let _ = worker.join();
        }
    }
}

extern "C" fn terminate_near_heap_limit(
    data: *mut c_void,
    current_heap_limit: usize,
    _initial_heap_limit: usize,
) -> usize {
    // SAFETY: Engine owns the pinned Box for at least as long as the callback
    // remains registered on its Isolate.
    let state = unsafe { &*data.cast::<HeapLimitState>() };
    if !state.reached.swap(true, Ordering::AcqRel) {
        // V8 正在 GC callback 内时，同线程请求 termination 可能等不到正常的
        // interrupt 检查点。只启动一次外部终止线程，让 callback 先返回 V8。
        let isolate = state.isolate.clone();
        let stop = state.terminator_stop.clone();
        let _ = thread::Builder::new()
            .name("ali-slider-v8-heap-limit".to_owned())
            .spawn(move || {
                // 让 near-heap callback 先退出；在 callback 尚未返回时调用
                // TerminateExecution，V8 可能在 GC 路径里吞掉该请求。
                thread::sleep(Duration::from_millis(2));
                while !stop.load(Ordering::Acquire) {
                    isolate.terminate_execution();
                    thread::sleep(Duration::from_millis(1));
                }
            });
    }
    // V8 aborts the process when a callback does not raise the limit. Give the
    // termination request a small amount of unwind room, then restore the cap
    // after the outer Eval/Call has left all HandleScopes.
    current_heap_limit.saturating_add(HEAP_TERMINATION_GRACE_BYTES)
}

impl Drop for Engine {
    fn drop(&mut self) {
        self.heap_limit
            .terminator_stop
            .store(true, Ordering::Release);
        self.isolate
            .remove_near_heap_limit_callback(terminate_near_heap_limit, MAX_ISOLATE_HEAP_BYTES);
    }
}

impl Engine {
    fn new(host: Arc<HostBridge>) -> Self {
        initialize_v8();
        let params = v8::CreateParams::default().heap_limits(0, MAX_ISOLATE_HEAP_BYTES);
        let mut isolate = v8::Isolate::new(params);
        isolate.set_slot(host);
        let execution = Arc::new(ExecutionState {
            child_timed_out: AtomicBool::new(false),
        });
        isolate.set_slot(execution.clone());
        let mut heap_limit = Box::new(HeapLimitState {
            isolate: isolate.thread_safe_handle(),
            reached: AtomicBool::new(false),
            terminator_stop: Arc::new(AtomicBool::new(true)),
        });
        isolate.add_near_heap_limit_callback(
            terminate_near_heap_limit,
            (&mut *heap_limit as *mut HeapLimitState).cast(),
        );
        let context = {
            v8::scope!(let scope, &mut isolate);
            let context = v8::Context::new(scope, Default::default());
            let scope = &mut v8::ContextScope::new(scope, context);
            let name = v8::String::new(scope, "__aliNativeHostCall").unwrap();
            let function = v8::Function::new(scope, native_host_call).unwrap();
            assert!(
                context
                    .global(scope)
                    .set(scope, name.into(), function.into())
                    .unwrap()
            );
            let create_name = v8::String::new(scope, "__aliV8CreateContext").unwrap();
            let create_function = v8::Function::new(scope, native_vm_create_context).unwrap();
            assert!(
                context
                    .global(scope)
                    .set(scope, create_name.into(), create_function.into())
                    .unwrap()
            );
            let run_name = v8::String::new(scope, "__aliV8RunInContext").unwrap();
            let run_function = v8::Function::new(scope, native_vm_run_in_context).unwrap();
            assert!(
                context
                    .global(scope)
                    .set(scope, run_name.into(), run_function.into())
                    .unwrap()
            );
            let microtask_name = v8::String::new(scope, "__aliV8QueueMicrotask").unwrap();
            let microtask_function = v8::Function::new(scope, native_queue_microtask).unwrap();
            assert!(
                context
                    .global(scope)
                    .set(scope, microtask_name.into(), microtask_function.into())
                    .unwrap()
            );
            v8::Global::new(scope, context)
        };
        Self {
            isolate,
            context,
            heap_limit,
            execution,
            poisoned: false,
        }
    }

    fn eval(&mut self, source: &str, filename: &str) -> RuntimeResult {
        if self.poisoned {
            return RuntimeResult::error("V8 runtime is poisoned after heap limit");
        }
        self.execution
            .child_timed_out
            .store(false, Ordering::Release);
        self.heap_limit
            .terminator_stop
            .store(false, Ordering::Release);
        let result = self.eval_inner(source, filename);
        self.heap_limit
            .terminator_stop
            .store(true, Ordering::Release);
        let result = self.classify_termination(result);
        if self.heap_limit.reached.load(Ordering::Acquire) {
            self.poisoned = true;
        }
        result
    }

    fn eval_inner(&mut self, source: &str, filename: &str) -> RuntimeResult {
        let host = self
            .isolate
            .get_slot::<Arc<HostBridge>>()
            .expect("V8 host bridge slot is missing")
            .clone();
        host.cancelled.store(false, Ordering::Release);
        if self.isolate.is_execution_terminating() {
            self.isolate.cancel_terminate_execution();
        }
        v8::scope!(let scope, &mut self.isolate);
        let context = v8::Local::new(scope, &self.context);
        let scope = &mut v8::ContextScope::new(scope, context);
        v8::tc_scope!(let try_catch, scope);

        let source = if filename.is_empty() {
            source.to_owned()
        } else {
            format!("{source}\n//# sourceURL={}", sanitize_source_url(filename))
        };
        let Some(source) = v8::String::new(try_catch, &source) else {
            return RuntimeResult::error("V8 source allocation failed");
        };
        let Some(script) = v8::Script::compile(try_catch, source, None) else {
            return execution_error!(try_catch);
        };
        let Some(value) = script.run(try_catch) else {
            return execution_error!(try_catch);
        };
        json_result(try_catch, value)
    }

    fn call(&mut self, function_name: &str, input_json: &str) -> RuntimeResult {
        if self.poisoned {
            return RuntimeResult::error("V8 runtime is poisoned after heap limit");
        }
        self.execution
            .child_timed_out
            .store(false, Ordering::Release);
        self.heap_limit
            .terminator_stop
            .store(false, Ordering::Release);
        let result = self.call_inner(function_name, input_json);
        self.heap_limit
            .terminator_stop
            .store(true, Ordering::Release);
        let result = self.classify_termination(result);
        if self.heap_limit.reached.load(Ordering::Acquire) {
            self.poisoned = true;
        }
        result
    }

    fn call_inner(&mut self, function_name: &str, input_json: &str) -> RuntimeResult {
        let host = self
            .isolate
            .get_slot::<Arc<HostBridge>>()
            .expect("V8 host bridge slot is missing")
            .clone();
        host.cancelled.store(false, Ordering::Release);
        if self.isolate.is_execution_terminating() {
            self.isolate.cancel_terminate_execution();
        }
        v8::scope!(let scope, &mut self.isolate);
        let context = v8::Local::new(scope, &self.context);
        let scope = &mut v8::ContextScope::new(scope, context);
        v8::tc_scope!(let try_catch, scope);

        let Some(name) = v8::String::new(try_catch, function_name) else {
            return RuntimeResult::error("V8 function name allocation failed");
        };
        let global = context.global(try_catch);
        let Some(value) = global.get(try_catch, name.into()) else {
            return execution_error!(try_catch);
        };
        let Ok(function) = v8::Local::<v8::Function>::try_from(value) else {
            return RuntimeResult::error("requested global is not a function");
        };
        let Some(input) = v8::String::new(try_catch, input_json) else {
            return RuntimeResult::error("V8 input allocation failed");
        };
        let Some(input) = v8::json::parse(try_catch, input) else {
            return RuntimeResult::error("input is not valid JSON");
        };
        let Some(mut value) = function.call(try_catch, global.into(), &[input]) else {
            return execution_error!(try_catch);
        };

        try_catch.perform_microtask_checkpoint();
        if value.is_promise() {
            let promise = v8::Local::<v8::Promise>::try_from(value)
                .expect("V8 reported a Promise with an incompatible handle");
            let deadline = Instant::now() + Duration::from_secs(60);
            while promise.state() == v8::PromiseState::Pending {
                if host.cancelled.load(Ordering::Acquire) {
                    if try_catch.is_execution_terminating() {
                        try_catch.cancel_terminate_execution();
                    }
                    return RuntimeResult::terminated();
                }
                if Instant::now() >= deadline {
                    return RuntimeResult::error("Promise host event loop timed out");
                }

                let next_name = v8::String::new(try_catch, "__aliV8NextTimerDelay").unwrap();
                let pump_name = v8::String::new(try_catch, "__aliV8PumpTimers").unwrap();
                let next = global.get(try_catch, next_name.into());
                let pump = global.get(try_catch, pump_name.into());
                let (Some(next), Some(pump)) = (next, pump) else {
                    return RuntimeResult::error(
                        "Promise is pending; host timer hooks are unavailable",
                    );
                };
                let (Ok(next), Ok(pump)) = (
                    v8::Local::<v8::Function>::try_from(next),
                    v8::Local::<v8::Function>::try_from(pump),
                ) else {
                    return RuntimeResult::error("V8 host timer hooks are invalid");
                };
                let Some(delay) = next.call(try_catch, global.into(), &[]) else {
                    return execution_error!(try_catch);
                };
                let delay = delay.number_value(try_catch).unwrap_or(1.0);
                let sleep_ms = if delay.is_finite() && delay > 0.0 {
                    delay.ceil().clamp(1.0, 10.0) as u64
                } else {
                    0
                };
                if sleep_ms > 0 {
                    thread::sleep(Duration::from_millis(sleep_ms));
                } else {
                    thread::yield_now();
                }
                let Some(_) = pump.call(try_catch, global.into(), &[]) else {
                    return execution_error!(try_catch);
                };
                try_catch.perform_microtask_checkpoint();
            }
            match promise.state() {
                v8::PromiseState::Fulfilled => value = promise.result(try_catch),
                v8::PromiseState::Rejected => {
                    return RuntimeResult::error(
                        promise.result(try_catch).to_rust_string_lossy(try_catch),
                    );
                }
                v8::PromiseState::Pending => unreachable!(),
            }
        }
        json_result(try_catch, value)
    }

    fn classify_termination(&self, result: RuntimeResult) -> RuntimeResult {
        if result.status != 2 {
            return result;
        }
        if self.heap_limit.reached.load(Ordering::Acquire) {
            return RuntimeResult::error("V8 isolate heap limit exceeded");
        }
        if self.execution.child_timed_out.load(Ordering::Acquire) {
            return RuntimeResult::error("V8 child context execution timed out");
        }
        result
    }
}

fn native_host_call(
    scope: &mut v8::PinScope,
    arguments: v8::FunctionCallbackArguments,
    mut return_value: v8::ReturnValue,
) {
    let request = arguments
        .get(0)
        .to_string(scope)
        .map(|value| value.to_rust_string_lossy(scope));
    let Some(request) = request else {
        throw_js_error(scope, "V8 host request must be a string");
        return;
    };
    let host = scope.get_slot::<Arc<HostBridge>>().cloned();
    let Some(host) = host else {
        throw_js_error(scope, "V8 host bridge is unavailable");
        return;
    };
    match host.call(&request) {
        Ok(response) => {
            let Some(response) = v8::String::new(scope, &response) else {
                throw_js_error(scope, "V8 host response allocation failed");
                return;
            };
            return_value.set(response.into());
        }
        Err(error) => throw_js_error(scope, &error),
    }
}

fn native_queue_microtask(
    scope: &mut v8::PinScope,
    arguments: v8::FunctionCallbackArguments,
    _return_value: v8::ReturnValue,
) {
    let Ok(callback) = v8::Local::<v8::Function>::try_from(arguments.get(0)) else {
        throw_js_error(scope, "queueMicrotask callback must be a function");
        return;
    };
    // 不能用 Promise.resolve().then(callback) 模拟。FeiLin 会按能力检测替换
    // Promise；它内部的调度器再调用 queueMicrotask 时会形成无限递归链。
    scope.enqueue_microtask(callback);
}

fn native_vm_create_context(
    scope: &mut v8::PinScope,
    _arguments: v8::FunctionCallbackArguments,
    mut return_value: v8::ReturnValue,
) {
    let parent = scope.get_current_context();
    let security_token = parent.get_security_token(scope);
    let context = v8::Context::new(scope, Default::default());
    // Node vm 的 contextified global 可由宿主 bridge 直接读写。V8 默认给不同
    // Context 不同安全域；显式继承宿主 token，只开放同一 Isolate 内的对象访问。
    context.set_security_token(security_token);
    let child_scope = &mut v8::ContextScope::new(scope, context);
    let global = context.global(child_scope);
    return_value.set(global.into());
}

fn native_vm_run_in_context(
    scope: &mut v8::PinScope,
    arguments: v8::FunctionCallbackArguments,
    mut return_value: v8::ReturnValue,
) {
    let source = arguments
        .get(0)
        .to_string(scope)
        .map(|value| value.to_rust_string_lossy(scope));
    let Some(mut source) = source else {
        throw_js_error(scope, "V8 context source must be a string");
        return;
    };
    let target = arguments.get(1);
    let Ok(target) = v8::Local::<v8::Object>::try_from(target) else {
        throw_js_error(scope, "V8 context target must be an object");
        return;
    };
    let Some(context) = target.get_creation_context(scope) else {
        throw_js_error(scope, "V8 context target has no creation context");
        return;
    };
    let filename = arguments
        .get(2)
        .to_string(scope)
        .map(|value| value.to_rust_string_lossy(scope))
        .filter(|value| !value.is_empty())
        .unwrap_or_else(|| "v8-context.js".to_owned());
    if filename != "v8-context.js" {
        source.push_str("\n//# sourceURL=");
        source.push_str(&sanitize_source_url(&filename));
    }

    let requested_timeout = arguments
        .get(3)
        .number_value(scope)
        .filter(|value| value.is_finite() && *value > 0.0)
        .map(|value| value.floor() as u64)
        .unwrap_or(DEFAULT_VM_SCRIPT_TIMEOUT_MS);
    let timeout = Duration::from_millis(requested_timeout.min(MAX_VM_SCRIPT_TIMEOUT_MS));
    // Go context 的 deadline 能终止整个 Call；这里再限制每个公开 child
    // script，避免同步死循环/异常高分配在 Promise 返回前拖垮进程。
    let Some(execution) = scope.get_slot::<Arc<ExecutionState>>().cloned() else {
        throw_js_error(scope, "V8 execution state is unavailable");
        return;
    };
    let _deadline = ExecutionDeadline::start(scope.thread_safe_handle(), timeout, execution);

    let scope = &mut v8::ContextScope::new(scope, context);
    v8::tc_scope!(let try_catch, scope);
    // sourceURL 也会影响 V8 的 ScriptOrigin；缓存键必须覆盖
    // 最终编译文本，不能让不同动态 PE 分片串用 CachedData。
    let cache_key = source.clone();
    let Some(source) = v8::String::new(try_catch, &source) else {
        throw_js_error(try_catch, "V8 context source allocation failed");
        let _ = try_catch.rethrow();
        return;
    };

    // 缓存只保留编译产物；每轮仍 bind 到新 child context
    // 并重新 run。同源码 miss 按 key singleflight，不同精确 PE
    // 可并发编译；预留在 script.run 前完成，不会与嵌套
    // iframe context 的同源码执行形成重入等待。
    let script = match script_code_cache().lookup(&cache_key) {
        ScriptCacheLookup::Cached(cached_bytes) => {
            let cached_data = v8::CachedData::new(&cached_bytes);
            let mut compiler_source =
                v8::script_compiler::Source::new_with_cached_data(source, None, cached_data);
            let script = v8::script_compiler::compile(
                try_catch,
                &mut compiler_source,
                v8::script_compiler::CompileOptions::ConsumeCodeCache,
                v8::script_compiler::NoCacheReason::NoReason,
            );
            let rejected = compiler_source
                .get_cached_data()
                .is_none_or(v8::CachedData::rejected);
            if rejected || script.is_none() {
                script_code_cache().remove(&cache_key);
            }
            script
        }
        ScriptCacheLookup::Compile(reservation) => {
            let mut compiler_source = v8::script_compiler::Source::new(source, None);
            let unbound = v8::script_compiler::compile_unbound_script(
                try_catch,
                &mut compiler_source,
                v8::script_compiler::CompileOptions::EagerCompile,
                v8::script_compiler::NoCacheReason::NoReason,
            );
            match unbound {
                Some(unbound) => {
                    let cached_data = unbound
                        .create_code_cache()
                        .map(|cached_data| cached_data.to_vec());
                    reservation.finish(cached_data);
                    Some(unbound.bind_to_current_context(try_catch))
                }
                None => {
                    reservation.finish(None);
                    None
                }
            }
        }
    };
    let Some(script) = script else {
        let _ = try_catch.rethrow();
        return;
    };
    let Some(value) = script.run(try_catch) else {
        let _ = try_catch.rethrow();
        return;
    };
    return_value.set(value);
}

fn throw_js_error(scope: &mut v8::PinScope, message: &str) {
    let message = v8::String::new(scope, message).unwrap();
    let exception = v8::Exception::error(scope, message);
    scope.throw_exception(exception);
}

fn initialize_v8() {
    INITIALIZE_V8.call_once(|| {
        // rusty_v8 的预编译静态库只带 ICU stub。显式装入与 V8 14.9
        // 匹配的 ICU 77 数据，否则 Intl.DateTimeFormat 会直接终止进程。
        v8::icu::set_common_data_77(deno_core_icudata::ICU_DATA)
            .expect("initialize embedded ICU 77 data");
        let platform = v8::new_default_platform(0, false).make_shared();
        v8::V8::initialize_platform(platform);
        v8::V8::initialize();
    });
}

fn engine_worker(
    receiver: mpsc::Receiver<Command>,
    ready: mpsc::SyncSender<Option<v8::IsolateHandle>>,
    host: Arc<HostBridge>,
) {
    let engine = catch_unwind(AssertUnwindSafe(|| Engine::new(host)));
    let Ok(mut engine) = engine else {
        let _ = ready.send(None);
        return;
    };
    let _ = ready.send(Some(engine.isolate.thread_safe_handle()));
    while let Ok(command) = receiver.recv() {
        match command {
            Command::Eval {
                source,
                filename,
                reply,
            } => {
                let _ = reply.send(engine.eval(&source, &filename));
            }
            Command::Call {
                function_name,
                input_json,
                reply,
            } => {
                let _ = reply.send(engine.call(&function_name, &input_json));
            }
            Command::Shutdown => break,
        }
    }
}

fn json_result<'s>(
    scope: &mut v8::PinScope<'s, '_>,
    value: v8::Local<'s, v8::Value>,
) -> RuntimeResult {
    let Some(json) = v8::json::stringify(scope, value) else {
        return RuntimeResult::error("JavaScript result is not JSON serializable");
    };
    RuntimeResult::success(json.to_rust_string_lossy(scope))
}

fn sanitize_source_url(value: &str) -> String {
    value
        .chars()
        .map(|character| match character {
            '\r' | '\n' => '_',
            other => other,
        })
        .collect()
}

fn json_quote(value: &str) -> String {
    let mut output = String::with_capacity(value.len() + 2);
    output.push('"');
    for character in value.chars() {
        match character {
            '"' => output.push_str("\\\""),
            '\\' => output.push_str("\\\\"),
            '\n' => output.push_str("\\n"),
            '\r' => output.push_str("\\r"),
            '\t' => output.push_str("\\t"),
            character if character <= '\u{001f}' => {
                use std::fmt::Write;
                let _ = write!(output, "\\u{:04x}", character as u32);
            }
            character => output.push(character),
        }
    }
    output.push('"');
    output
}

unsafe fn read_utf8(
    pointer: *const u8,
    length: usize,
    maximum: usize,
    label: &str,
) -> Result<String, String> {
    if length > maximum {
        return Err(format!("{label} exceeds {maximum} bytes"));
    }
    if length == 0 {
        return Ok(String::new());
    }
    if pointer.is_null() {
        return Err(format!("{label} pointer is null"));
    }
    // SAFETY: The caller owns this memory for the duration of the C ABI call.
    let bytes = unsafe { slice::from_raw_parts(pointer, length) };
    String::from_utf8(bytes.to_vec()).map_err(|_| format!("{label} is not UTF-8"))
}

fn boxed_result(result: RuntimeResult) -> *mut c_void {
    Box::into_raw(Box::new(result)).cast()
}

fn dispatch(
    runtime: *mut c_void,
    command: impl FnOnce(mpsc::SyncSender<RuntimeResult>) -> Command,
) -> *mut c_void {
    if runtime.is_null() {
        return boxed_result(RuntimeResult::error("runtime pointer is null"));
    }
    // SAFETY: RuntimeHandle pointers are created and destroyed only by this ABI.
    let runtime = unsafe { &*runtime.cast::<RuntimeHandle>() };
    let (reply, receive) = mpsc::sync_channel(1);
    if runtime.sender.send(command(reply)).is_err() {
        return boxed_result(RuntimeResult::error("V8 worker is unavailable"));
    }
    match receive.recv() {
        Ok(result) => boxed_result(result),
        Err(_) => boxed_result(RuntimeResult::error("V8 worker stopped unexpectedly")),
    }
}

#[unsafe(no_mangle)]
pub extern "C" fn ali_slider_v8_abi_version() -> u32 {
    ABI_VERSION
}

#[unsafe(no_mangle)]
pub extern "C" fn ali_slider_v8_engine_version() -> *mut c_void {
    match catch_unwind(AssertUnwindSafe(|| {
        initialize_v8();
        boxed_result(RuntimeResult::success(json_quote(v8::V8::get_version())))
    })) {
        Ok(result) => result,
        Err(_) => boxed_result(RuntimeResult::error("native V8 version query panicked")),
    }
}

#[unsafe(no_mangle)]
pub extern "C" fn ali_slider_v8_runtime_create() -> *mut c_void {
    match catch_unwind(AssertUnwindSafe(|| {
        let (sender, receiver) = mpsc::channel();
        let (ready, initialized) = mpsc::sync_channel(1);
        let host = Arc::new(HostBridge::new());
        let worker_host = host.clone();
        let worker = thread::Builder::new()
            .name("ali-slider-v8".to_owned())
            .spawn(move || engine_worker(receiver, ready, worker_host))
            .ok()?;
        let isolate = initialized.recv().ok().flatten()?;
        let runtime = Box::into_raw(Box::new(RuntimeHandle {
            sender,
            isolate,
            host,
            worker: Some(worker),
        }));
        Some(runtime.cast::<c_void>())
    })) {
        Ok(Some(runtime)) => runtime,
        _ => std::ptr::null_mut(),
    }
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn ali_slider_v8_runtime_terminate(runtime: *mut c_void) -> i32 {
    if runtime.is_null() {
        return 0;
    }
    match catch_unwind(AssertUnwindSafe(|| {
        // SAFETY: RuntimeHandle pointers are created and destroyed only by this ABI.
        let runtime = unsafe { &*runtime.cast::<RuntimeHandle>() };
        runtime.host.cancelled.store(true, Ordering::Release);
        if runtime.isolate.terminate_execution() {
            1
        } else {
            0
        }
    })) {
        Ok(terminated) => terminated,
        Err(_) => 0,
    }
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn ali_slider_v8_runtime_set_host(
    runtime: *mut c_void,
    callback: usize,
    user_data: usize,
) -> i32 {
    if runtime.is_null() {
        return 0;
    }
    match catch_unwind(AssertUnwindSafe(|| {
        // SAFETY: RuntimeHandle pointers are created and destroyed only by this ABI.
        let runtime = unsafe { &*runtime.cast::<RuntimeHandle>() };
        runtime.host.user_data.store(user_data, Ordering::Release);
        runtime.host.callback.store(callback, Ordering::Release);
        1
    })) {
        Ok(configured) => configured,
        Err(_) => 0,
    }
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn ali_slider_v8_runtime_destroy(runtime: *mut c_void) {
    if runtime.is_null() {
        return;
    }
    let _ = catch_unwind(AssertUnwindSafe(|| {
        // SAFETY: Ownership is transferred back exactly once by the caller.
        drop(unsafe { Box::from_raw(runtime.cast::<RuntimeHandle>()) });
    }));
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn ali_slider_v8_runtime_eval(
    runtime: *mut c_void,
    source: *const u8,
    source_length: usize,
    filename: *const u8,
    filename_length: usize,
) -> *mut c_void {
    match catch_unwind(AssertUnwindSafe(|| {
        // SAFETY: Input buffers are borrowed only for this call and copied.
        let source = unsafe { read_utf8(source, source_length, MAX_SOURCE_BYTES, "source") }?;
        // SAFETY: Same ownership rule as source.
        let filename = unsafe { read_utf8(filename, filename_length, MAX_NAME_BYTES, "filename") }?;
        Ok::<_, String>(dispatch(runtime, move |reply| Command::Eval {
            source,
            filename,
            reply,
        }))
    })) {
        Ok(Ok(result)) => result,
        Ok(Err(error)) => boxed_result(RuntimeResult::error(error)),
        Err(_) => boxed_result(RuntimeResult::error("native V8 eval panicked")),
    }
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn ali_slider_v8_runtime_call(
    runtime: *mut c_void,
    function_name: *const u8,
    function_name_length: usize,
    input_json: *const u8,
    input_json_length: usize,
) -> *mut c_void {
    match catch_unwind(AssertUnwindSafe(|| {
        // SAFETY: Input buffers are borrowed only for this call and copied.
        let function_name = unsafe {
            read_utf8(
                function_name,
                function_name_length,
                MAX_NAME_BYTES,
                "function name",
            )
        }?;
        // SAFETY: Same ownership rule as function_name.
        let input_json =
            unsafe { read_utf8(input_json, input_json_length, MAX_JSON_BYTES, "input JSON") }?;
        Ok::<_, String>(dispatch(runtime, move |reply| Command::Call {
            function_name,
            input_json,
            reply,
        }))
    })) {
        Ok(Ok(result)) => result,
        Ok(Err(error)) => boxed_result(RuntimeResult::error(error)),
        Err(_) => boxed_result(RuntimeResult::error("native V8 call panicked")),
    }
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn ali_slider_v8_result_status(result: *const c_void) -> i32 {
    if result.is_null() {
        return 1;
    }
    // SAFETY: Result pointers are immutable until result_free.
    unsafe { (*result.cast::<RuntimeResult>()).status }
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn ali_slider_v8_result_data(result: *const c_void) -> *const u8 {
    if result.is_null() {
        return std::ptr::null();
    }
    // SAFETY: Result pointers are immutable until result_free.
    unsafe { (*result.cast::<RuntimeResult>()).data.as_ptr() }
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn ali_slider_v8_result_length(result: *const c_void) -> usize {
    if result.is_null() {
        return 0;
    }
    // SAFETY: Result pointers are immutable until result_free.
    unsafe { (*result.cast::<RuntimeResult>()).data.len() }
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn ali_slider_v8_result_free(result: *mut c_void) {
    if result.is_null() {
        return;
    }
    // SAFETY: Ownership is transferred back exactly once by the caller.
    drop(unsafe { Box::from_raw(result.cast::<RuntimeResult>()) });
}

#[cfg(test)]
mod tests {
    use super::*;

    unsafe fn copy_result(result: *mut c_void) -> (i32, String) {
        // SAFETY: The result belongs to this test until the matching free call.
        let status = unsafe { ali_slider_v8_result_status(result) };
        // SAFETY: The result buffer remains valid until free.
        let data = unsafe { ali_slider_v8_result_data(result) };
        // SAFETY: Same result lifetime as data.
        let length = unsafe { ali_slider_v8_result_length(result) };
        // SAFETY: The ABI guarantees a readable buffer of result_length bytes.
        let text = unsafe { slice::from_raw_parts(data, length) };
        let text = String::from_utf8(text.to_vec()).unwrap();
        // SAFETY: Each result is released exactly once.
        unsafe { ali_slider_v8_result_free(result) };
        (status, text)
    }

    #[test]
    fn quotes_json_errors() {
        assert_eq!(json_quote("a\n\"b\\c"), "\"a\\n\\\"b\\\\c\"");
    }

    #[test]
    fn code_cache_coalesces_same_source_and_keeps_other_sources_independent() {
        let store = ScriptCodeCacheStore::new();
        let first = match store.lookup("same-source") {
            ScriptCacheLookup::Compile(reservation) => reservation,
            ScriptCacheLookup::Cached(_) => panic!("new cache unexpectedly hit"),
        };
        let other = match store.lookup("other-source") {
            ScriptCacheLookup::Compile(reservation) => reservation,
            ScriptCacheLookup::Cached(_) => panic!("different source unexpectedly hit"),
        };
        other.finish(Some(vec![4, 5, 6]));

        thread::scope(|scope| {
            let waiter = scope.spawn(|| match store.lookup("same-source") {
                ScriptCacheLookup::Cached(value) => value,
                ScriptCacheLookup::Compile(reservation) => {
                    reservation.finish(None);
                    panic!("same source started a second concurrent compile")
                }
            });
            first.finish(Some(vec![1, 2, 3]));
            assert_eq!(waiter.join().unwrap(), vec![1, 2, 3]);
        });
        match store.lookup("other-source") {
            ScriptCacheLookup::Cached(value) => assert_eq!(value, vec![4, 5, 6]),
            ScriptCacheLookup::Compile(reservation) => {
                reservation.finish(None);
                panic!("completed different source was not cached")
            }
        }
    }

    #[test]
    fn keeps_one_isolate_and_returns_json() {
        let runtime = ali_slider_v8_runtime_create();
        assert!(!runtime.is_null());

        let source = br#"
          globalThis.counter = 0;
          globalThis.bump = async ({ step }) => ({
            value: counter += step,
            engine: "v8",
          });
          ({ ready: true });
        "#;
        let filename = b"runtime-spike.js";
        // SAFETY: All byte buffers remain alive for the duration of the call.
        let evaluated = unsafe {
            ali_slider_v8_runtime_eval(
                runtime,
                source.as_ptr(),
                source.len(),
                filename.as_ptr(),
                filename.len(),
            )
        };
        // SAFETY: evaluated is a fresh ABI-owned result.
        assert_eq!(
            unsafe { copy_result(evaluated) },
            (0, r#"{"ready":true}"#.to_owned())
        );

        for (input, expected) in [(br#"{"step":2}"#, 2), (br#"{"step":3}"#, 5)] {
            let name = b"bump";
            // SAFETY: All byte buffers remain alive for the duration of the call.
            let called = unsafe {
                ali_slider_v8_runtime_call(
                    runtime,
                    name.as_ptr(),
                    name.len(),
                    input.as_ptr(),
                    input.len(),
                )
            };
            // SAFETY: called is a fresh ABI-owned result.
            let (status, text) = unsafe { copy_result(called) };
            assert_eq!(status, 0);
            assert_eq!(text, format!(r#"{{"value":{expected},"engine":"v8"}}"#));
        }

        let bad_source = b"throw new Error('spike-boom')";
        // SAFETY: All byte buffers remain alive for the duration of the call.
        let failed = unsafe {
            ali_slider_v8_runtime_eval(
                runtime,
                bad_source.as_ptr(),
                bad_source.len(),
                std::ptr::null(),
                0,
            )
        };
        // SAFETY: failed is a fresh ABI-owned result.
        let (status, text) = unsafe { copy_result(failed) };
        assert_eq!(status, 1);
        assert!(text.contains("spike-boom"));

        // SAFETY: runtime is released exactly once after all calls complete.
        unsafe { ali_slider_v8_runtime_destroy(runtime) };
    }

    #[test]
    fn isolates_vm_context_intrinsics() {
        let runtime = ali_slider_v8_runtime_create();
        assert!(!runtime.is_null());
        let source = br#"
          const child = __aliV8CreateContext();
          child.seed = 40;
          child.globalThis = child;
          const value = __aliV8RunInContext(
            "Map.prototype.values = undefined; seed + 2",
            child,
            "child-context.js",
          );
          ({
            value,
            rootMapIntact: typeof Map.prototype.values === "function",
            childMapChanged: __aliV8RunInContext(
              "typeof Map.prototype.values",
              child,
              "child-check.js",
            ),
          });
        "#;
        let filename = b"context-spike.js";
        // SAFETY: All byte buffers remain alive for the duration of the call.
        let result = unsafe {
            ali_slider_v8_runtime_eval(
                runtime,
                source.as_ptr(),
                source.len(),
                filename.as_ptr(),
                filename.len(),
            )
        };
        // SAFETY: result is a fresh ABI-owned result.
        assert_eq!(
            unsafe { copy_result(result) },
            (
                0,
                r#"{"value":42,"rootMapIntact":true,"childMapChanged":"undefined"}"#.to_owned(),
            )
        );
        // SAFETY: runtime was created above and is destroyed exactly once.
        unsafe { ali_slider_v8_runtime_destroy(runtime) };
    }

    #[test]
    fn formats_dates_with_embedded_icu_data() {
        let runtime = ali_slider_v8_runtime_create();
        assert!(!runtime.is_null());
        let source = br#"new Intl.DateTimeFormat("en-US", {
          year: "numeric",
          month: "2-digit",
          day: "2-digit",
          timeZone: "UTC",
        }).format(0)"#;
        // SAFETY: All byte buffers and runtime remain alive for the call.
        let result = unsafe {
            ali_slider_v8_runtime_eval(runtime, source.as_ptr(), source.len(), std::ptr::null(), 0)
        };
        // SAFETY: result is a fresh ABI-owned result.
        assert_eq!(
            unsafe { copy_result(result) },
            (0, r#""01/01/1970""#.to_owned())
        );
        // SAFETY: runtime is released exactly once after the evaluation.
        unsafe { ali_slider_v8_runtime_destroy(runtime) };
    }

    #[test]
    fn terminates_runaway_script_and_reuses_runtime() {
        let runtime = ali_slider_v8_runtime_create();
        assert!(!runtime.is_null());
        let runtime_address = runtime as usize;
        let execution = thread::spawn(move || {
            let runtime = runtime_address as *mut c_void;
            let source = b"while (true) {}";
            // SAFETY: runtime outlives this thread and source remains valid for the call.
            let result = unsafe {
                ali_slider_v8_runtime_eval(
                    runtime,
                    source.as_ptr(),
                    source.len(),
                    std::ptr::null(),
                    0,
                )
            };
            result as usize
        });
        thread::sleep(std::time::Duration::from_millis(20));
        // SAFETY: runtime is alive while the worker executes JavaScript.
        assert_eq!(unsafe { ali_slider_v8_runtime_terminate(runtime) }, 1);
        let terminated = execution.join().unwrap() as *mut c_void;
        // SAFETY: terminated is a fresh ABI-owned result.
        assert_eq!(unsafe { copy_result(terminated) }.0, 2);

        let source = b"21 * 2";
        // SAFETY: runtime is reusable and source remains valid for the call.
        let recovered = unsafe {
            ali_slider_v8_runtime_eval(runtime, source.as_ptr(), source.len(), std::ptr::null(), 0)
        };
        // SAFETY: recovered is a fresh ABI-owned result.
        assert_eq!(unsafe { copy_result(recovered) }, (0, "42".to_owned()));
        // SAFETY: runtime is released exactly once after all calls complete.
        unsafe { ali_slider_v8_runtime_destroy(runtime) };
    }

    #[test]
    fn times_out_child_context_and_reuses_runtime() {
        let runtime = ali_slider_v8_runtime_create();
        assert!(!runtime.is_null());
        let source = br#"
          const child = __aliV8CreateContext({});
          __aliV8RunInContext(
            "while (true) {}",
            child,
            "child-timeout.js",
            20,
          );
        "#;
        let started = Instant::now();
        // SAFETY: All byte buffers and runtime remain alive for the call.
        let timed_out = unsafe {
            ali_slider_v8_runtime_eval(runtime, source.as_ptr(), source.len(), std::ptr::null(), 0)
        };
        // SAFETY: timed_out is a fresh ABI-owned result.
        let (status, message) = unsafe { copy_result(timed_out) };
        assert_eq!(status, 1);
        assert!(message.contains("child context execution timed out"));
        assert!(started.elapsed() < Duration::from_secs(1));

        let recovered_source = b"21 * 2";
        // SAFETY: Runtime remains valid and must be reusable after termination.
        let recovered = unsafe {
            ali_slider_v8_runtime_eval(
                runtime,
                recovered_source.as_ptr(),
                recovered_source.len(),
                std::ptr::null(),
                0,
            )
        };
        // SAFETY: recovered is a fresh ABI-owned result.
        assert_eq!(unsafe { copy_result(recovered) }, (0, "42".to_owned()));
        // SAFETY: runtime is released exactly once after all calls complete.
        unsafe { ali_slider_v8_runtime_destroy(runtime) };
    }
}
