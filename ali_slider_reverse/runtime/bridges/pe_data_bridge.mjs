#!/usr/bin/env node

/**
 * 当前公开动态 PE 的 Verify 参数运行桥。
 *
 * Python 负责 Init、图片识别、轨迹缩放与 HTTP；本桥只在隔离的 Node `vm`
 * 中加载同一轮公开 AliyunCaptcha.js / pe.*.js，构造最小 DOM，向 PE 自己注册
 * 的 touch handler 回放一条轨迹，并截获 `captchaVerifyCallback` 参数。
 *
 * stdin/stdout 只用于单轮 JSON IPC。桥不会调用 Chrome，也不会主动发送 Init、
 * Verify 或业务请求；VM 内的 XHR 和动态 script 网络均保持关闭。
 */

import fs from "node:fs";
import vm from "node:vm";
import { pathToFileURL } from "node:url";

import {
  RUNTIME_PROFILE_IDS,
  makeBrowserContext,
} from "./sdk_device_bridge.mjs";


const IDENTIFIER = "[A-Za-z_$][\\w$]*";
const LOGICAL_VERIFY_FRESHNESS_MARGIN_MS = 750;
const RUNTIME_CALLBACK_TURN_MS = 0;


function safePeBridgeFailureMessage(_error) {
  return "Node PE bridge 执行失败";
}


function writeFatalError(error) {
  fs.writeFileSync(
    2,
    `${JSON.stringify({
      error: safePeBridgeFailureMessage(error),
      name: "Error",
    })}\n`,
  );
  process.exit(1);
}


function parseArguments(argv) {
  const options = {
    sdkPath: "",
    pePath: "",
    prefix: "fsgtmi",
    region: "cn",
    timeoutMs: 20_000,
  };
  for (let index = 2; index < argv.length; index += 1) {
    const argument = argv[index];
    if (argument === "--sdk") {
      options.sdkPath = argv[++index] ?? "";
    } else if (argument === "--pe") {
      options.pePath = argv[++index] ?? "";
    } else if (argument === "--prefix") {
      options.prefix = argv[++index] ?? "";
    } else if (argument === "--region") {
      options.region = argv[++index] ?? "";
    } else if (argument === "--timeout-ms") {
      options.timeoutMs = Number(argv[++index]);
    } else {
      throw new Error(`未知参数：${argument}`);
    }
  }
  for (const [name, value] of [
    ["--sdk", options.sdkPath],
    ["--pe", options.pePath],
  ]) {
    if (!value || !fs.statSync(value).isFile()) {
      throw new Error(`${name} 必须指向本地公开 JS 文件`);
    }
  }
  if (!Number.isInteger(options.timeoutMs) || options.timeoutMs < 1_000) {
    throw new Error("--timeout-ms 必须是至少 1000 的整数");
  }
  return options;
}


function requireText(value, label, maximum = 2048) {
  if (
    typeof value !== "string"
    || value.length < 1
    || value.length > maximum
  ) {
    throw new Error(`${label} 必须是 1..${maximum} 字符字符串`);
  }
  return value;
}


function requirePositiveInteger(value, label, maximum = 10_000) {
  if (
    !Number.isInteger(value)
    || value < 1
    || value > maximum
  ) {
    throw new Error(`${label} 必须是 1..${maximum} 的整数`);
  }
  return value;
}


function readInput() {
  const source = fs.readFileSync(0, "utf8");
  if (source.length > 2_000_000) {
    throw new Error("PE bridge stdin 超过 2MB");
  }
  let input;
  try {
    input = JSON.parse(source);
  } catch {
    throw new Error("PE bridge stdin 不是有效 JSON");
  }
  if (!input || typeof input !== "object" || Array.isArray(input)) {
    throw new Error("PE bridge stdin 顶层必须是 object");
  }

  const track = input.track;
  if (
    !Array.isArray(track)
    || track.length < 3
    || track.length > 512
  ) {
    throw new Error("track 必须包含 3..512 个采样");
  }
  let totalDuration = 0;
  const normalizedTrack = track.map((sample, index) => {
    if (!sample || typeof sample !== "object" || Array.isArray(sample)) {
      throw new Error(`track[${index}] 必须是 object`);
    }
    const x = Number(sample.x);
    const y = Number(sample.y);
    const dt = Number(sample.dt);
    if (
      !Number.isFinite(x)
      || !Number.isFinite(y)
      || !Number.isInteger(dt)
      || dt < 0
      || (index === 0 && dt !== 0)
    ) {
      throw new Error(`track[${index}] 的 x/y/dt 无效`);
    }
    totalDuration += dt;
    const inferredType = index === 0
      ? "touchstart"
      : index === track.length - 1
        ? "touchend"
        : "touchmove";
    const type = sample.type === undefined
      ? inferredType
      : String(sample.type);
    if (
      !["touchstart", "touchmove", "touchend"].includes(type)
      || (index === 0 && type !== "touchstart")
      || (index === track.length - 1 && type !== "touchend")
      || (
        index > 0
        && index < track.length - 1
        && type !== "touchmove"
      )
    ) {
      throw new Error(`track[${index}].type 顺序无效`);
    }
    return { x, y, dt, type };
  });
  if (totalDuration > 60_000) {
    throw new Error("track 总时长不能超过 60 秒");
  }

  const dimensions = input.dimensions ?? {};
  const normalizedDimensions = {
    imageWidth: requirePositiveInteger(
      dimensions.imageWidth,
      "dimensions.imageWidth",
    ),
    imageHeight: requirePositiveInteger(
      dimensions.imageHeight,
      "dimensions.imageHeight",
    ),
    puzzleWidth: requirePositiveInteger(
      dimensions.puzzleWidth,
      "dimensions.puzzleWidth",
    ),
    puzzleHeight: requirePositiveInteger(
      dimensions.puzzleHeight,
      "dimensions.puzzleHeight",
    ),
    renderedWidth: requirePositiveInteger(
      dimensions.renderedWidth ?? 300,
      "dimensions.renderedWidth",
    ),
    handleWidth: requirePositiveInteger(
      dimensions.handleWidth ?? 40,
      "dimensions.handleWidth",
    ),
  };
  if (
    normalizedDimensions.handleWidth
    > normalizedDimensions.renderedWidth
  ) {
    throw new Error(
      "dimensions.handleWidth 不能大于 renderedWidth",
    );
  }
  let expectedXPos = null;
  if (
    input.expectedXPos !== undefined
    && input.expectedXPos !== null
  ) {
    expectedXPos = Number(input.expectedXPos);
    const maximumSlidePos = (
      normalizedDimensions.renderedWidth
      - normalizedDimensions.handleWidth
    );
    const maximumPuzzleX = (
      maximumSlidePos * (3 * maximumSlidePos + 65) / 845
    );
    if (
      !Number.isInteger(expectedXPos)
      || expectedXPos < 0
      || expectedXPos > maximumPuzzleX
    ) {
      throw new Error(
        "expectedXPos 超出当前 PE 可达整数坐标域",
      );
    }
  }

  const argProfile = input.verifyArgProfile;
  if (!argProfile || typeof argProfile !== "object") {
    throw new Error("verifyArgProfile 缺失");
  }
  const accessSec = requireText(
    argProfile.accessSec,
    "verifyArgProfile.accessSec",
    128,
  );
  const sessionIdSalt = requireText(
    argProfile.sessionIdSalt,
    "verifyArgProfile.sessionIdSalt",
    128,
  );
  const initBeginTime = Number(
    input.initBeginTime ?? Date.now(),
  );
  if (
    !Number.isInteger(initBeginTime)
    || initBeginTime < 1
    || initBeginTime > Date.now() + 60_000
  ) {
    throw new Error("initBeginTime 必须是有效 epoch 毫秒");
  }
  const firstTouchAgeMs = requirePositiveInteger(
    Number(input.firstTouchAgeMs),
    "firstTouchAgeMs",
    120_000,
  );

  return {
    sceneId: requireText(input.sceneId, "sceneId", 128),
    certifyId: requireText(input.certifyId, "certifyId", 128),
    deviceToken: requireText(input.deviceToken, "deviceToken", 65_536),
    captchaType: requireText(
      input.captchaType ?? "PUZZLE",
      "captchaType",
      64,
    ),
    image: requireText(input.image ?? "background.png", "image", 4096),
    puzzleImage: requireText(
      input.puzzleImage ?? "puzzle.png",
      "puzzleImage",
      4096,
    ),
    verifyArgProfile: { accessSec, sessionIdSalt },
    deviceConfig: (
      input.deviceConfig
      && typeof input.deviceConfig === "object"
      && !Array.isArray(input.deviceConfig)
    ) ? input.deviceConfig : {},
    dimensions: normalizedDimensions,
    track: normalizedTrack,
    originX: Number.isFinite(Number(input.originX))
      ? Number(input.originX)
      : 94,
    originY: Number.isFinite(Number(input.originY))
      ? Number(input.originY)
      : 548,
    initBeginTime,
    firstTouchAgeMs,
    expectedXPos,
  };
}


function makeClassList(element) {
  const values = () => new Set(
    String(element.className ?? "")
      .split(/\s+/)
      .filter(Boolean),
  );
  const commit = (items) => {
    element.className = [...items].join(" ");
  };
  return {
    add(...names) {
      const items = values();
      for (const name of names) items.add(String(name));
      commit(items);
    },
    remove(...names) {
      const items = values();
      for (const name of names) items.delete(String(name));
      commit(items);
    },
    contains(name) {
      return values().has(String(name));
    },
    toggle(name, force) {
      const items = values();
      const normalized = String(name);
      const enabled = force === undefined
        ? !items.has(normalized)
        : Boolean(force);
      if (enabled) items.add(normalized);
      else items.delete(normalized);
      commit(items);
      return enabled;
    },
  };
}


function installCaptchaDom(context, dimensions) {
  const byId = new Map();
  const allElements = new Set([
    context.document.documentElement,
    context.document.head,
    context.document.body,
  ]);
  const listeners = new WeakMap();
  const originalCreateElement = context.document.createElement.bind(
    context.document,
  );

  function eventBucket(target) {
    let bucket = listeners.get(target);
    if (!bucket) {
      bucket = new Map();
      listeners.set(target, bucket);
    }
    return bucket;
  }

  function installEventTarget(target) {
    const bucket = eventBucket(target);
    target.addEventListener = (type, callback) => {
      if (typeof callback !== "function") return;
      const name = String(type);
      if (!bucket.has(name)) bucket.set(name, []);
      bucket.get(name).push(callback);
    };
    target.removeEventListener = (type, callback) => {
      const callbacks = bucket.get(String(type));
      if (!callbacks) return;
      const index = callbacks.indexOf(callback);
      if (index >= 0) callbacks.splice(index, 1);
    };
    target.dispatchEvent = (event) => {
      if (!event || typeof event !== "object") {
        throw new TypeError("dispatchEvent 需要 event object");
      }
      if (!event.type) {
        throw new TypeError("event.type 不能为空");
      }
      if (!("target" in event)) event.target = target;
      event.currentTarget = target;
      const propertyHandler = target[`on${event.type}`];
      if (typeof propertyHandler === "function") {
        propertyHandler.call(target, event);
      }
      for (const callback of [...(bucket.get(String(event.type)) ?? [])]) {
        callback.call(target, event);
      }
      return !event.defaultPrevented;
    };
    return target;
  }

  function selectorElements(selector) {
    const text = String(selector).trim();
    if (text === "head") return [context.document.head];
    if (text === "body") return [context.document.body];
    if (text === "html") return [context.document.documentElement];

    const idMatches = [...text.matchAll(/#([\w-]+)/g)];
    if (idMatches.length) {
      const value = byId.get(idMatches.at(-1)[1]);
      return value ? [value] : [];
    }
    const classMatches = [...text.matchAll(/\.([\w-]+)/g)];
    if (classMatches.length) {
      const className = classMatches.at(-1)[1];
      return [...allElements].filter(
        (element) => element.classList?.contains(className),
      );
    }
    return [];
  }

  function layoutFor(id, tagName) {
    if (tagName === "HTML" || tagName === "BODY") {
      return { width: 430, height: 932 };
    }
    if (tagName === "HEAD") {
      return { width: 430, height: 0 };
    }
    if (id === "aliyunCaptcha-img") {
      return {
        width: dimensions.renderedWidth,
        height: dimensions.imageHeight,
        naturalWidth: dimensions.imageWidth,
        naturalHeight: dimensions.imageHeight,
      };
    }
    if (id === "aliyunCaptcha-puzzle") {
      return {
        width: dimensions.puzzleWidth,
        height: dimensions.puzzleHeight,
        naturalWidth: dimensions.puzzleWidth,
        naturalHeight: dimensions.puzzleHeight,
      };
    }
    if (
      id === "aliyunCaptcha-img-box"
      || id === "aliyunCaptcha-sliding-body"
    ) {
      return {
        width: dimensions.renderedWidth,
        height: id === "aliyunCaptcha-img-box"
          ? dimensions.imageHeight
          : 40,
      };
    }
    if (id === "aliyunCaptcha-sliding-slider") {
      return { width: dimensions.handleWidth, height: 40 };
    }
    if (tagName === "IMG") {
      return {
        width: dimensions.renderedWidth,
        height: dimensions.imageHeight,
        naturalWidth: dimensions.imageWidth,
        naturalHeight: dimensions.imageHeight,
      };
    }
    return { width: 100, height: 20 };
  }

  function enhanceElement(element, {
    id = "",
    className = "",
  } = {}) {
    if (element.__aliPeEnhanced) {
      if (id) {
        element.id = id;
        byId.set(id, element);
      }
      if (className) element.className = className;
      return element;
    }
    Object.defineProperty(element, "__aliPeEnhanced", {
      value: true,
      enumerable: false,
    });
    installEventTarget(element);
    element.id = id;
    element.className = className;
    element.classList = makeClassList(element);
    element.ownerDocument = context.document;
    element.closest = (selector) => (
      selectorElements(selector).includes(element) ? element : null
    );
    element.querySelector = (selector) => selectorElements(selector)[0] ?? null;
    element.querySelectorAll = (selector) => selectorElements(selector);
    element.insertAdjacentHTML = (_position, html) => registerHtml(html);

    const layout = layoutFor(id, element.tagName);
    for (const [name, value] of Object.entries(layout)) {
      element[name] = value;
    }
    Object.defineProperties(element, {
      offsetLeft: {
        configurable: true,
        get() {
          return Number.parseFloat(this.style.left) || 0;
        },
      },
      offsetTop: {
        configurable: true,
        get() {
          return Number.parseFloat(this.style.top) || 0;
        },
      },
      offsetWidth: {
        configurable: true,
        get() {
          return Number.parseFloat(this.style.width) || layout.width;
        },
      },
      offsetHeight: {
        configurable: true,
        get() {
          return Number.parseFloat(this.style.height) || layout.height;
        },
      },
      clientWidth: {
        configurable: true,
        get() {
          return Number.parseFloat(this.style.width) || layout.width;
        },
      },
      clientHeight: {
        configurable: true,
        get() {
          return Number.parseFloat(this.style.height) || layout.height;
        },
      },
    });
    element.getBoundingClientRect = () => {
      const left = Number.parseFloat(element.style.left) || 0;
      const top = Number.parseFloat(element.style.top) || 0;
      const width = element.offsetWidth;
      const height = element.offsetHeight;
      return {
        x: left,
        y: top,
        left,
        top,
        right: left + width,
        bottom: top + height,
        width,
        height,
      };
    };

    if (element.tagName === "IMG") {
      let source = "";
      Object.defineProperty(element, "src", {
        configurable: true,
        get() {
          return source;
        },
        set(value) {
          source = String(value);
          element.currentSrc = source;
          element.complete = true;
          context.setTimeout(() => {
            if (typeof element.onload === "function") {
              element.onload.call(element);
            }
          }, 0);
        },
      });
    }
    if (id) byId.set(id, element);
    allElements.add(element);
    return element;
  }

  function registerHtml(html) {
    const source = String(html);
    const expression = /<([a-zA-Z][\w-]*)([^>]*)>/g;
    for (const match of source.matchAll(expression)) {
      const tagName = match[1];
      const attributes = match[2];
      const id = attributes.match(/\bid=["']([^"']+)["']/)?.[1] ?? "";
      const className = (
        attributes.match(/\bclass=["']([^"']+)["']/)?.[1] ?? ""
      );
      if (!id && !className) continue;
      const element = enhanceElement(
        originalCreateElement(tagName),
        { id, className },
      );
      context.document.body.appendChild(element);
    }
  }

  installEventTarget(context.document);
  enhanceElement(context.document.documentElement);
  enhanceElement(context.document.head);
  enhanceElement(context.document.body);
  context.document.createElement = (tagName) => enhanceElement(
    originalCreateElement(tagName),
  );
  context.document.getElementById = (id) => byId.get(String(id)) ?? null;
  context.document.querySelector = (selector) => (
    selectorElements(selector)[0] ?? null
  );
  context.document.querySelectorAll = (selector) => (
    selectorElements(selector)
  );
  context.document.body.insertAdjacentHTML = (_position, html) => {
    registerHtml(html);
  };

  return {
    byId,
    listenerCount(target, type) {
      return eventBucket(target).get(String(type))?.length ?? 0;
    },
  };
}


function exposePuzzleConstructor(source) {
  const constructorPattern = new RegExp(
    `default:this\\.CaptchaConstructor=(${IDENTIFIER})}`,
  );
  const constructorMatch = source.match(constructorPattern);
  if (!constructorMatch) {
    throw new Error("当前 PE 未找到 Puzzle CaptchaConstructor");
  }
  const assignmentPattern = new RegExp(
    `(${IDENTIFIER})\\.AliyunCaptcha=(${IDENTIFIER})`,
    "g",
  );
  const assignments = [...source.matchAll(assignmentPattern)];
  const assignment = assignments.at(-1);
  if (!assignment) {
    throw new Error("当前 PE 未找到 window.AliyunCaptcha 导出点");
  }
  const original = assignment[0];
  const globalName = assignment[1];
  const replacement = (
    `${globalName}.__ALI_PE_PUZZLE_CTOR__=${constructorMatch[1]},`
    + original
  );
  let exposed = source.slice(0, assignment.index)
    + replacement
    + source.slice(assignment.index + original.length);
  return exposed;
}


function makeTouchEvent(context, type, x, y) {
  const point = {
    identifier: 0,
    clientX: x,
    clientY: y,
    pageX: x,
    pageY: y,
    screenX: x,
    screenY: y,
    radiusX: 1,
    radiusY: 1,
    rotationAngle: 0,
    force: type === "touchend" ? 0 : 0.5,
    target: null,
  };
  return {
    type,
    bubbles: true,
    cancelable: true,
    composed: true,
    isTrusted: true,
    timeStamp: context.performance.now(),
    touches: type === "touchend" ? [] : [point],
    targetTouches: type === "touchend" ? [] : [point],
    changedTouches: [point],
    defaultPrevented: false,
    preventDefault() {
      if (this.cancelable) this.defaultPrevented = true;
    },
    stopPropagation() {},
    stopImmediatePropagation() {},
  };
}


function wait(delayMs) {
  return new Promise((resolve) => setTimeout(resolve, delayMs));
}


// 跨过一个完整的宿主事件循环轮次（timers → poll → check）：已到期的 timer 与
// 排队的 microtask 照常执行，但不付 Node 把 setTimeout(0) 抬到 1ms 的固定代价。
function nextHostTurn() {
  return new Promise((resolve) => setImmediate(resolve));
}


function alignLogicalReplayInput(input, hostNowMs) {
  if (!Number.isInteger(hostNowMs) || hostNowMs < 1) {
    throw new Error("hostNowMs 必须是有效 epoch 毫秒");
  }
  const trackDurationMs = input.track.reduce(
    (total, sample) => total + sample.dt,
    0,
  );
  const latestFreshEpochMs = (
    hostNowMs
    - input.firstTouchAgeMs
    - trackDurationMs
    - LOGICAL_VERIFY_FRESHNESS_MARGIN_MS
  );
  if (!Number.isInteger(latestFreshEpochMs) || latestFreshEpochMs < 1) {
    throw new Error("无法建立有效的 PE 逻辑 epoch");
  }
  return {
    ...input,
    // 完整保留首触摸年龄与拖动时长，同时把逻辑终点放在真实发送时刻之前。
    // 否则 36 秒逻辑年龄在 1～3 秒墙钟内回放会生成未来 VerifyTime。
    initBeginTime: Math.min(
      input.initBeginTime,
      latestFreshEpochMs,
    ),
  };
}


function makeLogicalClock(epochMs) {
  if (!Number.isInteger(epochMs) || epochMs < 1) {
    throw new Error("逻辑时钟 epoch 必须是正整数");
  }
  let elapsedMs = 0;
  return Object.freeze({
    epochMs,
    nowEpochMs() {
      return epochMs + elapsedMs;
    },
    nowPerformanceMs() {
      return elapsedMs;
    },
    advanceTo(targetElapsedMs) {
      if (
        !Number.isFinite(targetElapsedMs)
        || targetElapsedMs < elapsedMs
      ) {
        throw new Error("PE 逻辑时钟不得倒退");
      }
      elapsedMs = targetElapsedMs;
      return elapsedMs;
    },
  });
}


async function waitForSlider(dom, timeoutMs) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const slider = dom.byId.get("aliyunCaptcha-sliding-slider");
    if (
      slider
      && (
        dom.listenerCount(slider, "touchstart") > 0
        || typeof slider.ontouchstart === "function"
      )
    ) {
      return slider;
    }
    await wait(10);
  }
  throw new Error("PE 未在超时内注册 slider touchstart");
}


async function replayTrack(
  context,
  dom,
  input,
  logicalClock,
  hasPendingAnimationFrame = () => true,
) {
  const slider = await waitForSlider(dom, 3_000);
  logicalClock.advanceTo(input.firstTouchAgeMs);
  const actualFirstTouchAgeMs = context.performance.now();
  const dispatchLatenessMs = (
    actualFirstTouchAgeMs - input.firstTouchAgeMs
  );
  if (Math.abs(dispatchLatenessMs) > 250) {
    throw new Error(
      "PE 首 touch 错过目标时钟："
      + `${dispatchLatenessMs.toFixed(3)}ms`,
    );
  }
  let scheduledElapsedMs = 0;
  for (let index = 0; index < input.track.length; index += 1) {
    const sample = input.track[index];
    scheduledElapsedMs += sample.dt;
    logicalClock.advanceTo(
      input.firstTouchAgeMs + scheduledElapsedMs,
    );
    const event = makeTouchEvent(
      context,
      sample.type,
      input.originX + sample.x,
      input.originY + sample.y,
    );
    event.changedTouches[0].target = slider;
    if (event.touches[0]) event.touches[0].target = slider;
    if (event.targetTouches[0]) event.targetTouches[0].target = slider;
    if (sample.type === "touchstart") {
      slider.dispatchEvent(event);
      // TouchEvent 在真实 DOM 中会从 slider 冒泡到 document；内部拖动处理器
      // 与全局 TrackList collector 分别依赖这两个阶段。
      context.document.dispatchEvent(event);
    } else {
      context.document.dispatchEvent(event);
    }
    // 每条事件后跨一个真实 macrotask，让 dispatch 期间同步排入的 host callback
    // 在下一事件前执行；不按逻辑 dt 真实睡眠。
    //
    // 这个 turn 有多长是有讲究的。PE 的 requestAnimationFrame 由真实
    // setTimeout(cb, 1) 驱动，一条回调自我续注册的链需要真实时间才能推进；实测
    // 这条链约 11 环，在旧的 1ms turn 下正好在拖拽前段跑完。若整段回放压到
    // setImmediate 的量级，链就跑不完，TrackList 会少掉子流并被 Python 侧的字段
    // 校验拦下——这是实测过的失败模式，不是假想。
    //
    // 因此按"链是否还活着"分流：还有待触发帧就付真实 1ms 让它按自己的节奏走完，
    // 之后的事件只跨一个宿主 turn。回放期间 PE 不注册任何非 RAF 宿主定时器
    // （全在 construct/init 阶段），所以快路径上没有会被饿死的回调。
    await (
      hasPendingAnimationFrame()
        ? wait(RUNTIME_CALLBACK_TURN_MS)
        : nextHostTurn()
    );
  }
  return {
    targetFirstTouchAgeMs: input.firstTouchAgeMs,
    actualFirstTouchAgeMs,
    dispatchLatenessMs,
  };
}


function normalizeVerifyParam(callbackValue, input) {
  if (typeof callbackValue === "string" && callbackValue) {
    try {
      const parsed = JSON.parse(callbackValue);
      if (
        parsed
        && typeof parsed === "object"
        && !Array.isArray(parsed)
        && typeof parsed.data === "string"
      ) {
        return parsed;
      }
    } catch {
      // 当前 PE 也可能只把 opaque data 字符串交给回调。
    }
    return {
      sceneId: input.sceneId,
      certifyId: input.certifyId,
      deviceToken: input.deviceToken,
      data: callbackValue,
    };
  }
  return callbackValue;
}


function installDeviceGetterProbe(context, input) {
  const calls = [];
  const makeOwner = (owner) => Object.freeze({
    getToken(...args) {
      calls.push({
        owner,
        observedAtMs: context.performance.now(),
        argumentCount: args.length,
        arguments: args.map((value) => ({
          type: value === null
            ? "null"
            : Array.isArray(value)
              ? "array"
              : typeof value,
          length: (
            typeof value === "string"
            || Array.isArray(value)
          ) ? value.length : null,
          value: (
            typeof value === "string"
            && value.length <= 512
          ) ? value : null,
          equalsCertifyId: value === input.certifyId,
          equalsSceneId: value === input.sceneId,
        })),
      });
      return input.deviceToken;
    },
  });
  for (const [name, owner] of [
    ["z_um", makeOwner("z_um")],
    ["um", makeOwner("um")],
  ]) {
    Object.defineProperty(context, name, {
      configurable: true,
      enumerable: true,
      value: owner,
      writable: false,
    });
  }
  return calls;
}


function outputResult(
  callbackValue,
  input,
  payloadMeta,
  replayMeta,
  deviceGetterCalls,
) {
  const verifyParam = normalizeVerifyParam(callbackValue, input);
  if (
    !verifyParam
    || typeof verifyParam !== "object"
    || Array.isArray(verifyParam)
  ) {
    throw new Error("PE captchaVerifyCallback 参数不是 object");
  }
  const data = verifyParam.data;
  if (typeof data !== "string" || !data) {
    throw new Error("PE captchaVerifyCallback 参数缺少 data");
  }
  const expectedKeys = ["sceneId", "certifyId", "deviceToken", "data"];
  const actualKeys = Object.keys(verifyParam);
  if (
    actualKeys.length !== expectedKeys.length
    || actualKeys.some((key, index) => key !== expectedKeys[index])
  ) {
    throw new Error("PE captchaVerifyCallback 参数字段顺序异常");
  }
  if (
    verifyParam.sceneId !== input.sceneId
    || verifyParam.certifyId !== input.certifyId
  ) {
    throw new Error(
      "PE captchaVerifyCallback 会话字段与输入不一致："
      + JSON.stringify({
        keys: Object.keys(verifyParam),
        sceneId: verifyParam.sceneId === input.sceneId,
        certifyId: verifyParam.certifyId === input.certifyId,
        types: {
          sceneId: typeof verifyParam.sceneId,
          certifyId: typeof verifyParam.certifyId,
          deviceToken: typeof verifyParam.deviceToken,
          data: typeof verifyParam.data,
        },
      }),
    );
  }
  if (
    typeof verifyParam.deviceToken !== "string"
    || !verifyParam.deviceToken
  ) {
    throw new Error("PE captchaVerifyCallback 返回了空 deviceToken");
  }
  const nativeDeviceTokenMatched = (
    verifyParam.deviceToken === input.deviceToken
  );
  // PE 的独立 VM 会从自身的公开 SDK runtime 生成一枚本地 token。轨迹 data
  // 原文不含该字段；最终协议外层必须继续使用 Log1/Log2 同一 session 的 Verify
  // token，因此这里只保留 PE 原生 data，并以调用方会话字段规范化外层对象。
  const normalizedVerifyParam = {
    sceneId: input.sceneId,
    certifyId: input.certifyId,
    deviceToken: input.deviceToken,
    data: verifyParam.data,
  };
  if (!payloadMeta || typeof payloadMeta !== "object") {
    throw new Error("未观测到动态 PE 的原始 payload");
  }
  if (
    input.expectedXPos !== null
    && Math.abs(
      Number(payloadMeta.xPos) - input.expectedXPos,
    ) > 1
  ) {
    throw new Error(
      `动态 PE xPos 与图片结果不一致：`
      + `${String(payloadMeta.xPos)} != ${input.expectedXPos}`,
    );
  }
  const normalizedPayloadMeta = {
    ...payloadMeta,
    ...replayMeta,
  };
  fs.writeFileSync(
    1,
    `${JSON.stringify({
      verifyParam: normalizedVerifyParam,
      trackEventCount: input.track.length,
      nativeDeviceTokenMatched,
      deviceGetterCalls,
      payloadMeta: normalizedPayloadMeta,
    })}\n`,
  );
}


function makePeBrowserContext(options) {
  return makeBrowserContext(
    {
      prefix: options.prefix,
      region: options.region,
      timeoutMs: options.timeoutMs,
      networkEnabled: false,
      // FeiLin 的 compact profile 是命名实验，不得经共享 factory
      // 静默污染动态 PE 所见的原 captured-mobile 环境。
      runtimeProfile: RUNTIME_PROFILE_IDS.CAPTURED_MOBILE,
    },
    () => {},
  );
}


async function main() {
  const options = parseArguments(process.argv);
  const HostDate = globalThis.Date;
  const input = alignLogicalReplayInput(
    readInput(),
    HostDate.now(),
  );
  const context = makePeBrowserContext(options);
  const hostPerformance = globalThis.performance;
  const logicalClock = makeLogicalClock(input.initBeginTime);
  let animationClockOverride = null;
  const virtualNow = () => (
    animationClockOverride !== null
      ? input.initBeginTime + animationClockOverride
      : logicalClock.nowEpochMs()
  );
  function VirtualDate(...arguments_) {
    if (new.target) {
      return new HostDate(
        ...(arguments_.length ? arguments_ : [virtualNow()]),
      );
    }
    return new HostDate(virtualNow()).toString();
  }
  Object.setPrototypeOf(VirtualDate, HostDate);
  VirtualDate.prototype = HostDate.prototype;
  VirtualDate.now = virtualNow;
  context.Date = VirtualDate;
  const shiftedPerformance = new Proxy(hostPerformance, {
    get(target, property) {
      if (property === "now") {
        return () => (
          animationClockOverride !== null
            ? animationClockOverride
            : logicalClock.nowPerformanceMs()
        );
      }
      if (property === "timeOrigin") {
        return input.initBeginTime;
      }
      const value = Reflect.get(target, property, target);
      return typeof value === "function" ? value.bind(target) : value;
    },
  });
  context.performance = shiftedPerformance;
  let animationTimestamp = null;
  // 待触发的 RAF 帧数。回放循环据此决定这一条事件后要不要付真实 1ms——
  // 见 replayTrack 里的说明。
  let pendingAnimationFrames = 0;
  context.requestAnimationFrame = (callback) => {
    pendingAnimationFrames += 1;
    return context.setTimeout(
      () => {
        pendingAnimationFrames -= 1;
        animationTimestamp = animationTimestamp === null
          ? context.performance.now()
          : animationTimestamp + (1000 / 120);
        animationClockOverride = animationTimestamp;
        try {
          callback(animationTimestamp);
        } finally {
          animationClockOverride = null;
        }
      },
      1,
    );
  };
  context.cancelAnimationFrame = (identifier) => {
    if (pendingAnimationFrames > 0) pendingAnimationFrames -= 1;
    context.clearTimeout(identifier);
  };
  context.NodeList = class NodeList extends Array {};
  context.HTMLCollection = class HTMLCollection extends Array {};
  const dom = installCaptchaDom(context, input.dimensions);

  const sdkProbeMarker = "if(window.AliyunCaptchaConfig&&";
  let sdkSource = fs.readFileSync(options.sdkPath, "utf8");
  if (!sdkSource.includes(sdkProbeMarker)) {
    throw new Error("公开 SDK 结构变化：未找到运行配置导出点");
  }
  sdkSource = sdkSource.replace(
    sdkProbeMarker,
    `window.__ALI_PE_SDK_RUNTIME__=nr;${sdkProbeMarker}`,
  );
  vm.runInContext(sdkSource, context, {
    filename: options.sdkPath,
    timeout: Math.min(options.timeoutMs, 10_000),
  });
  vm.runInContext(
    `(() => {
      const nativeStringify = JSON.stringify;
      window.__ALI_PE_PAYLOAD_META__ = null;
      JSON.stringify = function(value, ...rest) {
        if (value && typeof value === "object" && !Array.isArray(value)) {
          const keys = Object.keys(value);
          if (
            keys.includes("TrackList")
            && keys.includes("xPos")
            && keys.includes("slidePos")
            && keys.includes("arg")
          ) {
            const track = value.TrackList;
            window.__ALI_PE_PAYLOAD_META__ = {
              keys,
              valueTypes: Object.fromEntries(
                keys.map((key) => [key, typeof value[key]]),
              ),
              xPos: value.xPos,
              slidePos: value.slidePos,
              trackStartTime: value.TrackStartTime,
              verifyTime: value.VerifyTime,
              trackKeys: (
                track && typeof track === "object" && !Array.isArray(track)
              ) ? Object.keys(track) : [],
              trackValueTypes: (
                track && typeof track === "object" && !Array.isArray(track)
              ) ? Object.fromEntries(
                Object.keys(track).map((key) => [key, typeof track[key]]),
              ) : {},
            };
          }
        }
        return nativeStringify.call(this, value, ...rest);
      };
    })();`,
    context,
    {
      filename: "ali-pe-payload-probe.js",
      timeout: 1_000,
    },
  );
  const peSource = exposePuzzleConstructor(
    fs.readFileSync(options.pePath, "utf8"),
  );
  vm.runInContext(peSource, context, {
    filename: options.pePath,
    timeout: Math.min(options.timeoutMs, 10_000),
  });
  if (typeof context.__ALI_PE_PUZZLE_CTOR__ !== "function") {
    throw new Error("当前 PE 没有暴露 Puzzle constructor");
  }
  const sdkRuntime = context.__ALI_PE_SDK_RUNTIME__;
  if (!sdkRuntime || typeof sdkRuntime !== "object") {
    throw new Error("公开 SDK 没有导出动态 PE 运行配置");
  }
  const runtimeDeviceConfig = {};
  const runtimeChain = [];
  for (
    let current = sdkRuntime;
    current && current !== Object.prototype;
    current = Object.getPrototypeOf(current)
  ) {
    runtimeChain.unshift(current);
  }
  for (const current of runtimeChain) {
    for (const name of Object.getOwnPropertyNames(current)) {
      if (name === "constructor") continue;
      runtimeDeviceConfig[name] = current[name];
    }
  }
  // 真实 SDK loader 传给动态 PE 的 `deviceConfig` 是完整的 nr 运行时对象；
  // Log1 解密出的飞麟配置只挂在 nr.deviceConfig 下，而不是直接作为该参数。
  runtimeDeviceConfig.deviceConfig = input.deviceConfig;
  runtimeDeviceConfig.DeviceToken = input.deviceToken;

  let resolveVerify;
  let rejectVerify;
  const verifyPromise = new Promise((resolve, reject) => {
    resolveVerify = resolve;
    rejectVerify = reject;
  });
  const configTarget = Object.assign({}, runtimeDeviceConfig, {
    SceneId: input.sceneId,
    CertifyId: input.certifyId,
    DeviceToken: input.deviceToken,
    CaptchaType: input.captchaType,
    Image: input.image,
    PuzzleImage: input.puzzleImage,
    PowVerifyString: "",
    verifyType: "2.0",
    mode: "popup",
    language: "cn",
    upLang: "cn",
    isFromTraceless: false,
    showSecondChallenge: false,
    immediate: true,
    initBeginTime: input.initBeginTime,
    initialRequestTime: input.initBeginTime,
    rem: 1,
    imgServer: "",
    captchaJsPath(value) {
      return String(value);
    },
    captchaCssPath(value) {
      return String(value);
    },
    ACCESS_SEC: input.verifyArgProfile.accessSec,
    SESSION_ID_SALT: input.verifyArgProfile.sessionIdSalt,
    ERR: {
      VERIFY_FAIL: "VERIFY_FAIL",
      REFRESH_FAIL: "REFRESH_FAIL",
    },
    log() {},
    fallbackCb(error) {
      rejectVerify(
        new Error(safePeBridgeFailureMessage(error)),
      );
    },
    getInstance() {},
    _extend(value) {
      Object.assign(this, value);
    },
  });
  const config = new Proxy(configTarget, {
    get(target, property, receiver) {
      const value = Reflect.get(target, property, receiver);
      if (
        value === undefined
        && typeof property === "string"
      ) {
        throw new Error(`动态 PE 读取了未提供的 config.${property}`);
      }
      return value;
    },
  });
  const parent = {
    config,
    deviceConfig: runtimeDeviceConfig,
    captchaVerifyCallback(verifyParam) {
      resolveVerify(verifyParam);
      return Promise.resolve(undefined);
    },
    loading() {},
    onBizSuccess() {},
    onBizFail() {},
    initPopup(html) {
      context.document.body.insertAdjacentHTML("beforeend", html);
    },
    initEmbed(html) {
      context.document.body.insertAdjacentHTML("beforeend", html);
    },
    initFloat(html) {
      context.document.body.insertAdjacentHTML("beforeend", html);
    },
  };
  // 真实 SDK loader 会在实例化动态 PE 前把本轮运行态挂到公开外层构造器
  // prototype；混淆分片中的 arg / device log 路径会直接从这里取值。
  Object.assign(context.AliyunCaptcha.prototype, parent);
  Object.assign(context.AliyunCaptchaConfig, {
    ACCESS_SEC: input.verifyArgProfile.accessSec,
    SESSION_ID_SALT: input.verifyArgProfile.sessionIdSalt,
    deviceConfig: runtimeDeviceConfig,
  });

  const deviceGetterCalls = installDeviceGetterProbe(context, input);
  // Constructor 会走当前 PE 的真实 init / initDom / bindEvents。
  new context.__ALI_PE_PUZZLE_CTOR__({
    AliyunCaptcha: parent,
    mode: "popup",
    initialData: config,
  });

  const timeoutPromise = new Promise((_, reject) => {
    setTimeout(
      () => reject(new Error("等待 PE captchaVerifyCallback 超时")),
      options.timeoutMs,
    );
  });
  const replayMeta = await replayTrack(
    context,
    dom,
    input,
    logicalClock,
    () => pendingAnimationFrames > 0,
  );
  const verifyParam = await Promise.race([
    verifyPromise,
    timeoutPromise,
  ]);
  outputResult(
    verifyParam,
    input,
    context.__ALI_PE_PAYLOAD_META__,
    replayMeta,
    deviceGetterCalls,
  );
  process.exit(0);
}


export {
  alignLogicalReplayInput,
  exposePuzzleConstructor,
  installDeviceGetterProbe,
  installCaptchaDom,
  makeLogicalClock,
  makePeBrowserContext,
  replayTrack,
  safePeBridgeFailureMessage,
};


if (
  process.argv[1]
  && import.meta.url === pathToFileURL(process.argv[1]).href
) {
  process.on("uncaughtException", writeFatalError);
  process.on("unhandledRejection", writeFatalError);
  main().catch(writeFatalError);
}
