package pe

import (
	_ "embed"
	"errors"
	"fmt"
	"strings"
)

const (
	sdkV8CoreStart = "const TARGET_ORIGIN"
	peV8CoreStart  = "const IDENTIFIER"
	v8ExportMarker = "\n\nexport {\n"
)

//go:embed runtime/v8_host_bootstrap.js
var v8HostBootstrapSource string

const sdkV8Prelude = `(() => {
"use strict";
const {
  Buffer,
  ExitSignal,
  Worker,
  createInterface,
  fs,
  isMainThread,
  parentPort,
  pathToFileURL,
  process,
  vm,
  webcrypto,
  workerData,
} = globalThis.__aliNodeCompat;
const {
  AbortController,
  Date,
  Intl,
  TextDecoder,
  TextEncoder,
  URL,
  URLSearchParams,
  clearInterval,
  clearTimeout,
  console,
  fetch,
  performance,
  queueMicrotask,
  setImmediate,
  setInterval,
  setTimeout,
} = globalThis;
`

const sdkV8Adapter = `

globalThis.__aliSdkModule = Object.freeze({
  browserRequestHeaders,
  callFeiLinGetter,
  callPeFeiLinGetter,
  decodeDeviceProfile,
  installSdkStateCapture,
  isSdkRuntimeState,
  makeBrowserContext,
  makeElementFactory,
  parseWorkerCompletionPayload,
  replayFeiLinInteractionEvents,
  refreshFeiLinToken,
  safeDeviceBridgeFailureMessage,
  selectFeiLinGetterOwner,
});

let v8DeviceMain = null;
let v8DeviceFailure = null;
let v8CompletionResolver = null;
const v8StageQueue = [];
const v8StageWaiters = [];

function v8NormalizeError(error) {
  const detail = error && typeof error.stack === "string"
    ? error.stack
    : String(error);
  return new Error(detail);
}

function v8FailDevice(error) {
  const failure = v8NormalizeError(error);
  v8DeviceFailure = failure;
  while (v8StageWaiters.length > 0) {
    v8StageWaiters.shift().reject(failure);
  }
}

function v8PublishStage(payload) {
  if (v8StageWaiters.length > 0) {
    v8StageWaiters.shift().resolve(payload);
    return;
  }
  v8StageQueue.push(payload);
}

function v8WaitForStage(expected) {
  if (v8StageQueue.length > 0) {
    const payload = v8StageQueue.shift();
    if (payload?.stage !== expected) {
      return Promise.reject(
        new Error("V8 Device stage mismatch: " + String(payload?.stage)),
      );
    }
    return Promise.resolve(payload);
  }
  if (v8DeviceFailure !== null) {
    return Promise.reject(v8DeviceFailure);
  }
  return new Promise((resolve, reject) => {
    v8StageWaiters.push({
      resolve(payload) {
        if (payload?.stage !== expected) {
          reject(
            new Error("V8 Device stage mismatch: " + String(payload?.stage)),
          );
          return;
        }
        resolve(payload);
      },
      reject,
    });
  });
}

writeBridgeOutput = v8PublishStage;
readWorkerCompletionInput = () => new Promise((resolve, reject) => {
  if (v8CompletionResolver !== null) {
    reject(new Error("V8 Device completion resolver already exists"));
    return;
  }
  v8CompletionResolver = (payload) => {
    v8CompletionResolver = null;
    try {
      resolve(parseWorkerCompletionPayload(payload));
    } catch (error) {
      reject(error);
    }
  };
});

globalThis.__aliV8DeviceOpen = async (input) => {
  if (!input || typeof input !== "object" || Array.isArray(input)) {
    throw new Error("V8 Device input must be an object");
  }
  if (v8DeviceMain !== null) {
    throw new Error("V8 Device runtime already has an active session");
  }
  const sdkSource = String(input.sdkSource ?? "");
  if (!sdkSource) throw new Error("V8 Device SDK source is empty");
  const profile = input.deviceProfile;
  if (!profile || typeof profile !== "object" || Array.isArray(profile)) {
    throw new Error("V8 Device profile must be an object");
  }

  fs._clear();
  fs._set("/sdk.js", sdkSource);
  v8DeviceFailure = null;
  v8CompletionResolver = null;
  v8StageQueue.length = 0;
  v8StageWaiters.length = 0;
  process.argv = [
    "v8",
    "sdk_device_bridge.mjs",
    "--mode",
    "challenge-worker",
    "--sdk",
    "/sdk.js",
    "--prefix",
    String(input.prefix ?? "fsgtmi"),
    "--region",
    String(input.region ?? "cn"),
    "--timeout-ms",
    String(input.timeoutMs ?? 15_000),
    "--device-profile",
    Buffer.from(JSON.stringify(profile), "utf8").toString("base64"),
  ];

  v8DeviceMain = main().then(
    () => ({ ok: true, error: null }),
    (error) => {
      if (error instanceof ExitSignal && error.code === 0) {
        return { ok: true, error: null };
      }
      const failure = v8NormalizeError(error);
      v8FailDevice(failure);
      return { ok: false, error: failure };
    },
  );
  return v8WaitForStage("init");
};

globalThis.__aliV8DeviceComplete = async (input) => {
  if (v8DeviceMain === null) {
    throw new Error("V8 Device runtime has no active session");
  }
  if (v8DeviceFailure !== null) throw v8DeviceFailure;
  if (v8CompletionResolver === null) {
    await Promise.resolve();
  }
  if (v8CompletionResolver === null) {
    throw new Error("V8 Device runtime is not awaiting completion");
  }
  const resolveCompletion = v8CompletionResolver;
  resolveCompletion(input);
  const stage = await v8WaitForStage("verify");
  const outcome = await v8DeviceMain;
  v8DeviceMain = null;
  if (!outcome.ok) throw outcome.error;
  return stage;
};
})();
true;
`

const peV8Prelude = `(() => {
"use strict";
const {
  Buffer,
  ExitSignal,
  fs,
  pathToFileURL,
  process,
  vm,
} = globalThis.__aliNodeCompat;
const {
  Date,
  clearInterval,
  clearTimeout,
  performance,
  queueMicrotask,
  setInterval,
  setTimeout,
} = globalThis;
const {
  decodeDeviceProfile,
  installSdkStateCapture,
  makeBrowserContext,
} = globalThis.__aliSdkModule;
`

const peV8Adapter = `

globalThis.__aliV8PERun = async (input) => {
  if (!input || typeof input !== "object" || Array.isArray(input)) {
    throw new Error("V8 PE input must be an object");
  }
  const sdkSource = String(input.sdkSource ?? "");
  const peSource = String(input.peSource ?? "");
  const bridgeInput = input.bridgeInput;
  const profile = input.deviceProfile;
  if (!sdkSource || !peSource) {
    throw new Error("V8 PE public script source is empty");
  }
  if (!bridgeInput || typeof bridgeInput !== "object" || Array.isArray(bridgeInput)) {
    throw new Error("V8 PE bridge input must be an object");
  }
  if (!profile || typeof profile !== "object" || Array.isArray(profile)) {
    throw new Error("V8 PE profile must be an object");
  }

  fs._clear();
  fs._set("/sdk.js", sdkSource);
  fs._set("/pe.js", peSource);
  fs._set(0, JSON.stringify(bridgeInput));
  process.argv = [
    "v8",
    "pe_key_bridge.mjs",
    "--sdk",
    "/sdk.js",
    "--pe",
    "/pe.js",
    "--output-mode",
    String(input.outputMode ?? "profile"),
    "--prefix",
    String(input.prefix ?? "fsgtmi"),
    "--region",
    String(input.region ?? "cn"),
    "--timeout-ms",
    String(input.timeoutMs ?? 20_000),
    "--device-profile",
    Buffer.from(JSON.stringify(profile), "utf8").toString("base64"),
  ];

  try {
    await main();
  } catch (error) {
    if (!(error instanceof ExitSignal) || error.code !== 0) throw error;
  }
  const writes = fs._takeWrites().filter((entry) => entry.fd === 1);
  if (writes.length !== 1) {
    throw new Error("V8 PE bridge wrote " + writes.length + " stdout records");
  }
  return JSON.parse(writes[0].value.trim());
};
})();
true;
`

func extractV8BridgeCore(source []byte, startMarker string) (string, error) {
	text := strings.ReplaceAll(string(source), "\r\n", "\n")
	start := strings.Index(text, startMarker)
	if start < 0 {
		return "", fmt.Errorf("V8 bridge start marker %q is missing", startMarker)
	}
	endRelative := strings.Index(text[start:], v8ExportMarker)
	if endRelative < 0 {
		return "", errors.New("V8 bridge export marker is missing")
	}
	core := text[start : start+endRelative]
	if strings.Contains(core, "\nimport ") || strings.Contains(core, "\nexport ") {
		return "", errors.New("V8 bridge core still contains ESM syntax")
	}
	return core, nil
}

func buildV8SDKBundle() (string, error) {
	core, err := extractV8BridgeCore(sdkDeviceBridgeSource, sdkV8CoreStart)
	if err != nil {
		return "", fmt.Errorf("build V8 SDK bundle: %w", err)
	}
	// challenge-host 的 Worker 分支不会在 V8 模式启用，但 import.meta 即使位于
	// 未调用函数中也会让 classic script 解析失败，因此只替换这一个模块 URL。
	if strings.Count(core, "import.meta.url") != 1 {
		return "", errors.New("build V8 SDK bundle: unexpected import.meta count")
	}
	core = strings.ReplaceAll(
		core,
		"import.meta.url",
		`"file:///sdk_device_bridge.mjs"`,
	)
	return sdkV8Prelude + core + sdkV8Adapter, nil
}

func buildV8PEBundle() (string, error) {
	core, err := extractV8BridgeCore(peKeyBridgeSource, peV8CoreStart)
	if err != nil {
		return "", fmt.Errorf("build V8 PE bundle: %w", err)
	}
	return peV8Prelude + core + peV8Adapter, nil
}
