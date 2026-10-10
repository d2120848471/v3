package waf

import (
	"encoding/json"
	"errors"
	"html"
	"strings"
	"testing"

	domainwaf "github.com/d2120848471/v3/ali-slider-go/internal/domain/waf"
)

const fixtureRequestInfo = `{"type":"GET","data":"","token":"fixture-token","sceneId":"fixture-scene","userUserId":"fixture-user-user","userId":"fixture-user","region":"cn","traceid":"fixture-trace"}`

func fixturePage(info string) []byte {
	return []byte(`<html><textarea id="renderData" style="display:none">var requestInfo = ` + info + `;</textarea></html>`)
}

func TestParseChallengeKnownPageAndEntities(t *testing.T) {
	want := domainwaf.Challenge{
		Type: "GET", Token: "fixture-token", SceneID: "fixture-scene", UserUserID: "fixture-user-user",
		UserID: "fixture-user", Region: "cn", TraceID: "fixture-trace",
	}
	for _, page := range [][]byte{
		fixturePage(fixtureRequestInfo),
		fixturePage(html.EscapeString(fixtureRequestInfo)),
		[]byte(`<HTML><TEXTAREA id='renderData'>\nvar requestInfo = ` + fixtureRequestInfo + `;\n</TEXTAREA></HTML>`),
	} {
		page = []byte(strings.ReplaceAll(string(page), `\n`, "\n"))
		got, err := ParseChallenge(page)
		if err != nil || got != want {
			t.Fatalf("parse known challenge: match=%t error=%v", got == want, err)
		}
	}
	page := fixturePage(strings.Replace(fixtureRequestInfo, `"region":"cn"`, `"region":"sgp","language":"en"`, 1))
	got, err := ParseChallenge(page)
	if err != nil || got.Region != "sgp" || got.Language != "en" {
		t.Fatalf("parse Singapore region: region=%s error=%v", got.Region, err)
	}
	want.Token, want.Language = strings.Repeat("x", 4096), strings.Repeat("l", 64)
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err = ParseChallenge(fixturePage(string(encoded)))
	if err != nil || got != want {
		t.Fatalf("parse challenge at byte limits: match=%t error=%v", got == want, err)
	}
}

func TestParseChallengeRejectsUnsupportedAndExecutableInput(t *testing.T) {
	for _, test := range []struct {
		name string
		page []byte
	}{
		{"ordinary HTML", []byte(`<html>fixture content</html>`)},
		{"assignment outside textarea", []byte(`<script>var requestInfo = ` + fixtureRequestInfo + `;</script>`)},
		{"missing token", fixturePage(strings.Replace(fixtureRequestInfo, `"token":"fixture-token",`, "", 1))},
		{"empty trace", fixturePage(strings.Replace(fixtureRequestInfo, `"fixture-trace"`, `""`, 1))},
		{"blank token", fixturePage(strings.Replace(fixtureRequestInfo, `"fixture-token"`, `" \t\n\u3000"`, 1))},
		{"oversized token", fixturePage(strings.Replace(fixtureRequestInfo, `"fixture-token"`, `"`+strings.Repeat("x", 4097)+`"`, 1))},
		{"oversized scene", fixturePage(strings.Replace(fixtureRequestInfo, `"fixture-scene"`, `"`+strings.Repeat("x", 4097)+`"`, 1))},
		{"oversized language", fixturePage(strings.Replace(fixtureRequestInfo, `"region":"cn"`, `"region":"cn","language":"`+strings.Repeat("x", 65)+`"`, 1))},
		{"wrong field type", fixturePage(strings.Replace(fixtureRequestInfo, `"fixture-user"`, `7`, 1))},
		{"null field", fixturePage(strings.Replace(fixtureRequestInfo, `"fixture-user"`, `null`, 1))},
		{"wrong method", fixturePage(strings.Replace(fixtureRequestInfo, `"GET"`, `"POST"`, 1))},
		{"unknown region", fixturePage(strings.Replace(fixtureRequestInfo, `"cn"`, `"us"`, 1))},
		{"JS object", fixturePage(strings.Replace(fixtureRequestInfo, `"token":`, `token:`, 1))},
		{"trailing executable code", fixturePage(fixtureRequestInfo + `;globalThis.attack()`)},
		{"expression", fixturePage(`JSON.parse("fixture")`)},
		{"ambiguous pages", append(fixturePage(fixtureRequestInfo), fixturePage(fixtureRequestInfo)...)},
		{"oversized page", []byte(strings.Repeat("x", (2<<20)+1))},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseChallenge(test.page)
			if !errors.Is(err, ErrUnsupportedPage) || got != (domainwaf.Challenge{}) {
				t.Fatalf("unsupported challenge accepted: empty=%t error=%v", got == (domainwaf.Challenge{}), err)
			}
			if strings.Contains(err.Error(), "fixture-user") || strings.Contains(err.Error(), "fixture-token") {
				t.Fatal("parse error disclosed challenge data")
			}
		})
	}
}
