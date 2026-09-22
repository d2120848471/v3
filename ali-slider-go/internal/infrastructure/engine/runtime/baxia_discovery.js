/* 仅观测 AWSC 的脚本选择；脚本插入和宿主传输均不会发起网络请求。 */
(() => {
  "use strict";
  const { vm } = globalThis.__aliNodeCompat;

  globalThis.__aliV8BaxiaDiscover = (input) => {
    const browser = globalThis.__aliV8BaxiaBrowser(input);
    const urls = new Set();
    const observe = (element) => {
      if (String(element?.tagName).toLowerCase() !== "script") return element;
      if (element.id === "AWSC_fyModule") {
        try { urls.add(new URL(String(element.src), input.awscURL).href); }
        catch { throw new Error("BAXIA_SDK_CONTRACT: AWSC inserted an invalid Fireye URL"); }
      }
      return element;
    };
    // AWSC 从已有 script.src 推导 CDN 基址，必须使用真正下载后的 AWSC URL。
    const existing = browser.document.createElement("script");
    existing.src = input.awscURL;
    existing.hasAttribute = (name) => name === "src";
    existing.parentNode = { insertBefore: observe };
    const getElements = browser.document.getElementsByTagName.bind(browser.document);
    browser.document.getElementsByTagName = (name) => (
      String(name).toLowerCase() === "script" ? [existing] : getElements(name)
    );
    for (const node of [browser.document.head, browser.document.body, browser.document.documentElement]) {
      node.appendChild = observe;
      node.insertBefore = observe;
    }

    // use("fy") 的插入是同步动作；不触发 load/error 回调，不推进 SDK 下载
    // 计时器或遥测 timers。AWSC 的原始随机灰度决策保持不变。
    let timerID = 0;
    browser.setTimeout = () => ++timerID;
    browser.setInterval = () => ++timerID;
    browser.clearTimeout = () => {};
    browser.clearInterval = () => {};
    vm.runInContext(input.awscSource, browser, {
      filename: "awsc-source.js", timeout: input.timeoutMs,
    });
    if (!browser.AWSC || typeof browser.AWSC.use !== "function") {
      throw new Error("BAXIA_SDK_CONTRACT: AWSC.use is unavailable");
    }
    vm.runInContext('AWSC.use("fy")', browser, {
      filename: "awsc-use-fy.js", timeout: input.timeoutMs,
    });
    return { urls: [...urls] };
  };
})();
true;
