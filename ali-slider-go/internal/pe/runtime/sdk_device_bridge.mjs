#!/usr/bin/env node

/**
 * 公开 AliyunCaptcha.js 的最小 Node 运行桥。
 *
 * - `probe-log1`：不发送网络请求，只运行到首次 XHR.send 并输出 Log1 边界；
 * - `live-token`：允许 SDK 正常请求 Log1 与公开 FeiLin 脚本，返回当前 DeviceToken；
 * - `profile-token`：同样只走公开 Log1/Log2，但只输出 FeiLin 扁平化字段的
 *   键名、类型、空值和长度，不输出 token、key 或字段明文；
 * - `challenge-worker`：保持同一个 FeiLin VM；先输出 Init token，再从 stdin
 *   接收动态 PE 实际观测到的 getter 参数，在同一 VM 内回放动态 PE native mm，
 *   并生成 Verify token；
 * - `challenge-host`：一个 Node 进程内启动多个隔离 Worker/VM，按
 *   `sessionId` 对 JSONL 消息分流；池化模式可原位重建已完成的 VM 槽位。
 */

import fs from "node:fs";
import { createInterface } from "node:readline";
import vm from "node:vm";
import { webcrypto } from "node:crypto";
import { pathToFileURL } from "node:url";
import {
  Worker,
  isMainThread,
  parentPort,
  workerData,
} from "node:worker_threads";


const TARGET_ORIGIN = "http://localhost:38185";
const TARGET_REFERER = `${TARGET_ORIGIN}/`;
const DEVICE_TOKEN_MODES = Object.freeze([
  "live-token",
  "profile-token",
  "challenge-worker",
  "challenge-host",
]);
const WORKER_COMPLETION_MAX_BYTES = 64 * 1024;
const HOST_COMMAND_MAX_BYTES = WORKER_COMPLETION_MAX_BYTES + 1024;
const HOST_MAX_VM_COUNT = 8;
const BRIDGE_EVENT_STATE = Symbol("bridgeEventState");
const DEVICE_PROFILE_MAX_BYTES = 8 * 1024;
const DYNAMIC_ASSET_MAX_BYTES = 2 * 1024 * 1024;


/**
 * 解开 Go 逐轮生成的设备画像。
 *
 * 画像不在 Node 侧生成：本轮的 HTTP 头由 Go 发出，两个 Node bridge 各自构造自己
 * 的 DOM，三处必须是同一台设备。唯一的事实来源因此只能在 Go，这里只负责解
 * 码与结构校验。经 base64 传递是为了让 argv 里不出现引号、空格与非 ASCII。
 */
function decodeDeviceProfile(encoded) {
  if (typeof encoded !== "string" || !encoded) {
    throw new Error("必须通过 --device-profile 传入本轮设备画像");
  }
  if (encoded.length > DEVICE_PROFILE_MAX_BYTES) {
    throw new Error("--device-profile 超出长度上限");
  }
  let profile;
  try {
    profile = JSON.parse(Buffer.from(encoded, "base64").toString("utf8"));
  } catch {
    throw new Error("--device-profile 不是有效的 base64 JSON");
  }
  if (!profile || typeof profile !== "object") {
    throw new Error("--device-profile 顶层必须是 object");
  }
  for (const name of [
    "userAgent",
    "platform",
    "secChUa",
    "acceptLanguage",
    "screen",
    "gpu",
    "canvasSeed",
  ]) {
    if (profile[name] === undefined || profile[name] === null) {
      throw new Error(`设备画像缺少字段 ${name}`);
    }
  }
  return Object.freeze(profile);
}


/**
 * 由画像的 canvasSeed 派生一条确定性伪随机字节流。
 *
 * Canvas 与字体度量必须同时满足两个相反的要求：同一轮内**每次读取都一致**（真实
 * GPU 渲染同一幅图不会变），跨轮之间**互不相同**（否则又变回一个恒定主键）。用
 * 画像自带的种子做 xorshift 正好两头都满足。
 */
function makeSeededStream(seed) {
  let state = 0x811c9dc5;
  for (const character of String(seed)) {
    state = Math.imul(state ^ character.charCodeAt(0), 0x01000193) >>> 0;
  }
  return function next() {
    state ^= state << 13;
    state >>>= 0;
    state ^= state >>> 17;
    state ^= state << 5;
    state >>>= 0;
    return state;
  };
}


function seededBase64(seed, length) {
  const alphabet = (
    "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
  );
  const next = makeSeededStream(seed);
  let output = "";
  while (output.length < length) {
    output += alphabet[next() % alphabet.length];
  }
  return output;
}


function browserRequestHeaders(
  profile,
  extra = {},
  {
    destination = "empty",
    includeOrigin = true,
    mode = "cors",
  } = {},
) {
  return {
    Accept: "*/*",
    "Accept-Language": profile.acceptLanguage,
    ...(includeOrigin ? { Origin: TARGET_ORIGIN } : {}),
    Referer: TARGET_REFERER,
    "Sec-CH-UA": profile.secChUa,
    "Sec-CH-UA-Mobile": profile.secChUaMobile,
    "Sec-CH-UA-Platform": profile.secChUaPlatform,
    "Sec-Fetch-Dest": destination,
    "Sec-Fetch-Mode": mode,
    "Sec-Fetch-Site": "cross-site",
    Priority: "u=1, i",
    "User-Agent": profile.userAgent,
    ...extra,
  };
}


const SAFE_SDK_FAILURE_CODES = new Set([
  "TRACELESS_TIMEOUT_READY",
  "TRACELESS_TIMEOUT_CALLING_INIT",
  "TRACELESS_TIMEOUT_INIT_REJECTED",
  "TRACELESS_TIMEOUT_INIT_RESPONSE",
  "TRACELESS_TIMEOUT_INSTANCE",
  "TRACELESS_TIMEOUT_CLICK",
  "TRACELESS_TIMEOUT_VERIFY_REQUEST",
  "TRACELESS_TIMEOUT_VERIFY_RESPONSE",
  "TRACELESS_INIT_THROW",
  "TRACELESS_FAIL_CALLBACK",
  "TRACELESS_CLOSE_CALLBACK",
  "SLIDING_TIMEOUT_READY",
  "SLIDING_TIMEOUT_CALLING_INIT",
  "SLIDING_TIMEOUT_INIT_REJECTED",
  "SLIDING_TIMEOUT_INIT_RESPONSE",
  "SLIDING_TIMEOUT_INSTANCE",
  "SLIDING_TIMEOUT_DOM",
  "SLIDING_TIMEOUT_DRAG",
  "SLIDING_TIMEOUT_DRAG_COMPLETE",
  "SLIDING_TIMEOUT_VERIFY_REQUEST",
  "SLIDING_TIMEOUT_VERIFY_RESPONSE",
  "SLIDING_INIT_THROW",
  "SLIDING_FAIL_CALLBACK",
  "SLIDING_ERROR_CALLBACK",
  "SLIDING_EVENT_CONSTRUCT",
  "SLIDING_TOUCHSTART_DISPATCH",
  "SLIDING_TOUCHMOVE_DISPATCH",
  "SLIDING_TOUCHEND_DISPATCH",
  "SLIDING_TRACK_DISPATCH",
]);


function tracelessBridgeError(code) {
  const error = new Error("无痕验证码执行失败");
  error.safeCode = code;
  return error;
}


function slidingBridgeError(code) {
  const error = new Error("拖动验证码执行失败");
  error.safeCode = code;
  return error;
}


function safeDeviceBridgeFailureMessage(error) {
  const code = error?.safeCode;
  return SAFE_SDK_FAILURE_CODES.has(code)
    ? `Node 设备桥执行失败 [${code}]`
    : "Node 设备桥执行失败";
}


function multiplexSessionId() {
  const sessionId = workerData?.challengeSessionId;
  return (
    !isMainThread
    && parentPort !== null
    && Number.isInteger(sessionId)
    && sessionId >= 0
  ) ? sessionId : null;
}


function writeBridgeOutput(payload) {
  const sessionId = multiplexSessionId();
  if (sessionId !== null) {
    parentPort.postMessage({ sessionId, ...payload });
    return;
  }
  fs.writeFileSync(1, `${JSON.stringify(payload)}\n`);
}


function writeFatalError(error) {
  const sessionId = multiplexSessionId();
  if (sessionId !== null) {
    parentPort.postMessage({
      sessionId,
      stage: "error",
      error: safeDeviceBridgeFailureMessage(error),
    });
    // 给 parentPort 一个 turn 送出固定错误，host 收到后会终止该槽位。
    setTimeout(() => process.exit(1), 25);
    return;
  }
  fs.writeFileSync(
    2,
    `${JSON.stringify({
      error: safeDeviceBridgeFailureMessage(error),
      name: "Error",
    })}\n`,
  );
  process.exit(1);
}


function parseArguments(argv) {
  const options = {
    mode: "probe-log1",
    sdkPath: "",
    prefix: "fsgtmi",
    region: "cn",
    timeoutMs: 15_000,
    networkEnabled: false,
    deviceProfile: null,
    vmCount: 1,
    persistentHost: false,
  };
  let encodedProfile = "";
  for (let index = 2; index < argv.length; index += 1) {
    const argument = argv[index];
    if (argument === "--mode") {
      options.mode = argv[++index] ?? "";
    } else if (argument === "--sdk") {
      options.sdkPath = argv[++index] ?? "";
    } else if (argument === "--prefix") {
      options.prefix = argv[++index] ?? "";
    } else if (argument === "--region") {
      options.region = argv[++index] ?? "";
    } else if (argument === "--timeout-ms") {
      options.timeoutMs = Number(argv[++index]);
    } else if (argument === "--device-profile") {
      encodedProfile = argv[++index] ?? "";
    } else if (argument === "--vm-count") {
      options.vmCount = Number(argv[++index]);
    } else if (argument === "--persistent-host") {
      options.persistentHost = true;
    } else {
      throw new Error(`未知参数：${argument}`);
    }
  }
  if (
    ![
      "probe-log1",
      "live-token",
      "challenge-worker",
      "challenge-host",
      "profile-token",
    ].includes(options.mode)
  ) {
    throw new Error(
      "mode 必须是 probe-log1、live-token、profile-token、"
        + "challenge-worker 或 challenge-host，"
        + `实际为 ${options.mode}`,
    );
  }
  if (!options.sdkPath) {
    throw new Error("必须通过 --sdk 指定公开 AliyunCaptcha.js");
  }
  if (!Number.isInteger(options.timeoutMs) || options.timeoutMs < 1_000) {
    throw new Error("--timeout-ms 必须是至少 1000 的整数");
  }
  if (
    !Number.isInteger(options.vmCount)
    || options.vmCount < 1
    || options.vmCount > HOST_MAX_VM_COUNT
  ) {
    throw new Error(`--vm-count 必须是 1..${HOST_MAX_VM_COUNT} 的整数`);
  }
  if (options.persistentHost && options.mode !== "challenge-host") {
    throw new Error("--persistent-host 只能用于 challenge-host");
  }
  options.networkEnabled = DEVICE_TOKEN_MODES.includes(options.mode);
  // 画像是必填的：没有它就没有"本轮这台设备"，也就无从与 Go 的 HTTP 头和
  // 动态 PE 的 DOM 对齐。
  options.deviceProfile = decodeDeviceProfile(encodedProfile);
  return options;
}


const LOG1_PLAINTEXT_CAPTURE_HOOK = "__ALI_LOG1_PLAINTEXT_CAPTURE__";
const SDK_STATE_CAPTURE_FIELDS = Object.freeze([
  "prefix",
  "region",
  "appName",
  "appKey",
  "endpoints",
  "deviceCallback",
  "deviceConfig",
  "DeviceConfig",
]);


/**
 * 公开 SDK 热更新会重命名闭包变量，也可能改变对象方法的压缩写法。状态探针
 * 因此不再改写 `_extend:function(...)` 一类源码文本，而是在 VM 内短暂观察这些
 * 稳定协议字段的首次赋值。赋值会立即落成普通自有属性；捕获目标后所有探针
 * 都会恢复，所以不会进入后续 FeiLin 指纹采集。
 */
function isNonEmptyString(value) {
  return typeof value === "string" && value.length > 0;
}


/**
 * 按公开协议字段识别承载 Log1/Log2/FeiLin 状态的配置对象。
 *
 * owner 上的四项是 SDK 设备 RPC 的稳定合同；patch 则允许两条证据路径：
 * 初始化时的设备回调配置，或 Log1 解密后的 DeviceConfig。两条都只看语义字段，
 * 不看构造函数、闭包位置和任何混淆符号名。
 */
function isSdkRuntimeState(owner, patch) {
  if (
    owner === null
    || typeof owner !== "object"
    || patch === null
    || typeof patch !== "object"
    || Array.isArray(patch)
    || ![
      "ACCESS_SEC",
      "SESSION_ID_SALT",
      "APP_NAME",
      "APP_KEY",
    ].every((name) => isNonEmptyString(owner[name]))
  ) {
    return false;
  }

  const bootstrapFields = [
    "prefix",
    "region",
    "appName",
    "appKey",
    "endpoints",
    "deviceCallback",
  ];
  const bootstrapScore = bootstrapFields.reduce(
    (score, name) => score + Number(Object.hasOwn(patch, name)),
    0,
  );
  if (
    bootstrapScore >= 4
    && typeof patch.deviceCallback === "function"
  ) {
    return true;
  }

  const deviceConfig = patch.deviceConfig;
  return (
    deviceConfig !== null
    && typeof deviceConfig === "object"
    && !Array.isArray(deviceConfig)
    && [
      "key",
      "sessionId",
      "version",
      "pluginElements",
      "pluginResource",
      "globalVariable",
      "timestamp",
      "ip",
    ].every((name) => typeof deviceConfig[name] === "string")
  );
}


function installSdkStateCapture(context, onCapture) {
  // makeBrowserContext 会把全局 Object 映射到宿主实现；对象字面量仍继承 VM
  // realm 自己的 intrinsic prototype，必须从字面量反查，不能读 Object.prototype。
  const objectPrototype = vm.runInContext("Object.getPrototypeOf({})", context);
  const installed = [];
  const candidates = new Set();
  let active = true;

  const stop = () => {
    if (!active) {
      return;
    }
    active = false;
    candidates.clear();
    for (const { field, setter } of installed) {
      const descriptor = Object.getOwnPropertyDescriptor(
        objectPrototype,
        field,
      );
      if (descriptor?.set === setter) {
        delete objectPrototype[field];
      }
    }
  };

  const observe = (owner) => {
    if (!active) {
      return;
    }
    try {
      if (isSdkRuntimeState(owner, owner)) {
        stop();
        onCapture(owner);
      }
    } catch {}
  };

  const flush = () => {
    if (!active) {
      return;
    }
    for (const owner of candidates) {
      observe(owner);
      if (!active) {
        return;
      }
    }
  };

  for (const field of SDK_STATE_CAPTURE_FIELDS) {
    // 不覆盖 SDK 或宿主已经显式定义的 Object.prototype 合同。
    if (Object.hasOwn(objectPrototype, field)) {
      continue;
    }
    const setter = function captureSdkStateField(value) {
      candidates.add(this);
      Object.defineProperty(this, field, {
        configurable: true,
        enumerable: true,
        value,
        writable: true,
      });
      try {
        observe(this);
        // 某些版本把末尾字段预先放在自定义原型上，因此最后一次赋值不会命中
        // Object.prototype。等本轮同步 merge 结束后，再用完整 owner 复核一次。
        queueMicrotask(flush);
      } catch {}
    };
    Object.defineProperty(objectPrototype, field, {
      configurable: true,
      enumerable: false,
      get() {
        return undefined;
      },
      set: setter,
    });
    installed.push({ field, setter });
  }
  return Object.freeze({ flush, stop });
}


function publicConfigSummary(runtimeConfig) {
  const verifyType = isNonEmptyString(runtimeConfig.verifyType)
    ? runtimeConfig.verifyType
    : "2.0";
  const region = isNonEmptyString(runtimeConfig.region)
    ? runtimeConfig.region
    : "cn";
  const appName = isNonEmptyString(runtimeConfig.appName)
    ? runtimeConfig.appName
    : runtimeConfig.APP_NAME;
  const appKey = isNonEmptyString(runtimeConfig.appKey)
    ? runtimeConfig.appKey
    : runtimeConfig.APP_KEY;
  const dynamicJsPathTemplate = (
    typeof runtimeConfig.dynamicJsPath === "function"
      ? runtimeConfig.dynamicJsPath("__VERSION__")
      : null
  );
  return {
    appNameMap: { [verifyType]: appName },
    appKeyMap: { [verifyType]: { [region]: appKey } },
    apiVersion: runtimeConfig.API_VERSION,
    appVersion: runtimeConfig.APP_VERSION,
    platform: runtimeConfig.PLATFORM,
    defaultAppName: runtimeConfig.APP_NAME,
    defaultAppKey: runtimeConfig.APP_KEY,
    accessSec: runtimeConfig.ACCESS_SEC,
    wafEndpoints: runtimeConfig.WAF_ENDPOINTS,
    cnEndpoints: runtimeConfig.CN_ENDPOINTS,
    cdnServers: runtimeConfig.cdnServers,
    httpsScheme: runtimeConfig.https,
    dynamicJsPathTemplate,
  };
}


/**
 * probe-log1 通过六段稳定 schema 观察加密前的 Data，而不是调用某版 SDK 的
 * 局部 AES 函数。命中后立即恢复原生 join，避免观察逻辑进入后续 FeiLin 环境。
 */
function installLog1PlaintextCapture(context, onCapture) {
  Object.defineProperty(context, LOG1_PLAINTEXT_CAPTURE_HOOK, {
    configurable: true,
    enumerable: false,
    value: onCapture,
    writable: false,
  });
  vm.runInContext(
    [
      "(()=>{",
      "const nativeJoin=Array.prototype.join;",
      "Array.prototype.join=function(separator){",
      "const result=nativeJoin.call(this,separator);",
      "if(separator==='#'&&this.length===6&&",
      "this[4]==='CLOUD'&&this[5]===''&&",
      "this.slice(0,4).every((value)=>typeof value==='string')){",
      `globalThis.${LOG1_PLAINTEXT_CAPTURE_HOOK}(result);`,
      "Array.prototype.join=nativeJoin;",
      "}",
      "return result;",
      "};",
      "})();",
    ].join(""),
    context,
  );
}


function instrumentFeiLinProfile(source) {
  const entry = /function ([A-Za-z_$][\w$]*)\(t,r\)\{var n=Object\.entries\(t\),e=\{\},i=!0,a=!1,o=void 0;/.exec(
    source,
  );
  if (!entry) {
    return source;
  }
  const tailSource = source.slice(entry.index, entry.index + 1_500);
  const tail = /return ([A-Za-z_$][\w$]*)\(e\)\}function ([A-Za-z_$][\w$]*)\(/.exec(
    tailSource,
  );
  if (!tail) {
    return source;
  }
  const replacement = [
    `var p=${tail[1]}(e);`,
    "try{",
    "var d=function(v){return Object.keys(v).map(function(k){",
    "var x=v[k],y=typeof x;",
    "return{key:k,type:Array.isArray(x)?'array':",
    "null===x?'null':y,empty:null==x||''===x||",
    "(Array.isArray(x)&&0===x.length),length:",
    "'string'===y||Array.isArray(x)?x.length:null}",
    "})};",
    "(window.__ALI_FEILIN_RG_META__||",
    "(window.__ALI_FEILIN_RG_META__=[])).push({",
    "top:d(t),flat:d(p)",
    "})",
    "}catch(v){}",
    `return p}function ${tail[2]}(`,
  ].join("");
  const tailIndex = entry.index + tail.index;
  return (
    source.slice(0, tailIndex)
    + replacement
    + source.slice(tailIndex + tail[0].length)
  );
}


function feiLinProfileBootstrap() {
  return [
    "(()=>{",
    "const nativeStringify=JSON.stringify;",
    "window.__ALI_FEILIN_JSON_META__=[];",
    "JSON.stringify=function(value,...rest){",
    "try{",
    "if(value&&typeof value==='object'&&!Array.isArray(value)){",
    "const keys=Object.keys(value);",
    "if(keys.length>=90&&keys.length<=150&&",
    "window.__ALI_FEILIN_JSON_META__.length<12){",
    "window.__ALI_FEILIN_JSON_META__.push(keys.map((key)=>{",
    "const item=value[key],type=Array.isArray(item)?'array':",
    "item===null?'null':typeof item;",
    "return{key,type,empty:item==null||item===''||",
    "(Array.isArray(item)&&item.length===0),length:",
    "typeof item==='string'||Array.isArray(item)?item.length:null};",
    "}));",
    "}",
    "}",
    "}catch(error){}",
    "return nativeStringify.call(this,value,...rest);",
    "};",
    "})();",
  ].join("");
}


function makeStorage() {
  const values = new Map();
  return {
    get length() {
      return values.size;
    },
    clear() {
      values.clear();
    },
    getItem(key) {
      const normalized = String(key);
      return values.has(normalized) ? values.get(normalized) : null;
    },
    key(index) {
      return [...values.keys()][index] ?? null;
    },
    removeItem(key) {
      values.delete(String(key));
    },
    setItem(key, value) {
      values.set(String(key), String(value));
    },
  };
}


/**
 * 按本轮画像产出 DOM 元素工厂。
 *
 * 元素本身要携带三样与设备强相关的东西：视口尺寸（getBoundingClientRect）、
 * GPU 标识（WebGL）与 Canvas 读回值。它们必须来自同一台设备，所以统一由画像
 * 注入，而不是各自写死常量。
 */
function makeElementFactory(profile) {
  const viewportWidth = profile.screen.innerWidth;
  const viewportHeight = profile.screen.innerHeight;
  const gpu = profile.gpu;
  // Canvas 读回值在同一轮里必须稳定，因此在工厂层算一次并共享。
  const canvasDataUrl = (
    "data:image/png;base64,"
    + seededBase64(`${profile.canvasSeed}:canvas`, 3400 + (
      makeSeededStream(`${profile.canvasSeed}:len`)() % 420
    ))
  );
  const imageDataStream = makeSeededStream(`${profile.canvasSeed}:pixels`);
  const canvasPixels = Uint8ClampedArray.from(
    [0, 1, 2, 3],
    () => imageDataStream() % 256,
  );

  return function makeElement(tagName = "div") {
    const attributes = new Map();
    const children = [];
    const elementListeners = new Map();
    const styleTarget = {
      setProperty(name, value) {
        this[String(name)] = String(value);
      },
      getPropertyValue(name) {
        return this[String(name)] ?? "";
      },
      removeProperty(name) {
        const normalized = String(name);
        const previous = this[normalized] ?? "";
        delete this[normalized];
        return previous;
      },
    };
    const style = new Proxy(styleTarget, {
      get(target, property, receiver) {
        if (Reflect.has(target, property)) {
          return Reflect.get(target, property, receiver);
        }
        return typeof property === "string" ? "" : undefined;
      },
      set(target, property, value, receiver) {
        return Reflect.set(
          target,
          property,
          typeof property === "string" ? String(value) : value,
          receiver,
        );
      },
    });
    const normalizedTag = String(tagName).toUpperCase();
    const element = {
      nodeType: 1,
      tagName: normalizedTag,
      nodeName: normalizedTag,
      style,
      children,
      childNodes: children,
      className: "",
      id: "",
      innerHTML: "",
      textContent: "",
      parentNode: null,
      offsetLeft: 0,
      offsetTop: 0,
      clientLeft: 0,
      clientTop: 0,
      scrollLeft: 0,
      scrollTop: 0,
      get outerHTML() {
        if (normalizedTag === "HTML") {
          return "<html><head></head><body></body></html>";
        }
        const name = normalizedTag.toLowerCase();
        return `<${name}>${this.innerHTML}</${name}>`;
      },
      get firstChild() {
        return children[0] ?? null;
      },
      get lastChild() {
        return children.at(-1) ?? null;
      },
      get parentElement() {
        return this.parentNode?.nodeType === 1 ? this.parentNode : null;
      },
      appendChild(child) {
        if (child && typeof child === "object") {
          child.parentNode = this;
        }
        children.push(child);
        return child;
      },
      insertBefore(child, reference) {
        if (child && typeof child === "object") {
          child.parentNode = this;
        }
        const index = children.indexOf(reference);
        if (index < 0) {
          children.push(child);
        } else {
          children.splice(index, 0, child);
        }
        return child;
      },
      insertAdjacentHTML(position, html) {
        const normalizedPosition = String(position).toLowerCase();
        const markup = String(html);
        const match = markup.match(/<([a-z][\w-]*)\b[^>]*>/i);
        const inserted = makeElement(match?.[1] ?? "span");
        const id = markup.match(/\bid=["']([^"']+)["']/i)?.[1];
        const className = markup.match(/\bclass=["']([^"']*)["']/i)?.[1];
        if (id) inserted.id = id;
        if (className !== undefined) inserted.className = className;
        inserted.innerHTML = markup;
        inserted.textContent = markup.replace(/<[^>]*>/g, "");
        if (normalizedPosition === "beforebegin") {
          this.parentNode?.insertBefore(inserted, this);
        } else if (normalizedPosition === "afterbegin") {
          this.insertBefore(inserted, this.firstChild);
        } else if (normalizedPosition === "beforeend") {
          this.appendChild(inserted);
        } else if (normalizedPosition === "afterend") {
          const siblings = this.parentNode?.children ?? [];
          const index = siblings.indexOf(this);
          this.parentNode?.insertBefore(inserted, siblings[index + 1] ?? null);
        } else {
          throw new TypeError("insertAdjacentHTML position 无效");
        }
      },
      removeChild(child) {
        const index = children.indexOf(child);
        if (index >= 0) {
          children.splice(index, 1);
        }
        return child;
      },
      addEventListener(type, listener, listenerOptions) {
        const normalizedType = String(type ?? "");
        const valid = (
          typeof listener === "function"
          || typeof listener?.handleEvent === "function"
        );
        if (!normalizedType || !valid) return;
        if (!elementListeners.has(normalizedType)) {
          elementListeners.set(normalizedType, []);
        }
        const bucket = elementListeners.get(normalizedType);
        const capture = listenerOptions === true || Boolean(listenerOptions?.capture);
        if (bucket.some((entry) => entry.listener === listener && entry.capture === capture)) {
          return;
        }
        bucket.push({
          listener,
          capture,
          once: Boolean(listenerOptions?.once),
        });
      },
      removeEventListener(type, listener, listenerOptions) {
        const bucket = elementListeners.get(String(type));
        if (!bucket) return;
        const capture = listenerOptions === true || Boolean(listenerOptions?.capture);
        const index = bucket.findIndex((entry) => (
          entry.listener === listener && entry.capture === capture
        ));
        if (index >= 0) bucket.splice(index, 1);
      },
      dispatchEvent(event) {
        if (!event || typeof event !== "object" || !event.type) {
          throw new TypeError("dispatchEvent 需要带 type 的 event object");
        }
        let state = event[BRIDGE_EVENT_STATE];
        if (!state) {
          state = {
            defaultPrevented: Boolean(event.defaultPrevented),
            propagationStopped: false,
            immediateStopped: false,
          };
          Object.defineProperty(event, BRIDGE_EVENT_STATE, {
            configurable: true,
            value: state,
          });
          Object.defineProperties(event, {
            defaultPrevented: {
              configurable: true,
              get() { return state.defaultPrevented; },
            },
            preventDefault: {
              configurable: true,
              value() { state.defaultPrevented = true; },
            },
            stopPropagation: {
              configurable: true,
              value() { state.propagationStopped = true; },
            },
            stopImmediatePropagation: {
              configurable: true,
              value() {
                state.propagationStopped = true;
                state.immediateStopped = true;
              },
            },
          });
        }
        if (!("target" in event)) event.target = this;
        event.currentTarget = this;
        const propertyHandler = this[`on${String(event.type)}`];
        if (typeof propertyHandler === "function") {
          propertyHandler.call(this, event);
        }
        const bucket = elementListeners.get(String(event.type)) ?? [];
        for (const entry of [...bucket]) {
          if (state.immediateStopped) break;
          if (entry.once) {
            const index = bucket.indexOf(entry);
            if (index >= 0) bucket.splice(index, 1);
          }
          if (typeof entry.listener === "function") {
            entry.listener.call(this, event);
          } else {
            entry.listener.handleEvent.call(entry.listener, event);
          }
        }
        return !event.defaultPrevented;
      },
      setAttribute(name, value) {
        attributes.set(String(name), String(value));
        this[String(name)] = String(value);
      },
      getAttribute(name) {
        return attributes.get(String(name)) ?? null;
      },
      getBoundingClientRect() {
        return {
          x: 0,
          y: 0,
          top: 0,
          left: 0,
          right: viewportWidth,
          bottom: viewportHeight,
          width: viewportWidth,
          height: viewportHeight,
        };
      },
      querySelector() {
        return null;
      },
      querySelectorAll() {
        return [];
      },
      getContext(type) {
        if (normalizedTag !== "CANVAS") {
          return null;
        }
        if (String(type).includes("webgl")) {
          const extensions = [
            "ANGLE_instanced_arrays",
            "EXT_blend_minmax",
            "EXT_clip_control",
            "EXT_color_buffer_half_float",
            "EXT_depth_clamp",
            "EXT_disjoint_timer_query",
            "EXT_float_blend",
            "EXT_frag_depth",
            "EXT_polygon_offset_clamp",
            "EXT_shader_texture_lod",
            "EXT_texture_compression_bptc",
            "EXT_texture_compression_rgtc",
            "EXT_texture_filter_anisotropic",
            "EXT_texture_mirror_clamp_to_edge",
            "EXT_sRGB",
            "KHR_parallel_shader_compile",
            "OES_element_index_uint",
            "OES_fbo_render_mipmap",
            "OES_standard_derivatives",
            "OES_texture_float",
            "OES_texture_float_linear",
            "OES_texture_half_float",
            "OES_texture_half_float_linear",
            "OES_vertex_array_object",
            "WEBGL_blend_func_extended",
            "WEBGL_color_buffer_float",
            "WEBGL_compressed_texture_astc",
            "WEBGL_compressed_texture_etc",
            "WEBGL_compressed_texture_etc1",
            "WEBGL_compressed_texture_pvrtc",
            "WEBGL_compressed_texture_s3tc",
            "WEBGL_compressed_texture_s3tc_srgb",
            "WEBGL_debug_renderer_info",
            "WEBGL_debug_shaders",
            "WEBGL_depth_texture",
            "WEBGL_draw_buffers",
            "WEBGL_lose_context",
            "WEBGL_multi_draw",
            "WEBGL_polygon_mode",
          ];
          const webglParameters = new Map([
            [7936, "WebKit"],
            [7937, "WebKit WebGL"],
            [7938, "WebGL 1.0 (OpenGL ES 2.0 Chromium)"],
            [3379, gpu.maxTextureSize],
            [35724, "WebGL GLSL ES 1.0 (OpenGL ES GLSL ES 1.0 Chromium)"],
            [37445, gpu.unmaskedVendor],
            [37446, gpu.unmaskedRenderer],
          ]);
          const webglContext = {
            VENDOR: 7936,
            RENDERER: 7937,
            VERSION: 7938,
            MAX_TEXTURE_SIZE: 3379,
            SHADING_LANGUAGE_VERSION: 35724,
            getExtension(name) {
              if (name === "WEBGL_debug_renderer_info") {
                return {
                  UNMASKED_VENDOR_WEBGL: 37445,
                  UNMASKED_RENDERER_WEBGL: 37446,
                };
              }
              return extensions.includes(String(name)) ? {} : null;
            },
            getSupportedExtensions() {
              return [...extensions];
            },
            getParameter(name) {
              return webglParameters.get(name) ?? 0;
            },
            getContextAttributes() {
              return {
                alpha: true,
                antialias: true,
                depth: true,
                failIfMajorPerformanceCaveat: false,
                powerPreference: "default",
                premultipliedAlpha: true,
                preserveDrawingBuffer: false,
                stencil: false,
              };
            },
            getShaderPrecisionFormat() {
              return { rangeMin: 127, rangeMax: 127, precision: 23 };
            },
          };
          return new Proxy(webglContext, {
            get(target, property, receiver) {
              if (Reflect.has(target, property)) {
                return Reflect.get(target, property, receiver);
              }
              if (typeof property === "string") {
                const noop = () => null;
                Reflect.set(target, property, noop);
                return noop;
              }
              return undefined;
            },
          });
        }
        // FeiLin 只需要可调用、确定性的 Canvas 2D 表面来采集环境特征。
        const canvasContext = {
          canvas: this,
          fillStyle: "#000000",
          strokeStyle: "#000000",
          font: "10px sans-serif",
          textBaseline: "alphabetic",
          beginPath() {},
          closePath() {},
          moveTo() {},
          lineTo() {},
          bezierCurveTo() {},
          quadraticCurveTo() {},
          arc() {},
          arcTo() {},
          ellipse() {},
          rect() {},
          fill() {},
          stroke() {},
          clip() {},
          save() {},
          restore() {},
          translate() {},
          rotate() {},
          scale() {},
          transform() {},
          setTransform() {},
          resetTransform() {},
          fillRect() {},
          strokeRect() {},
          clearRect() {},
          fillText() {},
          strokeText() {},
          drawImage() {},
          putImageData() {},
          setLineDash() {},
          getLineDash() {
            return [];
          },
          createLinearGradient() {
            return { addColorStop() {} };
          },
          createRadialGradient() {
            return { addColorStop() {} };
          },
          createPattern() {
            return null;
          },
          isPointInPath() {
            return false;
          },
          isPointInStroke() {
            return false;
          },
          measureText(text) {
            // 字体度量是独立的指纹面：同一字符串在不同机器上的宽度并不相同。
            return { width: String(text).length * profile.textMetricScale };
          },
          getImageData() {
            return { data: canvasPixels };
          },
        };
        // FeiLin 的混淆控制流会按运行时字符串访问 Canvas 方法。真实 Canvas 2D
        // 对象的方法面很大；未知方法按无副作用空函数补齐，避免把环境缺口误当算法失败。
        return new Proxy(canvasContext, {
          get(target, property, receiver) {
            if (Reflect.has(target, property)) {
              return Reflect.get(target, property, receiver);
            }
            if (typeof property === "string") {
              const noop = () => {};
              Reflect.set(target, property, noop);
              return noop;
            }
            return undefined;
          },
        });
      },
      toDataURL() {
        // 指纹画布的长度与内容都由本轮画像的种子决定：同一轮内稳定，跨轮不同。
        return normalizedTag === "CANVAS" ? canvasDataUrl : "";
      },
      offsetWidth: 100,
      offsetHeight: 20,
      clientWidth: 100,
      clientHeight: 20,
    };
    element.classList = {
      add(...names) {
        const values = new Set(String(element.className).split(/\s+/).filter(Boolean));
        names.forEach((name) => values.add(String(name)));
        element.className = [...values].join(" ");
      },
      remove(...names) {
        const removed = new Set(names.map(String));
        element.className = String(element.className)
          .split(/\s+/)
          .filter((name) => name && !removed.has(name))
          .join(" ");
      },
      contains(name) {
        return String(element.className).split(/\s+/).includes(String(name));
      },
      toggle(name, force) {
        const normalized = String(name);
        const present = this.contains(normalized);
        const shouldAdd = force === undefined ? !present : Boolean(force);
        if (shouldAdd) this.add(normalized);
        else this.remove(normalized);
        return shouldAdd;
      },
    };
    return element;
  };
}


function makeBrowserContext(options, onRequest) {
  // 本轮唯一的设备事实来源：DOM、navigator、screen、WebGL、Canvas 与外发请求头
  // 全部由它派生，任何一处另起炉灶都会让服务端看到两台设备。
  const deviceProfile = options.deviceProfile;
  const screenProfile = deviceProfile.screen;
  const makeElement = makeElementFactory(deviceProfile);
  const head = makeElement("head");
  const body = makeElement("body");
  const documentElement = makeElement("html");
  const eventRegistrations = [];
  const eventListeners = new Map();
  let contextReference = null;
  let timerSequence = 1;
  const timeoutHandles = new Map();
  const intervalHandles = new Map();
  documentElement.clientWidth = screenProfile.innerWidth;
  documentElement.clientHeight = screenProfile.innerHeight;

  function listenerBucket(target, type) {
    if (!eventListeners.has(target)) {
      eventListeners.set(target, new Map());
    }
    const byType = eventListeners.get(target);
    if (!byType.has(type)) {
      byType.set(type, []);
    }
    return byType.get(type);
  }

  function recordEventRegistration(target) {
    return function addEventListener(type, listener, listenerOptions) {
      try {
        const normalizedType = typeof type === "string" ? type : "";
        const capture = (
          listenerOptions === true
          || Boolean(listenerOptions && listenerOptions.capture)
        );
        const once = Boolean(listenerOptions && listenerOptions.once);
        const isListener = (
          typeof listener === "function"
          || (
            listener !== null
            && typeof listener === "object"
            && typeof listener.handleEvent === "function"
          )
        );
        if (!normalizedType || !isListener) return;
        const callbacks = listenerBucket(target, normalizedType);
        if (callbacks.some((entry) => (
          entry.listener === listener && entry.capture === capture
        ))) return;
        eventRegistrations.push({
          target,
          type: normalizedType,
          listenerType: typeof listener,
          capture,
        });
        callbacks.push({ listener, capture, once });
      } catch {}
    };
  }

  function removeEventRegistration(target) {
    return function removeEventListener(type, listener, listenerOptions) {
      const byType = eventListeners.get(target);
      const callbacks = byType?.get(String(type));
      if (!callbacks) return;
      const capture = (
        listenerOptions === true
        || Boolean(listenerOptions && listenerOptions.capture)
      );
      const index = callbacks.findIndex((entry) => (
        entry.listener === listener && entry.capture === capture
      ));
      if (index >= 0) callbacks.splice(index, 1);
    };
  }

  function eventState(event) {
    let state = event[BRIDGE_EVENT_STATE];
    if (state) return state;
    state = {
      defaultPrevented: Boolean(event.defaultPrevented),
      propagationStopped: false,
      immediateStopped: false,
    };
    Object.defineProperty(event, BRIDGE_EVENT_STATE, {
      configurable: true,
      value: state,
    });
    Object.defineProperties(event, {
      defaultPrevented: {
        configurable: true,
        get() { return state.defaultPrevented; },
      },
      preventDefault: {
        configurable: true,
        value() { state.defaultPrevented = true; },
      },
      stopPropagation: {
        configurable: true,
        value() { state.propagationStopped = true; },
      },
      stopImmediatePropagation: {
        configurable: true,
        value() {
          state.propagationStopped = true;
          state.immediateStopped = true;
        },
      },
    });
    return state;
  }

  function dispatchRegisteredEvent(target, targetObject, event) {
    if (!event || typeof event !== "object" || !event.type) {
      throw new TypeError("dispatchEvent 需要带 type 的 event object");
    }
    const state = eventState(event);
    if (!("target" in event)) event.target = targetObject;
    event.currentTarget = targetObject;
    const propertyHandler = targetObject[`on${String(event.type)}`];
    if (typeof propertyHandler === "function") {
      propertyHandler.call(targetObject, event);
    }
    const callbacks = (
      eventListeners.get(target)?.get(String(event.type)) ?? []
    );
    for (const entry of [...callbacks]) {
      if (state.immediateStopped) break;
      if (entry.once) {
        const index = callbacks.indexOf(entry);
        if (index >= 0) callbacks.splice(index, 1);
      }
      if (typeof entry.listener === "function") {
        entry.listener.call(targetObject, event);
      } else {
        entry.listener.handleEvent.call(entry.listener, event);
      }
    }
    return !state.defaultPrevented;
  }
  head.addEventListener = recordEventRegistration("head");
  head.removeEventListener = removeEventRegistration("head");
  head.dispatchEvent = (event) => dispatchRegisteredEvent("head", head, event);
  body.addEventListener = recordEventRegistration("body");
  body.removeEventListener = removeEventRegistration("body");
  body.dispatchEvent = (event) => dispatchRegisteredEvent("body", body, event);
  documentElement.addEventListener = recordEventRegistration("html");
  documentElement.removeEventListener = removeEventRegistration("html");
  documentElement.dispatchEvent = (event) => (
    dispatchRegisteredEvent("html", documentElement, event)
  );

  function browserSetTimeout(callback, delay = 0, ...args) {
    const id = timerSequence++;
    const handle = setTimeout(() => {
      timeoutHandles.delete(id);
      callback(...args);
    }, Number(delay) || 0);
    timeoutHandles.set(id, handle);
    return id;
  }

  function browserClearTimeout(id) {
    const handle = timeoutHandles.get(Number(id));
    if (handle !== undefined) {
      clearTimeout(handle);
      timeoutHandles.delete(Number(id));
    }
  }

  function browserSetInterval(callback, delay = 0, ...args) {
    const id = timerSequence++;
    const handle = setInterval(
      () => callback(...args),
      Number(delay) || 0,
    );
    intervalHandles.set(id, handle);
    return id;
  }

  function browserClearInterval(id) {
    const handle = intervalHandles.get(Number(id));
    if (handle !== undefined) {
      clearInterval(handle);
      intervalHandles.delete(Number(id));
    }
  }

  function assertAllowedNetworkUrl(value) {
    const parsed = new URL(String(value));
    const hostAllowed =
      parsed.hostname === "g.alicdn.com" ||
      parsed.hostname === "x.alicdn.com" ||
      parsed.hostname.endsWith(".aliyuncs.com");
    if (
      parsed.protocol !== "https:"
      || parsed.port !== ""
      || parsed.username !== ""
      || parsed.password !== ""
      || !hostAllowed
    ) {
      throw new Error(`Node 设备桥拒绝访问非白名单 URL：${parsed.href}`);
    }
    return parsed.href;
  }

  async function fetchAllowedNetworkUrl(value, init) {
    let currentUrl = assertAllowedNetworkUrl(value);
    let method = String(init.method ?? "GET").toUpperCase();
    let body = init.body;
    for (let redirectCount = 0; ; redirectCount += 1) {
      const response = await fetch(currentUrl, {
        ...init,
        method,
        body,
        redirect: "manual",
      });
      if (![301, 302, 303, 307, 308].includes(response.status)) {
        return response;
      }
      if (redirectCount >= 3) {
        await response.body?.cancel();
        throw new Error("Node 设备桥网络重定向超过上限");
      }
      const location = response.headers.get("location");
      if (!location) {
        await response.body?.cancel();
        throw new Error("Node 设备桥网络重定向缺少 Location");
      }
      const nextUrl = new URL(location, currentUrl).href;
      currentUrl = assertAllowedNetworkUrl(nextUrl);
      if (
        ((response.status === 301 || response.status === 302) && method === "POST")
        || (response.status === 303 && method !== "GET" && method !== "HEAD")
      ) {
        method = "GET";
        body = undefined;
      }
      await response.body?.cancel();
    }
  }

  const appendToHead = head.appendChild.bind(head);
  head.appendChild = function appendChild(child) {
    const appended = appendToHead(child);
    if (
      options.networkEnabled &&
      child?.tagName === "SCRIPT" &&
      child.src
    ) {
      const sourceUrl = assertAllowedNetworkUrl(child.src);
      Promise.resolve()
        .then(async () => {
          const response = await fetchAllowedNetworkUrl(sourceUrl, {
            method: "GET",
            headers: browserRequestHeaders(
              deviceProfile,
              {},
              {
                destination: "script",
                includeOrigin: false,
                mode: "no-cors",
              },
            ),
          });
          if (!response.ok) {
            throw new Error(
              `FeiLin 脚本下载失败：HTTP ${response.status}`,
            );
          }
          const scriptBytes = Buffer.from(await response.arrayBuffer());
          if (scriptBytes.length > DYNAMIC_ASSET_MAX_BYTES) {
            throw new Error("动态脚本超过大小上限");
          }
          let script = scriptBytes.toString("utf8");
          if (options.mode === "profile-token") {
            const instrumented = instrumentFeiLinProfile(script);
            contextReference.__ALI_FEILIN_SCRIPT_META__ = {
              sourceUrl,
              sourceLength: script.length,
              instrumented: instrumented !== script,
            };
            script = feiLinProfileBootstrap() + instrumented;
          }
          document.currentScript = child;
          vm.runInContext(script, contextReference, {
            filename: sourceUrl,
            timeout: options.timeoutMs,
          });
          document.currentScript = null;
          child.readyState = "complete";
          child.onreadystatechange?.();
          child.onload?.();
        })
        .catch((error) => {
          document.currentScript = null;
          child.onerror?.(error);
        });
    }
    if (
      options.networkEnabled
      && child?.tagName === "LINK"
      && child.href
    ) {
      const styleUrl = assertAllowedNetworkUrl(child.href);
      Promise.resolve()
        .then(async () => {
          const response = await fetchAllowedNetworkUrl(styleUrl, {
            method: "GET",
            headers: browserRequestHeaders(
              deviceProfile,
              {},
              {
                destination: "style",
                includeOrigin: false,
                mode: "no-cors",
              },
            ),
          });
          if (!response.ok) {
            throw new Error(
              `验证码样式下载失败：HTTP ${response.status}`,
            );
          }
          const styleBytes = await response.arrayBuffer();
          if (styleBytes.byteLength > DYNAMIC_ASSET_MAX_BYTES) {
            throw new Error("验证码样式超过大小上限");
          }
          child.readyState = "complete";
          child.onreadystatechange?.();
          child.onload?.();
        })
        .catch((error) => child.onerror?.(error));
    }
    return appended;
  };

  const document = {
    nodeType: 9,
    readyState: "complete",
    visibilityState: "visible",
    hidden: false,
    cookie: "",
    charset: "UTF-8",
    characterSet: "UTF-8",
    compatMode: "CSS1Compat",
    domain: "localhost",
    referrer: "",
    URL: "http://localhost:38185/package_product/pages/PaySubmit/index",
    head,
    body,
    documentElement,
    currentScript: null,
    createElement(tagName) {
      const element = makeElement(tagName);
      const prototype = contextReference?.HTMLElement?.prototype;
      if (prototype) {
        Object.setPrototypeOf(element, prototype);
      }
      if (String(tagName).toLowerCase() === "iframe") {
        // about:blank iframe 在浏览器中拥有独立的 Window、Document 与内建对象
        // realm；FeiLin 会显式检查这一边界，不能把主 context 原样回填。
        const frame = makeIframeRealm(element);
        element.contentWindow = frame.contentWindow;
        element.contentDocument = frame.contentDocument;
        element.srcdoc = "";
      }
      return element;
    },
    createTextNode(text) {
      return { nodeType: 3, textContent: String(text) };
    },
    createComment(text) {
      return { nodeType: 8, textContent: String(text) };
    },
    createDocumentFragment() {
      return makeElement("fragment");
    },
    createRange() {
      return {
        collapsed: true,
        commonAncestorContainer: documentElement,
        setStart() {},
        setEnd() {},
        selectNode() {},
        selectNodeContents() {},
        collapse() {},
        cloneRange() {
          return this;
        },
        getBoundingClientRect() {
          return {
            x: 0,
            y: 0,
            top: 0,
            left: 0,
            right: 0,
            bottom: 0,
            width: 0,
            height: 0,
          };
        },
        getClientRects() {
          return [];
        },
      };
    },
    createEvent(type) {
      return {
        type,
        initEvent(name, bubbles, cancelable) {
          this.type = name;
          this.bubbles = bubbles;
          this.cancelable = cancelable;
        },
        initCustomEvent(name, bubbles, cancelable, detail) {
          this.type = name;
          this.bubbles = bubbles;
          this.cancelable = cancelable;
          this.detail = detail;
        },
        initMouseEvent(
          name,
          bubbles,
          cancelable,
          view,
          detail,
          screenX,
          screenY,
          clientX,
          clientY,
        ) {
          Object.assign(this, {
            type: name,
            bubbles,
            cancelable,
            view,
            detail,
            screenX,
            screenY,
            clientX,
            clientY,
          });
        },
      };
    },
    getElementById() {
      // 返回 null 可确保 SDK 选择 V3 页面实际使用的 verifyType=2.0。
      return null;
    },
    getElementsByTagName(name) {
      const normalized = String(name).toLowerCase();
      if (normalized === "head") {
        return [head];
      }
      if (normalized === "body") {
        return [body];
      }
      if (normalized === "html") {
        return [documentElement];
      }
      return [];
    },
    querySelector(selector) {
      const normalized = String(selector).toLowerCase();
      if (normalized === "head") {
        return head;
      }
      if (normalized === "body") {
        return body;
      }
      if (normalized === "html") {
        return documentElement;
      }
      return null;
    },
    querySelectorAll() {
      return [];
    },
    addEventListener: recordEventRegistration("document"),
    removeEventListener: removeEventRegistration("document"),
    dispatchEvent(event) {
      return dispatchRegisteredEvent("document", document, event);
    },
  };

  class BridgeXMLHttpRequest {
    constructor() {
      this.headers = {};
      this.method = "";
      this.url = "";
      this.async = true;
      this.status = 0;
      this.response = "";
      this.responseText = "";
      this.responseType = "text";
      this.timeout = 0;
      this.withCredentials = false;
      this.readyState = 0;
      this._responseHeaders = new Map();
      this._aborted = false;
    }

    open(method, url, async = true) {
      this.method = String(method);
      this.url = String(url);
      this.async = Boolean(async);
      this.readyState = 1;
    }

    setRequestHeader(name, value) {
      this.headers[String(name)] = String(value);
    }

    getResponseHeader(name) {
      return this._responseHeaders.get(String(name).toLowerCase()) ?? null;
    }

    send(body = null) {
      const effectiveHeaders = browserRequestHeaders(
        deviceProfile,
        this.headers,
      );
      const request = {
        method: this.method,
        url: this.url,
        async: this.async,
        headers: this.headers,
        effectiveHeaderNames: Object.keys(effectiveHeaders)
          .map((name) => name.toLowerCase())
          .sort(),
        body: body === null ? null : String(body),
      };
      onRequest(request);
      if (!options.networkEnabled) {
        return;
      }

      const applyResponse = (status, responseText, responseHeaders = []) => {
        if (this._aborted) return;
        this.status = status;
        this.responseText = responseText;
        this.response = this.responseType === "json"
          ? JSON.parse(responseText)
          : responseText;
        this._responseHeaders = new Map(
          responseHeaders.map(([name, value]) => [
            String(name).toLowerCase(),
            String(value),
          ]),
        );
        this.readyState = 4;
        this.onreadystatechange?.();
        this.onload?.();
      };

      const form = parseForm(request.body);
      const sdkInit = contextReference?.__ALI_SDK_INIT__;
      if (form.Action === "InitCaptchaV3" && sdkInit) {
        Promise.resolve()
          .then(() => {
            if (
              sdkInit.consumed
              || form.SceneId !== sdkInit.sceneId
              || typeof form.DeviceToken !== "string"
              || form.DeviceToken.length < 1
              || form.DeviceToken.length > 32 * 1024
            ) {
              throw new Error("SDK Init 会话不一致");
            }
            sdkInit.consumed = true;
            sdkInit.observedDeviceToken = form.DeviceToken;
            sdkInit.phase = "init_response";
            applyResponse(200, JSON.stringify({
              CertifyId: sdkInit.certifyId,
              Message: "success",
              RequestId: "synthetic-sdk-init",
              Code: "Success",
              LimitFlow: false,
              Success: true,
              ...(sdkInit.securityToken
                ? { SecurityToken: sdkInit.securityToken }
                : {}),
              StaticPath: sdkInit.staticPath,
              CaptchaType: sdkInit.captchaType,
            }), [["content-type", "application/json"]]);
          })
          .catch(() => {
            sdkInit.phase = "init_rejected";
            this.onerror?.(new Error("SDK Init 会话无效"));
          });
        return;
      }

      if (form.Action === "VerifyCaptchaV3" && sdkInit) {
        if (
          sdkInit.verifyRequested
          || !sdkInit.consumed
          || form.SceneId !== sdkInit.sceneId
          || form.CertifyId !== sdkInit.certifyId
        ) {
          sdkInit.phase = "verify_rejected";
          Promise.resolve().then(() => {
            this.onerror?.(new Error("SDK Verify 请求无效"));
          });
          return;
        }
        // Verify 一旦准备发送即永久消耗本轮机会；网络结果未知也不补发。
        sdkInit.verifyRequested = true;
        sdkInit.phase = "verify_request";
      }

      const requestUrl = assertAllowedNetworkUrl(this.url);
      const timeoutController = new AbortController();
      const timeoutId = this.timeout > 0
        ? setTimeout(() => timeoutController.abort(), this.timeout)
        : null;
      Promise.resolve()
        .then(async () => {
          const response = await fetchAllowedNetworkUrl(requestUrl, {
            method: this.method,
            headers: effectiveHeaders,
            body: request.body,
            signal: timeoutController.signal,
          });
          const responseText = await response.text();
          if (timeoutId !== null) {
            clearTimeout(timeoutId);
          }
          if (this._aborted) {
            return;
          }
          if (form.Action === "VerifyCaptchaV3" && sdkInit) {
            sdkInit.verifyResult = parseTracelessVerifyResponse(
              responseText,
              sdkInit.certifyId,
            );
            sdkInit.phase = "verify_response";
          }
          applyResponse(
            response.status,
            responseText,
            [...response.headers.entries()],
          );
        })
        .catch((error) => {
          if (timeoutId !== null) {
            clearTimeout(timeoutId);
          }
          if (this._aborted) {
            return;
          }
          if (error?.name === "AbortError") {
            this.ontimeout?.();
          } else {
            this.onerror?.(error);
          }
        });
    }

    abort() {
      this._aborted = true;
    }
  }

  class BasicEvent {
    constructor(type, init = {}) {
      this.type = String(type);
      Object.assign(this, init);
    }
  }

  class BasicCustomEvent extends BasicEvent {
    constructor(type, init = {}) {
      super(type, init);
      this.detail = init.detail;
    }
  }

  class BasicWorker {
    constructor() {
      this.onmessage = null;
      this.onerror = null;
    }

    postMessage() {}

    addEventListener() {}

    removeEventListener() {}

    terminate() {}
  }

  const location = {
    protocol: "http:",
    host: "localhost:38185",
    hostname: "localhost",
    port: "38185",
    origin: TARGET_ORIGIN,
    pathname: "/package_product/pages/PaySubmit/index",
    search: "",
    hash: "",
    href: `${TARGET_ORIGIN}/package_product/pages/PaySubmit/index`,
    toString() {
      return this.href;
    },
  };
  function makeNavigator() {
    const makeMimeType = (type) => ({
      type,
      suffixes: "pdf",
      description: "Portable Document Format",
    });
    // 移动版 Chrome 没有内置 PDF 插件，navigator.plugins 与 mimeTypes 恒为空；
    // 桌面版则固定是这五个。跟着家族走，不要两边都给。
    const mimeTypes = deviceProfile.pdfViewer
      ? [makeMimeType("application/pdf"), makeMimeType("text/pdf")]
      : [];
    const plugins = (
      deviceProfile.pdfViewer
        ? [
          "PDF Viewer",
          "Chrome PDF Viewer",
          "Chromium PDF Viewer",
          "Microsoft Edge PDF Viewer",
          "WebKit built-in PDF",
        ]
        : []
    ).map((name) => Object.assign(
      [
        makeMimeType("application/pdf"),
        makeMimeType("text/pdf"),
      ],
      {
        name,
        filename: "internal-pdf-viewer",
        description: "Portable Document Format",
        item(index) {
          return this[index] ?? null;
        },
        namedItem(type) {
          return this.find((item) => item?.type === type) ?? null;
        },
      },
    ));
    const brands = Object.freeze(
      deviceProfile.brands.map((entry) => Object.freeze({ ...entry })),
    );
    const userAgentData = {
      async getHighEntropyValues(hints) {
        const values = {
          architecture: deviceProfile.uaArchitecture,
          bitness: deviceProfile.uaBitness,
          model: deviceProfile.uaModel,
          platformVersion: deviceProfile.uaPlatformVersion,
          uaFullVersion: deviceProfile.uaFullVersion,
          fullVersionList: deviceProfile.fullVersionList.map(
            (entry) => ({ ...entry }),
          ),
          wow64: false,
        };
        return Object.fromEntries(
          [...hints].map((hint) => [hint, values[hint]]),
        );
      },
    };
    Object.defineProperties(userAgentData, {
      brands: {
        value: brands,
        enumerable: true,
      },
      mobile: {
        value: deviceProfile.mobile,
        enumerable: true,
      },
      platform: {
        value: deviceProfile.uaPlatform,
        enumerable: true,
      },
    });
    Object.freeze(userAgentData);

    const navigator = {
      language: deviceProfile.language,
      languages: Object.freeze([...deviceProfile.languages]),
      hardwareConcurrency: deviceProfile.hardwareConcurrency,
      // Chrome 会把 deviceMemory 量化后再暴露，上限就是 8——报更大的值是穿帮。
      deviceMemory: deviceProfile.deviceMemory,
      maxTouchPoints: deviceProfile.maxTouchPoints,
      cookieEnabled: true,
      webdriver: false,
      onLine: true,
      vendor: deviceProfile.vendor,
      product: "Gecko",
      productSub: "20030107",
      appName: "Netscape",
      appCodeName: "Mozilla",
      doNotTrack: null,
      vendorSub: "",
      pdfViewerEnabled: deviceProfile.pdfViewer,
      plugins: Object.assign(plugins, {
        item(index) {
          return this[index] ?? null;
        },
        namedItem(name) {
          return this.find((item) => item?.name === name) ?? null;
        },
        refresh() {},
      }),
      mimeTypes: Object.assign(mimeTypes, {
        item(index) {
          return this[index] ?? null;
        },
        namedItem(type) {
          return this.find((item) => item?.type === type) ?? null;
        },
      }),
      connection: {
        effectiveType: "4g",
        downlink: 10,
        rtt: 50,
        saveData: false,
      },
      permissions: {
        async query() {
          return { state: "prompt", onchange: null };
        },
      },
      mediaDevices: {
        async enumerateDevices() {
          return [];
        },
      },
    };
    Object.defineProperties(navigator, {
      userAgent: {
        value: deviceProfile.userAgent,
        enumerable: true,
      },
      platform: {
        value: deviceProfile.platform,
        enumerable: true,
      },
      appVersion: {
        value: deviceProfile.appVersion,
        enumerable: true,
      },
      userAgentData: {
        value: userAgentData,
        enumerable: true,
      },
    });
    return navigator;
  }
  const navigator = makeNavigator();
  const screen = {
    width: screenProfile.width,
    height: screenProfile.height,
    availWidth: screenProfile.availWidth,
    availHeight: screenProfile.availHeight,
    availLeft: screenProfile.availLeft,
    availTop: screenProfile.availTop,
    colorDepth: screenProfile.colorDepth,
    pixelDepth: screenProfile.colorDepth,
    isExtended: false,
    orientation: {
      type: screenProfile.orientationType,
      angle: screenProfile.orientationAngle,
    },
  };

  function makeIframeRealm(frameElement) {
    if (contextReference === null) {
      throw new Error("主 VM 尚未建立，无法创建 iframe realm");
    }
    const childHead = makeElement("head");
    const childBody = makeElement("body");
    const childDocumentElement = makeElement("html");
    childDocumentElement.clientWidth = documentElement.clientWidth;
    childDocumentElement.clientHeight = documentElement.clientHeight;
    const childLocation = {
      protocol: "about:",
      host: "",
      hostname: "",
      port: "",
      origin: location.origin,
      pathname: "blank",
      search: "",
      hash: "",
      href: "about:blank",
      toString() {
        return this.href;
      },
    };
    const childDocument = {
      nodeType: 9,
      readyState: "complete",
      visibilityState: "visible",
      hidden: false,
      cookie: "",
      charset: "UTF-8",
      characterSet: "UTF-8",
      compatMode: "CSS1Compat",
      domain: document.domain,
      referrer: location.href,
      URL: "about:blank",
      head: childHead,
      body: childBody,
      documentElement: childDocumentElement,
      currentScript: null,
      defaultView: null,
      createElement(tagName) {
        return makeElement(tagName);
      },
      createTextNode(text) {
        return { nodeType: 3, textContent: String(text) };
      },
      createComment(text) {
        return { nodeType: 8, textContent: String(text) };
      },
      createDocumentFragment() {
        return makeElement("fragment");
      },
      getElementById() {
        return null;
      },
      getElementsByTagName(name) {
        const normalized = String(name).toLowerCase();
        if (normalized === "head") return [childHead];
        if (normalized === "body") return [childBody];
        if (normalized === "html") return [childDocumentElement];
        return [];
      },
      querySelector() {
        return null;
      },
      querySelectorAll() {
        return [];
      },
      addEventListener() {},
      removeEventListener() {},
    };
    const childNavigator = makeNavigator();
    const childScreen = {
      ...screen,
      orientation: { ...screen.orientation },
    };
    const childSandbox = {
      console,
      Buffer,
      URL,
      URLSearchParams,
      TextEncoder,
      TextDecoder,
      performance: globalThis.performance,
      Intl,
      crypto: webcrypto,
      document: childDocument,
      location: childLocation,
      navigator: childNavigator,
      screen: childScreen,
      localStorage: makeStorage(),
      sessionStorage: makeStorage(),
      setTimeout: browserSetTimeout,
      clearTimeout: browserClearTimeout,
      setInterval: browserSetInterval,
      clearInterval: browserClearInterval,
      queueMicrotask,
      Event: BasicEvent,
      CustomEvent: BasicCustomEvent,
      matchMedia(query) {
        return {
          media: String(query),
          matches: false,
          onchange: null,
          addListener() {},
          removeListener() {},
          addEventListener() {},
          removeEventListener() {},
          dispatchEvent() {
            return true;
          },
        };
      },
      getComputedStyle() {
        return {
          getPropertyValue() {
            return "";
          },
        };
      },
      innerWidth: screenProfile.innerWidth,
      innerHeight: screenProfile.innerHeight,
      outerWidth: screenProfile.outerWidth,
      outerHeight: screenProfile.outerHeight,
      devicePixelRatio: screenProfile.devicePixelRatio,
    };
    const childContext = vm.createContext(childSandbox);
    childContext.window = childContext;
    childContext.self = childContext;
    childContext.globalThis = childContext;
    childContext.parent = contextReference;
    childContext.top = contextReference;
    childContext.frameElement = frameElement;
    childContext.addEventListener = () => {};
    childContext.removeEventListener = () => {};
    childContext.dispatchEvent = () => true;
    vm.runInContext(
      [
        "this.Object=Object",
        "this.Function=Function",
        "this.Array=Array",
        "this.Promise=Promise",
        "this.Map=Map",
        "this.Set=Set",
        "this.WeakMap=WeakMap",
        "this.WeakSet=WeakSet",
        "this.Symbol=Symbol",
        "this.Math=Math",
        "this.Date=Date",
        "this.JSON=JSON",
        "this.RegExp=RegExp",
        "this.Error=Error",
        "this.TypeError=TypeError",
        "this.Window=function Window(){}",
        "this.Document=function Document(){}",
        "this.HTMLDocument=function HTMLDocument(){}",
        "this.Navigator=function Navigator(){}",
        "this.Screen=function Screen(){}",
        "this.Element=function Element(){}",
        "this.HTMLElement=function HTMLElement(){}",
      ].join(";"),
      childContext,
      { timeout: 1_000 },
    );
    childDocument.defaultView = childContext;
    Object.setPrototypeOf(
      childDocument,
      childContext.HTMLDocument.prototype,
    );
    Object.setPrototypeOf(
      childNavigator,
      childContext.Navigator.prototype,
    );
    Object.setPrototypeOf(
      childScreen,
      childContext.Screen.prototype,
    );
    for (const element of [
      childHead,
      childBody,
      childDocumentElement,
    ]) {
      Object.setPrototypeOf(element, childContext.HTMLElement.prototype);
    }
    return {
      contentWindow: childContext,
      contentDocument: childDocument,
    };
  }

  const context = {
    console,
    Buffer,
    URL,
    URLSearchParams,
    TextEncoder,
    TextDecoder,
    ArrayBuffer,
    Uint8Array,
    Uint8ClampedArray,
    Int8Array,
    Uint16Array,
    Int16Array,
    Uint32Array,
    Int32Array,
    Float32Array,
    Float64Array,
    DataView,
    Promise,
    Map,
    Set,
    WeakMap,
    WeakSet,
    Symbol,
    Math,
    Date,
    JSON,
    RegExp,
    Error,
    TypeError,
    Object,
    Function,
    parseInt,
    parseFloat,
    isNaN,
    encodeURIComponent,
    decodeURIComponent,
    encodeURI,
    decodeURI,
    setTimeout: browserSetTimeout,
    clearTimeout: browserClearTimeout,
    setInterval: browserSetInterval,
    clearInterval: browserClearInterval,
    queueMicrotask,
    performance: globalThis.performance,
    Intl,
    crypto: webcrypto,
    document,
    location,
    navigator,
    screen,
    localStorage: makeStorage(),
    sessionStorage: makeStorage(),
    XMLHttpRequest: BridgeXMLHttpRequest,
    Event: BasicEvent,
    CustomEvent: BasicCustomEvent,
    AnimationEvent: BasicEvent,
    TransitionEvent: BasicEvent,
    TouchEvent: BasicEvent,
    MouseEvent: BasicEvent,
    Touch: function Touch() {},
    Window: function Window() {},
    Document: function Document() {},
    HTMLDocument: function HTMLDocument() {},
    Navigator: function Navigator() {},
    Screen: function Screen() {},
    History: function History() {},
    Location: function Location() {},
    Storage: function Storage() {},
    Performance: function Performance() {},
    CSSStyleDeclaration: function CSSStyleDeclaration() {},
    CSSRule: function CSSRule() {},
    CSSStyleRule: function CSSStyleRule() {},
    CSSMediaRule: function CSSMediaRule() {},
    StyleSheet: function StyleSheet() {},
    CSSStyleSheet: function CSSStyleSheet() {},
    Node: function Node() {},
    NodeList: function NodeList() {},
    Text: function Text() {},
    Comment: function Comment() {},
    DocumentFragment: function DocumentFragment() {},
    Range: function Range() {},
    Element: function Element() {},
    HTMLElement: function HTMLElement() {},
    SVGElement: function SVGElement() {},
    WebGLRenderingContext: function WebGLRenderingContext() {},
    WebGL2RenderingContext: function WebGL2RenderingContext() {},
    Worker: BasicWorker,
    SharedWorker: BasicWorker,
    HTMLCanvasElement: function HTMLCanvasElement() {},
    Image: function Image() {},
    Audio(src = "") {
      const audio = makeElement("audio");
      audio.src = String(src);
      audio.autoplay = false;
      audio.controls = false;
      audio.currentTime = 0;
      audio.duration = Number.NaN;
      audio.muted = false;
      audio.paused = true;
      audio.preload = "auto";
      audio.volume = 1;
      audio.canPlayType = () => "";
      audio.load = () => {};
      audio.pause = () => {
        audio.paused = true;
      };
      audio.play = () => {
        audio.paused = false;
        return Promise.resolve();
      };
      return audio;
    },
    Attr: function Attr(name = "", value = "") {
      this.name = String(name);
      this.nodeName = this.name;
      this.value = String(value);
      this.nodeValue = this.value;
      this.ownerElement = null;
      this.specified = true;
    },
    Option(text = "", value = "", defaultSelected = false, selected = false) {
      const option = makeElement("option");
      option.text = String(text);
      option.value = String(value);
      option.defaultSelected = Boolean(defaultSelected);
      option.selected = Boolean(selected);
      return option;
    },
    Blob: globalThis.Blob,
    File: globalThis.File,
    FormData: globalThis.FormData,
    Headers: globalThis.Headers,
    Request: globalThis.Request,
    Response: globalThis.Response,
    atob(value) {
      return Buffer.from(String(value), "base64").toString("binary");
    },
    btoa(value) {
      return Buffer.from(String(value), "binary").toString("base64");
    },
    innerWidth: screenProfile.innerWidth,
    innerHeight: screenProfile.innerHeight,
    outerWidth: screenProfile.outerWidth,
    outerHeight: screenProfile.outerHeight,
    devicePixelRatio: screenProfile.devicePixelRatio,
    name: "",
    history: {
      length: 1,
      state: null,
      pushState() {},
      replaceState() {},
    },
    chrome: {
      runtime: {},
      app: {},
    },
  };
  context.window = context;
  context.self = context;
  context.globalThis = context;
  context.top = context;
  context.parent = context;
  context.AliyunCaptchaConfig = {
    prefix: options.prefix,
    region: options.region,
  };
  context.addEventListener = recordEventRegistration("window");
  context.removeEventListener = removeEventRegistration("window");
  context.dispatchEvent = (event) => (
    dispatchRegisteredEvent("window", contextReference, event)
  );
  context.ontouchstart = null;
  context.moveTo = () => {};
  context.moveBy = () => {};
  context.resizeTo = () => {};
  context.resizeBy = () => {};
  context.scrollTo = () => {};
  context.scroll = context.scrollTo;
  context.scrollBy = () => {};
  context.open = () => null;
  context.close = () => {};
  context.alert = () => {};
  context.confirm = () => false;
  context.prompt = () => null;
  context.print = () => {};
  context.focus = () => {};
  context.blur = () => {};
  context.__ALI_EVENT_REGISTRATIONS__ = eventRegistrations;
  context.matchMedia = (query) => ({
    media: String(query),
    matches: false,
    onchange: null,
    addListener() {},
    removeListener() {},
    addEventListener() {},
    removeEventListener() {},
    dispatchEvent() {
      return true;
    },
  });
  context.getComputedStyle = () => ({
    getPropertyValue() {
      return "";
    },
  });
  contextReference = vm.createContext(context);
  Object.setPrototypeOf(
    contextReference.HTMLElement.prototype,
    contextReference.Element.prototype,
  );
  for (const element of [head, body, documentElement]) {
    Object.setPrototypeOf(element, contextReference.HTMLElement.prototype);
  }
  return contextReference;
}


function parseForm(body) {
  if (body === null) {
    return {};
  }
  return Object.fromEntries(new URLSearchParams(body));
}


function parseTracelessVerifyResponse(responseText, expectedCertifyId) {
  let payload;
  try {
    payload = JSON.parse(responseText);
  } catch {
    throw new Error("无痕 Verify 响应不是有效 JSON");
  }
  const result = payload?.Result;
  const certifyId = result?.certifyId || expectedCertifyId;
  if (
    !result
    || typeof result !== "object"
    || Array.isArray(result)
    || typeof result.VerifyCode !== "string"
    || result.VerifyCode.length < 1
    || result.VerifyCode.length > 64
    || typeof result.VerifyResult !== "boolean"
    || typeof result.securityToken !== "string"
    || result.securityToken.length > 16 * 1024
    || typeof certifyId !== "string"
    || certifyId.length < 1
    || certifyId.length > 512
    || certifyId !== expectedCertifyId
  ) {
    throw new Error("无痕 Verify 响应合同无效");
  }
  return {
    verifyCode: result.VerifyCode,
    verifyResult: result.VerifyResult,
    securityToken: result.securityToken,
    certifyId,
  };
}


function wait(delayMs) {
  return new Promise((resolve) => setTimeout(resolve, delayMs));
}


// 跨过一个完整的宿主事件循环轮次（timers → poll → check），语义与 wait(0)
// 相同——已到期的 timer 与排队的 microtask 照常执行——但不付 Node 把
// setTimeout(0) 抬到 1ms 的固定代价。回放 86 条事件时这是 106ms 对 1.7ms 的差距。
function nextHostTurn() {
  return new Promise((resolve) => setImmediate(resolve));
}


function selectFeiLinGetterOwner(context) {
  const getterOwner = context?.z_um || context?.um;
  return getterOwner
    && typeof getterOwner.getToken === "function"
    ? getterOwner
    : null;
}


function callFeiLinGetter(getterOwner, getterArguments) {
  if (
    !getterOwner
    || typeof getterOwner.getToken !== "function"
  ) {
    throw new Error("FeiLin getToken 不可用");
  }
  if (
    !Array.isArray(getterArguments)
    || getterArguments.some(
      (value) => typeof value !== "string",
    )
  ) {
    throw new Error("FeiLin getter 参数列表无效");
  }
  const token = getterOwner.getToken(...getterArguments);
  if (typeof token !== "string" || !token) {
    throw new Error("FeiLin getToken() 返回空 token");
  }
  return token;
}


function refreshFeiLinToken(getterOwner) {
  return callFeiLinGetter(getterOwner, []);
}


function callPeFeiLinGetter(getterOwner, getterArguments) {
  try {
    return callFeiLinGetter(getterOwner, getterArguments);
  } catch {
    // PE 实参属于敏感运行态；底层 getter 可能把它拼入异常文本。
    // challenge-worker 的所有错误通道只能暴露固定原因，不能传播
    // message、stack、cause 或序列化后的原始异常。
    throw new Error("challenge-worker PE getToken() 失败");
  }
}


function parseWorkerCompletionPayload(payload) {
  if (payload?.mode === "sliding") {
    const staticPath = payload.staticPath;
    const track = payload.track;
    const slideWidth = payload.slideWidth;
    const handleWidth = payload.handleWidth;
    const target = slideWidth - handleWidth;
    const validTrack = (
      Array.isArray(track)
      && track.length >= 3
      && track.length <= 512
      && track.every((event, index) => (
        event !== null
        && typeof event === "object"
        && !Array.isArray(event)
        && Object.keys(event).length === 7
        && ["touchstart", "touchmove", "touchend"].includes(event.type)
        && Number.isInteger(event.x)
        && event.x >= -64
        && event.x <= target + 64
        && Number.isInteger(event.y)
        && event.y >= -256
        && event.y <= 256
        && Number.isInteger(event.dt)
        && event.dt >= 0
        && event.dt <= 5_000
        && Number.isFinite(event.force)
        && event.force >= 0
        && event.force <= 1
        && Number.isFinite(event.radiusX)
        && event.radiusX > 0
        && event.radiusX <= 128
        && Number.isFinite(event.radiusY)
        && event.radiusY > 0
        && event.radiusY <= 128
        && (index !== 0 || (event.type === "touchstart" && event.x === 0 && event.dt === 0))
        && (index === 0 || index === track.length - 1 || event.type === "touchmove")
        && (index !== track.length - 1 || (event.type === "touchend" && event.x === target))
      ))
      && track.at(-2)?.x === target
      && track.reduce((total, event) => total + event.dt, 0) <= 15_000
    );
    if (
      payload === null
      || typeof payload !== "object"
      || Array.isArray(payload)
      || payload.complete !== true
      || Object.keys(payload).length !== 10
      || typeof payload.sceneId !== "string"
      || payload.sceneId.length < 1
      || payload.sceneId.length > 64
      || typeof payload.certifyId !== "string"
      || payload.certifyId.length < 1
      || payload.certifyId.length > 512
      || typeof payload.captchaType !== "string"
      || payload.captchaType.toUpperCase() !== "SLIDING"
      || typeof payload.deviceToken !== "string"
      || payload.deviceToken.length < 1
      || payload.deviceToken.length > 32 * 1024
      || typeof staticPath !== "string"
      || staticPath.length < 1
      || staticPath.length > 512
      || staticPath.startsWith("/")
      || staticPath.includes("://")
      || staticPath.split("/").some((segment) => (
        segment === "" || segment === "." || segment === ".."
      ))
      || !Number.isInteger(slideWidth)
      || slideWidth < 320
      || slideWidth > 1024
      || !Number.isInteger(handleWidth)
      || handleWidth < 30
      || handleWidth >= slideWidth
      || !validTrack
    ) {
      throw new Error("challenge-worker 拖动完成信号无效");
    }
    return {
      complete: true,
      mode: "sliding",
      sceneId: payload.sceneId,
      certifyId: payload.certifyId,
      captchaType: "SLIDING",
      deviceToken: payload.deviceToken,
      staticPath,
      track: track.map((event) => ({ ...event })),
      slideWidth,
      handleWidth,
    };
  }
  if (payload?.mode === "traceless") {
    const staticPath = payload.staticPath;
    if (
      payload === null
      || typeof payload !== "object"
      || Array.isArray(payload)
      || payload.complete !== true
      || Object.keys(payload).length !== 7
      || typeof payload.sceneId !== "string"
      || payload.sceneId.length < 1
      || payload.sceneId.length > 64
      || typeof payload.certifyId !== "string"
      || payload.certifyId.length < 1
      || payload.certifyId.length > 512
      || typeof payload.captchaType !== "string"
      || payload.captchaType.toUpperCase() !== "TRACELESS"
      || typeof payload.deviceToken !== "string"
      || payload.deviceToken.length < 1
      || payload.deviceToken.length > 32 * 1024
      || typeof staticPath !== "string"
      || staticPath.length < 1
      || staticPath.length > 512
      || staticPath.startsWith("/")
      || staticPath.includes("://")
      || staticPath.split("/").includes("..")
    ) {
      throw new Error("challenge-worker 无痕完成信号无效");
    }
    return {
      complete: true,
      mode: "traceless",
      sceneId: payload.sceneId,
      certifyId: payload.certifyId,
      captchaType: "TRACELESS",
      deviceToken: payload.deviceToken,
      staticPath,
    };
  }
  const interactionEvents = payload?.interactionEvents;
  const postInteractionDelayMs = payload?.postInteractionDelayMs;
  if (
    payload === null
    || typeof payload !== "object"
    || Array.isArray(payload)
    || payload.complete !== true
    || Object.keys(payload).length !== 4
    || !Array.isArray(payload.getterArguments)
    || payload.getterArguments.length !== 1
    || typeof payload.getterArguments[0] !== "string"
    || payload.getterArguments[0].length < 1
    || payload.getterArguments[0].length > 512
    || !Array.isArray(interactionEvents)
    || interactionEvents.length < 1
    || interactionEvents.length > 512
    || !Number.isInteger(postInteractionDelayMs)
    || postInteractionDelayMs < 0
    || postInteractionDelayMs > 500
  ) {
    throw new Error("challenge-worker 完成信号无效");
  }
  let previousTimeStamp = null;
  const normalizedEvents = interactionEvents.map((event) => {
    if (
      event === null
      || typeof event !== "object"
      || Array.isArray(event)
      || Object.keys(event).length !== 5
      || event.type !== "mousemove"
      || !Number.isFinite(event.x)
      || !Number.isFinite(event.y)
      || !Number.isFinite(event.timeStamp)
      || event.timeStamp < 0
      || event.timeStamp > 180_000
      || Math.abs(event.x) > 10_000
      || Math.abs(event.y) > 10_000
      || (
        previousTimeStamp !== null
        && event.timeStamp < previousTimeStamp
      )
      || event.isTrusted !== true
    ) {
      throw new Error("challenge-worker 交互事件无效");
    }
    const normalized = {
      type: "mousemove",
      x: Number(event.x),
      y: Number(event.y),
      timeStamp: Number(event.timeStamp),
      isTrusted: true,
    };
    previousTimeStamp = normalized.timeStamp;
    return normalized;
  });
  if (
    normalizedEvents.at(-1).timeStamp
      - normalizedEvents[0].timeStamp
    > 60_000
  ) {
    throw new Error("challenge-worker 交互事件总时长无效");
  }
  return {
    complete: true,
    getterArguments: [...payload.getterArguments],
    interactionEvents: normalizedEvents,
    postInteractionDelayMs,
  };
}


async function readWorkerCompletionInput() {
  if (multiplexSessionId() !== null) {
    const payload = await new Promise((resolve, reject) => {
      const fail = () => reject(
        new Error("challenge-worker stdin 不是有效 JSON"),
      );
      parentPort.once("message", resolve);
      parentPort.once("messageerror", fail);
    });
    return parseWorkerCompletionPayload(payload);
  }

  const chunks = [];
  let length = 0;
  let source;
  try {
    source = await new Promise((resolve, reject) => {
      const cleanUp = () => {
        process.stdin.off("data", onData);
        process.stdin.off("end", onEnd);
        process.stdin.off("error", onError);
      };
      const fail = () => {
        cleanUp();
        process.stdin.pause();
        reject(new Error("challenge-worker stdin 不是有效 JSON"));
      };
      const onData = (chunk) => {
        const buffer = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk);
        length += buffer.length;
        if (length > WORKER_COMPLETION_MAX_BYTES) {
          fail();
          return;
        }
        chunks.push(buffer);
      };
      const onEnd = () => {
        cleanUp();
        resolve(Buffer.concat(chunks).toString("utf8"));
      };
      const onError = () => fail();
      process.stdin.on("data", onData);
      process.stdin.once("end", onEnd);
      process.stdin.once("error", onError);
      process.stdin.resume();
    });
  } catch {
    throw new Error("challenge-worker stdin 不是有效 JSON");
  }
  let payload;
  try {
    payload = JSON.parse(source);
  } catch {
    throw new Error("challenge-worker stdin 不是有效 JSON");
  }
  return parseWorkerCompletionPayload(payload);
}


async function runTracelessCaptcha(
  context,
  runtimeConfig,
  options,
  completion,
) {
  if (
    typeof context?.initAliyunCaptcha !== "function"
    || !runtimeConfig
    || typeof runtimeConfig !== "object"
  ) {
    throw new Error("无痕验证码运行态不可用");
  }

  const elementId = "ali-traceless-captcha";
  const buttonId = "ali-traceless-captcha-button";
  const element = context.document.createElement("div");
  const button = context.document.createElement("button");
  element.id = elementId;
  button.id = buttonId;
  button.type = "button";
  button.tabIndex = -1;

  const clickListeners = [];
  button.addEventListener = (type, listener) => {
    if (
      type === "click"
      && (
        typeof listener === "function"
        || typeof listener?.handleEvent === "function"
      )
    ) {
      clickListeners.push(listener);
    }
  };
  button.removeEventListener = (type, listener) => {
    if (type !== "click") return;
    const index = clickListeners.indexOf(listener);
    if (index >= 0) clickListeners.splice(index, 1);
  };
  button.click = () => {
    const event = new context.Event("click", {
      bubbles: true,
      cancelable: true,
    });
    event.target = button;
    event.currentTarget = button;
    if (typeof button.onclick === "function") {
      button.onclick.call(button, event);
    }
    for (const listener of [...clickListeners]) {
      if (typeof listener === "function") {
        listener.call(button, event);
      } else {
        listener.handleEvent.call(listener, event);
      }
    }
    return !event.defaultPrevented;
  };
  button.dispatchEvent = (event) => {
    if (event?.type !== "click") return true;
    return button.click();
  };

  const elements = new Map([
    [elementId, element],
    [buttonId, button],
  ]);
  const originalGetElementById = context.document.getElementById;
  const originalQuerySelector = context.document.querySelector;
  const originalQuerySelectorAll = context.document.querySelectorAll;
  context.document.getElementById = function getElementById(id) {
    return elements.get(String(id))
      ?? originalGetElementById.call(this, id);
  };
  context.document.querySelector = function querySelector(selector) {
    const text = String(selector);
    return text.startsWith("#")
      ? elements.get(text.slice(1)) ?? null
      : originalQuerySelector.call(this, selector);
  };
  context.document.querySelectorAll = function querySelectorAll(selector) {
    const matched = this.querySelector(selector);
    return matched ? [matched] : originalQuerySelectorAll.call(this, selector);
  };
  context.document.body.appendChild(element);
  context.document.body.appendChild(button);

  const hadDeviceToken = Object.prototype.hasOwnProperty.call(
    runtimeConfig,
    "DeviceToken",
  );
  const originalDeviceToken = runtimeConfig.DeviceToken;
  let captchaInstance = null;
  let timeoutId = null;
  try {
    runtimeConfig.DeviceToken = completion.deviceToken;
    if (runtimeConfig.DeviceToken !== completion.deviceToken) {
      throw new Error("无痕验证码 DeviceToken 不可写");
    }
    context.__ALI_SDK_INIT__ = {
      sceneId: completion.sceneId,
      certifyId: completion.certifyId,
      captchaType: "TRACELESS",
      securityToken: "",
      staticPath: completion.staticPath,
      consumed: false,
      verifyRequested: false,
      observedDeviceToken: "",
      verifyResult: null,
      phase: "ready",
    };
    const captchaVerifyParam = await new Promise((resolve, reject) => {
      let settled = false;
      const finish = (callback, value) => {
        if (settled) return;
        settled = true;
        if (timeoutId !== null) clearTimeout(timeoutId);
        callback(value);
      };
      timeoutId = setTimeout(
        () => {
          const phase = context.__ALI_SDK_INIT__?.phase;
          const code = {
            ready: "TRACELESS_TIMEOUT_READY",
            calling_init: "TRACELESS_TIMEOUT_CALLING_INIT",
            init_rejected: "TRACELESS_TIMEOUT_INIT_REJECTED",
            init_response: "TRACELESS_TIMEOUT_INIT_RESPONSE",
            instance: "TRACELESS_TIMEOUT_INSTANCE",
            click: "TRACELESS_TIMEOUT_CLICK",
            verify_request: "TRACELESS_TIMEOUT_VERIFY_REQUEST",
            verify_response: "TRACELESS_TIMEOUT_VERIFY_RESPONSE",
          }[phase] ?? "TRACELESS_TIMEOUT_READY";
          finish(reject, tracelessBridgeError(code));
        },
        options.timeoutMs,
      );
      try {
        context.__ALI_SDK_INIT__.phase = "calling_init";
        context.initAliyunCaptcha({
          SceneId: completion.sceneId,
          mode: "popup",
          element: `#${elementId}`,
          button: `#${buttonId}`,
          language: "cn",
          timeout: Math.min(options.timeoutMs, 5_000),
          rem: 1,
          success(value) {
            context.__ALI_SDK_INIT__.phase = "success";
            if (
              !context.__ALI_SDK_INIT__?.consumed
              || context.__ALI_SDK_INIT__?.verifyResult === null
              || typeof value !== "string"
              || value.length < 1
              || value.length > 16 * 1024
            ) {
              finish(reject, new Error("无痕验证码成功参数无效"));
              return;
            }
            finish(resolve, value);
          },
          fail() {
            finish(reject, tracelessBridgeError("TRACELESS_FAIL_CALLBACK"));
          },
          onClose() {
            finish(reject, tracelessBridgeError("TRACELESS_CLOSE_CALLBACK"));
          },
          getInstance(instance) {
            captchaInstance = instance;
            context.__ALI_SDK_INIT__.phase = "instance";
            setTimeout(() => {
              context.__ALI_SDK_INIT__.phase = "click";
              button.click();
            }, 0);
          },
        });
      } catch {
        finish(reject, tracelessBridgeError("TRACELESS_INIT_THROW"));
      }
    });
    const observedDeviceToken = (
      context.__ALI_SDK_INIT__?.observedDeviceToken
    );
    if (
      typeof observedDeviceToken !== "string"
      || observedDeviceToken.length < 1
      || observedDeviceToken.length > 32 * 1024
    ) {
      throw new Error("无痕验证码 DeviceToken 无效");
    }
    return {
      captchaVerifyParam,
      deviceToken: observedDeviceToken,
      verifyResult: context.__ALI_SDK_INIT__.verifyResult,
    };
  } finally {
    if (timeoutId !== null) clearTimeout(timeoutId);
    try {
      captchaInstance?.destroy?.();
    } catch {}
    context.document.getElementById = originalGetElementById;
    context.document.querySelector = originalQuerySelector;
    context.document.querySelectorAll = originalQuerySelectorAll;
    context.document.body.removeChild(element);
    context.document.body.removeChild(button);
    delete context.__ALI_SDK_INIT__;
    if (hadDeviceToken) {
      runtimeConfig.DeviceToken = originalDeviceToken;
    } else {
      delete runtimeConfig.DeviceToken;
    }
  }
}


function setSlidingElementBox(element, width, height, defaultLeft = 0) {
  element.offsetWidth = width;
  element.clientWidth = width;
  element.offsetHeight = height;
  element.clientHeight = height;
  Object.defineProperties(element, {
    offsetLeft: {
      configurable: true,
      get() {
        const styledLeft = Number.parseFloat(element.style.left);
        return Number.isFinite(styledLeft) ? styledLeft : defaultLeft;
      },
    },
    offsetTop: {
      configurable: true,
      get() { return 0; },
    },
  });
  element.getBoundingClientRect = () => {
    const styledLeft = Number.parseFloat(element.style.left);
    const left = Number.isFinite(styledLeft) ? styledLeft : defaultLeft;
    return {
      x: left,
      y: 0,
      top: 0,
      left,
      right: left + width,
      bottom: height,
      width,
      height,
    };
  };
}


function buildSlidingElementTree(context, host, slideWidth, handleWidth) {
  const elements = new Map([[host.id, host]]);
  const make = (tagName, id, className = "") => {
    const element = context.document.createElement(tagName);
    element.id = id;
    element.className = className;
    elements.set(id, element);
    return element;
  };
  const embed = make("div", "aliyunCaptcha-window-embed", "aliyunCaptcha-show");
  const wrapper = make("div", "aliyunCaptcha-sliding-wrapper", "aliyun-captcha");
  const body = make("div", "aliyunCaptcha-sliding-body", "sliding");
  const left = make("div", "aliyunCaptcha-sliding-left", "aliyunCaptcha-sliding-slided");
  const slider = make("div", "aliyunCaptcha-sliding-slider", "aliyunCaptcha-sliding-slider");
  const textBox = make("div", "aliyunCaptcha-sliding-text-box", "aliyunCaptcha-sliding-text-box");
  const text = make("span", "aliyunCaptcha-sliding-text", "aliyunCaptcha-sliding-text");
  const errorCode = make("div", "aliyunCaptcha-sliding-errorCode");
  const failTip = make("span", "aliyunCaptcha-sliding-failTip");

  slider.textContent = "\ue624";
  text.textContent = "请按住滑块，拖动到最右边";
  textBox.appendChild(text);
  body.appendChild(left);
  body.appendChild(slider);
  body.appendChild(textBox);
  wrapper.appendChild(body);
  wrapper.appendChild(errorCode);
  wrapper.appendChild(failTip);
  embed.appendChild(wrapper);
  host.appendChild(embed);

  for (const element of [host, embed, wrapper, body, left, textBox]) {
    setSlidingElementBox(element, slideWidth, handleWidth);
  }
  setSlidingElementBox(slider, handleWidth, handleWidth);
  setSlidingElementBox(text, slideWidth - 2 * handleWidth, handleWidth);
  setSlidingElementBox(errorCode, 0, 0);
  setSlidingElementBox(failTip, 0, 0);

  const query = (selector) => {
    const normalized = String(selector);
    return normalized.startsWith("#")
      ? elements.get(normalized.slice(1)) ?? null
      : null;
  };
  const queryAll = (selector) => {
    if (String(selector) === "*") return [...elements.values()];
    const matched = query(selector);
    return matched ? [matched] : [];
  };
  for (const element of elements.values()) {
    element.querySelector = query;
    element.querySelectorAll = queryAll;
  }
  return { elements, slider, query, queryAll };
}


function makeTouchList(values) {
  const list = [...values];
  Object.defineProperty(list, "item", {
    configurable: true,
    value(index) {
      return list[index] ?? null;
    },
  });
  return list;
}


function dispatchSlidingEvent(context, slider, event) {
  let eventTarget = slider;
  while (eventTarget && eventTarget !== context.document.body) {
    eventTarget.dispatchEvent(event);
    if (event[BRIDGE_EVENT_STATE].propagationStopped) return;
    eventTarget = eventTarget.parentNode;
  }
  if (eventTarget === context.document.body) {
    context.document.body.dispatchEvent(event);
  }
  if (!event[BRIDGE_EVENT_STATE].propagationStopped) {
    context.document.dispatchEvent(event);
  }
  if (!event[BRIDGE_EVENT_STATE].propagationStopped) {
    context.dispatchEvent(event);
  }
}


async function replaySlidingTrack(context, slider, completion) {
  let elapsed = 0;
  let previousClientX = null;
  let previousClientY = null;
  for (const sample of completion.track) {
    if (sample.dt > 0) {
      await new Promise((resolve) => setTimeout(resolve, sample.dt));
    }
    elapsed += sample.dt;
    const clientX = completion.handleWidth / 2 + sample.x;
    const clientY = completion.handleWidth / 2 + sample.y;
    const movementX = previousClientX === null ? 0 : clientX - previousClientX;
    const movementY = previousClientY === null ? 0 : clientY - previousClientY;
    const touch = {
      identifier: 1,
      target: slider,
      clientX,
      clientY,
      pageX: clientX,
      pageY: clientY,
      screenX: clientX,
      screenY: clientY,
      radiusX: sample.radiusX,
      radiusY: sample.radiusY,
      rotationAngle: 0,
      force: sample.force,
    };
    const ended = sample.type === "touchend";
    const wireType = {
      touchstart: "mousedown",
      touchmove: "mousemove",
      touchend: "mouseup",
    }[sample.type];
    const active = makeTouchList(ended ? [] : [touch]);
    const changed = makeTouchList([touch]);
    let event;
    try {
      event = new context.MouseEvent(wireType, {
        bubbles: true,
        cancelable: true,
        composed: true,
        isTrusted: true,
        timeStamp: elapsed,
        target: slider,
        touches: active,
        targetTouches: active,
        changedTouches: changed,
        clientX,
        clientY,
        pageX: clientX,
        pageY: clientY,
        screenX: clientX,
        screenY: clientY,
        x: clientX,
        y: clientY,
        offsetX: clientX,
        offsetY: clientY,
        layerX: clientX,
        layerY: clientY,
        movementX,
        movementY,
        button: 0,
        buttons: ended ? 0 : 1,
        which: 1,
        detail: 1,
        view: context,
      });
    } catch {
      throw slidingBridgeError("SLIDING_EVENT_CONSTRUCT");
    }
    if (sample.type === "touchstart") {
      try {
        dispatchSlidingEvent(context, slider, event);
      } catch {
        throw slidingBridgeError("SLIDING_TOUCHSTART_DISPATCH");
      }
      previousClientX = clientX;
      previousClientY = clientY;
      continue;
    }
    try {
      dispatchSlidingEvent(context, slider, event);
    } catch {
      throw slidingBridgeError(
        sample.type === "touchend"
          ? "SLIDING_TOUCHEND_DISPATCH"
          : "SLIDING_TOUCHMOVE_DISPATCH",
      );
    }
    previousClientX = clientX;
    previousClientY = clientY;
  }
}


async function runSlidingCaptcha(
  context,
  runtimeConfig,
  options,
  completion,
) {
  if (
    typeof context?.initAliyunCaptcha !== "function"
    || !runtimeConfig
    || typeof runtimeConfig !== "object"
  ) {
    throw new Error("拖动验证码运行态不可用");
  }

  const elementId = "ali-sliding-captcha";
  const element = context.document.createElement("div");
  element.id = elementId;
  setSlidingElementBox(element, completion.slideWidth, completion.handleWidth);
  let slidingDOM = null;
  element.insertAdjacentHTML = (_position, html) => {
    if (
      slidingDOM === null
      && String(html).includes("aliyunCaptcha-sliding-wrapper")
    ) {
      slidingDOM = buildSlidingElementTree(
        context,
        element,
        completion.slideWidth,
        completion.handleWidth,
      );
    }
  };

  const originalGetElementById = context.document.getElementById;
  const originalQuerySelector = context.document.querySelector;
  const originalQuerySelectorAll = context.document.querySelectorAll;
  context.document.getElementById = function getElementById(id) {
    if (String(id) === elementId) return element;
    return slidingDOM?.elements.get(String(id))
      ?? originalGetElementById.call(this, id);
  };
  context.document.querySelector = function querySelector(selector) {
    const normalized = String(selector);
    if (normalized === `#${elementId}`) return element;
    return slidingDOM?.query(normalized)
      ?? originalQuerySelector.call(this, selector);
  };
  context.document.querySelectorAll = function querySelectorAll(selector) {
    const normalized = String(selector);
    if (normalized === `#${elementId}`) return [element];
    const matched = slidingDOM?.queryAll(normalized) ?? [];
    return matched.length > 0
      ? matched
      : originalQuerySelectorAll.call(this, selector);
  };
  element.querySelector = (selector) => slidingDOM?.query(selector) ?? null;
  element.querySelectorAll = (selector) => slidingDOM?.queryAll(selector) ?? [];
  context.document.body.appendChild(element);

  const hadDeviceToken = Object.prototype.hasOwnProperty.call(
    runtimeConfig,
    "DeviceToken",
  );
  const originalDeviceToken = runtimeConfig.DeviceToken;
  let captchaInstance = null;
  let timeoutId = null;
  let dragStarted = false;
  try {
    runtimeConfig.DeviceToken = completion.deviceToken;
    if (runtimeConfig.DeviceToken !== completion.deviceToken) {
      throw new Error("拖动验证码 DeviceToken 不可写");
    }
    context.__ALI_SDK_INIT__ = {
      sceneId: completion.sceneId,
      certifyId: completion.certifyId,
      captchaType: "SLIDING",
      securityToken: "",
      staticPath: completion.staticPath,
      consumed: false,
      verifyRequested: false,
      observedDeviceToken: "",
      verifyResult: null,
      phase: "ready",
    };
    const captchaVerifyParam = await new Promise((resolve, reject) => {
      let settled = false;
      const finish = (callback, value) => {
        if (settled) return;
        settled = true;
        if (timeoutId !== null) clearTimeout(timeoutId);
        callback(value);
      };
      timeoutId = setTimeout(
        () => {
          const phase = context.__ALI_SDK_INIT__?.phase;
          const code = {
            ready: "SLIDING_TIMEOUT_READY",
            calling_init: "SLIDING_TIMEOUT_CALLING_INIT",
            init_rejected: "SLIDING_TIMEOUT_INIT_REJECTED",
            init_response: "SLIDING_TIMEOUT_INIT_RESPONSE",
            instance: "SLIDING_TIMEOUT_INSTANCE",
            dom: "SLIDING_TIMEOUT_DOM",
            drag: "SLIDING_TIMEOUT_DRAG",
            drag_complete: "SLIDING_TIMEOUT_DRAG_COMPLETE",
            verify_request: "SLIDING_TIMEOUT_VERIFY_REQUEST",
            verify_response: "SLIDING_TIMEOUT_VERIFY_RESPONSE",
          }[phase] ?? "SLIDING_TIMEOUT_READY";
          finish(reject, slidingBridgeError(code));
        },
        options.timeoutMs,
      );
      try {
        context.__ALI_SDK_INIT__.phase = "calling_init";
        context.initAliyunCaptcha({
          prefix: options.prefix,
          SceneId: completion.sceneId,
          mode: "embed",
          element: `#${elementId}`,
          language: "cn",
          timeout: Math.min(options.timeoutMs, 5_000),
          rem: 1,
          slideStyle: {
            width: completion.slideWidth,
            height: completion.handleWidth,
          },
          showErrorTip: false,
          delayBeforeSuccess: false,
          success(value) {
            context.__ALI_SDK_INIT__.phase = "success";
            if (
              !context.__ALI_SDK_INIT__?.consumed
              || context.__ALI_SDK_INIT__?.captchaType !== "SLIDING"
              || context.__ALI_SDK_INIT__?.verifyResult === null
              || typeof value !== "string"
              || value.length <= 140
              || value.length > 16 * 1024
            ) {
              finish(reject, new Error("拖动验证码成功参数无效"));
              return;
            }
            finish(resolve, value);
          },
          fail() {
            finish(reject, slidingBridgeError("SLIDING_FAIL_CALLBACK"));
          },
          onError() {
            finish(reject, slidingBridgeError("SLIDING_ERROR_CALLBACK"));
          },
          onClose() {
            finish(reject, slidingBridgeError("SLIDING_FAIL_CALLBACK"));
          },
          getInstance(instance) {
            captchaInstance = instance;
            context.__ALI_SDK_INIT__.phase = "instance";
            if (dragStarted) return;
            dragStarted = true;
            setTimeout(() => {
              Promise.resolve()
                .then(async () => {
                  context.__ALI_SDK_INIT__.phase = "dom";
                  const slider = slidingDOM?.slider;
                  if (!slider) {
                    throw slidingBridgeError("SLIDING_TIMEOUT_DOM");
                  }
                  // getInstance 在滑动组件的 document 监听器完全挂载前就可能回调。
                  // 保留一个人手反应窗口，避免后挂的 move 处理器错过 touchstart。
                  await new Promise((resolveDelay) => setTimeout(
                    resolveDelay,
                    Math.min(260, Math.max(120, Math.floor(options.timeoutMs / 20))),
                  ));
                  context.__ALI_SDK_INIT__.phase = "drag";
                  await replaySlidingTrack(context, slider, completion);
                  context.__ALI_SDK_INIT__.phase = "drag_complete";
                })
                .catch((error) => {
                  finish(
                    reject,
                    error?.safeCode
                      ? error
                      : slidingBridgeError("SLIDING_TRACK_DISPATCH"),
                  );
                });
            }, 0);
          },
        });
      } catch {
        finish(reject, slidingBridgeError("SLIDING_INIT_THROW"));
      }
    });
    const observedDeviceToken = context.__ALI_SDK_INIT__?.observedDeviceToken;
    if (
      typeof observedDeviceToken !== "string"
      || observedDeviceToken.length < 1
      || observedDeviceToken.length > 32 * 1024
    ) {
      throw new Error("拖动验证码 DeviceToken 无效");
    }
    return {
      captchaVerifyParam,
      deviceToken: observedDeviceToken,
      verifyResult: context.__ALI_SDK_INIT__?.verifyResult,
    };
  } finally {
    if (timeoutId !== null) clearTimeout(timeoutId);
    try {
      captchaInstance?.destroyCaptcha?.();
    } catch {}
    try {
      captchaInstance?.destroy?.();
    } catch {}
    context.document.getElementById = originalGetElementById;
    context.document.querySelector = originalQuerySelector;
    context.document.querySelectorAll = originalQuerySelectorAll;
    context.document.body.removeChild(element);
    delete context.__ALI_SDK_INIT__;
    if (hadDeviceToken) {
      runtimeConfig.DeviceToken = originalDeviceToken;
    } else {
      delete runtimeConfig.DeviceToken;
    }
  }
}


function writeHostOutput(payload) {
  fs.writeFileSync(1, `${JSON.stringify(payload)}\n`);
}


async function runChallengeHost(options, source) {
  if (!isMainThread) {
    throw new Error("challenge-host 只能在 Node 主线程运行");
  }

  const input = createInterface({
    input: process.stdin,
    crlfDelay: Infinity,
    terminal: false,
  });
  const states = new Map();
  let remaining = options.vmCount;
  let settled = false;
  let resolveDone;
  let rejectDone;
  const done = new Promise((resolve, reject) => {
    resolveDone = resolve;
    rejectDone = reject;
  });

  const settleFailure = (error) => {
    if (!settled) {
      settled = true;
      rejectDone(error);
    }
  };
  const finishSlot = (state, worker) => {
    if (state.worker !== worker || state.terminal) {
      return;
    }
    state.worker = null;
    state.terminal = true;
    if (options.persistentHost) {
      if (state.restartRequested && !settled) {
        state.restartRequested = false;
        startSlot(state);
      }
      return;
    }
    remaining -= 1;
    if (remaining === 0 && !settled) {
      settled = true;
      input.close();
      resolveDone();
    }
  };
  const reportSlotError = (state, worker) => {
    if (
      state.worker !== worker
      || state.errorReported
      || state.cancelled
      || state.completed
    ) {
      return;
    }
    state.errorReported = true;
    writeHostOutput({
      sessionId: state.sessionId,
      stage: "error",
      error: safeDeviceBridgeFailureMessage(),
    });
  };

  function startSlot(state) {
    const worker = new Worker(new URL(import.meta.url), {
      workerData: {
        challengeSessionId: state.sessionId,
        options: {
          ...options,
          mode: "challenge-worker",
          networkEnabled: true,
          vmCount: 1,
        },
        source,
      },
    });
    state.cancelled = false;
    state.commandSent = false;
    state.completed = false;
    state.errorReported = false;
    state.terminal = false;
    state.worker = worker;

    worker.on("message", (payload) => {
      if (
        state.worker !== worker
        || payload === null
        || typeof payload !== "object"
        || Array.isArray(payload)
        || payload.sessionId !== state.sessionId
        || !["init", "verify", "traceless", "error"].includes(payload.stage)
      ) {
        reportSlotError(state, worker);
        void worker.terminate();
        return;
      }
      writeHostOutput(payload);
      if (payload.stage === "verify" || payload.stage === "traceless") {
        state.completed = true;
        void worker.terminate();
      } else if (payload.stage === "error") {
        state.errorReported = true;
        void worker.terminate();
      }
    });
    worker.once("error", () => {
      reportSlotError(state, worker);
    });
    worker.once("exit", () => {
      // 没有 verify/error/cancel 就退出，即使 exit code 为 0 也是槽位失败。
      reportSlotError(state, worker);
      finishSlot(state, worker);
    });
  }

  for (let sessionId = 0; sessionId < options.vmCount; sessionId += 1) {
    const state = {
      cancelled: false,
      commandSent: false,
      completed: false,
      errorReported: false,
      restartRequested: false,
      sessionId,
      terminal: true,
      worker: null,
    };
    states.set(sessionId, state);
    startSlot(state);
  }

  input.on("line", (line) => {
    try {
      if (
        !line
        || Buffer.byteLength(line, "utf8") > HOST_COMMAND_MAX_BYTES
      ) {
        throw new Error("challenge-host 命令无效");
      }
      const command = JSON.parse(line);
      if (
        command === null
        || typeof command !== "object"
        || Array.isArray(command)
        || !Number.isInteger(command.sessionId)
      ) {
        throw new Error("challenge-host 命令无效");
      }
      const state = states.get(command.sessionId);
      if (!state) {
        throw new Error("challenge-host 会话无效");
      }
      if (
        command.reset === true
        && Object.keys(command).length === 2
      ) {
        if (!options.persistentHost) {
          throw new Error("challenge-host 不支持重建会话");
        }
        if (state.restartRequested) {
          return;
        }
        if (state.terminal) {
          startSlot(state);
          return;
        }
        state.restartRequested = true;
        state.commandSent = true;
        state.cancelled = true;
        void state.worker.terminate();
        return;
      }
      if (
        command.cancel === true
        && Object.keys(command).length === 2
      ) {
        if (state.terminal || state.cancelled) {
          return;
        }
        state.commandSent = true;
        state.cancelled = true;
        void state.worker?.terminate();
        return;
      }
      if (state.terminal || state.commandSent) {
        throw new Error("challenge-host 会话无效");
      }
      const { sessionId: _sessionId, ...completion } = command;
      state.commandSent = true;
      state.worker.postMessage(parseWorkerCompletionPayload(completion));
    } catch (error) {
      settleFailure(error);
    }
  });
  input.once("close", () => {
    if (options.persistentHost && !settled) {
      settled = true;
      resolveDone();
      return;
    }
    if (remaining > 0) {
      settleFailure(new Error("challenge-host stdin 提前关闭"));
    }
  });

  try {
    await done;
  } finally {
    input.close();
    process.stdin.pause();
    for (const state of states.values()) {
      if (!state.terminal) {
        state.cancelled = true;
        void state.worker?.terminate();
      }
    }
  }
}


async function replayFeiLinInteractionEvents(context, events) {
  let previousX = null;
  let previousY = null;
  for (const sample of events) {
    const event = {
      type: sample.type,
      x: sample.x,
      y: sample.y,
      clientX: sample.x,
      clientY: sample.y,
      pageX: sample.x,
      pageY: sample.y,
      screenX: sample.x,
      screenY: sample.y,
      movementX: previousX === null ? 0 : sample.x - previousX,
      movementY: previousY === null ? 0 : sample.y - previousY,
      timeStamp: sample.timeStamp,
      isTrusted: sample.isTrusted,
      button: 0,
      buttons: 0,
      detail: 0,
      view: context,
      defaultPrevented: false,
    };
    context.document.dispatchEvent(event);
    if (!event[BRIDGE_EVENT_STATE].propagationStopped) {
      context.dispatchEvent(event);
    }
    previousX = sample.x;
    previousY = sample.y;
    // 保留 PE 提供的完整逻辑 timeStamp，但不把相邻时间差转换成真实
    // sleep；每条事件后仍跨一个 host turn，让 FeiLin 的 timer/microtask
    // listener 不会被压成单个同步 burst。
    await nextHostTurn();
  }
}


async function main() {
  const options = multiplexSessionId() === null
    ? parseArguments(process.argv)
    : workerData.options;
  const source = multiplexSessionId() === null
    ? fs.readFileSync(options.sdkPath, "utf8")
    : workerData.source;
  if (options.mode === "challenge-host") {
    await runChallengeHost(options, source);
    return;
  }
  let captured = null;
  let log1DataPlaintext = null;
  let runtimeConfig = null;
  const requests = [];
  const context = makeBrowserContext(options, (request) => {
    requests.push(request);
    if (captured === null) {
      captured = {
        ...request,
        form: parseForm(request.body),
      };
    }
  });
  if (options.mode === "probe-log1") {
    installLog1PlaintextCapture(context, (value) => {
      if (typeof value === "string" && value) {
        log1DataPlaintext = value;
        delete context[LOG1_PLAINTEXT_CAPTURE_HOOK];
      }
    });
  }
  const runtimeStateCapture = installSdkStateCapture(context, (owner) => {
    runtimeConfig = owner;
  });

  try {
    vm.runInContext(source, context, {
      filename: options.sdkPath,
      timeout: 10_000,
    });
  } catch (error) {
    runtimeStateCapture.flush();
    if (captured === null) {
      throw error;
    }
  }
  runtimeStateCapture.flush();
  if (captured === null && options.mode === "probe-log1") {
    throw new Error("SDK 未执行到 XHR.send；需要补充首分歧环境");
  }
  if (options.mode === "probe-log1" && runtimeConfig === null) {
    throw new Error("未识别 SDK 设备运行态，公开脚本结构可能已变化");
  }
  if (
    options.mode === "live-token"
    || options.mode === "profile-token"
    || options.mode === "challenge-worker"
  ) {
    const deadline = Date.now() + options.timeoutMs;
    let deviceToken = "";
    let tokenSource = "";

    while (Date.now() < deadline) {
      const runtimeToken = runtimeConfig?.DeviceToken;
      if (typeof runtimeToken === "string" && runtimeToken) {
        deviceToken = runtimeToken;
        tokenSource = "deviceCallback";
      }
      const candidateGetterOwner = selectFeiLinGetterOwner(context);
      if (!deviceToken && candidateGetterOwner) {
        try {
          const generated = refreshFeiLinToken(candidateGetterOwner);
          if (typeof generated === "string" && generated) {
            deviceToken = generated;
            tokenSource = "getToken";
          }
        } catch {}
      }
      if (deviceToken && runtimeConfig !== null) {
        break;
      }
      await wait(25);
    }

    if (runtimeConfig === null) {
      throw new Error("未识别 SDK 设备运行态，公开脚本结构可能已变化");
    }
    if (!deviceToken) {
      throw new Error("等待 DeviceToken 超时");
    }

    let getterOwner = null;
    while (Date.now() < deadline) {
      getterOwner = selectFeiLinGetterOwner(context);
      if (getterOwner) {
        break;
      }
      await wait(10);
    }

    const summarizeRequests = () => requests.map((request) => {
      const form = parseForm(request.body);
      return {
        host: new URL(request.url).host,
        action: form.Action ?? "",
        fieldNames: Object.keys(form),
        headerNames: request.effectiveHeaderNames,
      };
    });
    const deviceConfig = (
      runtimeConfig.deviceConfig ?? null
    );
    const verifyArgProfile = {
      accessSec: runtimeConfig.ACCESS_SEC,
      sessionIdSalt: runtimeConfig.SESSION_ID_SALT,
    };
    for (const [name, value] of Object.entries(verifyArgProfile)) {
      if (
        typeof value !== "string"
        || value.length < 1
        || value.length > 128
        || !/^[\x20-\x7e]+$/.test(value)
      ) {
        throw new Error(`公开 SDK 的 Verify arg profile.${name} 无效`);
      }
    }

    if (options.mode === "challenge-worker") {
      if (!getterOwner) {
        throw new Error("challenge-worker 等待 getToken 超时");
      }
      writeBridgeOutput({
        stage: "init",
        deviceToken,
        deviceConfig,
        verifyArgProfile,
        tokenSource,
        requestCount: requests.length,
        requests: summarizeRequests(),
      });
      const completion = await readWorkerCompletionInput();
      if (completion.mode === "sliding") {
        const slidingResult = await runSlidingCaptcha(
          context,
          runtimeConfig,
          options,
          completion,
        );
        writeBridgeOutput({
          stage: "sliding",
          captchaType: "SLIDING",
          sceneId: completion.sceneId,
          certifyId: completion.certifyId,
          captchaVerifyParam: slidingResult.captchaVerifyParam,
          slidingDeviceToken: slidingResult.deviceToken,
          verifyCode: slidingResult.verifyResult?.verifyCode,
          verifyResult: slidingResult.verifyResult?.verifyResult,
          securityToken: slidingResult.verifyResult?.securityToken,
          verifyCertifyId: slidingResult.verifyResult?.certifyId,
          deviceConfig,
          verifyArgProfile,
          requestCount: requests.length,
          requests: summarizeRequests(),
        });
        if (multiplexSessionId() !== null) {
          return;
        }
        process.exit(0);
      }
      if (completion.mode === "traceless") {
        const tracelessResult = await runTracelessCaptcha(
          context,
          runtimeConfig,
          options,
          completion,
        );
        writeBridgeOutput({
          stage: "traceless",
          captchaType: "TRACELESS",
          sceneId: completion.sceneId,
          certifyId: completion.certifyId,
          captchaVerifyParam: tracelessResult.captchaVerifyParam,
          tracelessDeviceToken: tracelessResult.deviceToken,
          verifyCode: tracelessResult.verifyResult.verifyCode,
          verifyResult: tracelessResult.verifyResult.verifyResult,
          securityToken: tracelessResult.verifyResult.securityToken,
          verifyCertifyId: tracelessResult.verifyResult.certifyId,
          deviceConfig,
          verifyArgProfile,
          requestCount: requests.length,
          requests: summarizeRequests(),
        });
        if (multiplexSessionId() !== null) {
          return;
        }
        process.exit(0);
      }
      getterOwner = selectFeiLinGetterOwner(context);
      if (!getterOwner) {
        throw new Error(
          "challenge-worker 完成时 FeiLin getToken 不可用",
        );
      }
      await replayFeiLinInteractionEvents(
        context,
        completion.interactionEvents,
      );
      if (completion.postInteractionDelayMs > 0) {
        // post delay 已保留在 PE data/getter 相对时间中；这里只维持 getter
        // 前的异步 turn，不再按该逻辑毫秒值真实等待。
        await nextHostTurn();
      }
      const verifyDeviceToken = callPeFeiLinGetter(
        getterOwner,
        completion.getterArguments,
      );
      writeBridgeOutput({
        stage: "verify",
        deviceToken,
        verifyDeviceToken,
        deviceConfig,
        verifyArgProfile,
        tokenSource: "pe-getToken",
        getterArgumentCount: completion.getterArguments.length,
        interactionEventCount: completion.interactionEvents.length,
        requestCount: requests.length,
        requests: summarizeRequests(),
      });
      if (multiplexSessionId() !== null) {
        return;
      }
      process.exit(0);
    }

    let verifyDeviceToken = deviceToken;
    if (getterOwner) {
      try {
        verifyDeviceToken = refreshFeiLinToken(getterOwner);
      } catch {
        // 首个 token 已有效时，第二次采集失败可安全回退到同会话 token。
      }
    }

    if (options.mode === "profile-token") {
      const rGProfiles = context.__ALI_FEILIN_RG_META__ ?? [];
      const jsonProfiles = context.__ALI_FEILIN_JSON_META__ ?? [];
      if (
        (!Array.isArray(rGProfiles) || rGProfiles.length < 1)
        && (!Array.isArray(jsonProfiles) || jsonProfiles.length < 1)
      ) {
        throw new Error(
          "profile-token 未命中 FeiLin 特征探针："
            + JSON.stringify(context.__ALI_FEILIN_SCRIPT_META__ ?? {}),
        );
      }
      fs.writeFileSync(
        1,
        `${JSON.stringify({
          profileCount: {
            rG: rGProfiles.length,
            json: jsonProfiles.length,
          },
          rGProfiles,
          jsonProfiles,
          tokenLengths: {
            init: deviceToken.length,
            verify: verifyDeviceToken.length,
          },
          eventRegistrations: (
            context.__ALI_EVENT_REGISTRATIONS__ ?? []
          ),
          requestCount: requests.length,
          requests: summarizeRequests(),
        })}\n`,
      );
      process.exit(0);
    }

    fs.writeFileSync(
      1,
      `${JSON.stringify({
        deviceToken,
        verifyDeviceToken,
        deviceConfig,
        verifyArgProfile,
        tokenSource,
        requestCount: requests.length,
        requests: summarizeRequests(),
      })}\n`,
    );
    process.exit(0);
  }

  captured.publicConfig = publicConfigSummary(runtimeConfig);
  captured.log1DataPlaintext = log1DataPlaintext;
  captured.log1DataPlaintextAvailable = isNonEmptyString(log1DataPlaintext);
  // 使用同步写入并立即退出，避免探针的伪 XHR 触发 SDK 五秒超时重试。
  fs.writeFileSync(1, `${JSON.stringify(captured)}\n`);
  process.exit(0);
}


export {
  browserRequestHeaders,
  callFeiLinGetter,
  callPeFeiLinGetter,
  decodeDeviceProfile,
  installSdkStateCapture,
  isSdkRuntimeState,
  makeBrowserContext,
  makeElementFactory,
  parseTracelessVerifyResponse,
  parseWorkerCompletionPayload,
  replayFeiLinInteractionEvents,
  refreshFeiLinToken,
  readWorkerCompletionInput,
  runSlidingCaptcha,
  safeDeviceBridgeFailureMessage,
  selectFeiLinGetterOwner,
};


if (
  multiplexSessionId() !== null
  || (
    process.argv[1]
    && import.meta.url === pathToFileURL(process.argv[1]).href
  )
) {
  process.on("uncaughtException", writeFatalError);
  process.on("unhandledRejection", writeFatalError);
  main().catch((error) => {
    writeFatalError(error);
  });
}
