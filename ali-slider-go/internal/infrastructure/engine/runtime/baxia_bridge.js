/* 独立 Fireye 会话；浏览器环境和宿主均禁用网络。 */
(() => {
  "use strict";

  const { vm } = globalThis.__aliNodeCompat;
  const { makeBrowserContext } = globalThis.__aliSdkModule;
  let session = null;

  globalThis.__aliV8BaxiaBrowser = (input) => {
    const browser = makeBrowserContext({
      deviceProfile: input.deviceProfile,
      networkEnabled: false,
      timeoutMs: input.timeoutMs,
      prefix: "",
      region: input.uaOptions.location,
    }, () => {});

    const page = new URL(input.pageURL);
    for (const name of [
      "protocol", "host", "hostname", "port", "origin",
      "pathname", "search", "hash", "href",
    ]) browser.location[name] = page[name];
    Object.assign(browser.document, {
      URL: page.href,
      baseURI: page.href,
      domain: page.hostname,
      // 会话表示直接进入 PageURL；没有前一页面可作为 referrer。
      referrer: "",
      location: browser.location,
    });
    browser.origin = page.origin;

    // 目标页面 Chrome 153 实测的 canPlayType 能力；未观测到的格式保持不支持。
    const mediaTypes = new Set([
      "audio/mpeg", 'audio/ogg; codecs="vorbis"', 'audio/wav; codecs="1"',
      'audio/webm; codecs="opus"', "audio/aac", "audio/flac",
      'video/mp4; codecs="avc1.42E01E"', 'video/webm; codecs="vp8, vorbis"',
      'video/webm; codecs="vp9"', 'video/mp4; codecs="mp4a.40.2"',
    ]);
    const createElement = browser.document.createElement.bind(browser.document);
    browser.document.createElement = (tagName) => {
      const element = createElement(tagName);
      if (["audio", "video"].includes(String(tagName).toLowerCase())) {
        element.canPlayType = (type) => mediaTypes.has(String(type)) ? "probably" : "";
      }
      return element;
    };

    // Fireye 加载阶段使用 postMessage 驱动消息事件；回调必须异步并保留
    // 同一 realm 的 window 身份，不能用同步回调代替浏览器消息队列。
    vm.runInContext(`
      window.postMessage = function postMessage(data, targetOrigin = "/") {
        if (targetOrigin !== "*" && targetOrigin !== "/" && targetOrigin !== location.origin) return;
        setTimeout(() => {
          const event = new Event("message");
          Object.assign(event, { data, origin: location.origin, source: window });
          dispatchEvent(event);
        }, 0);
      };
    `, browser, { filename: "baxia-browser.js", timeout: input.timeoutMs });

    // AWSC.configFYEx 在加载 SDK 之前暴露该对象，init/getter 继续复用它。
    browser.fyglobalopt = { ...input.uaOptions };
    browser.__ALI_BAXIA_OPTIONS__ = browser.fyglobalopt;
    return browser;
  };

  globalThis.__aliV8BaxiaOpen = async (input) => {
    if (session !== null) throw new Error("BAXIA_ALREADY_OPEN");
    const browser = globalThis.__aliV8BaxiaBrowser(input);

    // 字典可能由 packed 字符串池在运行时解出。SDK 加载前包装原生方法，
    // 以覆盖其缓存 charAt 的情形；仅 getter 取样期间记录，随后恢复原方法。
    // 包装保留原始计算结果，不修改 SDK 源码、混淆函数或任何索引。
    vm.runInContext(`
      (() => {
        const prototype = "".constructor.prototype;
        const descriptor = Object.getOwnPropertyDescriptor(prototype, "charAt");
        const known = new Set();
        const observations = new Map();
        let active = false;
        let count = 0;
        function alphabetOf(value) {
          const normalized = value.length === 64 ? value + "=" : value;
          if (known.has(normalized)) return normalized;
          if (normalized.length !== 65 || normalized[64] !== "="
            || !/^[A-Za-z0-9+/]{64}=$/.test(normalized)
            || new Set(normalized).size !== 65) return null;
          if (known.size >= 32) throw new Error("BAXIA_SDK_CONTRACT: too many alphabet candidates");
          known.add(normalized);
          return normalized;
        }
        const hooked = new Proxy(descriptor.value, {
          apply(original, receiver, args) {
            const character = Reflect.apply(original, receiver, args);
            if (active && typeof receiver === "string" && (receiver.length === 64 || receiver.length === 65)) {
              const alphabet = alphabetOf(receiver);
              if (alphabet !== null) {
                if (++count > 262144) throw new Error("BAXIA_SDK_CONTRACT: alphabet trace exceeds its limit");
                if (!observations.has(alphabet)) observations.set(alphabet, []);
                observations.get(alphabet).push(character);
              }
            }
            return character;
          },
        });
        Object.defineProperty(prototype, "charAt", { ...descriptor, value: hooked });
        globalThis.__ALI_BAXIA_TRACE__ = {
          begin() { active = true; },
          finish() {
            active = false;
            return [...observations].map(([alphabet, characters]) => ({ alphabet, characters: characters.join("") }));
          },
          restore() {
            active = false;
            Object.defineProperty(prototype, "charAt", descriptor);
          },
        };
      })();
    `, browser, { filename: "baxia-profile-trace.js", timeout: input.timeoutMs });

    vm.runInContext(input.sdkSource, browser, {
      filename: "fireye-sdk.js",
      timeout: input.timeoutMs,
    });
    const module = browser.__fyModule;
    if (!module || typeof module.init !== "function"
      || typeof module.getVersion !== "function"
      || typeof module.getFYToken !== "function") {
      throw new Error("BAXIA_SDK_CONTRACT: missing init/getVersion/getFYToken API");
    }
    const version = vm.runInContext("__fyModule.getVersion()", browser, {
      filename: "baxia-version.js", timeout: input.timeoutMs,
    });
    if (!Number.isSafeInteger(version) || version <= 0) {
      throw new Error("BAXIA_SDK_CONTRACT: getVersion must return a positive integer");
    }

    // baxiaCommon 持续复用这一对象，每次 getFYToken 前只更新 reqUrl。
    await new Promise((resolve, reject) => {
      let finished = false;
      const finish = (error) => {
        if (finished) return;
        finished = true;
        clearTimeout(deadline);
        delete browser.__ALI_BAXIA_READY__;
        if (error) reject(error);
        else resolve();
      };
      const deadline = setTimeout(() => {
        finish(new Error("BAXIA_INIT_TIMEOUT"));
      }, input.timeoutMs);
      browser.__ALI_BAXIA_READY__ = (status) => {
        finish(status === "initialized" ? null : new Error(
          status === "timeout" ? "BAXIA_INIT_TIMEOUT" : "BAXIA_INIT_FAILED",
        ));
      };
      try {
        vm.runInContext(
          "__fyModule.init(__ALI_BAXIA_OPTIONS__, __ALI_BAXIA_READY__)",
          browser,
          { filename: "baxia-init.js", timeout: input.timeoutMs },
        );
      } catch (error) {
        finish(error);
      }
    });
    // 采样调用也使用原始 options 对象和 getter。它会推进 SDK 计数状态，
    // 后续 Token 延续同一会话，而非重新初始化或重复使用样本。
    await new Promise((resolve) => setTimeout(resolve, 0));
    browser.__ALI_BAXIA_OPTIONS__.reqUrl = input.pageURL;
    const sample = vm.runInContext(`
      (() => {
        const trace = __ALI_BAXIA_TRACE__;
        trace.begin();
        try {
          const token = __fyModule.getFYToken(__ALI_BAXIA_OPTIONS__);
          if (typeof token !== "string") throw new Error("BAXIA_SDK_CONTRACT: getter sample is not a string");
          return { token, observations: trace.finish() };
        } finally {
          trace.restore();
          delete globalThis.__ALI_BAXIA_TRACE__;
        }
      })();
    `, browser, { filename: "baxia-profile-sample.js", timeout: input.timeoutMs });
    session = { browser, version };
    return { ...sample, version };
  };

  globalThis.__aliV8BaxiaToken = async (input) => {
    if (session === null) throw new Error("BAXIA_NOT_OPEN");
    // 原生宿主在等待 Promise 时推进 timers，给先前排队的 SDK 任务一个事件循环轮次。
    await new Promise((resolve) => setTimeout(resolve, 0));
    session.browser.__ALI_BAXIA_OPTIONS__.reqUrl = input.requestURL;
    const token = vm.runInContext(
      "__fyModule.getFYToken(__ALI_BAXIA_OPTIONS__)",
      session.browser,
      { filename: "baxia-token.js", timeout: 2_000 },
    );
    return { token, version: session.version };
  };
})();
true;
