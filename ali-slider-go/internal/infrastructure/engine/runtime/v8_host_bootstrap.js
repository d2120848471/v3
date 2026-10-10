/* ali-slider-go V8 host bootstrap. Plain script; no Node globals required. */
(() => {
  "use strict";

  if (typeof globalThis.__aliNativeHostCall !== "function") {
    throw new Error("native V8 host callback is unavailable");
  }
  if (
    typeof globalThis.__aliV8CreateContext !== "function"
    || typeof globalThis.__aliV8RunInContext !== "function"
    || typeof globalThis.__aliV8QueueMicrotask !== "function"
  ) {
    throw new Error("native V8 context callbacks are unavailable");
  }
  const nativeHostCall = globalThis.__aliNativeHostCall;
  const nativeCreateContext = globalThis.__aliV8CreateContext;
  const nativeCreateFunction = globalThis.__aliV8CreateNativeFunction;
  const nativeRunInContext = globalThis.__aliV8RunInContext;
  const nativeQueueMicrotask = globalThis.__aliV8QueueMicrotask;

  const nativeDateNow = Date.now.bind(Date);
  const timeOrigin = nativeDateNow();
  let timerSequence = 1;
  const timers = new Map();

  function hostCall(request) {
    const envelope = JSON.parse(
      nativeHostCall(JSON.stringify(request)),
    );
    if (!envelope || envelope.ok !== true) {
      throw new Error(envelope?.error || "V8 host operation failed");
    }
    return envelope.value;
  }

  function normalizeDelay(value) {
    const number = Number(value);
    if (!Number.isFinite(number) || number <= 0) return 0;
    return Math.min(Math.floor(number), 2_147_483_647);
  }

  function installTimer(callback, delay, repeat, args) {
    if (typeof callback !== "function") {
      callback = Function(String(callback));
    }
    const id = timerSequence++;
    // Node 对 0ms timeout/interval 也会至少跨一个 event-loop turn。若按同一
    // 毫秒立即重入，FeiLin 的 0ms 链会让 native Promise pump 忙循环。
    const interval = Math.max(1, normalizeDelay(delay));
    timers.set(id, {
      args,
      callback,
      due: nativeDateNow() + interval,
      interval,
      repeat,
    });
    return id;
  }

  globalThis.setTimeout = (callback, delay = 0, ...args) => (
    installTimer(callback, delay, false, args)
  );
  globalThis.clearTimeout = (id) => timers.delete(Number(id));
  globalThis.setInterval = (callback, delay = 0, ...args) => (
    installTimer(callback, delay, true, args)
  );
  globalThis.clearInterval = globalThis.clearTimeout;
  globalThis.setImmediate = (callback, ...args) => (
    installTimer(callback, 0, false, args)
  );
  globalThis.clearImmediate = globalThis.clearTimeout;
  globalThis.queueMicrotask = (callback) => {
    if (typeof callback !== "function") {
      throw new TypeError("queueMicrotask callback must be a function");
    }
    nativeQueueMicrotask(callback);
  };
  globalThis.__aliV8NextTimerDelay = () => {
    if (timers.size === 0) return 1;
    let due = Number.POSITIVE_INFINITY;
    for (const timer of timers.values()) due = Math.min(due, timer.due);
    return Math.max(0, due - nativeDateNow());
  };
  globalThis.__aliV8PumpTimers = () => {
    const now = nativeDateNow();
    const ready = [...timers.entries()]
      .filter(([, timer]) => timer.due <= now)
      .sort((left, right) => left[1].due - right[1].due);
    for (const [id, timer] of ready) {
      if (!timers.has(id)) continue;
      if (timer.repeat) {
        timer.due = now + Math.max(1, timer.interval);
      } else {
        timers.delete(id);
      }
      timer.callback(...timer.args);
    }
    return ready.length;
  };

  // PE 回放会用 Proxy 覆盖 now/timeOrigin。这里必须保留属性 configurable，
  // 否则 Proxy 返回虚拟时钟时会违反 ECMAScript 的不变量检查。
  globalThis.performance = {
    now: () => nativeDateNow() - timeOrigin,
    timeOrigin,
    toJSON: () => ({ timeOrigin }),
  };

  const utf8Encode = (text) => {
    const output = [];
    for (const character of String(text)) {
      const point = character.codePointAt(0);
      if (point <= 0x7f) output.push(point);
      else if (point <= 0x7ff) {
        output.push(0xc0 | (point >>> 6), 0x80 | (point & 0x3f));
      } else if (point <= 0xffff) {
        output.push(
          0xe0 | (point >>> 12),
          0x80 | ((point >>> 6) & 0x3f),
          0x80 | (point & 0x3f),
        );
      } else {
        output.push(
          0xf0 | (point >>> 18),
          0x80 | ((point >>> 12) & 0x3f),
          0x80 | ((point >>> 6) & 0x3f),
          0x80 | (point & 0x3f),
        );
      }
    }
    return new Uint8Array(output);
  };

  const utf8Decode = (input) => {
    const bytes = input instanceof Uint8Array
      ? input
      : new Uint8Array(input?.buffer ?? input ?? []);
    let output = "";
    for (let index = 0; index < bytes.length;) {
      const first = bytes[index++];
      let point;
      if (first < 0x80) point = first;
      else if ((first & 0xe0) === 0xc0) {
        point = ((first & 0x1f) << 6) | (bytes[index++] & 0x3f);
      } else if ((first & 0xf0) === 0xe0) {
        point = ((first & 0x0f) << 12)
          | ((bytes[index++] & 0x3f) << 6)
          | (bytes[index++] & 0x3f);
      } else {
        point = ((first & 0x07) << 18)
          | ((bytes[index++] & 0x3f) << 12)
          | ((bytes[index++] & 0x3f) << 6)
          | (bytes[index++] & 0x3f);
      }
      output += String.fromCodePoint(point);
    }
    return output;
  };

  class TextEncoder {
    get encoding() { return "utf-8"; }
    encode(value = "") { return utf8Encode(value); }
    encodeInto(value, destination) {
      const source = utf8Encode(value);
      const written = Math.min(source.length, destination.length);
      destination.set(source.subarray(0, written));
      return { read: String(value).length, written };
    }
  }

  class TextDecoder {
    constructor(label = "utf-8") { this.encoding = String(label).toLowerCase(); }
    decode(value = new Uint8Array()) { return utf8Decode(value); }
  }

  const base64Alphabet = (
    "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
  );
  function encodeBase64(bytes) {
    let output = "";
    for (let index = 0; index < bytes.length; index += 3) {
      const first = bytes[index];
      const second = bytes[index + 1];
      const third = bytes[index + 2];
      const value = (first << 16) | ((second ?? 0) << 8) | (third ?? 0);
      output += base64Alphabet[(value >>> 18) & 63];
      output += base64Alphabet[(value >>> 12) & 63];
      output += second === undefined ? "=" : base64Alphabet[(value >>> 6) & 63];
      output += third === undefined ? "=" : base64Alphabet[value & 63];
    }
    return output;
  }

  function decodeBase64(value) {
    const clean = String(value).replace(/[\t\n\r ]/g, "").replace(/=+$/, "");
    const output = [];
    let bits = 0;
    let count = 0;
    for (const character of clean) {
      const digit = base64Alphabet.indexOf(character);
      if (digit < 0) throw new TypeError("invalid base64");
      bits = (bits << 6) | digit;
      count += 6;
      if (count >= 8) {
        count -= 8;
        output.push((bits >>> count) & 0xff);
      }
    }
    return new Uint8Array(output);
  }

  function binaryEncode(value) {
    return new Uint8Array([...String(value)].map((character) => (
      character.charCodeAt(0) & 0xff
    )));
  }

  class Buffer extends Uint8Array {
    static from(value, encoding = "utf8") {
      if (typeof value === "string") {
        const normalized = String(encoding).toLowerCase();
        if (normalized === "base64") return new Buffer(decodeBase64(value));
        if (["binary", "latin1"].includes(normalized)) {
          return new Buffer(binaryEncode(value));
        }
        if (normalized === "hex") {
          const bytes = [];
          for (let index = 0; index + 1 < value.length; index += 2) {
            bytes.push(Number.parseInt(value.slice(index, index + 2), 16));
          }
          return new Buffer(bytes);
        }
        return new Buffer(utf8Encode(value));
      }
      if (value instanceof ArrayBuffer) return new Buffer(new Uint8Array(value));
      if (ArrayBuffer.isView(value)) {
        return new Buffer(new Uint8Array(value.buffer, value.byteOffset, value.byteLength));
      }
      return new Buffer(value ?? []);
    }
    static alloc(length, fill = 0) {
      const value = new Buffer(Number(length));
      value.fill(fill);
      return value;
    }
    static concat(values, totalLength) {
      const length = totalLength ?? values.reduce((sum, value) => sum + value.length, 0);
      const output = Buffer.alloc(length);
      let offset = 0;
      for (const value of values) {
        output.set(value.subarray(0, Math.max(0, length - offset)), offset);
        offset += value.length;
        if (offset >= length) break;
      }
      return output;
    }
    static isBuffer(value) { return value instanceof Buffer; }
    static byteLength(value, encoding = "utf8") {
      return Buffer.from(value, encoding).length;
    }
    toString(encoding = "utf8") {
      const normalized = String(encoding).toLowerCase();
      if (normalized === "base64") return encodeBase64(this);
      if (["binary", "latin1"].includes(normalized)) {
        return String.fromCharCode(...this);
      }
      if (normalized === "hex") {
        return [...this].map((byte) => byte.toString(16).padStart(2, "0")).join("");
      }
      return utf8Decode(this);
    }
  }

  globalThis.TextEncoder = TextEncoder;
  globalThis.TextDecoder = TextDecoder;
  globalThis.Buffer = Buffer;
  globalThis.atob = (value) => Buffer.from(value, "base64").toString("binary");
  globalThis.btoa = (value) => Buffer.from(value, "binary").toString("base64");

  const decodeForm = (value) => decodeURIComponent(String(value).replace(/\+/g, " "));
  const encodeForm = (value) => encodeURIComponent(String(value)).replace(/%20/g, "+");
  class URLSearchParams {
    constructor(init = "") {
      this._pairs = [];
      if (typeof init === "string") {
        for (const part of init.replace(/^\?/, "").split("&")) {
          if (!part) continue;
          const split = part.indexOf("=");
          this.append(
            decodeForm(split < 0 ? part : part.slice(0, split)),
            decodeForm(split < 0 ? "" : part.slice(split + 1)),
          );
        }
      } else if (init && typeof init[Symbol.iterator] === "function") {
        for (const pair of init) this.append(pair[0], pair[1]);
      } else if (init && typeof init === "object") {
        for (const [name, value] of Object.entries(init)) this.append(name, value);
      }
    }
    append(name, value) { this._pairs.push([String(name), String(value)]); }
    delete(name) { this._pairs = this._pairs.filter(([key]) => key !== String(name)); }
    get(name) { return this._pairs.find(([key]) => key === String(name))?.[1] ?? null; }
    getAll(name) { return this._pairs.filter(([key]) => key === String(name)).map(([, value]) => value); }
    has(name) { return this._pairs.some(([key]) => key === String(name)); }
    set(name, value) {
      this.delete(name);
      this.append(name, value);
    }
    sort() { this._pairs.sort((left, right) => left[0].localeCompare(right[0])); }
    entries() { return this._pairs[Symbol.iterator](); }
    keys() { return this._pairs.map(([name]) => name)[Symbol.iterator](); }
    values() { return this._pairs.map(([, value]) => value)[Symbol.iterator](); }
    forEach(callback, thisArg) {
      for (const [name, value] of this._pairs) callback.call(thisArg, value, name, this);
    }
    toString() {
      return this._pairs.map(([name, value]) => `${encodeForm(name)}=${encodeForm(value)}`).join("&");
    }
    [Symbol.iterator]() { return this.entries(); }
  }

  class URL {
    constructor(input, base) {
      let value = String(input);
      if (!/^[a-z][a-z\d+.-]*:/i.test(value)) {
        const baseURL = new URL(base);
        value = value.startsWith("//")
          ? `${baseURL.protocol}${value}`
          : value.startsWith("/")
            ? `${baseURL.origin}${value}`
            : `${baseURL.origin}${baseURL.pathname.replace(/[^/]*$/, "")}${value}`;
      }
      const match = /^([a-z][a-z\d+.-]*:)(?:\/\/([^/?#]*))?([^?#]*)(\?[^#]*)?(#.*)?$/i.exec(value);
      if (!match) throw new TypeError("invalid URL");
      this.protocol = match[1].toLowerCase();
      const authority = match[2] ?? "";
      const at = authority.lastIndexOf("@");
      const userInfo = at >= 0 ? authority.slice(0, at) : "";
      const host = at >= 0 ? authority.slice(at + 1) : authority;
      const colon = userInfo.indexOf(":");
      this.username = decodeURIComponent(colon < 0 ? userInfo : userInfo.slice(0, colon));
      this.password = decodeURIComponent(colon < 0 ? "" : userInfo.slice(colon + 1));
      const portMatch = /^(.*?)(?::(\d+))?$/.exec(host);
      this.hostname = portMatch?.[1]?.toLowerCase() ?? "";
      this.port = portMatch?.[2] ?? "";
      this.host = this.port ? `${this.hostname}:${this.port}` : this.hostname;
      this.pathname = match[3] || "/";
      this.search = match[4] ?? "";
      this.hash = match[5] ?? "";
      this.origin = this.host ? `${this.protocol}//${this.host}` : "null";
      const auth = userInfo ? `${userInfo}@` : "";
      this.href = `${this.protocol}//${auth}${this.host}${this.pathname}${this.search}${this.hash}`;
    }
    toString() { return this.href; }
    toJSON() { return this.href; }
  }

  class Headers {
    constructor(init = {}) {
      this._values = new Map();
      if (init instanceof Headers) init = init.entries();
      if (init && typeof init[Symbol.iterator] === "function") {
        for (const [name, value] of init) this.append(name, value);
      } else {
        for (const [name, value] of Object.entries(init ?? {})) this.append(name, value);
      }
    }
    append(name, value) {
      const key = String(name).toLowerCase();
      const text = String(value);
      this._values.set(key, this._values.has(key) ? `${this._values.get(key)}, ${text}` : text);
    }
    delete(name) { this._values.delete(String(name).toLowerCase()); }
    get(name) { return this._values.get(String(name).toLowerCase()) ?? null; }
    has(name) { return this._values.has(String(name).toLowerCase()); }
    set(name, value) { this._values.set(String(name).toLowerCase(), String(value)); }
    entries() { return this._values.entries(); }
    keys() { return this._values.keys(); }
    values() { return this._values.values(); }
    forEach(callback, thisArg) {
      for (const [name, value] of this._values) callback.call(thisArg, value, name, this);
    }
    [Symbol.iterator]() { return this.entries(); }
  }

  class Blob {
    constructor(parts = [], options = {}) {
      this.type = String(options.type ?? "").toLowerCase();
      this._bytes = Buffer.concat(parts.map((part) => (
        part instanceof Uint8Array ? Buffer.from(part) : Buffer.from(String(part))
      )));
      this.size = this._bytes.length;
    }
    async arrayBuffer() { return this._bytes.slice().buffer; }
    async text() { return this._bytes.toString("utf8"); }
    slice(start = 0, end = this.size, type = "") {
      return new Blob([this._bytes.slice(start, end)], { type });
    }
  }

  class File extends Blob {
    constructor(parts, name, options = {}) {
      super(parts, options);
      this.name = String(name);
      this.lastModified = Number(options.lastModified ?? nativeDateNow());
    }
  }

  class FormData {
    constructor() { this._pairs = []; }
    append(name, value, filename) { this._pairs.push([String(name), value, filename]); }
    delete(name) { this._pairs = this._pairs.filter(([key]) => key !== String(name)); }
    get(name) { return this._pairs.find(([key]) => key === String(name))?.[1] ?? null; }
    getAll(name) { return this._pairs.filter(([key]) => key === String(name)).map(([, value]) => value); }
    has(name) { return this._pairs.some(([key]) => key === String(name)); }
    set(name, value, filename) { this.delete(name); this.append(name, value, filename); }
    entries() { return this._pairs.map(([name, value]) => [name, value])[Symbol.iterator](); }
    [Symbol.iterator]() { return this.entries(); }
  }

  class Request {
    constructor(input, init = {}) {
      const source = input instanceof Request ? input : {};
      this.url = String(input instanceof Request ? input.url : input);
      this.method = String(init.method ?? source.method ?? "GET").toUpperCase();
      this.headers = new Headers(init.headers ?? source.headers ?? {});
      this.body = init.body ?? source.body ?? null;
      this.signal = init.signal ?? source.signal ?? null;
    }
    clone() { return new Request(this, this); }
  }

  class Response {
    constructor(body = "", init = {}) {
      this._body = body === null ? "" : String(body);
      this.status = Number(init.status ?? 200);
      this.statusText = String(init.statusText ?? "");
      this.headers = new Headers(init.headers ?? {});
      this.url = String(init.url ?? "");
      this.redirected = Boolean(init.redirected);
      this.type = "basic";
      this.ok = this.status >= 200 && this.status < 300;
      this.body = { cancel: async () => {} };
    }
    async text() { return this._body; }
    async json() { return JSON.parse(this._body); }
    async arrayBuffer() { return Buffer.from(this._body).buffer; }
    clone() {
      return new Response(this._body, {
        headers: this.headers,
        redirected: this.redirected,
        status: this.status,
        statusText: this.statusText,
        url: this.url,
      });
    }
  }

  class AbortSignal {
    constructor() { this.aborted = false; this.reason = undefined; this._listeners = []; }
    addEventListener(type, callback) { if (type === "abort") this._listeners.push(callback); }
    removeEventListener(type, callback) {
      if (type === "abort") this._listeners = this._listeners.filter((value) => value !== callback);
    }
  }

  class AbortController {
    constructor() { this.signal = new AbortSignal(); }
    abort(reason = new Error("AbortError")) {
      if (this.signal.aborted) return;
      this.signal.aborted = true;
      this.signal.reason = reason;
      for (const listener of this.signal._listeners) listener.call(this.signal, { type: "abort" });
    }
  }

  globalThis.URL = URL;
  globalThis.URLSearchParams = URLSearchParams;
  globalThis.Headers = Headers;
  globalThis.Blob = Blob;
  globalThis.File = File;
  globalThis.FormData = FormData;
  globalThis.Request = Request;
  globalThis.Response = Response;
  globalThis.AbortSignal = AbortSignal;
  globalThis.AbortController = AbortController;

  globalThis.fetch = (input, init = {}) => new Promise((resolve, reject) => {
    const request = input instanceof Request ? new Request(input, init) : new Request(input, init);
    if (request.signal?.aborted) {
      reject(request.signal.reason);
      return;
    }
    let response;
    try {
      response = hostCall({
        op: "http",
        request: {
          body: request.body === null ? null : String(request.body),
          headers: Object.fromEntries(request.headers.entries()),
          method: request.method,
          redirect: String(init.redirect ?? "follow"),
          url: request.url,
        },
      });
    } catch (error) {
      reject(error);
      return;
    }
    // 浏览器/Node 的 fetch 永远在后续 event-loop turn 完成。宿主 HTTP 虽然
    // 是同步 C ABI，也必须延后 resolve，避免整条 SDK 网络链被压进同一次
    // V8 microtask checkpoint。
    setTimeout(() => {
      if (request.signal?.aborted) {
        reject(request.signal.reason);
        return;
      }
      resolve(new Response(response.body, {
        headers: response.headers,
        redirected: response.redirected,
        status: response.status,
        statusText: response.statusText,
        url: response.url,
      }));
    }, 0);
  });

  globalThis.crypto = Object.freeze({
    getRandomValues(view) {
      if (!ArrayBuffer.isView(view) || view.byteLength > 65_536) {
        throw new TypeError("invalid getRandomValues target");
      }
      const response = hostCall({ op: "random", length: view.byteLength });
      const bytes = decodeBase64(response.base64);
      new Uint8Array(view.buffer, view.byteOffset, view.byteLength).set(bytes);
      return view;
    },
    randomUUID() {
      const bytes = this.getRandomValues(new Uint8Array(16));
      bytes[6] = (bytes[6] & 0x0f) | 0x40;
      bytes[8] = (bytes[8] & 0x3f) | 0x80;
      const hex = [...bytes].map((byte) => byte.toString(16).padStart(2, "0")).join("");
      return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
    },
    subtle: Object.freeze({}),
  });

  globalThis.console = globalThis.console ?? Object.freeze({
    debug() {}, error() {}, info() {}, log() {}, warn() {},
  });

  const virtualFiles = new Map();
  const virtualWrites = [];
  const fs = {
    _clear() { virtualFiles.clear(); virtualWrites.length = 0; },
    _set(path, value) { virtualFiles.set(String(path), String(value)); },
    _takeWrites() { return virtualWrites.splice(0); },
    readFileSync(path, encoding = null) {
      const key = String(path);
      if (!virtualFiles.has(key)) throw new Error(`virtual file not found: ${key}`);
      const value = virtualFiles.get(key);
      return encoding ? value : Buffer.from(value);
    },
    statSync(path) {
      const exists = virtualFiles.has(String(path));
      if (!exists) throw new Error("virtual file not found");
      return { isFile: () => true };
    },
    writeFileSync(fd, value) {
      virtualWrites.push({ fd: Number(fd), value: String(value) });
    },
  };

  const node24RealmCompatSource = `(() => {
    const defineGlobal = (name, value) => Object.defineProperty(globalThis, name, {
      configurable: true,
      writable: true,
      value,
    });

    if (typeof DisposableStack === "undefined") {
      class DisposableStack {
        constructor() { this._disposed = false; this._callbacks = []; }
        get disposed() { return this._disposed; }
        use(value) {
          if (value != null && typeof value[Symbol.dispose] === "function") {
            this._callbacks.push(() => value[Symbol.dispose]());
          }
          return value;
        }
        adopt(value, callback) { this._callbacks.push(() => callback(value)); return value; }
        defer(callback) { this._callbacks.push(callback); }
        move() {
          const next = new DisposableStack();
          next._callbacks = this._callbacks;
          this._callbacks = [];
          this._disposed = true;
          return next;
        }
        dispose() {
          if (this._disposed) return;
          this._disposed = true;
          for (const callback of this._callbacks.reverse()) callback();
          this._callbacks = [];
        }
      }
      defineGlobal("DisposableStack", DisposableStack);
    }

    if (typeof AsyncDisposableStack === "undefined") {
      class AsyncDisposableStack {
        constructor() { this._disposed = false; this._callbacks = []; }
        get disposed() { return this._disposed; }
        use(value) {
          const callback = value?.[Symbol.asyncDispose] ?? value?.[Symbol.dispose];
          if (typeof callback === "function") {
            this._callbacks.push(() => callback.call(value));
          }
          return value;
        }
        adopt(value, callback) { this._callbacks.push(() => callback(value)); return value; }
        defer(callback) { this._callbacks.push(callback); }
        move() {
          const next = new AsyncDisposableStack();
          next._callbacks = this._callbacks;
          this._callbacks = [];
          this._disposed = true;
          return next;
        }
        async disposeAsync() {
          if (this._disposed) return;
          this._disposed = true;
          for (const callback of this._callbacks.reverse()) await callback();
          this._callbacks = [];
        }
      }
      defineGlobal("AsyncDisposableStack", AsyncDisposableStack);
    }

    if (typeof SuppressedError === "undefined") {
      class SuppressedError extends Error {
        constructor(error, suppressed, message = "") {
          super(message);
          this.name = "SuppressedError";
          this.error = error;
          this.suppressed = suppressed;
        }
      }
      defineGlobal("SuppressedError", SuppressedError);
    }

    if (typeof Float16Array === "undefined") {
      class Float16Array extends Uint16Array {}
      Object.defineProperty(Float16Array, "BYTES_PER_ELEMENT", { value: 2 });
      Object.defineProperty(Float16Array.prototype, "BYTES_PER_ELEMENT", { value: 2 });
      defineGlobal("Float16Array", Float16Array);
    }

    if (typeof WebAssembly === "undefined") {
      class CompileError extends Error { constructor(message = "") { super(message); this.name = "CompileError"; } }
      class LinkError extends Error { constructor(message = "") { super(message); this.name = "LinkError"; } }
      class RuntimeError extends Error { constructor(message = "") { super(message); this.name = "RuntimeError"; } }
      const unavailable = () => { throw new CompileError("WebAssembly is unavailable in jitless V8"); };
      const unavailableAsync = async () => unavailable();
      const wasm = {};
      for (const [name, value] of Object.entries({
        compile: unavailableAsync,
        validate: () => false,
        instantiate: unavailableAsync,
        Module: class Module { constructor() { unavailable(); } },
        Instance: class Instance { constructor() { unavailable(); } },
        Table: class Table { constructor() { unavailable(); } },
        Memory: class Memory { constructor() { unavailable(); } },
        Global: class Global { constructor() { unavailable(); } },
        Tag: class Tag { constructor() { unavailable(); } },
        JSTag: Object.freeze({}),
        Exception: class Exception { constructor() { unavailable(); } },
        CompileError,
        LinkError,
        RuntimeError,
        compileStreaming: unavailableAsync,
        instantiateStreaming: unavailableAsync,
      })) {
        Object.defineProperty(wasm, name, { configurable: true, writable: true, value });
      }
      Object.defineProperty(wasm, Symbol.toStringTag, { value: "WebAssembly" });
      defineGlobal("WebAssembly", wasm);
    }

    return Reflect.deleteProperty(globalThis, "Temporal");
  })()`;

  const vmContexts = new WeakMap();
  const vm = {
    createNativeFunction(context, name, length, callback) {
      const target = vmContexts.get(context);
      if (!target || typeof nativeCreateFunction !== "function") {
        throw new Error("WAF browser bindings require an updated V8 runtime");
      }
      return nativeCreateFunction(target, name, length, callback);
    },
    createContext(sandbox) {
      if (!sandbox || typeof sandbox !== "object") {
        throw new TypeError("vm.createContext sandbox must be an object");
      }
      if (vmContexts.has(sandbox)) return sandbox;
      const nativeContext = nativeCreateContext();
      // 复制放在 root JS realm 完成，避免 native callback 同时枚举 root
      // 对象并进入 child Context。
      for (const name of Object.getOwnPropertyNames(sandbox)) {
        const value = sandbox[name];
        if (!Reflect.set(
          nativeContext,
          name,
          value === sandbox ? nativeContext : value,
        )) {
          throw new Error("V8 context sandbox property copy failed");
        }
      }
      // 将 child realm 的可观察全局面约束为生产 oracle Node 24.14.1，
      // 避免底层 V8 升级后公开脚本因能力检测走到不同分支。
      const realmCompatible = nativeRunInContext(
        node24RealmCompatSource,
        nativeContext,
        "node24-context-compat.js",
      );
      if (!realmCompatible) {
        throw new Error("unable to install Node 24 V8 realm compatibility");
      }
      // Node vm.createContext 返回的是 contextified sandbox；在 Context 内部，
      // globalThis 是另一个 global proxy。用一层宿主 Proxy 保留这个身份差异，
      // 同时把对 sandbox 的读写继续透传给真实 V8 global。
      let context;
      context = new Proxy(nativeContext, {
        get(target, property) {
          const value = Reflect.get(target, property, target);
          return value === target ? context : value;
        },
        set(target, property, value) {
          return Reflect.set(
            target,
            property,
            value === context ? target : value,
            target,
          );
        },
      });
      vmContexts.set(context, nativeContext);
      return context;
    },
    runInContext(source, context, options = {}) {
      const nativeContext = vmContexts.get(context);
      if (!nativeContext) {
        throw new TypeError("vm.runInContext target is not a V8 context");
      }
      return nativeRunInContext(
        String(source),
        nativeContext,
        String(options?.filename ?? "v8-context.js"),
        Number(options?.timeout ?? 1_000),
      );
    },
  };

  class ExitSignal extends Error {
    constructor(code) { super(`process exited with ${code}`); this.code = Number(code); }
  }
  const process = {
    argv: ["v8", "bridge"],
    env: Object.freeze({}),
    exit(code = 0) { throw new ExitSignal(code); },
    on() {},
    stdin: { on() {}, once() {}, off() {}, pause() {}, resume() {} },
  };

  globalThis.__aliNodeCompat = Object.freeze({
    Buffer,
    ExitSignal,
    Worker: class Worker {},
    createInterface() { throw new Error("challenge-host is unavailable in V8 mode"); },
    fs,
    isMainThread: true,
    parentPort: null,
    pathToFileURL(path) { return { href: `file://${String(path)}` }; },
    process,
    vm,
    webcrypto: globalThis.crypto,
    workerData: null,
  });
  globalThis.__aliHostCall = hostCall;
})();
true;
