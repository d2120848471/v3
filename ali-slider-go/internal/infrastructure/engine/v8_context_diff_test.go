package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/domain/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/foundation/runtimekit"
	"github.com/d2120848471/v3/ali-slider-go/internal/platform/v8runtime"
)

const v8ContextProbeSource = `(() => {
  const literalObjectPrototype = Object.getPrototypeOf({});
  const literalArrayPrototype = Object.getPrototypeOf([]);
  const iframe = document.createElement("iframe");
  const frame = iframe.contentWindow;
  const frameInternal = frame.Function("return {" +
    "globalThisMatchesThis:globalThis===this," +
    "windowMatchesGlobalThis:window===globalThis," +
    "selfMatchesGlobalThis:self===globalThis," +
    "parentMatchesGlobalThis:parent===globalThis," +
    "topMatchesGlobalThis:top===globalThis," +
    "temporalType:typeof Temporal};")();
  return {
    evalLength: eval.toString().length,
    globalTag: Object.prototype.toString.call(globalThis),
    globalSelf: globalThis.globalThis === globalThis,
    selfSelf: self === globalThis,
    windowSelf: window === globalThis,
    mappedObjectMatchesLiteral: Object.prototype === literalObjectPrototype,
    mappedArrayMatchesLiteral: Array.prototype === literalArrayPrototype,
    literalObjectNames: Object.getOwnPropertyNames(literalObjectPrototype).sort(),
    literalArrayNames: Object.getOwnPropertyNames(literalArrayPrototype).sort(),
    globals: Object.getOwnPropertyNames(globalThis).sort(),
    types: {
      Buffer: typeof Buffer,
      Worker: typeof Worker,
      process: typeof process,
      require: typeof require,
      structuredClone: typeof structuredClone,
    },
    iframe: {
      globalSelf: frame.globalThis === frame,
      selfSelf: frame.self === frame,
      windowSelf: frame.window === frame,
      parentIsMain: frame.parent === globalThis,
      topIsMain: frame.top === globalThis,
      frameElementMatches: frame.frameElement === iframe,
      objectPrototypeSeparate: frame.Object.prototype !== Object.prototype,
      arrayPrototypeSeparate: frame.Array.prototype !== Array.prototype,
      temporalType: typeof frame.Temporal,
      documentDefaultViewMatches: frame.document.defaultView === frame,
      internal: frameInternal,
    },
  };
})()`

func TestNodeAndV8BrowserContextContract(t *testing.T) {
	libraryPath := os.Getenv("ALI_SLIDER_V8_TEST_LIBRARY")
	if libraryPath == "" {
		t.Skip("ALI_SLIDER_V8_TEST_LIBRARY is not set")
	}
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node oracle is unavailable")
	}
	sources := runtimekit.NewSystemSources()
	profile, err := device.GenerateProfile(sources.Entropy)
	if err != nil {
		t.Fatal(err)
	}
	profileJSON, err := marshalBridgeProfile(profile)
	if err != nil {
		t.Fatal(err)
	}

	nodeResult := runNodeContextProbe(t, nodePath, profileJSON)
	v8Result := runV8ContextProbe(t, libraryPath, sources, profileJSON)
	if string(nodeResult) != string(v8Result) {
		t.Fatalf("browser context differs\nNode: %s\nV8:   %s", nodeResult, v8Result)
	}
}

func runNodeContextProbe(t *testing.T, nodePath string, profileJSON []byte) json.RawMessage {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate context diff test source failed")
	}
	bridgePath := filepath.Join(filepath.Dir(sourceFile), "runtime", "sdk_device_bridge.mjs")
	probeJSON, err := json.Marshal(v8ContextProbeSource)
	if err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`
    import vm from "node:vm";
    import { pathToFileURL } from "node:url";
    const bridge = await import(pathToFileURL(%q).href);
    const profile = %s;
    const context = bridge.makeBrowserContext({
      prefix: "fsgtmi",
      region: "cn",
      timeoutMs: 2000,
      networkEnabled: false,
      deviceProfile: profile,
      mode: "probe-log1",
    }, () => {});
    process.stdout.write(JSON.stringify(vm.runInContext(%s, context)));
  `, bridgePath, profileJSON, probeJSON)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, nodePath, "--input-type=module", "--eval", script).CombinedOutput()
	if err != nil {
		t.Fatalf("Node context probe failed: %v: %s", err, output)
	}
	if !json.Valid(output) {
		t.Fatalf("Node context probe returned invalid JSON: %q", output)
	}
	return json.RawMessage(strings.TrimSpace(string(output)))
}

func runV8ContextProbe(
	t *testing.T,
	libraryPath string,
	sources runtimekit.Sources,
	profileJSON []byte,
) json.RawMessage {
	t.Helper()
	library, err := v8runtime.Open(libraryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := library.Close(); err != nil {
			t.Errorf("Library.Close() error = %v", err)
		}
	}()
	host := newV8HostHandler(nil, sources.Entropy, false, nil)
	engine, err := library.NewWithHost(host)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := engine.Close(); err != nil {
			t.Errorf("Runtime.Close() error = %v", err)
		}
	}()
	sdkBundle, err := buildV8SDKBundle()
	if err != nil {
		t.Fatal(err)
	}
	for _, script := range []struct {
		name   string
		source string
	}{
		{"v8-host-bootstrap.js", v8HostBootstrapSource},
		{"sdk-device-v8-bundle.js", sdkBundle},
	} {
		if result, err := engine.Eval(context.Background(), script.source, script.name); err != nil || string(result) != "true" {
			t.Fatalf("Eval(%s) = %s, %v", script.name, result, err)
		}
	}
	probeJSON, err := json.Marshal(v8ContextProbeSource)
	if err != nil {
		t.Fatal(err)
	}
	setup := fmt.Sprintf(`
    globalThis.__aliV8ContextProbe = ({ profile }) => {
      const context = __aliSdkModule.makeBrowserContext({
        prefix: "fsgtmi",
        region: "cn",
        timeoutMs: 2000,
        networkEnabled: false,
        deviceProfile: profile,
        mode: "probe-log1",
      }, () => {});
      return __aliNodeCompat.vm.runInContext(
        %s,
        context,
        { filename: "browser-context-probe.js", timeout: 2000 },
      );
    };
    true;
  `, probeJSON)
	if result, err := engine.Eval(context.Background(), setup, "browser-context-probe-setup.js"); err != nil || string(result) != "true" {
		t.Fatalf("Eval(probe setup) = %s, %v", result, err)
	}
	payload := json.RawMessage(`{"profile":` + string(profileJSON) + `}`)
	callContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result, err := engine.Call(callContext, "__aliV8ContextProbe", payload)
	if err != nil {
		t.Fatalf("V8 context probe failed: %v", err)
	}
	return result
}
