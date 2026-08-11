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
    const parsedSliding = bridge.parseWorkerCompletionPayload({
      complete: true,
      mode: "sliding",
      sceneId: "159tlu75",
      certifyId: "fixture-sliding-certify",
      captchaType: "sliding",
      deviceToken: "fixture-device-token",
      staticPath: "3.22.0/sg.092.fixture.js",
      track: [
        {type:"touchstart",x:0,y:1,dt:0,force:0.6,radiusX:12,radiusY:11},
        {type:"touchmove",x:370,y:1,dt:200,force:0.6,radiusX:12,radiusY:11},
        {type:"touchend",x:370,y:1,dt:50,force:0.5,radiusX:12,radiusY:11},
      ],
      slideWidth: 418,
      handleWidth: 48,
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
    const verifyRequests = [];
    const verifyContext = bridge.makeBrowserContext({
      prefix: "1ulc59",
      region: "cn",
      timeoutMs: 2000,
      networkEnabled: true,
      deviceProfile: %s,
      mode: "challenge-worker",
    }, (request) => verifyRequests.push(request));
    verifyContext.__ALI_SDK_INIT__ = {
      sceneId: "159tlu75",
      certifyId: "fixture-sliding-certify",
      captchaType: "SLIDING",
      consumed: true,
      verifyRequested: false,
      verifyResult: null,
      phase: "ready",
    };
    const originalVerifyFetch = globalThis.fetch;
    let verifyFetchCount = 0;
    globalThis.fetch = async () => {
      verifyFetchCount += 1;
      return new Response(JSON.stringify({
        Code: "Success",
        Result: {
          VerifyCode: "T001",
          VerifyResult: true,
          securityToken: "fixture-security-token",
          certifyId: "fixture-sliding-certify",
        },
      }), {status: 200});
    };
    const sendVerify = () => new Promise((resolve) => {
      const xhr = new verifyContext.XMLHttpRequest();
      xhr.open("POST", "https://1ulc59.captcha-open.aliyuncs.com/", true);
      xhr.onload = () => resolve("load");
      xhr.onerror = () => resolve("error");
      xhr.send(new URLSearchParams({
        Action: "VerifyCaptchaV3",
        SceneId: "159tlu75",
        CertifyId: "fixture-sliding-certify",
        CaptchaVerifyParam: "fixture-device-parameter",
      }).toString());
    });
    const firstVerifyState = await sendVerify();
    const secondVerifyState = await sendVerify();
    globalThis.fetch = originalVerifyFetch;
    const duplicateVerifyBlocked = (
      firstVerifyState === "load"
      && secondVerifyState === "error"
      && verifyFetchCount === 1
      && verifyRequests.length === 2
    );
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
    let tracelessDelayBeforeSuccess = null;
    const tracelessContext = bridge.makeBrowserContext({
      prefix: "1ohgtl",
      region: "cn",
      timeoutMs: 2000,
      networkEnabled: true,
      deviceProfile: %s,
      mode: "challenge-worker",
    }, () => {});
    tracelessContext.initAliyunCaptcha = (options) => {
      tracelessDelayBeforeSuccess = options.delayBeforeSuccess;
      tracelessContext.document.querySelector(options.button).addEventListener(
        "click",
        () => {
          tracelessContext.__ALI_SDK_INIT__.consumed = true;
          tracelessContext.__ALI_SDK_INIT__.observedDeviceToken = "fixture-device-token";
          tracelessContext.__ALI_SDK_INIT__.verifyResult = {
            verifyCode: "T001",
            verifyResult: true,
            securityToken: "fixture-security-token",
            certifyId: "fixture-certify",
          };
          options.success("fixture-success");
        },
      );
      options.getInstance({destroy() {}});
    };
    const tracelessResult = await bridge.runTracelessCaptcha(
      tracelessContext,
      {},
      {prefix:"1ohgtl", timeoutMs:2000},
      {
        sceneId:"wa3238du",
        certifyId:"fixture-certify",
        staticPath:"3.29.0/pe.072.64e9154e3053635f.js",
        captchaType:"TRACELESS",
        deviceToken:"fixture-device-token",
      },
    );
    const secret = "must-not-reflect";
    const fixedFailure = bridge.safeDeviceBridgeFailureMessage(new Error(secret));
    const codedError = new Error(secret);
    codedError.safeCode = "TRACELESS_INIT_THROW";
    const codedFailure = bridge.safeDeviceBridgeFailureMessage(codedError);
    const eventElement = context.document.createElement("div");
    let elementEventCount = 0;
    eventElement.addEventListener("touchstart", () => { elementEventCount += 1; });
    eventElement.dispatchEvent(new context.Event("touchstart", {cancelable: true}));
    const styleDefaultEmpty = eventElement.style.left === "";
    eventElement.style.left = 12;
    const styleAssignmentString = eventElement.style.left === "12";
    const adjacentParent = context.document.createElement("div");
    const adjacentChild = context.document.createElement("span");
    adjacentParent.appendChild(adjacentChild);
    adjacentChild.insertAdjacentHTML(
      "beforebegin",
      '<span id="fixture-success-icon" class="verified"></span>',
    );
    const adjacentBeforeBegin = (
      adjacentParent.children.length === 2
      && adjacentParent.children[0].id === "fixture-success-icon"
      && adjacentParent.children[1] === adjacentChild
    );
    const slidingCodedError = new Error(secret);
    slidingCodedError.safeCode = "SLIDING_TIMEOUT_VERIFY_RESPONSE";
    const slidingCodedFailure = bridge.safeDeviceBridgeFailureMessage(slidingCodedError);
    process.stdout.write(JSON.stringify({
      dom,
      parsedMode: parsed.mode,
      parsedType: parsed.captchaType,
      parsedSlidingMode: parsedSliding.mode,
      parsedSlidingType: parsedSliding.captchaType,
      parsedSlidingDistance: parsedSliding.slideWidth - parsedSliding.handleWidth,
      verify,
      verifyMismatchRejected,
      invalidRejected,
      linkLoaded,
      fetchedURL,
      outsideRejected,
      fixedFailure,
      codedFailure,
      slidingCodedFailure,
      elementEventCount,
      styleDefaultEmpty,
      styleAssignmentString,
      adjacentBeforeBegin,
      duplicateVerifyBlocked,
      tracelessDelayBeforeSuccess,
      tracelessResultValid: (
        tracelessResult.captchaVerifyParam === "fixture-success"
        && tracelessResult.deviceToken === "fixture-device-token"
      ),
      reflected: (
        fixedFailure.includes(secret)
        || codedFailure.includes(secret)
        || slidingCodedFailure.includes(secret)
      ),
    }));
  `, bridgeURL, profileJSON, profileJSON, profileJSON)
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
		ParsedMode            string `json:"parsedMode"`
		ParsedType            string `json:"parsedType"`
		ParsedSlidingMode     string `json:"parsedSlidingMode"`
		ParsedSlidingType     string `json:"parsedSlidingType"`
		ParsedSlidingDistance int    `json:"parsedSlidingDistance"`
		Verify                struct {
			VerifyCode    string `json:"verifyCode"`
			VerifyResult  bool   `json:"verifyResult"`
			SecurityToken string `json:"securityToken"`
			CertifyID     string `json:"certifyId"`
		} `json:"verify"`
		VerifyMismatchRejected      bool   `json:"verifyMismatchRejected"`
		InvalidRejected             bool   `json:"invalidRejected"`
		LinkLoaded                  bool   `json:"linkLoaded"`
		FetchedURL                  string `json:"fetchedURL"`
		OutsideRejected             bool   `json:"outsideRejected"`
		FixedFailure                string `json:"fixedFailure"`
		CodedFailure                string `json:"codedFailure"`
		SlidingCodedFailure         string `json:"slidingCodedFailure"`
		ElementEventCount           int    `json:"elementEventCount"`
		StyleDefaultEmpty           bool   `json:"styleDefaultEmpty"`
		StyleAssignmentString       bool   `json:"styleAssignmentString"`
		AdjacentBeforeBegin         bool   `json:"adjacentBeforeBegin"`
		DuplicateVerifyBlocked      bool   `json:"duplicateVerifyBlocked"`
		TracelessDelayBeforeSuccess *bool  `json:"tracelessDelayBeforeSuccess"`
		TracelessResultValid        bool   `json:"tracelessResultValid"`
		Reflected                   bool   `json:"reflected"`
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
	if result.ParsedSlidingMode != "sliding" || result.ParsedSlidingType != "SLIDING" || result.ParsedSlidingDistance != 370 || result.ElementEventCount != 1 || !result.StyleDefaultEmpty || !result.StyleAssignmentString || !result.AdjacentBeforeBegin || !result.DuplicateVerifyBlocked {
		t.Fatalf("sliding completion/DOM contract=%+v", result)
	}
	if result.TracelessDelayBeforeSuccess == nil || *result.TracelessDelayBeforeSuccess || !result.TracelessResultValid {
		t.Fatalf("traceless fast-success contract=%+v", result)
	}
	if result.Verify.VerifyCode != "T001" || !result.Verify.VerifyResult || result.Verify.SecurityToken == "" || result.Verify.CertifyID != "fixture-certify" || !result.VerifyMismatchRejected {
		t.Fatalf("official Verify contract=%+v", result)
	}
	if !result.LinkLoaded || result.FetchedURL != "https://x.alicdn.com/captcha-frontend/dynamicJS/fixture.css" || !result.OutsideRejected {
		t.Fatalf("dynamic asset contract=%+v", result)
	}
	if result.Reflected || result.FixedFailure != "Node 设备桥执行失败" || !strings.Contains(result.CodedFailure, "TRACELESS_INIT_THROW") || !strings.Contains(result.SlidingCodedFailure, "SLIDING_TIMEOUT_VERIFY_RESPONSE") {
		t.Fatalf("failure redaction contract=%+v", result)
	}
}

func TestNodeSDKBridgeSlidingInteractionContract(t *testing.T) {
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
    const bridge = await import(%q);
    const requests = [];
    const context = bridge.makeBrowserContext({
      prefix: "1ulc59",
      region: "cn",
      timeoutMs: 2000,
      networkEnabled: true,
      deviceProfile: %s,
      mode: "challenge-worker",
    }, (request) => requests.push(request));
    let startCount = 0;
    let moveCount = 0;
    let endCount = 0;
    let lastX = 0;
    let verifyFetchCount = 0;
    const logicalDateSamples = [];
    const logicalPerformanceSamples = [];
    const eventTimeSamples = [];
    const recordClock = (event) => {
      logicalDateSamples.push(context.Date.now());
      logicalPerformanceSamples.push(context.performance.now());
      eventTimeSamples.push(event.timeStamp);
    };
    const originalFetch = globalThis.fetch;
    globalThis.fetch = async () => {
      verifyFetchCount += 1;
      return new Response(JSON.stringify({
        Code: "Success",
        Result: {
          VerifyCode: "T001",
          VerifyResult: true,
          securityToken: "fixture-upload-security-token",
          certifyId: "fixture-sliding-certify",
        },
      }), {status: 200});
    };
    context.initAliyunCaptcha = (options) => {
      const xhr = new context.XMLHttpRequest();
      xhr.open("POST", "https://1ulc59.captcha-open.aliyuncs.com/", true);
      xhr.onload = () => {
        const init = JSON.parse(xhr.responseText);
        const host = context.document.querySelector(options.element);
        host.insertAdjacentHTML("beforeend", '<div id="aliyunCaptcha-sliding-wrapper"></div>');
        const slider = context.document.querySelector("#aliyunCaptcha-sliding-slider");
        slider.addEventListener("mousedown", (event) => {
          startCount += 1;
          lastX = event.clientX;
          recordClock(event);
          event.preventDefault();
        });
        context.document.addEventListener("mousemove", (event) => {
          moveCount += 1;
          lastX = event.clientX;
          recordClock(event);
        });
        context.document.addEventListener("mouseup", (event) => {
          endCount += 1;
          lastX = event.clientX;
          recordClock(event);
          const verifyXHR = new context.XMLHttpRequest();
          verifyXHR.open("POST", "https://1ulc59.captcha-open.aliyuncs.com/", true);
          verifyXHR.onload = () => {
            options.success(context.btoa(JSON.stringify({
              certifyId: init.CertifyId,
              sceneId: options.SceneId,
              isSign: true,
              securityToken: "fixture-upload-security-token",
            })));
          };
          verifyXHR.send(new URLSearchParams({
            Action: "VerifyCaptchaV3",
            SceneId: options.SceneId,
            CertifyId: init.CertifyId,
            CaptchaVerifyParam: "fixture-device-parameter",
          }).toString());
        });
        options.getInstance({destroyCaptcha() {}});
      };
      xhr.send(new URLSearchParams({
        Action: "InitCaptchaV3",
        SceneId: options.SceneId,
        DeviceToken: context.__ALI_SDK_INIT__.observedDeviceToken || "fixture-device-token",
      }).toString());
    };
    const startedAt = performance.now();
    const result = await bridge.runSlidingCaptcha(
      context,
      {},
      {prefix:"1ulc59", timeoutMs:2000},
      {
        sceneId:"159tlu75",
        certifyId:"fixture-sliding-certify",
        staticPath:"3.22.0/sg.092.fixture.js",
        captchaType:"SLIDING",
        deviceToken:"fixture-device-token",
        slideWidth:418,
        handleWidth:48,
        track:[
          {type:"touchstart",x:0,y:1,dt:0,force:0.6,radiusX:12,radiusY:11},
          {type:"touchmove",x:180,y:2,dt:600,force:0.62,radiusX:13,radiusY:12},
          {type:"touchmove",x:370,y:1,dt:600,force:0.58,radiusX:12,radiusY:12},
          {type:"touchend",x:370,y:1,dt:600,force:0.55,radiusX:12,radiusY:11},
        ],
      },
    );
    globalThis.fetch = originalFetch;
    const decoded = JSON.parse(context.atob(result.captchaVerifyParam));
    process.stdout.write(JSON.stringify({
      startCount,
      moveCount,
      endCount,
      lastX,
      deviceTokenMatches: result.deviceToken === "fixture-device-token",
      decoded: {
        keys: Object.keys(decoded).sort(),
        certifyMatches: decoded.certifyId === "fixture-sliding-certify",
        sceneMatches: decoded.sceneId === "159tlu75",
        sign: decoded.isSign,
        tokenMatches: decoded.securityToken === "fixture-upload-security-token",
      },
      actions: requests.map((request) => new URLSearchParams(request.body).get("Action")),
      verifyFetchCount,
      wallElapsedMs: performance.now() - startedAt,
      zeroWallSourceContract: !/(?:wait|nextHostTurn|setTimeout)\s*\(/.test(
        bridge.replaySlidingTrack.toString(),
      ),
      logicalDateDeltaMs: logicalDateSamples.at(-1) - logicalDateSamples[0],
      logicalPerformanceDeltaMs: (
        logicalPerformanceSamples.at(-1) - logicalPerformanceSamples[0]
      ),
      eventTimeDeltaMs: eventTimeSamples.at(-1) - eventTimeSamples[0],
    }));
  `, bridgeURL, profileJSON)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, nodePath, "--input-type=module", "--eval", script).CombinedOutput()
	if err != nil {
		t.Fatalf("Node SDK sliding interaction failed: %v: %s", err, output)
	}
	var result struct {
		StartCount         int     `json:"startCount"`
		MoveCount          int     `json:"moveCount"`
		EndCount           int     `json:"endCount"`
		LastX              float64 `json:"lastX"`
		DeviceTokenMatches bool    `json:"deviceTokenMatches"`
		Decoded            struct {
			Keys           []string `json:"keys"`
			CertifyMatches bool     `json:"certifyMatches"`
			SceneMatches   bool     `json:"sceneMatches"`
			Sign           bool     `json:"sign"`
			TokenMatches   bool     `json:"tokenMatches"`
		} `json:"decoded"`
		Actions                   []string `json:"actions"`
		VerifyFetchCount          int      `json:"verifyFetchCount"`
		WallElapsedMS             float64  `json:"wallElapsedMs"`
		ZeroWallSourceContract    bool     `json:"zeroWallSourceContract"`
		LogicalDateDeltaMS        float64  `json:"logicalDateDeltaMs"`
		LogicalPerformanceDeltaMS float64  `json:"logicalPerformanceDeltaMs"`
		EventTimeDeltaMS          float64  `json:"eventTimeDeltaMs"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode Node sliding result: %v: %q", err, output)
	}
	if result.StartCount != 1 || result.MoveCount != 2 || result.EndCount != 1 || result.LastX != 394 || !result.DeviceTokenMatches {
		t.Fatalf("sliding mouse contract=%+v", result)
	}
	if strings.Join(result.Decoded.Keys, ",") != "certifyId,isSign,sceneId,securityToken" || !result.Decoded.CertifyMatches || !result.Decoded.SceneMatches || !result.Decoded.Sign || !result.Decoded.TokenMatches {
		t.Fatalf("success contract=%+v", result.Decoded)
	}
	if strings.Join(result.Actions, ",") != "InitCaptchaV3,VerifyCaptchaV3" || result.VerifyFetchCount != 1 {
		t.Fatalf("request actions=%v", result.Actions)
	}
	if !result.ZeroWallSourceContract || result.WallElapsedMS >= 250 {
		t.Fatalf("sliding replay used logical duration as wall time: %.0fms", result.WallElapsedMS)
	}
	if result.LogicalDateDeltaMS < 1_800 || result.LogicalDateDeltaMS >= 1_900 || result.LogicalPerformanceDeltaMS < 1_800 || result.LogicalPerformanceDeltaMS >= 1_900 || result.EventTimeDeltaMS != 1_800 {
		t.Fatalf("sliding logical clock mismatch: %+v", result)
	}
	t.Logf(
		"SLIDING zero-wall replay: wall=%.1fms logicalDate=%.1fms logicalPerformance=%.1fms eventTime=%.1fms",
		result.WallElapsedMS,
		result.LogicalDateDeltaMS,
		result.LogicalPerformanceDeltaMS,
		result.EventTimeDeltaMS,
	)
}
