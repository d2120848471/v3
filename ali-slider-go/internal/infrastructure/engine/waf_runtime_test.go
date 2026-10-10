package engine

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/domain/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/pe"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/waf"
	"github.com/d2120848471/v3/ali-slider-go/internal/foundation/runtimekit"
)

const wafFixtureSDK = `
if (document.URL !== location.href || document.domain !== location.hostname || location.hostname !== 'example.com') throw new Error('inconsistent WAF document');
if (window.isSecureContext !== true || document.createElement('iframe').contentWindow.isSecureContext !== true) throw new Error('missing HTTPS secure context');
if ((function(){}).constructor !== Function || Function('return this')() !== window || Function('return document')() !== document) throw new Error('WAF Function uses another realm');
const uaGetter = Object.getOwnPropertyDescriptor(Navigator.prototype, 'userAgent')?.get;
if (Object.hasOwn(navigator, 'userAgent') || typeof uaGetter !== 'function' ||
    uaGetter.constructor !== Function || uaGetter.length !== 0 ||
    !Function.prototype.toString.call(uaGetter).includes('[native code]')) throw new Error('invalid Navigator binding');
let rejectedReceiver = false;
try { uaGetter.call({}); } catch (error) { rejectedReceiver = error instanceof TypeError; }
if (!rejectedReceiver || uaGetter.call(navigator) !== navigator.userAgent) throw new Error('invalid Navigator receiver');
const canvas = document.createElement('canvas');
if (Object.hasOwn(canvas, 'toDataURL') || Object.getPrototypeOf(canvas) !== HTMLCanvasElement.prototype ||
    canvas.toDataURL.constructor !== Function || !canvas.toDataURL().startsWith('data:image/png;base64,')) throw new Error('invalid Canvas binding');
const child = document.createElement('iframe').contentWindow;
if (Object.hasOwn(child.navigator, 'userAgent') || uaGetter.call(child.navigator) !== navigator.userAgent ||
    child.document.createElement('canvas').toDataURL() !== canvas.toDataURL()) throw new Error('inconsistent iframe binding');
if (window.AliyunCaptchaConfig) throw new Error('unexpected V3 device bootstrap');
window.initAliyunCaptcha = options => {
  if (options.verifyType !== '1.0' || options.userId !== 'fixture-user' ||
      options.userUserId !== 'fixture-user-user' || options.UserCertifyId !== 'fixture-trace' ||
      options.region !== 'sgp' || options.language !== 'en') throw new Error('WAF config not preserved');
  const init = new XMLHttpRequest();
  init.open('POST', 'https://fixture.captcha-pro-open.aliyuncs.com/');
  init.onload = () => {
    const challenge = JSON.parse(init.responseText);
    const instance = {config:{}, destroyCaptcha(){}};
    setTimeout(() => {
      window.um = window.z_um = {getToken(){return 'fixture-device-token'}};
      const device = new XMLHttpRequest();
      device.open('POST', 'https://device.captcha-open.aliyuncs.com/');
      device.onload = () => {instance.config.DeviceToken = 'fixture-device-token'};
      device.send('Action=Log2');
    }, 20);
    const host = document.querySelector(options.element);
    host.insertAdjacentHTML('beforeend', '<div id="aliyunCaptcha-sliding-wrapper"></div>');
    let touchStarted = false, touchMoved = false;
    document.addEventListener('mousedown', () => {throw new Error('mobile WAF received mouse input')});
    document.addEventListener('touchstart', event => {
      if (!instance.config.DeviceToken || event.touches.length !== 1) throw new Error('invalid touch start');
      touchStarted = true;
    });
    document.addEventListener('touchmove', event => {
      if (!touchStarted || event.touches.length !== 1) throw new Error('invalid touch move');
      touchMoved = true;
    });
    document.addEventListener('touchend', event => {
      if (!instance.config.DeviceToken) throw new Error('interaction before device readiness');
      if (!touchMoved || event.touches.length !== 0 || event.changedTouches.length !== 1) throw new Error('incomplete touch input');
      const verify = new XMLHttpRequest();
      verify.open('POST', 'https://fixture.captcha-pro-open.aliyuncs.com/');
      verify.onload = () => options.success('fixture-signature');
      verify.send(new URLSearchParams({Action:'VerifyCaptchaV2',SceneId:options.SceneId,CertifyId:challenge.CertifyId}));
      const duplicate = new XMLHttpRequest();
      duplicate.open('POST', 'https://fixture.captcha-pro-open.aliyuncs.com/');
      duplicate.send(new URLSearchParams({Action:'VerifyCaptchaV2',SceneId:options.SceneId,CertifyId:challenge.CertifyId}));
    });
    options.getInstance(instance);
  };
  init.send(new URLSearchParams({Action:'InitCaptchaV2',SceneId:options.SceneId,UserId:options.userId,UserUserId:options.userUserId,UserCertifyId:options.UserCertifyId,DeviceData:'fixture-data'}));
};
`

func TestWAFVerifyCannotBeTransparentlyReplayed(t *testing.T) {
	body := "Action=VerifyCaptchaV2&SceneId=fixture&CertifyId=fixture"
	transport := v8RoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.GetBody != nil {
			t.Fatal("WAF Verify retains an HTTP replay body")
		}
		if request.ContentLength != int64(len(body)) {
			t.Fatal("WAF Verify content length changed")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})
	_, err := roundTripV8HostRequest(context.Background(), transport, v8HostHTTPRequest{
		URL: "https://fixture.captcha-pro-open.aliyuncs.com/", Method: http.MethodPost, Body: &body,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestV8WAFRuntimeOffline(t *testing.T) {
	library := os.Getenv("ALI_SLIDER_V8_TEST_LIBRARY")
	if library == "" {
		t.Skip("ALI_SLIDER_V8_TEST_LIBRARY is not set")
	}
	for _, scenario := range []string{"success", "denied", "false-success", "unsupported", "redirect", "network", "fallback"} {
		t.Run(scenario, func(t *testing.T) {
			resolver := NewKeyResolver(library)
			defer resolver.Close()
			sources := runtimekit.NewSystemSources()
			profile, err := device.GenerateProfile(sources.Entropy)
			if err != nil {
				t.Fatal(err)
			}
			var actions []string
			transport := v8RoundTripFunc(func(request *http.Request) (*http.Response, error) {
				content, status := wafFixtureSDK, http.StatusOK
				headers := make(http.Header)
				if request.URL.String() == keySDKURL {
					if request.Header.Get("Referer") != "https://example.com/" || request.Header.Get("Origin") != "" {
						t.Error("incorrect WAF SDK source headers")
					}
					if scenario == "denied" {
						content = strings.ReplaceAll(content, "options.success('fixture-signature')", "options.fail()")
					}
					if scenario == "fallback" {
						content += "\nwindow.initAliyunCaptcha = options => options.success('fixture-certify');"
					}
				} else {
					body, _ := io.ReadAll(request.Body)
					form, _ := url.ParseQuery(string(body))
					action := form.Get("Action")
					actions = append(actions, action)
					if request.Header.Get("Origin") != "https://example.com" || request.Header.Get("Referer") != "https://example.com/" {
						t.Error("incorrect WAF page headers")
					}
					switch action {
					case "Log2":
						content = `{"Success":true,"Code":"Success"}`
					case "InitCaptchaV2":
						content = `{"Success":true,"Code":"Success","CertifyId":"fixture-certify","CaptchaType":"SLIDING","StaticPath":"3/sg.fixture"}`
						if scenario == "unsupported" {
							content = strings.ReplaceAll(content, "SLIDING", "PUZZLE")
						}
					case "VerifyCaptchaV2":
						if request.GetBody != nil {
							t.Error("Verify can be replayed")
						}
						content = `{"Success":true,"Code":"Success","Result":{"VerifyResult":true,"VerifyCode":"T001"}}`
						switch scenario {
						case "denied", "false-success":
							content = `{"Success":true,"Code":"Success","Result":{"VerifyResult":false,"VerifyCode":"T003"}}`
						case "redirect":
							status = http.StatusTemporaryRedirect
							headers.Set("Location", "https://other.captcha-pro-open.aliyuncs.com/")
						case "network":
							return nil, errors.New("fixture transport failure")
						}
					default:
						t.Errorf("unexpected offline action %q", action)
						return nil, errors.New("unexpected request")
					}
				}
				return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(content))}, nil
			})
			result, err := resolver.SolveWAF(context.Background(), transport, profile, WAFRuntimeOptions{
				PageURL:   "https://EXAMPLE.COM:443/pc/index.html?orgId=fixture",
				Challenge: waf.Challenge{Type: "GET", SceneID: "fixture-scene", UserID: "fixture-user", UserUserID: "fixture-user-user", TraceID: "fixture-trace", Token: "fixture-token", Region: "sgp", Language: "en"},
				// WAF 轨迹按真实时间回放，随机速度可能使轨迹本身超过 3 秒。
				Timeout: 5 * time.Second, Sources: sources,
			})
			switch scenario {
			case "success":
				if err != nil || result.Signature != "fixture-signature" || !result.VerifyResult || result.VerifyCode != "T001" || result.CertifyID != "fixture-certify" {
					t.Fatalf("WAF success contract failed: %v", err)
				}
			case "denied":
				if err != nil || result.Signature != "" || result.VerifyResult || result.VerifyCode != "T003" {
					t.Fatalf("WAF rejection contract failed: %v", err)
				}
			case "unsupported":
				if !errors.Is(err, pe.ErrUnsupportedPE) {
					t.Fatalf("unsupported challenge: %v", err)
				}
			default:
				if !errors.Is(err, pe.ErrKeyRuntime) {
					t.Fatalf("WAF failure contract: %v", err)
				}
			}
			expectedActions := "InitCaptchaV2,Log2,VerifyCaptchaV2"
			if scenario == "unsupported" {
				expectedActions = "InitCaptchaV2"
			}
			if scenario == "fallback" {
				expectedActions = ""
			}
			if strings.Join(actions, ",") != expectedActions {
				t.Fatalf("unexpected WAF request sequence: %v", actions)
			}
		})
	}
}
