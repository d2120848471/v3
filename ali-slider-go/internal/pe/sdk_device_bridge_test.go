package pe

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/runtimekit"
)

func TestNodeSDKBridgeTracelessContracts(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node runtime is unavailable")
	}
	profile, err := device.GenerateProfile(runtimekit.NewSystemSources().Entropy)
	if err != nil {
		t.Fatal(err)
	}
	profileJSON, err := marshalBridgeProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate SDK bridge test source failed")
	}
	bridgePath := filepath.Join(filepath.Dir(sourceFile), "runtime", "sdk_device_bridge.mjs")
	bridgeURL := (&url.URL{Scheme: "file", Path: bridgePath}).String()
	script := fmt.Sprintf(`
    import vm from "node:vm";
    const bridge = await import(%q);
    const context = bridge.makeBrowserContext({
      prefix: "1ohgtl",
      region: "cn",
      timeoutMs: 2000,
      networkEnabled: true,
      deviceProfile: %s,
      mode: "challenge-worker",
    }, () => {});
    const dom = vm.runInContext("({" +
      "nodeListType:typeof NodeList," +
      "element:document.createElement('div') instanceof Element," +
      "htmlElement:document.createElement('div') instanceof HTMLElement" +
    "})", context);
    const parsed = bridge.parseWorkerCompletionPayload({
      complete: true,
      mode: "traceless",
      sceneId: "wa3238du",
      certifyId: "fixture-certify",
      captchaType: "traceless",
      deviceToken: "fixture-device-token",
      staticPath: "3.29.0/pe.072.64e9154e3053635f.js",
    });
    const verify = bridge.parseTracelessVerifyResponse(JSON.stringify({
      Code: "Success",
      Result: {
        VerifyCode: "T001",
        VerifyResult: true,
        securityToken: "fixture-security-token",
        certifyId: "fixture-certify",
      },
    }), "fixture-certify");
    let verifyMismatchRejected = false;
    try {
      bridge.parseTracelessVerifyResponse(JSON.stringify({
        Result: {
          VerifyCode: "T001",
          VerifyResult: true,
          securityToken: "fixture-security-token",
          certifyId: "other-certify",
        },
      }), "fixture-certify");
    } catch {
      verifyMismatchRejected = true;
    }
    let invalidRejected = false;
    try {
      bridge.parseWorkerCompletionPayload({
        ...parsed,
        extra: true,
      });
    } catch {
      invalidRejected = true;
    }
    const originalFetch = globalThis.fetch;
    let fetchedURL = "";
    globalThis.fetch = async (value) => {
      fetchedURL = String(value);
      return new Response("fixture-style", {status: 200});
    };
    const link = context.document.createElement("link");
    link.href = "https://x.alicdn.com/captcha-frontend/dynamicJS/fixture.css";
    const linkLoaded = await new Promise((resolve, reject) => {
      link.onload = () => resolve(true);
      link.onerror = reject;
      context.document.head.appendChild(link);
    });
    let outsideRejected = false;
    try {
      const outside = context.document.createElement("link");
      outside.href = "https://example.com/fixture.css";
      context.document.head.appendChild(outside);
    } catch {
      outsideRejected = true;
    }
    globalThis.fetch = originalFetch;
    const secret = "must-not-reflect";
    const fixedFailure = bridge.safeDeviceBridgeFailureMessage(new Error(secret));
    const codedError = new Error(secret);
    codedError.safeCode = "TRACELESS_INIT_THROW";
    const codedFailure = bridge.safeDeviceBridgeFailureMessage(codedError);
    process.stdout.write(JSON.stringify({
      dom,
      parsedMode: parsed.mode,
      parsedType: parsed.captchaType,
      verify,
      verifyMismatchRejected,
      invalidRejected,
      linkLoaded,
      fetchedURL,
      outsideRejected,
      fixedFailure,
      codedFailure,
      reflected: fixedFailure.includes(secret) || codedFailure.includes(secret),
    }));
  `, bridgeURL, profileJSON)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, nodePath, "--input-type=module", "--eval", script).CombinedOutput()
	if err != nil {
		t.Fatalf("Node SDK bridge contract failed: %v: %s", err, output)
	}
	var result struct {
		DOM struct {
			NodeListType string `json:"nodeListType"`
			Element      bool   `json:"element"`
			HTMLElement  bool   `json:"htmlElement"`
		} `json:"dom"`
		ParsedMode string `json:"parsedMode"`
		ParsedType string `json:"parsedType"`
		Verify     struct {
			VerifyCode    string `json:"verifyCode"`
			VerifyResult  bool   `json:"verifyResult"`
			SecurityToken string `json:"securityToken"`
			CertifyID     string `json:"certifyId"`
		} `json:"verify"`
		VerifyMismatchRejected bool   `json:"verifyMismatchRejected"`
		InvalidRejected        bool   `json:"invalidRejected"`
		LinkLoaded             bool   `json:"linkLoaded"`
		FetchedURL             string `json:"fetchedURL"`
		OutsideRejected        bool   `json:"outsideRejected"`
		FixedFailure           string `json:"fixedFailure"`
		CodedFailure           string `json:"codedFailure"`
		Reflected              bool   `json:"reflected"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode Node SDK bridge result: %v: %q", err, output)
	}
	if result.DOM.NodeListType != "function" || !result.DOM.Element || !result.DOM.HTMLElement {
		t.Fatalf("DOM contract=%+v", result.DOM)
	}
	if result.ParsedMode != "traceless" || result.ParsedType != "TRACELESS" || !result.InvalidRejected {
		t.Fatalf("completion contract=%+v", result)
	}
	if result.Verify.VerifyCode != "T001" || !result.Verify.VerifyResult || result.Verify.SecurityToken == "" || result.Verify.CertifyID != "fixture-certify" || !result.VerifyMismatchRejected {
		t.Fatalf("official Verify contract=%+v", result)
	}
	if !result.LinkLoaded || result.FetchedURL != "https://x.alicdn.com/captcha-frontend/dynamicJS/fixture.css" || !result.OutsideRejected {
		t.Fatalf("dynamic asset contract=%+v", result)
	}
	if result.Reflected || result.FixedFailure != "Node 设备桥执行失败" || !strings.Contains(result.CodedFailure, "TRACELESS_INIT_THROW") {
		t.Fatalf("failure redaction contract=%+v", result)
	}
}
