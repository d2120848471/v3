package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/application/service"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/pe"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/track"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/waf"
	"github.com/d2120848471/v3/ali-slider-go/internal/foundation/runtimekit"
)

type WAFRuntimeOptions struct {
	PageURL   string
	Challenge waf.Challenge
	Timeout   time.Duration
	Sources   runtimekit.Sources
}

// SolveWAF 使用原 SDK 完成一轮 InitV2/SLIDING/VerifyV2，PageURL 只用于页面环境与 Referer。
func (resolver *KeyResolver) SolveWAF(ctx context.Context, transport http.RoundTripper, profile device.Profile, options WAFRuntimeOptions) (waf.Result, error) {
	if ctx == nil || resolver == nil || transport == nil || service.ValidateWAFURL(options.PageURL) != nil ||
		options.Timeout <= 0 || options.Timeout > 5*time.Minute || options.Sources.Validate() != nil {
		return waf.Result{}, fmt.Errorf("%w: invalid WAF runtime options", pe.ErrKeyRuntime)
	}
	challenge := options.Challenge
	if err := challenge.Validate(); err != nil {
		return waf.Result{}, fmt.Errorf("%w: %v", pe.ErrUnsupportedPE, err)
	}
	ctx, cancel := context.WithTimeout(ctx, options.Timeout)
	defer cancel()
	pageURL, _ := url.Parse(options.PageURL)
	// 浏览器会统一域名大小写并省略默认 HTTPS 端口；URL 校验已排除其他端口。
	pageURL.Host = strings.ToLower(pageURL.Hostname())
	options.PageURL = pageURL.String()
	// 跨站资源按浏览器默认策略只携带来源 origin，DOM 仍保留完整业务 URL。
	sdk, err := resolver.sdkSourceWithReferer(ctx, transport, profile, "https://"+pageURL.Host+"/")
	if err != nil {
		return waf.Result{}, err
	}
	engine, err := resolver.newV8Engine(transport, options.Sources.Entropy, true)
	if err != nil {
		return waf.Result{}, err
	}
	defer engine.Close()
	if err := loadV8RuntimeScripts(ctx, engine, false); err != nil {
		return waf.Result{}, err
	}
	profileJSON, err := marshalBridgeProfile(profile)
	if err != nil {
		return waf.Result{}, fmt.Errorf("%w: WAF profile", pe.ErrKeyRuntime)
	}
	// 项目画像是移动 Chromium，匹配 WAF H5 页的 320×40 滑块。
	const slideWidth, handleWidth = 320, 40
	events, err := track.LoadDefault(slideWidth-handleWidth, options.Sources.Entropy)
	if err != nil {
		return waf.Result{}, fmt.Errorf("%w: WAF track", pe.ErrKeyRuntime)
	}
	payload, err := json.Marshal(struct {
		SDKSource     string          `json:"sdkSource"`
		PageURL       string          `json:"pageURL"`
		Challenge     waf.Challenge   `json:"challenge"`
		DeviceProfile json.RawMessage `json:"deviceProfile"`
		TimeoutMS     int64           `json:"timeoutMs"`
		SlideWidth    int             `json:"slideWidth"`
		HandleWidth   int             `json:"handleWidth"`
		Track         []track.Event   `json:"track"`
	}{string(sdk), options.PageURL, challenge, profileJSON, options.Timeout.Milliseconds(), slideWidth, handleWidth, events})
	if err != nil {
		return waf.Result{}, fmt.Errorf("%w: WAF input", pe.ErrKeyRuntime)
	}
	raw, err := engine.Call(ctx, "__aliV8WAFSolve", payload)
	if err != nil {
		if ctx.Err() != nil {
			return waf.Result{}, ctx.Err()
		}
		if strings.Contains(err.Error(), "WAF_INIT_UNSUPPORTED") {
			return waf.Result{}, fmt.Errorf("%w: WAF Init type or response", pe.ErrUnsupportedPE)
		}
		return waf.Result{}, fmt.Errorf("%w: WAF execution failed", pe.ErrKeyRuntime)
	}
	var result waf.Result
	if decodeV8Result(raw, &result) != nil || result.CaptchaType != "SLIDING" || result.CertifyID == "" ||
		len(result.CertifyID) > 512 || len(result.Signature) > 16<<10 || result.VerifyResult != (result.Signature != "") {
		return waf.Result{}, fmt.Errorf("%w: WAF result contract", pe.ErrKeyRuntime)
	}
	return result, nil
}
