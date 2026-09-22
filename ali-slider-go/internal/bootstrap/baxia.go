package bootstrap

import (
	"context"
	"fmt"

	"github.com/d2120848471/v3/ali-slider-go/internal/domain/baxia"
	"github.com/d2120848471/v3/ali-slider-go/internal/infrastructure/engine"
	"github.com/d2120848471/v3/ali-slider-go/internal/infrastructure/httpclient"
)

// NewBaxiaSession 组装独立 Fireye 会话；未提供 SDKSource 时重新发现并下载。
// 连续构造多个会话的宿主应复用 NewBaxiaClient 的条件请求缓存。
func NewBaxiaSession(ctx context.Context, config baxia.Config) (baxia.Session, error) {
	if len(config.SDKSource) != 0 {
		return engine.NewBaxiaSession(ctx, config)
	}
	client, err := NewBaxiaClient(baxia.ClientOptions{})
	if err != nil {
		return nil, err
	}
	defer client.Close()
	return client.NewSession(ctx, config)
}

// NewBaxiaClient 组装 Go 脚本下载器；构造时不访问网络。
func NewBaxiaClient(options baxia.ClientOptions) (baxia.Client, error) {
	transport, _, err := httpclient.NewTransport(options.Proxy, 4)
	if err != nil {
		return nil, fmt.Errorf("%w: download transport: %v", baxia.ErrConfig, err)
	}
	client, err := engine.NewBaxiaClient(transport, options.Timeout)
	if err != nil {
		transport.CloseIdleConnections()
		return nil, err
	}
	return client, nil
}
