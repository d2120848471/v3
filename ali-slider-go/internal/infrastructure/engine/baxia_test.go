package engine

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/domain/baxia"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/foundation/runtimekit"
)

const baxiaTestPageURL = "https://www.galaxyticketing.com/en/#/login/account?loginBefore=%252FuserCenter%252FaccountList"
const baxiaTestRequestURL = "https://rest-sig.imaitix.com/api/user/userLogin?_bx-v=2.5.37"
const baxiaStandardAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/="
const baxiaFireye231Alphabet = "+jom378IDThM/wyJ4ernRtbPvQ9LS6KFkV5Zp2fUEcdWzuXgAi1l0OaxBYqNGsCH="

func baxiaTestConfig(t *testing.T) baxia.Config {
	t.Helper()
	profile, err := device.GenerateProfile(runtimekit.NewSystemSources().Entropy)
	if err != nil {
		t.Fatal(err)
	}
	return baxia.Config{
		V8LibraryPath: "fixture-v8", SDKSource: []byte("fixture SDK"),
		PageURL: baxiaTestPageURL, Profile: profile,
	}
}

func TestBaxiaConfigValidation(t *testing.T) {
	config := baxiaTestConfig(t)
	normalized, _, err := normalizeBaxiaConfig(config)
	if err != nil || normalized.Timeout != 5*time.Second || *normalized.UAOptions != baxia.DefaultUAOptions() {
		t.Fatalf("default config = %+v, %v", normalized.UAOptions, err)
	}
	options := baxia.DefaultUAOptions()
	options.MaxMTLog = 0
	config.UAOptions = &options
	normalized, _, err = normalizeBaxiaConfig(config)
	if err != nil || normalized.UAOptions.MaxMTLog != 0 || normalized.UAOptions == &options {
		t.Fatalf("explicit options = %+v, %v", normalized.UAOptions, err)
	}
	for _, tc := range []struct {
		name string
		edit func(*baxia.Config)
	}{
		{"empty library", func(c *baxia.Config) { c.V8LibraryPath = "" }},
		{"empty SDK", func(c *baxia.Config) { c.SDKSource = nil }},
		{"invalid UTF-8", func(c *baxia.Config) { c.SDKSource = []byte{0xff} }},
		{"large SDK", func(c *baxia.Config) { c.SDKSource = make([]byte, baxiaMaximumSDKBytes+1) }},
		{"relative page", func(c *baxia.Config) { c.PageURL = "/login" }},
		{"credentials", func(c *baxia.Config) { c.PageURL = "https://user:pass@example.com/" }},
		{"low timeout", func(c *baxia.Config) { c.Timeout = time.Microsecond }},
		{"high timeout", func(c *baxia.Config) { c.Timeout = time.Minute }},
		{"empty profile", func(c *baxia.Config) { c.Profile = device.Profile{} }},
		{"invalid UA options", func(c *baxia.Config) { c.UAOptions = &baxia.UAOptions{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := config
			tc.edit(&candidate)
			if _, _, err := normalizeBaxiaConfig(candidate); !errors.Is(err, baxia.ErrConfig) {
				t.Fatalf("validation error = %v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewBaxiaSession(ctx, config); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled construction = %v", err)
	}
	//lint:ignore SA1012 此处故意传 nil，验证边界拒绝空 context。
	if _, err := NewBaxiaSession(nil, config); !errors.Is(err, baxia.ErrConfig) {
		t.Fatalf("nil context = %v", err)
	}
	config.V8LibraryPath = filepath.Join(t.TempDir(), "missing-library")
	if _, err := NewBaxiaSession(context.Background(), config); !errors.Is(err, baxia.ErrRuntime) {
		t.Fatalf("missing library = %v", err)
	}
}

type baxiaFakeRuntime struct {
	call   func(context.Context, string, json.RawMessage) (json.RawMessage, error)
	closed atomic.Int32
}

func (r *baxiaFakeRuntime) Call(ctx context.Context, name string, input json.RawMessage) (json.RawMessage, error) {
	return r.call(ctx, name, input)
}
func (r *baxiaFakeRuntime) Close() error { r.closed.Add(1); return nil }

type baxiaFakeLibrary struct{ closed atomic.Int32 }

func (l *baxiaFakeLibrary) Close() error { l.closed.Add(1); return nil }

func baxiaFakeSession(runtime *baxiaFakeRuntime, library *baxiaFakeLibrary) *BaxiaSession {
	lifetime, cancel := context.WithCancel(context.Background())
	return &BaxiaSession{
		engine: runtime, library: library, gate: make(chan struct{}, 1),
		lifetime: lifetime, cancel: cancel,
		profile: baxia.RuntimeProfile{Version: 231, Prefix: "231!", Alphabet: baxiaStandardAlphabet, SDKHash: "fixture-sha256"},
	}
}

func TestBaxiaSessionRetainsStateAndCloses(t *testing.T) {
	count := 0
	runtime := &baxiaFakeRuntime{call: func(_ context.Context, name string, raw json.RawMessage) (json.RawMessage, error) {
		var input struct {
			RequestURL string `json:"requestURL"`
		}
		if err := json.Unmarshal(raw, &input); err != nil || name != "__aliV8BaxiaToken" || input.RequestURL != baxiaTestRequestURL {
			t.Fatalf("call = %s, %s, %v", name, raw, err)
		}
		count++
		return json.RawMessage(fmt.Sprintf(`{"token":"231!%s","version":231}`, base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("fixture%d", count))))), nil
	}}
	library := &baxiaFakeLibrary{}
	session := baxiaFakeSession(runtime, library)
	for i := 1; i <= 2; i++ {
		result, err := session.Token(context.Background(), baxiaTestRequestURL)
		want := "231!" + base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("fixture%d", i)))
		if err != nil || result.Token != want || result.SDKHash != "fixture-sha256" || result.Profile != session.Profile() {
			t.Fatalf("Token = %+v, %v", result, err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := session.Token(context.Background(), baxiaTestRequestURL); !errors.Is(err, baxia.ErrClosed) {
		t.Fatalf("Token after Close = %v", err)
	}
	if runtime.closed.Load() != 1 || library.closed.Load() != 1 {
		t.Fatal("Close did not release each owned resource once")
	}
}

func TestBaxiaSessionCancellationAndCloseInterrupt(t *testing.T) {
	for _, closeSession := range []bool{false, true} {
		t.Run(fmt.Sprint("close=", closeSession), func(t *testing.T) {
			entered := make(chan struct{})
			runtime := &baxiaFakeRuntime{call: func(ctx context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
				close(entered)
				<-ctx.Done()
				return nil, ctx.Err()
			}}
			library := &baxiaFakeLibrary{}
			session := baxiaFakeSession(runtime, library)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := session.Token(ctx, baxiaTestRequestURL); done <- err }()
			<-entered
			if closeSession {
				if err := session.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				cancel()
			}
			select {
			case err := <-done:
				want := context.Canceled
				if closeSession {
					want = baxia.ErrClosed
				}
				if !errors.Is(err, want) {
					t.Fatalf("Token = %v, want %v", err, want)
				}
			case <-time.After(time.Second):
				t.Fatal("Token or Close deadlocked")
			}
			if runtime.closed.Load() != 1 || library.closed.Load() != 1 {
				t.Fatal("canceled session retained resources")
			}
		})
	}
}

func TestBaxiaQueuedCancellationPreservesSession(t *testing.T) {
	runtime := &baxiaFakeRuntime{call: func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"token":"231!Zml4dHVyZQ==","version":231}`), nil
	}}
	session := baxiaFakeSession(runtime, &baxiaFakeLibrary{})
	defer session.Close()
	session.gate <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := session.Token(ctx, baxiaTestRequestURL); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued Token = %v", err)
	}
	<-session.gate
	if _, err := session.Token(context.Background(), baxiaTestRequestURL); err != nil {
		t.Fatalf("session after queued cancellation = %v", err)
	}
}

func TestBaxiaSessionGetterFailuresReleaseResources(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  json.RawMessage
		err  error
		want error
	}{
		{"SDK exception", nil, errors.New("fixture exception"), baxia.ErrRuntime},
		{"missing token", json.RawMessage(`{"version":231}`), nil, baxia.ErrToken},
		{"default token", json.RawMessage(`{"token":"default","version":231}`), nil, baxia.ErrToken},
		{"malformed output", json.RawMessage(`{"token":123,"version":231}`), nil, baxia.ErrToken},
		{"version mismatch", json.RawMessage(`{"token":"231!Zml4dHVyZQ==","version":232}`), nil, baxia.ErrUnsupportedSDK},
		{"prefix mismatch", json.RawMessage(`{"token":"234!Zml4dHVyZQ==","version":231}`), nil, baxia.ErrToken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime := &baxiaFakeRuntime{call: func(context.Context, string, json.RawMessage) (json.RawMessage, error) { return tc.raw, tc.err }}
			library := &baxiaFakeLibrary{}
			session := baxiaFakeSession(runtime, library)
			if _, err := session.Token(context.Background(), baxiaTestRequestURL); !errors.Is(err, tc.want) {
				t.Fatalf("Token = %v, want %v", err, tc.want)
			}
			if runtime.closed.Load() != 1 || library.closed.Load() != 1 {
				t.Fatal("failed session retained resources")
			}
		})
	}
}

func baxiaNativeConfig(t *testing.T) baxia.Config {
	t.Helper()
	path := os.Getenv("ALI_SLIDER_V8_TEST_LIBRARY")
	if path == "" {
		t.Skip("ALI_SLIDER_V8_TEST_LIBRARY is not set")
	}
	config := baxiaTestConfig(t)
	config.V8LibraryPath = path
	return config
}

func TestBaxiaNativeSDKInitializationFailures(t *testing.T) {
	config := baxiaNativeConfig(t)
	for _, tc := range []struct {
		name, source string
		want         error
	}{
		{"missing module", "true;", baxia.ErrUnsupportedSDK},
		{"SDK exception", `throw new Error("fixture init error")`, baxia.ErrRuntime},
		{"invalid version", `__fyModule={init(){},getVersion(){return 0},getFYToken(){}}`, baxia.ErrUnsupportedSDK},
		{"timeout callback", `__fyModule={init(o,cb){setTimeout(()=>cb("timeout"),1)},getVersion(){return 231},getFYToken(){}}`, baxia.ErrRuntime},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config.SDKSource = []byte(tc.source)
			if session, err := NewBaxiaSession(context.Background(), config); !errors.Is(err, tc.want) || session != nil {
				t.Fatalf("NewBaxiaSession = %v, %v", session, err)
			}
		})
	}
	config.SDKSource = []byte(`__fyModule={init(o,cb){},getVersion(){return 231},getFYToken(){}}`)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := NewBaxiaSession(ctx, config); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("initialization cancellation = %v", err)
	}
}

func TestBaxiaNativeSessionContract(t *testing.T) {
	config := baxiaNativeConfig(t)
	config.SDKSource = []byte(`
      (() => {
        const original = fyglobalopt;
        const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/=";
        const encode = (value) => btoa(value).split("").map((c) => alphabet.charAt(alphabet.indexOf(c))).join("");
        let calls = 0, ticks = 0, messages = 0;
        addEventListener("message", (event) => {
          if (event.source !== window || event.origin !== location.origin) throw new Error("message identity");
          messages++;
        });
        postMessage("fixture", "*");
        window.__fyModule = {
          init(options, callback) {
            if (options !== original) throw new Error("options replaced during init");
            setTimeout(() => callback("initialized"), 1);
          },
          getVersion() { return 231; },
          getFYToken(options) {
            if (options !== original) throw new Error("options replaced during getter");
            calls++;
            const token = "231!" + encode(JSON.stringify({
              requestURL: options.reqUrl, calls, ticks, messages,
              pageURL: location.href, documentURL: document.URL, referrer: document.referrer,
              domain: document.domain, origin: window.origin,
              userAgent: navigator.userAgent, screenWidth: screen.width,
              logLimit: options.MaxMTLog,
              audioMPEG: document.createElement("audio").canPlayType("audio/mpeg"),
              audioTheora: document.createElement("audio").canPlayType('video/ogg; codecs="theora"'),
            }));
            setTimeout(() => { ticks++; }, 0);
            return token;
          },
        };
      })();
    `)
	session, err := NewBaxiaSession(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for i, requestURL := range []string{baxiaTestRequestURL, "https://other.example.test/path?q=2"} {
		result, err := session.Token(context.Background(), requestURL)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(result.Token, "231!"))
		if err != nil {
			t.Fatal(err)
		}
		var observed struct {
			RequestURL                                                  string `json:"requestURL"`
			Calls, Ticks, Messages                                      int
			PageURL                                                     string `json:"pageURL"`
			DocumentURL                                                 string `json:"documentURL"`
			Referrer, Domain, Origin, UserAgent, AudioMPEG, AudioTheora string
			ScreenWidth, LogLimit                                       int
		}
		if err := json.Unmarshal(decoded, &observed); err != nil {
			t.Fatal(err)
		}
		if observed.RequestURL != requestURL || observed.Calls != i+2 || observed.Ticks != i+1 || observed.Messages != 1 ||
			observed.PageURL != config.PageURL || observed.DocumentURL != config.PageURL || observed.Referrer != "" ||
			observed.Domain != "www.galaxyticketing.com" || observed.Origin != "https://www.galaxyticketing.com" ||
			observed.UserAgent != config.Profile.UserAgent || observed.ScreenWidth != config.Profile.Screen.Width || observed.LogLimit != 5 ||
			observed.AudioMPEG != "probably" || observed.AudioTheora != "" {
			t.Fatalf("browser contract = %+v", observed)
		}
	}
}

func TestBaxiaNativeTokenCancellation(t *testing.T) {
	config := baxiaNativeConfig(t)
	config.SDKSource = []byte(`
      let called = false;
      const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/=";
      __fyModule={init(o,cb){cb("initialized")},getVersion(){return 231},getFYToken(){
        if (called) { while(true){} }
        called = true;
        return "231!" + btoa("sample").split("").map((c) => alphabet.charAt(alphabet.indexOf(c))).join("");
      }};
    `)
	session, err := NewBaxiaSession(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := session.Token(ctx, baxiaTestRequestURL); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled native getter = %v", err)
	}
	if _, err := session.Token(context.Background(), baxiaTestRequestURL); !errors.Is(err, baxia.ErrClosed) {
		t.Fatalf("session after canceled getter = %v", err)
	}
}

func TestBaxiaNativeFireyeSDK(t *testing.T) {
	config := baxiaNativeConfig(t)
	path := os.Getenv("ALI_BAXIA_TEST_SDK")
	if path == "" {
		t.Skip("ALI_BAXIA_TEST_SDK is not set")
	}
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	config.SDKSource = source
	session, err := NewBaxiaSession(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	profile := session.Profile()
	if profile.Version < 1 || profile.Prefix != fmt.Sprintf("%d!", profile.Version) || len(profile.Alphabet) != 65 {
		t.Fatalf("invalid recovered profile: %+v", profile)
	}
	var previous string
	for _, requestURL := range []string{baxiaTestRequestURL, "https://example.test/probe?next=2"} {
		result, err := session.Token(context.Background(), requestURL)
		if err != nil {
			t.Fatal(err)
		}
		if result.SDKHash != fmt.Sprintf("%x", sha256.Sum256(source)) {
			t.Fatal("SDK hash mismatch")
		}
		if result.Token == previous {
			t.Fatal("SDK returned the same output for successive request URLs")
		}
		previous = result.Token
		if result.Profile != profile || result.Version != profile.Version {
			t.Fatal("SDK profile changed between sample and getter")
		}
		t.Logf("local Fireye output: version=%d tokenBytes=%d alphabet=%s sdkSha256=%s", result.Version, len(result.Token), profile.Alphabet, result.SDKHash)
	}
}
