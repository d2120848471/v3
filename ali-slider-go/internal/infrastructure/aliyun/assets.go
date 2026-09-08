package aliyun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/domain/device"
)

const assetHost = "static-captcha.aliyuncs.com"

var relativeAssetPattern = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// AssetLimits 限制 CDN 响应，避免无鉴权服务被压缩炸弹或巨型文件耗尽内存。
type AssetLimits struct {
	MaxBytes int64
}

// Assets 始终保存在内存；只有失败样本策略可能另行持久化脱敏副本。
type Assets struct {
	Background []byte
	Shadow     []byte
	TimingsMS  map[string]int
}

// ValidateAssetPath 只接受静态 CDN 根目录下的相对路径。
func ValidateAssetPath(value, label string) (string, error) {
	if value == "" || strings.HasPrefix(value, "/") || strings.Contains(value, "..") || !relativeAssetPattern.MatchString(value) {
		return "", fmt.Errorf("%s is not a safe relative asset path", label)
	}
	return value, nil
}

// DownloadAssets 并发下载两张图片。重定向的每一跳都重新验证 HTTPS 与主机白名单。
func DownloadAssets(ctx context.Context, transport http.RoundTripper, profile device.Profile, referer, backgroundPath, shadowPath string, limits AssetLimits) (Assets, error) {
	if transport == nil {
		return Assets{}, errors.New("asset transport is required")
	}
	if limits.MaxBytes < 1 {
		return Assets{}, errors.New("asset max bytes must be positive")
	}
	backgroundPath, err := ValidateAssetPath(backgroundPath, "Image")
	if err != nil {
		return Assets{}, err
	}
	shadowPath, err = ValidateAssetPath(shadowPath, "PuzzleImage")
	if err != nil {
		return Assets{}, err
	}
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 4 {
				return errors.New("asset redirect limit exceeded")
			}
			return validateAssetURL(request.URL)
		},
	}
	type result struct {
		name    string
		content []byte
		elapsed int
		err     error
	}
	downloadContext, cancelDownloads := context.WithCancel(ctx)
	defer cancelDownloads()
	results := make(chan result, 2)
	for _, item := range []struct{ name, path string }{{"background", backgroundPath}, {"shadow", shadowPath}} {
		go func(name, path string) {
			started := time.Now()
			content, downloadErr := downloadAsset(downloadContext, client, profile, referer, path, limits.MaxBytes)
			results <- result{name: name, content: content, elapsed: int(time.Since(started) / time.Millisecond), err: downloadErr}
		}(item.name, item.path)
	}
	assets := Assets{TimingsMS: make(map[string]int, 2)}
	var firstError error
	for range 2 {
		resultValue := <-results
		if resultValue.err != nil {
			if firstError == nil {
				firstError = resultValue.err
				cancelDownloads()
			}
			continue
		}
		assets.TimingsMS[resultValue.name] = max(0, resultValue.elapsed)
		if resultValue.name == "background" {
			assets.Background = resultValue.content
		} else {
			assets.Shadow = resultValue.content
		}
	}
	if firstError != nil {
		return Assets{}, firstError
	}
	return assets, nil
}

func downloadAsset(ctx context.Context, client *http.Client, profile device.Profile, referer, path string, limit int64) ([]byte, error) {
	assetURL := &url.URL{Scheme: "https", Host: assetHost, Path: "/" + path}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build asset request: %w", err)
	}
	for key, value := range profile.BrowserHeaders(referer, "", "image", "no-cors", false) {
		request.Header.Set(key, value)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("asset request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("asset request returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > limit {
		return nil, errors.New("asset response exceeds byte limit")
	}
	var buffer bytes.Buffer
	if _, err := io.Copy(&buffer, io.LimitReader(response.Body, limit+1)); err != nil {
		return nil, errors.New("read asset response failed")
	}
	if int64(buffer.Len()) > limit {
		return nil, errors.New("asset response exceeds byte limit")
	}
	return buffer.Bytes(), nil
}

func validateAssetURL(value *url.URL) error {
	if value == nil || value.Scheme != "https" || !strings.EqualFold(value.Hostname(), assetHost) || value.User != nil {
		return errors.New("asset redirect left the HTTPS allowlist")
	}
	return nil
}
