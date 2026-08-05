import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { once } from "node:events";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { createInterface } from "node:readline";
import test from "node:test";
import { fileURLToPath } from "node:url";
import vm from "node:vm";

import {
  installSdkStateCapture,
  isSdkRuntimeState,
} from "../ali_slider_reverse/runtime/bridges/sdk_device_bridge.mjs";
import {
  loadSdkRuntime,
} from "../ali_slider_reverse/runtime/bridges/pe_data_bridge.mjs";


const DEVICE_BRIDGE_PATH = fileURLToPath(new URL(
  "../ali_slider_reverse/runtime/bridges/sdk_device_bridge.mjs",
  import.meta.url,
));


function makeHostTestProfile() {
  return {
    profileId: "host-test-profile",
    family: "android-test",
    userAgent: "Mozilla/5.0 (Linux; Android 13) Chrome/130 Mobile",
    appVersion: "5.0 (Linux; Android 13) Chrome/130 Mobile",
    platform: "Linux armv8l",
    vendor: "Google Inc.",
    mobile: true,
    pdfViewer: false,
    secChUa: '"Chromium";v="130"',
    secChUaMobile: "?1",
    secChUaPlatform: '"Android"',
    acceptLanguage: "zh-CN,en;q=0.9",
    brands: [{ brand: "Chromium", version: "130" }],
    fullVersionList: [{ brand: "Chromium", version: "130.0.0.0" }],
    uaPlatform: "Android",
    uaPlatformVersion: "13.0.0",
    uaModel: "Test Phone",
    uaFullVersion: "130.0.0.0",
    uaArchitecture: "",
    uaBitness: "64",
    language: "zh-CN",
    languages: ["zh-CN", "en"],
    screen: {
      width: 393,
      height: 873,
      availWidth: 393,
      availHeight: 873,
      availLeft: 0,
      availTop: 0,
      colorDepth: 24,
      orientationType: "portrait-primary",
      orientationAngle: 0,
      devicePixelRatio: 2.75,
      innerWidth: 393,
      innerHeight: 697,
      outerWidth: 393,
      outerHeight: 873,
    },
    gpu: {
      unmaskedVendor: "Google Inc. (ARM)",
      unmaskedRenderer: "ANGLE (ARM, Mali Test)",
      maxTextureSize: 8192,
    },
    hardwareConcurrency: 8,
    deviceMemory: 8,
    maxTouchPoints: 5,
    canvasSeed: "0123456789abcdef0123456789abcdef",
    textMetricScale: 5.75,
  };
}


function jsonLineQueue(stream) {
  const input = createInterface({ input: stream, crlfDelay: Infinity });
  const buffered = [];
  const waiting = [];
  input.on("line", (line) => {
    const payload = JSON.parse(line);
    const waiter = waiting.shift();
    if (waiter) {
      clearTimeout(waiter.timer);
      waiter.resolve(payload);
    } else {
      buffered.push(payload);
    }
  });
  return {
    next(timeoutMs = 5_000) {
      if (buffered.length > 0) {
        return Promise.resolve(buffered.shift());
      }
      return new Promise((resolve, reject) => {
        const waiter = {
          resolve,
          timer: setTimeout(() => {
            const index = waiting.indexOf(waiter);
            if (index >= 0) waiting.splice(index, 1);
            reject(new Error("等待 challenge-host 输出超时"));
          }, timeoutMs),
        };
        waiting.push(waiter);
      });
    },
  };
}


function exerciseVariant({
  inheritedDeviceCallback = false,
  methodDeclaration,
  methodName,
  ownerName,
  patchName,
  shadowObject = false,
}) {
  const source = [
    "(()=>{",
    "const statePrototype={",
    "ACCESS_SEC:'access',SESSION_ID_SALT:'salt',",
    "APP_NAME:'app',APP_KEY:'key',",
    inheritedDeviceCallback ? "deviceCallback(){}," : "",
    `${methodDeclaration}{Object.assign(this,${patchName})}`,
    "};",
    `const ${ownerName}=Object.create(statePrototype);`,
    `${ownerName}[${JSON.stringify(methodName)}]({prefix:'p',region:'cn',appName:'app',`,
    "appKey:'key',endpoints:['https://example.invalid'],",
    "deviceCallback(){}});",
    `globalThis.result=${ownerName};`,
    "})();",
  ].join("");
  let captured = null;
  const context = vm.createContext(shadowObject ? { Object } : {});
  const capture = installSdkStateCapture(context, (owner) => {
    captured = owner;
  });
  vm.runInContext(source, context);
  capture.flush();
  return { captured, context };
}


test("SDK 状态探针不依赖变量名、合并方法名或方法声明形态", () => {
  for (const variant of [
    {
      methodDeclaration: "_extend:function(payload)",
      methodName: "_extend",
      ownerName: "renamedRuntime",
      patchName: "payload",
    },
    {
      methodDeclaration: "applyState($0)",
      methodName: "applyState",
      ownerName: "$state9",
      patchName: "$0",
    },
    {
      methodDeclaration: "['nextVersionMerge'](next)",
      methodName: "nextVersionMerge",
      ownerName: "futureOwner",
      patchName: "next",
    },
    {
      inheritedDeviceCallback: true,
      methodDeclaration: "mergeWithInheritedTail(next)",
      methodName: "mergeWithInheritedTail",
      ownerName: "prototypeHeavyOwner",
      patchName: "next",
      shadowObject: true,
    },
  ]) {
    const { captured, context } = exerciseVariant(variant);
    assert.equal(captured, context.result);
    assert.equal(
      vm.runInContext(
        [
          "Object.prototype.hasOwnProperty.call(",
          "Object.getPrototypeOf({}),'deviceCallback')",
        ].join(""),
        context,
      ),
      false,
    );
  }
});


test("SDK 状态探针拒绝只有相似字段、但没有设备运行合同的对象", () => {
  assert.equal(
    isSdkRuntimeState(
      { ACCESS_SEC: "access", SESSION_ID_SALT: "salt" },
      {
        prefix: "p",
        region: "cn",
        appName: "app",
        appKey: "key",
        endpoints: [],
        deviceCallback() {},
      },
    ),
    false,
  );
});


test("SDK 状态探针支持以解密后的 DeviceConfig 作为第二条结构证据", () => {
  const owner = {
    ACCESS_SEC: "access",
    SESSION_ID_SALT: "salt",
    APP_NAME: "app",
    APP_KEY: "key",
  };
  assert.equal(
    isSdkRuntimeState(owner, {
      deviceConfig: {
        key: "0123456789abcdef",
        sessionId: "session",
        version: "1.0",
        pluginElements: "",
        pluginResource: "",
        globalVariable: "",
        timestamp: "1",
        ip: "127.0.0.1",
      },
    }),
    true,
  );
});


test("PE 桥通过共享运行时探针取得状态，不依赖 SDK 合并函数文本", () => {
  const context = vm.createContext({});
  const runtime = loadSdkRuntime(
    context,
    [
      "(()=>{",
      "const proto={ACCESS_SEC:'access',SESSION_ID_SALT:'salt',",
      "APP_NAME:'app',APP_KEY:'key',",
      "applyFutureState(nextState){Object.assign(this,nextState)}",
      "};",
      "const completelyRenamedOwner=Object.create(proto);",
      "completelyRenamedOwner.applyFutureState({prefix:'p',region:'cn',",
      "appName:'app',appKey:'key',endpoints:[],deviceCallback(){}});",
      "})();",
    ].join(""),
    "renamed-sdk.js",
    1_000,
  );
  assert.equal(runtime.APP_NAME, "app");
  assert.equal(runtime.prefix, "p");
});


test("challenge-host 在一个 Node 内隔离多 VM 并可单槽取消", async (t) => {
  const directory = await mkdtemp(path.join(tmpdir(), "ali-host-test-"));
  const sdkPath = path.join(directory, "fixture-sdk.js");
  await writeFile(sdkPath, [
    "(()=>{",
    "const proto={ACCESS_SEC:'access',SESSION_ID_SALT:'salt',",
    "APP_NAME:'app',APP_KEY:'key',",
    "apply(next){Object.assign(this,next)}};",
    "const runtime=Object.create(proto);",
    "runtime.apply({prefix:'p',region:'cn',appName:'app',appKey:'key',",
    "endpoints:[],deviceCallback(){}});",
    "runtime.deviceConfig={key:'0123456789abcdef',sessionId:'session',",
    "version:'1.0',pluginElements:'',pluginResource:'',globalVariable:'',",
    "timestamp:'1',ip:'127.0.0.1'};",
    "globalThis.z_um={count:0,getToken(value='init'){",
    "if(value==='fail')throw new Error('sensitive getter input');",
    "this.count+=1;return `${value}:${this.count}`}};",
    "runtime.DeviceToken=globalThis.z_um.getToken();",
    "})();",
  ].join(""), "utf8");

  const encodedProfile = Buffer.from(
    JSON.stringify(makeHostTestProfile()),
    "utf8",
  ).toString("base64");
  const child = spawn(process.execPath, [
    DEVICE_BRIDGE_PATH,
    "--mode", "challenge-host",
    "--sdk", sdkPath,
    "--prefix", "p",
    "--region", "cn",
    "--timeout-ms", "2000",
    "--device-profile", encodedProfile,
    "--vm-count", "4",
  ], { stdio: ["pipe", "pipe", "pipe"] });
  const exitPromise = once(child, "exit");
  let stderr = "";
  child.stderr.setEncoding("utf8");
  child.stderr.on("data", (chunk) => { stderr += chunk; });
  t.after(async () => {
    if (child.exitCode === null) child.kill();
    await rm(directory, { recursive: true, force: true });
  });

  const output = jsonLineQueue(child.stdout);
  const initMessages = [
    await output.next(),
    await output.next(),
    await output.next(),
    await output.next(),
  ];
  assert.deepEqual(
    initMessages.map((item) => item.sessionId).sort(),
    [0, 1, 2, 3],
  );
  assert.ok(initMessages.every((item) => (
    item.stage === "init" && item.deviceToken === "init:1"
  )));

  const completion = (sessionId, getter) => ({
    sessionId,
    complete: true,
    getterArguments: [getter],
    interactionEvents: [{
      type: "mousemove",
      x: sessionId + 1,
      y: 2,
      timeStamp: 10,
      isTrusted: true,
    }],
    postInteractionDelayMs: 0,
  });
  child.stdin.write(`${JSON.stringify({ sessionId: 3, cancel: true })}\n`);
  child.stdin.write(`${JSON.stringify(completion(2, "fail"))}\n`);
  child.stdin.write(`${JSON.stringify(completion(1, "slot-1"))}\n`);
  child.stdin.write(`${JSON.stringify(completion(0, "slot-0"))}\n`);

  const terminalMessages = [
    await output.next(),
    await output.next(),
    await output.next(),
  ];
  const verifyMessages = terminalMessages.filter(
    (item) => item.stage === "verify",
  );
  const bySession = new Map(
    verifyMessages.map((item) => [item.sessionId, item]),
  );
  assert.equal(bySession.get(0).verifyDeviceToken, "slot-0:2");
  assert.equal(bySession.get(1).verifyDeviceToken, "slot-1:2");
  assert.ok(verifyMessages.every((item) => item.stage === "verify"));
  const failure = terminalMessages.find((item) => item.sessionId === 2);
  assert.equal(failure.stage, "error");
  assert.equal(failure.error, "Node 设备桥执行失败");
  assert.doesNotMatch(JSON.stringify(failure), /sensitive getter input/);

  const [exitCode] = await exitPromise;
  assert.equal(exitCode, 0, stderr);
  assert.equal(stderr, "");
});


test("persistent challenge-host 可原位重建单个 VM 槽位", async (t) => {
  const directory = await mkdtemp(path.join(tmpdir(), "ali-host-reset-test-"));
  const sdkPath = path.join(directory, "fixture-sdk.js");
  await writeFile(sdkPath, [
    "(()=>{",
    "const proto={ACCESS_SEC:'access',SESSION_ID_SALT:'salt',",
    "APP_NAME:'app',APP_KEY:'key',apply(next){Object.assign(this,next)}};",
    "const runtime=Object.create(proto);",
    "runtime.apply({prefix:'p',region:'cn',appName:'app',appKey:'key',",
    "endpoints:[],deviceCallback(){}});",
    "runtime.deviceConfig={key:'0123456789abcdef',sessionId:'session',",
    "version:'1.0',pluginElements:'',pluginResource:'',globalVariable:'',",
    "timestamp:'1',ip:'127.0.0.1'};",
    "globalThis.z_um={count:0,getToken(value='init'){",
    "this.count+=1;return `${value}:${this.count}`}};",
    "runtime.DeviceToken=globalThis.z_um.getToken();",
    "})();",
  ].join(""), "utf8");

  const encodedProfile = Buffer.from(
    JSON.stringify(makeHostTestProfile()),
    "utf8",
  ).toString("base64");
  const child = spawn(process.execPath, [
    DEVICE_BRIDGE_PATH,
    "--mode", "challenge-host",
    "--sdk", sdkPath,
    "--prefix", "p",
    "--region", "cn",
    "--timeout-ms", "2000",
    "--device-profile", encodedProfile,
    "--vm-count", "2",
    "--persistent-host",
  ], { stdio: ["pipe", "pipe", "pipe"] });
  const exitPromise = once(child, "exit");
  let stderr = "";
  child.stderr.setEncoding("utf8");
  child.stderr.on("data", (chunk) => { stderr += chunk; });
  t.after(async () => {
    if (child.exitCode === null) child.kill();
    await rm(directory, { recursive: true, force: true });
  });

  const output = jsonLineQueue(child.stdout);
  const initial = [await output.next(), await output.next()];
  assert.deepEqual(
    initial.map((item) => item.sessionId).sort(),
    [0, 1],
  );

  const completion = (sessionId, getter) => ({
    sessionId,
    complete: true,
    getterArguments: [getter],
    interactionEvents: [{
      type: "mousemove",
      x: sessionId + 1,
      y: 2,
      timeStamp: 10,
      isTrusted: true,
    }],
    postInteractionDelayMs: 0,
  });
  child.stdin.write(`${JSON.stringify(completion(0, "first"))}\n`);
  const firstVerify = await output.next();
  assert.equal(firstVerify.sessionId, 0);
  assert.equal(firstVerify.verifyDeviceToken, "first:2");

  child.stdin.write(`${JSON.stringify({ sessionId: 0, reset: true })}\n`);
  const resetInit = await output.next();
  assert.equal(resetInit.sessionId, 0);
  assert.equal(resetInit.stage, "init");
  assert.equal(resetInit.deviceToken, "init:1");

  child.stdin.write(`${JSON.stringify(completion(0, "second"))}\n`);
  child.stdin.write(`${JSON.stringify(completion(1, "sibling"))}\n`);
  const finalMessages = [await output.next(), await output.next()];
  const bySession = new Map(
    finalMessages.map((item) => [item.sessionId, item]),
  );
  assert.equal(bySession.get(0).verifyDeviceToken, "second:2");
  assert.equal(bySession.get(1).verifyDeviceToken, "sibling:2");

  child.stdin.end();
  const [exitCode] = await exitPromise;
  assert.equal(exitCode, 0, stderr);
  assert.equal(stderr, "");
});
