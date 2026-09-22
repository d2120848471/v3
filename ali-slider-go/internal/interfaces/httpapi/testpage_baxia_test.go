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
const elements = new Map();
for (const match of html.matchAll(/<([a-z]+)\b([^>]*\bid="([^"]+)"[^>]*)>/g)) {
  const [, tag, attrs, id] = match;
  const attr = (name) => attrs.match(new RegExp("\\b" + name + "=\"([^\"]*)\""))?.[1] || "";
  const element = {
    tag, value: attr("value"), type: attr("type"), pattern: attr("pattern"),
    required: /\brequired\b/.test(attrs), disabled: /\bdisabled\b/.test(attrs),
    hidden: /\bhidden\b/.test(attrs), checked: false, textContent: "", className: "",
    listeners: new Map(),
    addEventListener(type, callback) { this.listeners.set(type, callback); }
  };
  if (tag === "select") element.value = html.slice(match.index).match(/<option value="([^"]+)"/)[1];
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
get("solve-form").reportValidity = () => [...elements.values()].every((el) => {
  if (el.tag !== "input" || el.disabled || el.fieldset?.disabled) return true;
  if (el.required && !el.value.trim()) return false;
  if (el.type === "url" && el.value) {
    try { new URL(el.value); } catch { return false; }
  }
  return !el.pattern || !el.value || new RegExp("^(?:" + el.pattern + ")$").test(el.value);
});
get("solve-form").reset = () => {
  for (const el of elements.values()) { el.value = el.defaultValue; el.checked = false; }
};
const fire = async (id, event) => {
  const callback = get(id).listeners.get(event);
  assert.ok(callback, "missing " + event + " listener on " + id);
  return callback({preventDefault() {}});
};

const calls = [];
const timers = new Map();
let nextTimer = 0;
let pending;
const context = {
  document: {getElementById: get}, location: {origin: "http://127.0.0.1:18080"},
  window: {addEventListener() {}}, performance: {now: () => 100},
  AbortController, TextEncoder,
  setTimeout(callback, milliseconds) { timers.set(++nextTimer, {callback, milliseconds}); return nextTimer; },
  clearTimeout(id) { timers.delete(id); },
  fetch(url, options) {
    calls.push({url, options});
    if (url === "/health") return Promise.resolve({ok: true, json: async () => ({ok: true, status: "ready"})});
    assert.ok(["/api/slider", "/api/bxua"].includes(url), "unexpected network destination: " + url);
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
await new Promise(setImmediate);
assert.deepEqual(calls.map((call) => call.url), ["/health"], "loading must not call either business API");
assert.equal(timers.size, 0);
assert.equal(get("api-mode").value, "slider");
assert.equal(get("bxua-fields").hidden, true);
assert.equal(get("bxua-fields").disabled, true);

const businessCalls = () => calls.filter((call) => call.url !== "/health");
const reply = (body, status = 200) => {
  assert.ok(pending, "no request awaiting response");
  const current = pending;
  pending = undefined;
  current.resolve({
    status, statusText: status === 200 ? "OK" : "Other", ok: status >= 200 && status < 300,
    headers: {get: () => "header-trace"}, text: async () => JSON.stringify(body)
  });
};
const submit = () => fire("solve-form", "submit");
const selectMode = async (mode) => { get("api-mode").value = mode; await fire("api-mode", "change"); };
const sliderSuccess = {ok: true, VerifyCode: "T001", VerifyResult: true, securityToken: "slider-secret"};

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

await selectMode("bxua");
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
assert.equal(pending, undefined);
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
