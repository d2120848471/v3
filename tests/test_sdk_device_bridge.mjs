import assert from "node:assert/strict";
import test from "node:test";
import vm from "node:vm";

import {
  installSdkStateCapture,
  isSdkRuntimeState,
} from "../ali_slider_reverse/runtime/bridges/sdk_device_bridge.mjs";
import {
  loadSdkRuntime,
} from "../ali_slider_reverse/runtime/bridges/pe_data_bridge.mjs";


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
