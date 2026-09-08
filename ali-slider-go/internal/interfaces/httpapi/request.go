package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/d2120848471/v3/ali-slider-go/internal/application/solve"
)

func (h *Handler) decodeRequest(w http.ResponseWriter, r *http.Request) (solve.Request, error) {
	if r.Method == http.MethodGet {
		return h.decodeQueryRequest(r)
	}
	if r.ContentLength > MaxRequestBytes {
		return solve.Request{}, fmt.Errorf("请求体必须位于 0..%d 字节", MaxRequestBytes)
	}
	limited := http.MaxBytesReader(w, r.Body, MaxRequestBytes)
	defer limited.Close()

	var payload map[string]json.RawMessage
	decoder := json.NewDecoder(limited)
	err := decoder.Decode(&payload)
	if errors.Is(err, io.EOF) {
		payload = map[string]json.RawMessage{}
	} else if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return solve.Request{}, fmt.Errorf("请求体必须位于 0..%d 字节", MaxRequestBytes)
		}
		return solve.Request{}, errors.New("请求体不是合法 JSON")
	}
	if payload == nil {
		return solve.Request{}, errors.New("请求体必须是 JSON object")
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return solve.Request{}, err
	}

	return h.requestFromFields(func(primary, alias string, maxRunes int) (string, error) {
		return optionalAliasedText(payload, primary, alias, maxRunes)
	})
}

func (h *Handler) decodeQueryRequest(r *http.Request) (solve.Request, error) {
	if int64(len(r.URL.RawQuery)) > MaxRequestBytes {
		return solve.Request{}, fmt.Errorf("查询参数必须位于 0..%d 字节", MaxRequestBytes)
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return solve.Request{}, errors.New("查询参数不是合法 URL encoding")
	}
	return h.requestFromFields(func(primary, alias string, maxRunes int) (string, error) {
		values, found := query[primary]
		if !found {
			values = query[alias]
		}
		if len(values) == 0 {
			return "", nil
		}
		value := values[len(values)-1]
		// 旧实现经 JSON 编解码将每个非法 UTF-8 字节替换为 U+FFFD；
		// 保留该兼容语义，正常 query 直接读取字符串，不再绕经 JSON。
		if !utf8.ValidString(value) {
			value = string([]rune(value))
		}
		return normalizeText(value, maxRunes)
	})
}

// requestFromFields 统一两种传输格式的校验顺序与默认值；字段读取器只负责
// 各自格式的取值、别名优先级和字符串边界，不合并 body 与 query。
func (h *Handler) requestFromFields(readText func(string, string, int) (string, error)) (solve.Request, error) {
	sceneID, err := readText("SceneId", "sceneId", 64)
	if err != nil {
		return solve.Request{}, fmt.Errorf("SceneId %w", err)
	}
	rpcKeyID, err := readText("AaduaneId", "aaduaneId", 128)
	if err != nil {
		return solve.Request{}, fmt.Errorf("AaduaneId %w", err)
	}
	prefix, err := readText("prefix", "Prefix", 32)
	if err != nil {
		return solve.Request{}, fmt.Errorf("prefix %w", err)
	}
	if prefix != "" && !validPrefix(prefix) {
		return solve.Request{}, errors.New("prefix 只能是 ASCII 字母数字")
	}
	proxy, err := readText("proxy", "Proxy", 0)
	if err != nil {
		return solve.Request{}, fmt.Errorf("proxy %w", err)
	}
	proxy, err = normalizeProxy(proxy)
	if err != nil {
		return solve.Request{}, err
	}
	if sceneID == "" {
		sceneID = h.defaultSceneID
	}
	if prefix == "" {
		prefix = h.defaultPrefix
	}
	return solve.Request{SceneID: sceneID, Prefix: prefix, RPCKeyID: rpcKeyID, Proxy: proxy}, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra json.RawMessage
	if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return errors.New("请求体不是合法 JSON")
	}
	return errors.New("请求体只能包含一个 JSON object")
}

func optionalAliasedText(payload map[string]json.RawMessage, primary, alias string, maxRunes int) (string, error) {
	raw, found := payload[primary]
	if !found {
		raw, found = payload[alias]
	}
	if !found || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", errors.New("必须是字符串")
	}
	return normalizeText(value, maxRunes)
}

func normalizeText(value string, maxRunes int) (string, error) {
	value = strings.TrimSpace(value)
	if maxRunes > 0 && utf8.RuneCountInString(value) > maxRunes {
		return "", fmt.Errorf("超过 %d 字符", maxRunes)
	}
	return value, nil
}

func validPrefix(value string) bool {
	if value == "" || len(value) > 32 {
		return false
	}
	for _, character := range value {
		if character > 127 || !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9')) {
			return false
		}
	}
	return true
}

func normalizeProxy(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if !strings.Contains(value, "://") {
		value = "http://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", errors.New("proxy 格式无效")
	}
	scheme := strings.ToLower(parsed.Scheme)
	switch scheme {
	case "http", "https", "socks4", "socks5", "socks5h":
	default:
		return "", errors.New("proxy scheme 必须是 http/https/socks4/socks5/socks5h")
	}
	if parsed.Hostname() == "" {
		return "", errors.New("proxy 缺少主机名")
	}
	if parsed.Opaque != "" || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("proxy 不能包含 path、query 或 fragment")
	}
	if port := parsed.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return "", errors.New("proxy 端口必须位于 1..65535")
		}
	}
	if parsed.User != nil {
		username := parsed.User.Username()
		password, hasPassword := parsed.User.Password()
		switch scheme {
		case "socks4":
			if hasPassword || len(username) > 255 || strings.ContainsRune(username, '\x00') {
				return "", errors.New("socks4 proxy userinfo 只能包含合法 username")
			}
		case "socks5", "socks5h":
			if !hasPassword || len(username) < 1 || len(username) > 255 || len(password) < 1 || len(password) > 255 {
				return "", errors.New("socks5 proxy userinfo 必须包含 1..255 字节的 username/password")
			}
		}
	}
	parsed.Scheme = scheme
	return parsed.String(), nil
}
