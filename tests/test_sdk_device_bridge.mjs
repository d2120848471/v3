import assert from "node:assert/strict";
import test from "node:test";
import vm from "node:vm";

import {
  installSdkStateCapture,
  instrumentSdkStateCapture,
  isSdkRuntimeState,
} from "../ali_slider_reverse/runtime/bridges/sdk_device_bridge.mjs";
import {
  loadSdkRuntime,
} from "../ali_slider_reverse/runtime/bridges/pe_data_bridge.mjs";


function exerciseVariant({ methodDeclaration, ownerName, patchName }) {
  const source = [
    "(()=>{",
    "const statePrototype={",
    "ACCESS_SEC:'access',SESSION_ID_SALT:'salt',",
    "APP_NAME:'app',APP_KEY:'key',",
    `${methodDeclaration}{Object.assign(this,${patchName})}`,
    "};",
    `const ${ownerName}=Object.create(statePrototype);`,
    `${ownerName}._extend({prefix:'p',region:'cn',appName:'app',`,
    "appKey:'key',endpoints:['https://example.invalid'],",
    "deviceCallback(){}});",
    `globalThis.result=${ownerName};`,
    "})();",
  ].join("");
  const transformed = instrumentSdkStateCapture(source);
  let captured = null;
  const context = vm.createContext({});
  installSdkStateCapture(context, (owner) => {
    captured = owner;
  });
  vm.runInContext(transformed.source, context);
  return { captured, context, transformed };
}


test("SDK 状态探针不依赖 owner 或 patch 的混淆变量名", () => {
  for (const variant of [
    {
      methodDeclaration: "_extend:function(payload)",
      ownerName: "renamedRuntime",
      patchName: "payload",
    },
    {
      methodDeclaration: "'_extend':function($0)",
      ownerName: "$state9",
      patchName: "$0",
    },
  ]) {
    const { captured, context, transformed } = exerciseVariant(variant);
    assert.equal(transformed.count, 1);
    assert.equal(captured, context.result);
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


test("PE 桥通过共享语义探针取得运行态，不依赖 SDK 压缩变量名", () => {
  const context = vm.createContext({});
  const runtime = loadSdkRuntime(
    context,
    [
      "(()=>{",
      "const proto={ACCESS_SEC:'access',SESSION_ID_SALT:'salt',",
      "APP_NAME:'app',APP_KEY:'key',",
      "_extend:function(nextState){Object.assign(this,nextState)}",
      "};",
      "const completelyRenamedOwner=Object.create(proto);",
      "completelyRenamedOwner._extend({prefix:'p',region:'cn',",
      "appName:'app',appKey:'key',endpoints:[],deviceCallback(){}});",
      "})();",
    ].join(""),
    "renamed-sdk.js",
    1_000,
  );
  assert.equal(runtime.APP_NAME, "app");
  assert.equal(runtime.prefix, "p");
});
