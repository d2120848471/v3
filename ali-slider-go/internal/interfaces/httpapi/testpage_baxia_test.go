package httpapi

import (
	"bytes"
	"context"
	"os/exec"
	"regexp"
	"testing"
	"time"
)

func TestEmbeddedAPITestPageBaxiaControls(t *testing.T) {
	for _, pattern := range []string{
		`<select\s+id="api-mode">`,
		`<option\s+value="slider">验证码求解</option>`,
		`<option\s+value="bxua">Baxia bx-ua</option>`,
		`<fieldset[^>]+id="bxua-fields"[^>]+hidden[^>]+disabled>`,
		`<input[^>]+id="page-url"[^>]+type="url"[^>]+required\b`,
		`<input[^>]+id="request-url"[^>]+type="url"[^>]+required\b`,
	} {
		if !regexp.MustCompile(pattern).Match(testPageHTML) {
			t.Errorf("page missing mode control %q", pattern)
		}
	}
}

func TestEmbeddedAPITestPageWAFControls(t *testing.T) {
	for _, pattern := range []string{
		`<option\s+value="waf">WAF / ESA 验证</option>`,
		`<fieldset[^>]+id="waf-fields"[^>]+hidden[^>]+disabled>`,
		`<input[^>]+id="waf-page-url"[^>]+type="url"[^>]+required\b`,
	} {
		if !regexp.MustCompile(pattern).Match(testPageHTML) {
			t.Errorf("page missing WAF control %q", pattern)
		}
	}
	fields := regexp.MustCompile(`(?s)<fieldset[^>]+id="waf-fields"[^>]*>(.*?)</fieldset>`).FindSubmatch(testPageHTML)
	if len(fields) != 2 || bytes.Count(fields[1], []byte("<input ")) != 1 {
		t.Fatal("WAF form must only ask for pageUrl; proxy uses the shared input")
	}
}

func TestEmbeddedAPITestPageModeBehavior(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node runtime is unavailable")
	}

	// 执行真实页面脚本，但用内存 DOM、计时器和 fetch 替身隔离浏览器及所有上游。
	const script = `
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const html = fs.readFileSync(0, "utf8");
const source = html.match(/<script\b[^>]*>([\s\S]*?)<\/script>/)[1];
function createPage(initialHash = "") {
const elements = new Map();
const makeElement = (tag, attrs = "") => {
  const attributes = new Map([...attrs.matchAll(/([\w-]+)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'))?/g)]
    .map((match) => [match[1], match[2] ?? match[3] ?? ""]));
  const attr = (name) => attributes.get(name) || "";
  let text = "";
  const element = {
    tag, value: attr("value"), type: attr("type"), pattern: attr("pattern"),
    required: attributes.has("required"), disabled: attributes.has("disabled"),
    hidden: attributes.has("hidden"), checked: attributes.has("checked"),
    open: attributes.has("open"), className: attr("class"), children: [], listeners: new Map(),
    get textContent() { return text + this.children.map((child) => child.textContent ?? String(child)).join(""); },
    set textContent(value) { text = String(value); this.children = []; },
    append(...children) { this.children.push(...children); },
    replaceChildren(...children) { text = ""; this.children = [...children]; },
    setAttribute(name, value) { attributes.set(name, String(value)); },
    getAttribute(name) { return attributes.get(name) ?? null; },
    showModal() { this.open = true; },
    close() { this.open = false; },
    addEventListener(type, callback) {
      if (!this.listeners.has(type)) this.listeners.set(type, []);
      this.listeners.get(type).push(callback);
    }
  };
  return element;
};
for (const match of html.matchAll(/<([a-z][a-z0-9]*)\b([^>]*\bid="([^"]+)"[^>]*)>/g)) {
  const [, tag, attrs, id] = match;
  const element = makeElement(tag, attrs);
  if (tag === "select") {
    element.options = [...html.slice(match.index).split("</select>")[0].matchAll(/<option\b[^>]*value="([^"]+)"/g)]
      .map((option) => ({value: option[1]}));
    element.value = element.options[0].value;
  }
  element.defaultValue = element.value;
  elements.set(id, element);
}
const get = (id) => {
  assert.ok(elements.has(id), "unknown DOM id: " + id);
  return elements.get(id);
};
for (const group of html.matchAll(/<fieldset\b[^>]*id="([^"]+)"[^>]*>([\s\S]*?)<\/fieldset>/g)) {
  for (const child of group[2].matchAll(/<input\b[^>]*id="([^"]+)"/g)) get(child[1]).fieldset = get(group[1]);
}
// reset 与表单校验只作用于实际表单控件，不能重置抽屉外的示例语言。
const formHTML = html.match(/<form\b[^>]*id="solve-form"[^>]*>([\s\S]*?)<\/form>/)[1];
const formControls = [...formHTML.matchAll(/<(?:input|select)\b[^>]*id="([^"]+)"/g)].map((match) => get(match[1]));
get("solve-form").reportValidity = () => formControls.every((el) => {
  if (el.tag !== "input" || el.disabled || el.fieldset?.disabled) return true;
  if (el.required && !el.value.trim()) return false;
  if (el.type === "url" && el.value) {
    try { new URL(el.value); } catch { return false; }
  }
  return !el.pattern || !el.value || new RegExp("^(?:" + el.pattern + ")$").test(el.value);
});
get("solve-form").reset = () => {
  for (const el of formControls) { el.value = el.defaultValue; el.checked = false; }
};
const dispatch = async (target, type) => {
  const callbacks = target.listeners.get(type);
  assert.ok(callbacks?.length, "missing " + type + " listener");
  const event = {type, target, currentTarget: target, defaultPrevented: false,
    preventDefault() { this.defaultPrevented = true; }};
  return Promise.all(callbacks.map((callback) => callback(event)));
};
const fire = (id, event) => dispatch(get(id), event);
const window = {listeners: new Map(), addEventListener: makeElement("window").addEventListener};
const fireWindow = (event) => dispatch(window, event);
let hash = "";
const location = {
  origin: "http://127.0.0.1:18080",
  get hash() { return hash; },
  set hash(value) { hash = value && !value.startsWith("#") ? "#" + value : value; }
};
location.hash = initialHash;
const historyChanges = [];
const clipboardWrites = [];

const calls = [];
const timers = new Map();
let nextTimer = 0;
let pending;
const context = {
  document: {getElementById: get, createElement: makeElement}, location, window,
  history: {replaceState(_state, _title, url) { historyChanges.push(url); location.hash = new URL(url, location.origin).hash; }},
  navigator: {clipboard: {async writeText(value) { clipboardWrites.push(value); }}},
  performance: {now: () => 100},
  AbortController, TextEncoder,
  setTimeout(callback, milliseconds) { timers.set(++nextTimer, {callback, milliseconds}); return nextTimer; },
  clearTimeout(id) { timers.delete(id); },
  fetch(url, options) {
    calls.push({url, options});
    if (url === "/health") return Promise.resolve({ok: true, json: async () => ({ok: true, status: "ready"})});
    assert.ok(["/api/slider", "/api/bxua", "/api/waf"].includes(url), "unexpected network destination: " + url);
    assert.equal(pending, undefined, "overlapping API requests");
    return new Promise((resolve, reject) => {
      pending = {resolve, reject};
      options.signal.addEventListener("abort", () => {
        pending = undefined;
        reject(Object.assign(new Error("aborted"), {name: "AbortError"}));
      });
    });
  }
};
vm.runInNewContext(source, context, {timeout: 1000});
const reply = (body, status = 200) => {
  assert.ok(pending, "no request awaiting response");
  const current = pending;
  pending = undefined;
  current.resolve({
    status, statusText: status === 200 ? "OK" : "Other", ok: status >= 200 && status < 300,
    headers: {get: () => "header-trace"}, text: async () => JSON.stringify(body)
  });
};
return {get, fire, fireWindow, context, calls, timers, reply, historyChanges, clipboardWrites,
  get pending() { return pending; }};
}

const sceneCases = [
  ["auto", "slider", "自动选择"], ["puzzle", "slider", "图片拼图"],
  ["traceless", "slider", "无痕验证"], ["sliding", "slider", "拖动滑块"],
  ["waf", "waf", "页面验证"], ["bxua", "bxua", "bx-ua 生成"]
];
const sceneKeys = sceneCases.map(([key]) => key);
const tableRows = (page, id) => page.get(id).children.map((row) => row.children.map((cell) => cell.textContent));
const assertScene = (page, key, mode, title) => {
  assert.equal(page.get("api-mode").value, mode);
  assert.equal(page.get("scene-title").textContent, title);
  assert.equal(page.get("debug-title").textContent, "在线调试 · " + title);
  assert.equal(page.get("endpoint").textContent, page.context.location.origin + "/api/" + mode);
  assert.equal(page.get("debug-endpoint").textContent, page.get("endpoint").textContent);
  assert.equal(page.get("response-note").textContent, page.get("result-hint").textContent);
  for (const candidate of sceneKeys) {
    assert.equal(page.get("nav-" + candidate).getAttribute("aria-current"), candidate === key ? "page" : "false");
  }
  for (const candidate of ["slider", "waf", "bxua"]) {
    assert.equal(page.get(candidate + "-fields").hidden, candidate !== mode);
    assert.equal(page.get(candidate + "-fields").disabled, candidate !== mode);
  }
};

// 深链接只选择文档；未知 hash（含 Object 原型名称）安全回到默认分类。
for (const [hash, key, mode, title] of [
  ...sceneCases.map(([key, mode, title]) => ["#" + key, key, mode, title]),
  ...["", "#unknown", "#toString", "#__proto__"].map((hash) => [hash, "auto", "slider", "自动选择"])
]) {
  const initialPage = createPage(hash);
  await new Promise(setImmediate);
  assertScene(initialPage, key, mode, title);
  assert.deepEqual(initialPage.calls.map((call) => call.url), ["/health"], "deep link must not call a business API");
  assert.equal(initialPage.timers.size, 0);
  assert.equal(initialPage.get("debug-dialog").open, false);
  assert.equal(initialPage.pending, undefined);
}

const page = createPage();
const {get, fire, fireWindow, context, calls, timers, reply, historyChanges, clipboardWrites} = page;
await new Promise(setImmediate);
assert.deepEqual(calls.map((call) => call.url), ["/health"], "loading must not call either business API");
assert.equal(timers.size, 0);
assert.equal(get("api-mode").value, "slider");
assert.equal(get("bxua-fields").hidden, true);
assert.equal(get("bxua-fields").disabled, true);
assert.equal(get("waf-fields").hidden, true);
assert.equal(get("waf-fields").disabled, true);

const businessCalls = () => calls.filter((call) => call.url !== "/health");
const submit = () => fire("solve-form", "submit");
const selectMode = async (mode) => { get("api-mode").value = mode; await fire("api-mode", "change"); };
const selectScene = async (key) => { await fire("nav-" + key, "click"); await fireWindow("hashchange"); };
const sliderSuccess = {ok: true, VerifyCode: "T001", VerifyResult: true, securityToken: "slider-secret"};

// 分类与代码预览同步，所有模板均不读取调试输入或发送真实请求。
assert.equal(get("debug-dialog").tag, "dialog");
assert.equal(get("example-language").tag, "select");
assert.deepEqual(get("example-language").options.map((option) => option.value), ["curl", "python", "go", "javascript"]);
get("scene-id").value = "runtime-scene-private";
get("rpc-key").value = "runtime-rpc-private";
get("proxy").value = "http://runtime-user:runtime-proxy-private@127.0.0.1:7890";
get("page-url").value = "https://runtime-private.test/login";
get("request-url").value = "https://runtime-private.test/api/login";
const exampleBody = (language, code) => {
  if (language === "go") {
    const literal = code.match(/body := strings.NewReader\(("(?:\\.|[^"\\])*")\)/)[1];
    return JSON.parse(JSON.parse(literal));
  }
  const pattern = language === "curl" ? /--data '([\s\S]*)'$/
    : language === "python" ? /body = ([\s\S]*?)\nrequest = Request\(/
    : /body: JSON.stringify\(([\s\S]*?)\),\n/;
  return JSON.parse(code.match(pattern)[1]);
};
for (const [key, mode, title] of sceneCases) {
  assert.equal(get("nav-" + key).tag, "a");
  assert.equal(get("nav-" + key).getAttribute("href"), "#" + key);
  await selectScene(key);
  assertScene(page, key, mode, title);
  assert.equal(context.location.hash, "#" + key);
  const parameters = tableRows(page, "parameter-rows");
  assert.ok(parameters.every((row) => row.length === 4));
  assert.deepEqual(parameters.map((row) => row[0]), mode === "slider"
    ? ["SceneId", "prefix", "AaduaneId", "proxy"] : mode === "waf"
    ? ["pageUrl", "proxy"] : ["pageUrl", "requestUrl", "proxy"]);
  const responses = tableRows(page, "response-rows");
  assert.ok(responses.every((row) => row.length === 3));
  assert.ok(responses.some((row) => row[0] === (mode === "slider" ? "securityToken" : mode === "waf" ? "u_atoken / u_asig" : "bx-ua")));
  const expectedBody = mode === "waf" ? {pageUrl: "https://example.com/protected-page"}
    : mode === "bxua" ? {pageUrl: "https://example.com/login", requestUrl: "https://example.com/api/login"}
    : key === "auto" ? {} : {SceneId: "YOUR_SCENE_ID", prefix: "YOURPREFIX"};
  assert.ok(get("example-code").textContent.includes(get("endpoint").textContent), "navigation must refresh the current code preview");
  assert.deepEqual(exampleBody(get("example-language").value, get("example-code").textContent), expectedBody);
  for (const [language, marker] of [["curl", "--request POST"], ["python", 'method="POST"'], ["go", "client.Post("], ["javascript", 'method: "POST"']]) {
    get("example-language").value = language;
    await fire("example-language", "change");
    const code = get("example-code").textContent;
    assert.ok(code.includes(get("endpoint").textContent), "example endpoint must follow the selected type");
    assert.ok(code.includes(marker), "example must use the selected language and POST");
    assert.ok(code.includes("application/json"));
    assert.deepEqual(exampleBody(language, code), expectedBody, "example payload must match the selected interface contract");
    assert.ok(!code.includes("runtime-"), "examples must not expose live debug inputs");
  }
}
assert.equal(businessCalls().length, 0, "navigation and code previews must not send requests");
const copiedCode = get("example-code").textContent;
await fire("copy-example", "click");
assert.deepEqual(clipboardWrites, [copiedCode]);
assert.equal(get("example-copy-status").textContent, "代码已复制。");
context.navigator.clipboard.writeText = async () => { throw new Error("clipboard denied"); };
await fire("copy-example", "click");
assert.match(get("example-copy-status").textContent, /手动复制/);
assert.equal(get("example-code").textContent, copiedCode);

// hashchange 可切换分类；无效 hash 不改变当前分类和表单。
context.location.hash = "#sliding";
await fireWindow("hashchange");
assertScene(page, "sliding", "slider", "拖动滑块");
context.location.hash = "#unknown";
await fireWindow("hashchange");
assertScene(page, "sliding", "slider", "拖动滑块");
assert.equal(get("scene-id").value, "runtime-scene-private", "navigation must preserve typed inputs");

// 筛选支持类型名、分组和 endpoint，并正确隐藏空分组。
for (const [query, expected] of [
  ["  SLIDING  ", ["sliding"]], ["无痕", ["traceless"]], ["/api/bxua", ["bxua"]],
  ["V3", ["auto", "puzzle", "traceless", "sliding"]], ["no-matching-type", []], ["", sceneKeys]
]) {
  get("search").value = query;
  get("navigation-details").open = false;
  await fire("search", "input");
  assert.deepEqual(sceneKeys.filter((key) => !get("nav-" + key).hidden), expected);
  for (const [group, keys] of [["start", ["auto"]], ["captcha", ["puzzle", "traceless", "sliding"]], ["waf", ["waf"]], ["bxua", ["bxua"]]]) {
    assert.equal(get("group-" + group).hidden, !keys.some((key) => expected.includes(key)));
  }
  assert.equal(get("search-empty").hidden, expected.length !== 0);
  assert.equal(get("navigation-details").open, true);
  assertScene(page, "sliding", "slider", "拖动滑块");
}
assert.equal(businessCalls().length, 0, "hash and search interactions must not send requests");

await fire("open-debug", "click");
assert.equal(get("debug-dialog").open, true);
await fire("close-debug", "click");
assert.equal(get("debug-dialog").open, false);
assert.equal(get("scene-id").value, "runtime-scene-private", "closing the drawer must preserve form inputs");
assert.equal(businessCalls().length, 0, "opening or closing the drawer must not send requests");
get("solve-form").reset();
get("example-language").value = "curl";
await selectScene("auto");
await fire("open-debug", "click");

// 默认滑块请求不受隐藏的必填 URL 影响。
let running = submit();
assert.deepEqual(JSON.parse(businessCalls().at(-1).options.body), {});
reply(sliderSuccess);
await running;
assert.equal(get("result-label").textContent, "验证成功");
assert.ok(!get("response-output").textContent.includes("slider-secret"));

get("scene-id").value = "old-scene";
get("prefix").value = "invalid hidden prefix!";
get("rpc-key").value = "old-rpc-secret";
await selectMode("bxua");
assert.equal(businessCalls().length, 1, "changing mode must not send a request");
assert.equal(get("slider-fields").hidden, true);
assert.equal(get("slider-fields").disabled, true);
assert.equal(get("bxua-fields").hidden, false);
assert.equal(get("bxua-fields").disabled, false);
assert.equal(get("endpoint").textContent, "http://127.0.0.1:18080/api/bxua");
assert.ok(!get("result-hint").textContent.includes("VerifyCode"));
await submit();
assert.equal(businessCalls().length, 1, "empty pageUrl must block sending");
get("page-url").value = "https://example.test/login";
await submit();
assert.equal(businessCalls().length, 1, "empty requestUrl must block sending");
get("request-url").value = "not a URL";
await submit();
assert.equal(businessCalls().length, 1, "invalid requestUrl must block sending");
get("request-url").value = "https://example.test/api/login?from=page";
get("proxy").value = "  http://proxy-user:proxy-secret@127.0.0.1:7890  ";

running = submit();
const sent = businessCalls().at(-1);
assert.equal(sent.url, "/api/bxua");
assert.deepEqual(JSON.parse(sent.options.body), {
  pageUrl: "https://example.test/login", requestUrl: "https://example.test/api/login?from=page",
  proxy: "http://proxy-user:proxy-secret@127.0.0.1:7890"
});
assert.equal(sent.options.method, "POST");
assert.equal(sent.options.credentials, "omit");
assert.equal(sent.options.redirect, "error");
assert.equal(sent.options.cache, "no-store");
assert.equal(sent.options.referrerPolicy, "no-referrer");
assert.equal(sent.options.headers["Content-Type"], "application/json");
assert.equal(get("api-mode").disabled, true);
assert.equal(get("submit").disabled, true);
assert.equal(get("clear").disabled, true);
assert.equal(get("cancel").disabled, false);
for (const key of sceneKeys) assert.equal(get("nav-" + key).getAttribute("aria-disabled"), "true");
await fire("nav-waf", "click");
assertScene(page, "bxua", "bxua", "bx-ua 生成");
assert.equal(context.location.hash, "#bxua", "busy navigation must keep the selected type");
context.location.hash = "#waf";
await fireWindow("hashchange");
assertScene(page, "bxua", "bxua", "bx-ua 生成");
assert.equal(context.location.hash, "#bxua", "busy hashchange must restore the selected type");
assert.equal(historyChanges.at(-1), "#bxua");
await fire("close-debug", "click");
assert.equal(get("debug-dialog").open, false);
assert.equal(sent.options.signal.aborted, false, "closing the drawer must not silently cancel a request");
await fire("open-debug", "click");
assert.equal(get("debug-dialog").open, true);
assert.equal(get("result-label").textContent, "请求执行中");
await submit();
assert.equal(businessCalls().length, 2, "duplicate submit must not send again");
assert.ok(!get("request-output").textContent.includes("proxy-secret"));

const bxuaSuccess = {
  ok: true, "bx-ua": "bxua-secret", version: 9, sdkURL: "https://example.test/sdk.js",
  sdkSha256: "a".repeat(64), awscSha256: "b".repeat(64), profile: {encoded: "fixture"},
  uaHeaders: {"User-Agent": "fixture-ua", "BX-UA": "nested-secret"}, proxied: true, elapsedMs: 12, traceId: "body-trace"
};
reply(bxuaSuccess);
await running;
assert.equal(get("result-label").textContent, "bx-ua 已生成", "bx-ua success must not require VerifyCode");
assert.match(get("result-detail").textContent, /生成成功不代表站点登录/);
assert.equal(get("trace-id").textContent, "header-trace");
assert.equal(get("api-mode").disabled, false);
assert.equal(get("cancel").disabled, true);
for (const key of sceneKeys) assert.equal(get("nav-" + key).getAttribute("aria-disabled"), "false");
let displayed = JSON.parse(get("response-output").textContent);
assert.equal(displayed["bx-ua"], "••••••");
assert.equal(displayed.uaHeaders["BX-UA"], "••••••");
assert.deepEqual(displayed.profile, bxuaSuccess.profile);
assert.equal(displayed.sdkSha256, bxuaSuccess.sdkSha256);
get("reveal-sensitive").checked = true;
await fire("reveal-sensitive", "change");
assert.deepEqual(JSON.parse(get("response-output").textContent), bxuaSuccess);
assert.ok(get("request-output").textContent.includes("proxy-secret"));

await selectMode("slider");
assertScene(page, "auto", "slider", "自动选择");
assert.equal(context.location.hash, "#auto", "the mode select must update the documentation hash");
assert.equal(get("reveal-sensitive").checked, false);
assert.equal(get("response-output").textContent, "尚未发送请求。");
assert.equal(get("bxua-fields").disabled, true);
get("page-url").value = "invalid hidden URL";
get("request-url").value = "";
get("prefix").value = "valid7";
running = submit();
assert.equal(businessCalls().at(-1).url, "/api/slider");
assert.deepEqual(JSON.parse(businessCalls().at(-1).options.body), {
  SceneId: "old-scene", prefix: "valid7", AaduaneId: "old-rpc-secret",
  proxy: "http://proxy-user:proxy-secret@127.0.0.1:7890"
});
reply({...sliderSuccess, VerifyResult: false});
await running;
assert.equal(get("result-label").textContent, "响应合同异常", "slider verification must still be checked");

// WAF 只提交页面 URL 和代理，不带 V3 参数或 Baxia requestUrl。
get("prefix").value = "invalid hidden prefix!";
get("page-url").value = "invalid hidden URL";
get("request-url").value = "";
const beforeWAF = businessCalls().length;
await selectMode("waf");
assert.equal(businessCalls().length, beforeWAF, "selecting WAF must not access its page");
assert.equal(get("slider-fields").disabled, true);
assert.equal(get("bxua-fields").disabled, true);
assert.equal(get("waf-fields").hidden, false);
assert.equal(get("waf-fields").disabled, false);
assert.equal(get("endpoint").textContent, "http://127.0.0.1:18080/api/waf");
assert.match(get("result-hint").textContent, /u_atoken.*u_asig/);
await submit();
assert.equal(businessCalls().length, beforeWAF, "empty WAF pageUrl must block sending");
get("waf-page-url").value = "not a URL";
await submit();
assert.equal(businessCalls().length, beforeWAF, "invalid WAF pageUrl must block sending");
get("waf-page-url").value = "https://example.com/pc/index.html?orgId=fixture";
running = submit();
assert.equal(businessCalls().at(-1).url, "/api/waf");
assert.deepEqual(JSON.parse(businessCalls().at(-1).options.body), {
  pageUrl: "https://example.com/pc/index.html?orgId=fixture", proxy: "http://proxy-user:proxy-secret@127.0.0.1:7890"
});
assert.equal(get("api-mode").disabled, true);
assert.equal(get("submit").disabled, true);
await submit();
assert.equal(businessCalls().length, beforeWAF + 1, "duplicate WAF submit must not send again");
const wafSuccess = {
  ok: true, sceneId: "fixture-scene", captchaType: "SLIDING", verifyCode: "T001",
  u_atoken: "waf-secret-token", u_asig: "waf-secret-signature",
  uaHeaders: {"User-Agent": "fixture-ua", "U_ASIG": "nested-waf-secret"}, proxied: true, elapsedMs: 15, traceId: "body-trace"
};
reply(wafSuccess);
await running;
assert.equal(get("result-label").textContent, "WAF 验证成功");
assert.equal(get("business-status").textContent, "T001");
assert.match(get("result-detail").textContent, /原页面和登录业务仍由调用方处理/);
displayed = JSON.parse(get("response-output").textContent);
assert.equal(displayed.u_atoken, "••••••");
assert.equal(displayed.u_asig, "••••••");
assert.equal(displayed.uaHeaders["U_ASIG"], "••••••");
assert.ok(!get("response-output").textContent.includes("waf-secret"));
get("reveal-sensitive").checked = true;
await fire("reveal-sensitive", "change");
assert.deepEqual(JSON.parse(get("response-output").textContent), wafSuccess);
get("reveal-sensitive").checked = false;
for (const [body, status, label] of [
  [{ok: true, u_atoken: "token"}, 200, "响应合同异常"],
  [{ok: true, u_atoken: "token", u_asig: " "}, 200, "响应合同异常"],
  [wafSuccess, 201, "响应合同异常"],
  [{ok: false, verifyCode: "F015"}, 200, "请求完成，验证未通过"],
  [{ok: false, errorType: "WAFRuntimeError"}, 500, "API 返回错误"]
]) {
  running = submit();
  reply(body, status);
  await running;
  assert.equal(get("result-label").textContent, label);
  if (body.verifyCode === "F015") assert.equal(get("business-status").textContent, "F015");
}
await fire("clear", "click");
assert.equal(get("api-mode").value, "waf", "clear keeps WAF mode");
assert.equal(get("waf-fields").disabled, false);
assert.equal(get("waf-page-url").value, "");
assert.equal(get("response-output").textContent, "尚未发送请求。");

await selectMode("bxua");
assert.equal(get("waf-fields").disabled, true);
get("page-url").value = "https://example.test/login";
get("request-url").value = "https://example.test/api/login";
get("proxy").value = "";
for (const [body, status, label] of [
  [{ok: true}, 200, "响应合同异常"],
  [{ok: true, "bx-ua": "  "}, 200, "响应合同异常"],
  [bxuaSuccess, 201, "响应合同异常"],
  [{ok: false}, 200, "请求完成，生成未成功"],
  [{ok: false, errorType: "ValidationError"}, 400, "API 返回错误"]
]) {
  running = submit();
  assert.equal(Object.hasOwn(JSON.parse(businessCalls().at(-1).options.body), "proxy"), false);
  reply(body, status);
  await running;
  assert.equal(get("result-label").textContent, label);
}

for (const reason of ["cancel", "timeout"]) {
  const before = businessCalls().length;
  running = submit();
  const signal = businessCalls().at(-1).options.signal;
  if (reason === "cancel") {
    await fire("cancel", "click");
  } else {
    assert.equal(timers.size, 1);
    const timer = [...timers.values()][0];
    assert.equal(timer.milliseconds, 30000);
    timer.callback();
  }
  await running;
  assert.equal(signal.aborted, true);
  assert.equal(businessCalls().length, before + 1, "abort must not retry");
  assert.equal(get("result-label").textContent, reason === "cancel" ? "请求已取消" : "页面等待超时");
  assert.equal(get("api-mode").disabled, false);
  for (const key of sceneKeys) assert.equal(get("nav-" + key).getAttribute("aria-disabled"), "false");
  assert.equal(get("submit").disabled, false);
  assert.equal(get("cancel").disabled, true);
  assert.equal(timers.size, 0);
}

get("reveal-sensitive").checked = true;
await fire("clear", "click");
assert.equal(get("api-mode").value, "bxua", "clear keeps the chosen mode");
assert.equal(get("bxua-fields").disabled, false);
assert.equal(get("page-url").value, "");
assert.equal(get("request-url").value, "");
assert.equal(get("reveal-sensitive").checked, false);
assert.equal(get("request-output").textContent, "{}");
assert.equal(get("response-output").textContent, "尚未发送请求。");
assert.equal(page.pending, undefined);

// 四个 V3 分类仍提交统一合同，不携带分类名或示例占位符。
get("scene-id").value = "typed-scene";
get("prefix").value = "valid8";
get("rpc-key").value = "typed-rpc-private";
get("page-url").value = "invalid hidden URL";
get("request-url").value = "";
get("waf-page-url").value = "invalid hidden WAF URL";
get("example-language").value = "python";
for (const [key, mode, title] of sceneCases.filter(([, mode]) => mode === "slider")) {
  const before = businessCalls().length;
  await selectScene(key);
  assertScene(page, key, mode, title);
  assert.equal(get("scene-id").value, "typed-scene");
  assert.equal(get("example-language").value, "python", "navigation keeps the chosen example language");
  assert.equal(businessCalls().length, before, "V3 classification must not automatically send a request");
  running = submit();
  const sent = businessCalls().at(-1);
  assert.equal(sent.url, "/api/slider");
  assert.deepEqual(JSON.parse(sent.options.body), {SceneId: "typed-scene", prefix: "valid8", AaduaneId: "typed-rpc-private"});
  reply(sliderSuccess);
  await running;
  assert.equal(get("result-label").textContent, "验证成功");
  assert.ok(!get("response-output").textContent.includes("slider-secret"));
  assert.equal(businessCalls().length, before + 1);
}
await fire("clear", "click");
assertScene(page, "sliding", "slider", "拖动滑块");
assert.equal(get("example-language").value, "python", "clearing the form must preserve the example language");
assert.equal(context.location.hash, "#sliding");
assert.equal(page.pending, undefined);
assert.equal(timers.size, 0);
process.stdout.write("page mode behavior passed\n");
`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, nodePath, "--input-type=module", "--eval", script)
	command.Stdin = bytes.NewReader(testPageHTML)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("page behavior check failed: %v\n%s", err, output)
	}
}
