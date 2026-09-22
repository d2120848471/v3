package engine

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/d2120848471/v3/ali-slider-go/internal/domain/baxia"
	"github.com/d2120848471/v3/ali-slider-go/internal/foundation/runtimekit"
	"github.com/d2120848471/v3/ali-slider-go/internal/platform/v8runtime"
)

//go:embed runtime/baxia_discovery.js
var baxiaDiscoverySource string

// DiscoverBaxiaSDK 在无网络的独立 V8 context 中运行 AWSC，返回 use("fy")
// 实际插入的脚本 URL。awscURL 是下载后的实际来源；由调用方的白名单下载器
// 下载返回的 URL。本函数不运行被发现的脚本，也不请求 ET 或遥测资源。
func DiscoverBaxiaSDK(ctx context.Context, config baxia.Config, awscURL string, awscSource []byte) (string, error) {
	if ctx == nil {
		return "", fmt.Errorf("%w: context is nil", baxia.ErrConfig)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	config, profileJSON, err := normalizeBaxiaEnvironment(config, false)
	if err != nil {
		return "", err
	}
	if err := validateBaxiaURL(awscURL); err != nil {
		return "", fmt.Errorf("%w: AWSC source URL %v", baxia.ErrConfig, err)
	}
	if len(awscSource) == 0 || len(awscSource) > baxiaMaximumSDKBytes || !utf8.Valid(awscSource) {
		return "", fmt.Errorf("%w: AWSC source must be nonempty UTF-8 within 2 MiB", baxia.ErrConfig)
	}
	ctx, cancel := context.WithTimeout(ctx, config.Timeout)
	defer cancel()
	library, err := v8runtime.Open(config.V8LibraryPath)
	if err != nil {
		return "", fmt.Errorf("%w: load V8 library: %v", baxia.ErrRuntime, err)
	}
	engine, err := library.NewWithHost(newV8HostHandler(nil, runtimekit.NewSystemSources().Entropy, false, nil))
	if err != nil {
		return "", errors.Join(fmt.Errorf("%w: create V8 runtime: %v", baxia.ErrRuntime, err), library.Close())
	}
	defer func() {
		_ = engine.Close()
		_ = library.Close()
	}()
	if err := loadBaxiaRuntimeScripts(ctx, engine, true); err != nil {
		return "", err
	}
	payload, err := json.Marshal(struct {
		AWSCSource    string           `json:"awscSource"`
		AWSCURL       string           `json:"awscURL"`
		DeviceProfile json.RawMessage  `json:"deviceProfile"`
		PageURL       string           `json:"pageURL"`
		UAOptions     *baxia.UAOptions `json:"uaOptions"`
		TimeoutMS     int64            `json:"timeoutMs"`
	}{string(awscSource), awscURL, profileJSON, config.PageURL, config.UAOptions, config.Timeout.Milliseconds()})
	if err != nil {
		return "", fmt.Errorf("%w: encode AWSC discovery input", baxia.ErrConfig)
	}
	raw, err := engine.Call(ctx, "__aliV8BaxiaDiscover", payload)
	if err != nil {
		return "", baxiaRuntimeError(ctx, "discover Fireye SDK through AWSC", err)
	}
	var result struct {
		URLs []string `json:"urls"`
	}
	if err := decodeV8Result(raw, &result); err != nil {
		return "", fmt.Errorf("%w: invalid AWSC discovery result", baxia.ErrUnsupportedSDK)
	}
	if len(result.URLs) != 1 {
		return "", fmt.Errorf("%w: AWSC selected %d Fireye URLs; expected exactly one", baxia.ErrUnsupportedSDK, len(result.URLs))
	}
	selected := result.URLs[0]
	parsed, err := url.Parse(selected)
	if validateBaxiaURL(selected) != nil || err != nil || parsed.Scheme != "https" || !strings.HasSuffix(parsed.Path, "/fireyejs.js") {
		return "", fmt.Errorf("%w: AWSC selected an invalid Fireye URL", baxia.ErrUnsupportedSDK)
	}
	return selected, nil
}

func loadBaxiaRuntimeScripts(ctx context.Context, engine *v8runtime.Runtime, discovery bool) error {
	bundle, err := buildV8SDKBundle()
	if err != nil {
		return fmt.Errorf("%w: build browser environment: %v", baxia.ErrRuntime, err)
	}
	scripts := []struct{ name, source string }{
		{"v8-host-bootstrap.js", v8HostBootstrapSource},
		{"sdk-device-v8-bundle.js", bundle},
		{"baxia-bridge.js", baxiaBridgeSource},
	}
	if discovery {
		scripts = append(scripts, struct{ name, source string }{"baxia-discovery.js", baxiaDiscoverySource})
	}
	for _, script := range scripts {
		result, err := engine.Eval(ctx, script.source, script.name)
		if err != nil {
			return baxiaRuntimeError(ctx, "load "+script.name, err)
		}
		if string(result) != "true" {
			return fmt.Errorf("%w: initialize %s", baxia.ErrRuntime, script.name)
		}
	}
	return nil
}
