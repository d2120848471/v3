package engine

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/d2120848471/v3/ali-slider-go/internal/domain/baxia"
)

func TestBaxiaTokenMatchesRecoveredAlphabet(t *testing.T) {
	profile := baxia.RuntimeProfile{Version: 231, Prefix: "231!", Alphabet: baxiaFireye231Alphabet}
	// 真实 SDK 的 oZS= 在此字母表下填充位为零，在标准表下为非零。
	if err := validateBaxiaToken(baxia.Result{Token: "231!oZS=", Version: 231}, profile); err != nil {
		t.Fatalf("SDK custom alphabet padding = %v", err)
	}
	profile.Alphabet = baxiaStandardAlphabet
	if err := validateBaxiaToken(baxia.Result{Token: "231!oZS=", Version: 231}, profile); !errors.Is(err, baxia.ErrToken) {
		t.Fatalf("different alphabet accepted noncanonical padding: %v", err)
	}
	for _, token := range []string{"", "default", "231!", "234!Zg==", "231!one\ntwo", "231!中文", "231!YQ", "231!YQ==\n", "231!Zh==", "231!" + strings.Repeat("a", baxiaMaximumTokenSize)} {
		if err := validateBaxiaToken(baxia.Result{Token: token, Version: 231}, profile); !errors.Is(err, baxia.ErrToken) {
			t.Errorf("token validation = %v", err)
		}
	}
	if err := validateBaxiaToken(baxia.Result{Token: "231!Zg==", Version: 234}, profile); !errors.Is(err, baxia.ErrUnsupportedSDK) {
		t.Fatalf("version mismatch = %v", err)
	}
}

func TestBaxiaProfileRequiresUniqueGetterEvidence(t *testing.T) {
	payload := base64.NewEncoding(baxiaFireye231Alphabet[:64]).EncodeToString([]byte("opaque"))
	stage := baxiaProfileStage{
		Version: 234, Token: "234!" + payload,
		Observations: []baxiaAlphabetObservation{
			{Alphabet: baxiaStandardAlphabet, Characters: "unrelated"},
			{Alphabet: baxiaFireye231Alphabet, Characters: payload},
		},
	}
	profile, err := recoverBaxiaProfile(stage, "sdk-digest")
	if err != nil || profile.Version != 234 || profile.Prefix != "234!" || profile.Alphabet != baxiaFireye231Alphabet || profile.SDKHash != "sdk-digest" {
		t.Fatalf("recovered profile = %+v, %v", profile, err)
	}
	// 无 padding 时，任意置换字母表都能 decode/reencode，绝不能借此猜表。
	for _, alphabet := range []string{baxiaStandardAlphabet, baxiaFireye231Alphabet} {
		encoding := base64.NewEncoding(alphabet[:64]).Strict()
		data, err := encoding.DecodeString(payload)
		if err != nil || encoding.EncodeToString(data) != payload {
			t.Fatalf("unpadded counterexample = %v", err)
		}
	}
	for _, tc := range []struct {
		name string
		edit func(*baxiaProfileStage)
	}{
		{"no observation", func(s *baxiaProfileStage) { s.Observations = nil }},
		{"changed structure", func(s *baxiaProfileStage) { s.Observations[1].Characters = "prefix" + payload }},
		{"ambiguous observations", func(s *baxiaProfileStage) { s.Observations[0].Characters = payload }},
		{"invalid dictionary", func(s *baxiaProfileStage) { s.Observations[1].Alphabet = strings.Repeat("a", 64) + "=" }},
		{"version prefix mismatch", func(s *baxiaProfileStage) { s.Token = "231!" + payload }},
		{"invalid version", func(s *baxiaProfileStage) { s.Version = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := stage
			candidate.Observations = append([]baxiaAlphabetObservation(nil), stage.Observations...)
			tc.edit(&candidate)
			if _, err := recoverBaxiaProfile(candidate, "sdk-digest"); !errors.Is(err, baxia.ErrUnsupportedSDK) {
				t.Fatalf("recovery error = %v", err)
			}
		})
	}
}

func baxiaNativeFixture(version int, alphabet string) []byte {
	return []byte(fmt.Sprintf(`
      (() => {
        const alphabet = %q;
        const standard = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/=";
        // 缓存加载时的方法，验证取样包装覆盖 SDK 的提前缓存。
        const charAt = String.prototype.charAt;
        let count = 0;
        __fyModule = {
          init(options, callback) { callback("initialized"); },
          getVersion() { return %d; },
          getFYToken(options) {
            count++;
            const text = options.reqUrl + "|" + options.location + "|" + count;
            return %q + btoa(text).split("").map((c) => charAt.call(alphabet, standard.indexOf(c))).join("");
          },
        };
      })();
    `, alphabet, version, fmt.Sprintf("%d!", version)))
}

func TestBaxiaNativeProfilesRemainBoundToSession(t *testing.T) {
	config := baxiaNativeConfig(t)
	options := baxia.DefaultUAOptions()
	options.Location = "intl"
	config.UAOptions = &options
	var sessions []*BaxiaSession
	for i, alphabet := range []string{baxiaStandardAlphabet, baxiaFireye231Alphabet} {
		version := 234 + i
		config.SDKSource = baxiaNativeFixture(version, alphabet)
		session, err := NewBaxiaSession(context.Background(), config)
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		sessions = append(sessions, session)
		want := baxia.RuntimeProfile{Version: version, Prefix: fmt.Sprintf("%d!", version), Alphabet: alphabet, SDKHash: fmt.Sprintf("%x", sha256.Sum256(config.SDKSource))}
		if session.Profile() != want {
			t.Fatalf("profile = %+v, want %+v", session.Profile(), want)
		}
	}
	for _, session := range sessions {
		result, err := session.Token(context.Background(), baxiaTestRequestURL)
		if err != nil || result.Profile != session.Profile() || result.SDKHash != session.Profile().SDKHash {
			t.Fatalf("token profile = %+v, %v", result.Profile, err)
		}
		decoded, err := base64.NewEncoding(result.Profile.Alphabet[:64]).Strict().DecodeString(strings.TrimPrefix(result.Token, result.Profile.Prefix))
		if err != nil || string(decoded) != baxiaTestRequestURL+"|intl|2" {
			t.Fatalf("retained SDK options/counter = %q, %v", decoded, err)
		}
	}
	copy := sessions[0].Profile()
	copy.Version = 900
	if sessions[0].Profile().Version == 900 || sessions[0].Profile().SDKHash == sessions[1].Profile().SDKHash {
		t.Fatal("profiles are mutable or cross SDK source identities")
	}
}

func TestBaxiaNativeRejectsUnprovenEncoding(t *testing.T) {
	config := baxiaNativeConfig(t)
	for _, tc := range []struct{ name, getter string }{
		{"array indexing", `return "234!" + btoa("sample").split("").map(c => alphabet[alphabet.indexOf(c)]).join("")`},
		{"cross-version prefix", `return "231!" + btoa("sample").split("").map(c => alphabet.charAt(alphabet.indexOf(c))).join("")`},
		{"two used alphabets", `const text = btoa("sample"); for (const a of [alphabet, alphabet.slice(0,64).split("").reverse().join("")+"="]) { for (const c of text) a.charAt(a.indexOf(c)); } return "234!" + text`},
		{"non-string token", `return { token: "234!c2FtcGxl" }`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config.SDKSource = []byte(fmt.Sprintf(`
              const alphabet = %q;
              __fyModule = {init(o, cb){cb("initialized")}, getVersion(){return 234}, getFYToken(){%s}};
            `, baxiaStandardAlphabet, tc.getter))
			session, err := NewBaxiaSession(context.Background(), config)
			if session != nil || !errors.Is(err, baxia.ErrUnsupportedSDK) {
				t.Fatalf("unproven SDK accepted: session=%v, err=%v", session, err)
			}
		})
	}
}
