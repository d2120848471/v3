package engine

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/domain/pe"
)

// nodeProxyEnvironment 为可选 Node oracle 配置代理；SOCKS 复用 Go transport 的拨号器。
func nodeProxyEnvironment(proxy string, transport http.RoundTripper) ([]string, bool, *connectRelay, error) {
	value := strings.TrimSpace(proxy)
	if value == "" {
		return []string{}, false, nil, nil
	}
	if !strings.Contains(value, "://") {
		value = "http://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil && parsed.User.Username() == "" {
		return nil, false, nil, fmt.Errorf("%w: invalid Node proxy", pe.ErrKeyRuntime)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
	case "socks4", "socks5", "socks5h":
		base, ok := transport.(*http.Transport)
		if !ok || base.DialContext == nil {
			return nil, false, nil, fmt.Errorf("%w: SOCKS transport is unavailable", pe.ErrKeyRuntime)
		}
		relay, relayErr := startConnectRelay(base.DialContext)
		if relayErr != nil {
			return nil, false, nil, fmt.Errorf("%w: start SOCKS relay", pe.ErrKeyRuntime)
		}
		value = "http://" + relay.listener.Addr().String()
		return []string{
			"HTTP_PROXY=" + value, "HTTPS_PROXY=" + value,
			"NO_PROXY=localhost,127.0.0.1", "NODE_USE_ENV_PROXY=1",
		}, true, relay, nil
	default:
		return nil, false, nil, fmt.Errorf("%w: invalid Node proxy scheme", pe.ErrKeyRuntime)
	}
	return []string{
		"HTTP_PROXY=" + parsed.String(), "HTTPS_PROXY=" + parsed.String(),
		"NO_PROXY=localhost,127.0.0.1", "NODE_USE_ENV_PROXY=1",
	}, true, nil, nil
}

// connectRelay 将 Node 的 HTTPS CONNECT 请求转接到已有拨号器，
// 并在关闭时等待活跃连接及处理协程退出。
type connectRelay struct {
	listener   net.Listener
	dial       func(context.Context, string, string) (net.Conn, error)
	ctx        context.Context
	cancel     context.CancelFunc
	acceptDone chan struct{}
	handlers   sync.WaitGroup
	closeOnce  sync.Once
	mu         sync.Mutex
	closed     bool
	active     map[net.Conn]struct{}
}

func startConnectRelay(dial func(context.Context, string, string) (net.Conn, error)) (*connectRelay, error) {
	if dial == nil {
		return nil, errors.New("relay dialer is nil")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	relay := &connectRelay{
		listener: listener, dial: dial, ctx: ctx, cancel: cancel,
		acceptDone: make(chan struct{}), active: make(map[net.Conn]struct{}),
	}
	go relay.serve()
	return relay, nil
}

func (relay *connectRelay) serve() {
	defer close(relay.acceptDone)
	for {
		connection, err := relay.listener.Accept()
		if err != nil {
			return
		}
		if !relay.register(connection) {
			continue
		}
		relay.handlers.Add(1)
		go func() {
			defer relay.handlers.Done()
			relay.handle(connection)
		}()
	}
}

func (relay *connectRelay) register(connection net.Conn) bool {
	relay.mu.Lock()
	defer relay.mu.Unlock()
	if relay.closed {
		_ = connection.Close()
		return false
	}
	relay.active[connection] = struct{}{}
	return true
}

func (relay *connectRelay) unregister(connection net.Conn) {
	relay.mu.Lock()
	delete(relay.active, connection)
	relay.mu.Unlock()
}

func (relay *connectRelay) handle(client net.Conn) {
	defer func() {
		relay.unregister(client)
		_ = client.Close()
	}()
	_ = client.SetDeadline(time.Now().Add(10 * time.Second))
	request, err := http.ReadRequest(bufio.NewReader(client))
	if err != nil {
		return
	}
	defer request.Body.Close()
	host, port, splitErr := net.SplitHostPort(request.Host)
	allowed := strings.EqualFold(host, keyPEHost) || strings.EqualFold(host, keyCaptchaAssetHost) || strings.HasSuffix(strings.ToLower(host), ".aliyuncs.com")
	if request.Method != http.MethodConnect || splitErr != nil || port != "443" || !allowed {
		_, _ = io.WriteString(client, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")
		return
	}
	upstream, err := relay.dial(relay.ctx, "tcp", request.Host)
	if err != nil {
		_, _ = io.WriteString(client, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}
	if !relay.register(upstream) {
		return
	}
	defer func() {
		relay.unregister(upstream)
		_ = upstream.Close()
	}()
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	_ = client.SetDeadline(time.Time{})
	copyDone := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, client)
		copyDone <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, upstream)
		copyDone <- struct{}{}
	}()
	completed := 0
	select {
	case <-copyDone:
		completed = 1
	case <-relay.ctx.Done():
	}
	_ = client.Close()
	_ = upstream.Close()
	for completed < 2 {
		<-copyDone
		completed++
	}
}

func (relay *connectRelay) Close() {
	if relay == nil {
		return
	}
	relay.closeOnce.Do(func() {
		relay.cancel()
		_ = relay.listener.Close()
		<-relay.acceptDone
		relay.mu.Lock()
		relay.closed = true
		for connection := range relay.active {
			_ = connection.Close()
		}
		relay.mu.Unlock()
		relay.handlers.Wait()
	})
}
