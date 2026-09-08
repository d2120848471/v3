package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestQueryTextNormalizationMatchesJSON(t *testing.T) {
	handler := &Handler{defaultSceneID: "default-scene", defaultPrefix: "default1"}
	for _, query := range []string{
		"",
		"SceneId=first&SceneId=last&sceneId=alias&prefix=prefix1",
		"SceneId=&sceneId=alias&prefix=&Prefix=alias1",
		"SceneId=+scene+&AaduaneId=+key+&proxy=proxy.invalid%3A8080",
		"SceneId=%FF%FE&future=%FF",
		"SceneId=%E4%B8&future=ignored",
		"SceneId=%00%E4%B8%AD&prefix=abc",
		"prefix=%FF&proxy=invalid%3Aport",
		"SceneId=" + strings.Repeat("%E4%B8%AD", 65),
		"AaduaneId=" + strings.Repeat("x", 129) + "&prefix=bad-prefix",
		"proxy=socks5%3A%2F%2Fuser%3Apass%40proxy.invalid%3A1080",
	} {
		t.Run(query, func(t *testing.T) {
			values, err := url.ParseQuery(query)
			if err != nil {
				t.Fatal(err)
			}
			// JSON 路径作为独立格式的合同对照，覆盖旧 query 经 JSON
			// 转换时的非法 UTF-8 替换、同名末值和规范字段优先级。
			payload := make(map[string]string, len(values))
			for key, entries := range values {
				payload[key] = entries[len(entries)-1]
			}
			body, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			post := httptest.NewRequest(http.MethodPost, SolvePath, strings.NewReader(string(body)))
			want, wantErr := handler.decodeRequest(httptest.NewRecorder(), post)
			get := httptest.NewRequest(http.MethodGet, SolvePath+"?"+query, nil)
			got, gotErr := handler.decodeRequest(httptest.NewRecorder(), get)
			if got != want || fmt.Sprint(gotErr) != fmt.Sprint(wantErr) {
				t.Fatalf("query/JSON contract differs: query error=%v JSON error=%v", gotErr, wantErr)
			}
		})
	}
}
