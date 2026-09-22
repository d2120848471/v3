package baxiacli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/bootstrap"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/baxia"
	"github.com/d2120848471/v3/ali-slider-go/internal/domain/device"
	"github.com/d2120848471/v3/ali-slider-go/internal/foundation/runtimekit"
)

// Run 自动获取当前 SDK、恢复输出 profile，再生成一次 bx-ua 并输出 JSON。
// 显式传入 -sdk 时使用本地源码，跳过网络发现和下载。
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("bxua", flag.ContinueOnError)
	flags.SetOutput(stderr)
	sdkPath := flags.String("sdk", "", "可选本地 Fireye 文件；省略时按 AWSC 当前配置下载")
	libraryPath := flags.String("v8-library", "", "同平台 V8 wrapper 路径（必填）")
	pageURL := flags.String("page-url", "", "页面完整 URL（必填）")
	requestURL := flags.String("request-url", "", "待生成 bx-ua 的请求 URL（必填，不会发送请求）")
	profilePath := flags.String("profile", "", "设备 Profile JSON；省略时生成移动 Chromium 画像")
	proxy := flags.String("proxy", "", "公开 SDK 下载代理；不用于 SDK 内部网络")
	timeout := flags.Duration("timeout", 30*time.Second, "发现、下载、初始化和生成的总执行上限")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *libraryPath == "" || *pageURL == "" || *requestURL == "" {
		return errors.New("需要 -v8-library、-page-url 和 -request-url，且不接受位置参数")
	}
	if ctx == nil || *timeout < time.Millisecond || *timeout > 2*time.Minute {
		return errors.New("context 或 timeout 无效；timeout 应在 1ms 至 2m 之间")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	var source []byte
	var err error
	if *sdkPath != "" {
		source, err = readLimitedFile(*sdkPath, 2<<20)
		if err != nil {
			return fmt.Errorf("读取 Fireye SDK: %w", err)
		}
	}
	var profile device.Profile
	if *profilePath == "" {
		profile, err = device.GenerateProfile(runtimekit.NewSystemSources().Entropy)
	} else {
		var raw []byte
		raw, err = readLimitedFile(*profilePath, 32<<10)
		if err == nil {
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			err = decoder.Decode(&profile)
			if err == nil {
				var extra any
				if trailing := decoder.Decode(&extra); trailing != io.EOF {
					err = errors.New("画像必须是单个 JSON 对象")
				}
			}
		}
	}
	if err != nil {
		return fmt.Errorf("读取设备画像: %w", err)
	}
	client, err := bootstrap.NewBaxiaClient(baxia.ClientOptions{Proxy: *proxy, Timeout: *timeout})
	if err != nil {
		return err
	}
	defer client.Close()
	session, err := client.NewSession(ctx, baxia.Config{
		V8LibraryPath: *libraryPath, SDKSource: source, Profile: profile,
		PageURL: *pageURL, Timeout: min(*timeout, 30*time.Second),
	})
	if err != nil {
		return err
	}
	result, tokenErr := session.Token(ctx, *requestURL)
	closeErr := session.Close()
	if tokenErr != nil || closeErr != nil {
		return errors.Join(tokenErr, closeErr)
	}
	// 输出画像对应的请求头，避免宿主发送与 token 中设备矛盾的 User-Agent。
	output := struct {
		BXUA      string               `json:"bx-ua"`
		Version   int                  `json:"version"`
		SDKHash   string               `json:"sdkSha256"`
		SDKURL    string               `json:"sdkURL,omitempty"`
		AWSCHash  string               `json:"awscSha256,omitempty"`
		Profile   baxia.RuntimeProfile `json:"profile"`
		UAHeaders map[string]string    `json:"uaHeaders"`
	}{
		BXUA: result.Token, Version: result.Version, SDKHash: result.SDKHash,
		SDKURL: result.SDKURL, AWSCHash: result.AWSCHash, Profile: result.Profile,
		UAHeaders: map[string]string{
			"User-Agent": profile.UserAgent, "Accept-Language": profile.AcceptLanguage(),
			"Sec-CH-UA": profile.SecCHUA(), "Sec-CH-UA-Mobile": profile.SecCHUAMobile(),
			"Sec-CH-UA-Platform": profile.SecCHUAPlatform(),
		},
	}
	return json.NewEncoder(stdout).Encode(output)
}

func readLimitedFile(path string, limit int64) ([]byte, error) {
	// FIFO 的 Open 可能无限等待写端，先排除它，再核对打开后的文件。
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("必须是普通文件")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("必须是普通文件")
	}
	value, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if len(value) == 0 || int64(len(value)) > limit {
		return nil, errors.New("文件为空或超出大小上限")
	}
	return value, nil
}
