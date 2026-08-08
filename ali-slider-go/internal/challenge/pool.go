package challenge

import (
	"crypto/sha256"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// ErrTransportRouteCapacity 表示路由池只剩不可淘汰的直连 route。
var ErrTransportRouteCapacity = errors.New("transport route capacity exceeded")

type transportRoute struct {
	transport *http.Transport
	direct    bool
	lastUsed  uint64
}

// TransportPool 按代理路由隔离连接池；不同出口绝不复用同一 TCP/TLS 连接。
// 路由数量有硬上限，避免无鉴权调用方用随机 proxy 制造无界 idle pool。
type TransportPool struct {
	mu             sync.Mutex
	maxRoutes      int
	maxConnections int
	closed         bool
	lastUsed       uint64
	routes         map[[sha256.Size]byte]*transportRoute
}

func (*TransportPool) String() string   { return "challenge.TransportPool{redacted}" }
func (*TransportPool) GoString() string { return "challenge.TransportPool{redacted}" }

func NewTransportPool(maxRoutes, maxConnections int) (*TransportPool, error) {
	if maxRoutes < 1 || maxRoutes > 32 || maxConnections < 1 || maxConnections > 32 {
		return nil, errors.New("transport pool limits must be within 1..32")
	}
	return &TransportPool{
		maxRoutes: maxRoutes, maxConnections: maxConnections,
		routes: make(map[[sha256.Size]byte]*transportRoute, maxRoutes),
	}, nil
}

// Get 返回指定 route 的共享 Transport。空 route 是显式直连。
func (p *TransportPool) Get(proxy string) (*http.Transport, bool, error) {
	canonical, err := canonicalProxyRoute(proxy)
	if err != nil {
		return nil, false, err
	}
	key := sha256.Sum256([]byte(canonical))

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, false, errors.New("transport pool is closed")
	}
	if route := p.routes[key]; route != nil {
		route.lastUsed = p.nextUseLocked()
		transport, proxied := route.transport, !route.direct
		p.mu.Unlock()
		return transport, proxied, nil
	}

	transport, proxied, err := NewTransport(canonical, p.maxConnections)
	if err != nil {
		p.mu.Unlock()
		return nil, false, err
	}

	var evicted *http.Transport
	if len(p.routes) >= p.maxRoutes {
		evictionKey, route, ok := p.leastRecentlyUsedProxyLocked()
		if !ok {
			p.mu.Unlock()
			transport.CloseIdleConnections()
			return nil, false, ErrTransportRouteCapacity
		}
		delete(p.routes, evictionKey)
		evicted = route.transport
	}
	p.routes[key] = &transportRoute{transport: transport, direct: canonical == "", lastUsed: p.nextUseLocked()}
	p.mu.Unlock()

	if evicted != nil {
		evicted.CloseIdleConnections()
	}
	return transport, proxied, nil
}

func (p *TransportPool) CloseIdleConnections() {
	p.mu.Lock()
	routes := make([]*http.Transport, 0, len(p.routes))
	for _, route := range p.routes {
		routes = append(routes, route.transport)
	}
	p.closed = true
	clear(p.routes)
	p.mu.Unlock()
	for _, transport := range routes {
		transport.CloseIdleConnections()
	}
}

func (p *TransportPool) RouteCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.routes)
}

func (p *TransportPool) nextUseLocked() uint64 {
	p.lastUsed++
	return p.lastUsed
}

func (p *TransportPool) leastRecentlyUsedProxyLocked() ([sha256.Size]byte, *transportRoute, bool) {
	var selectedKey [sha256.Size]byte
	var selected *transportRoute
	// ponytail: route 最多 32 项，淘汰时 O(n) 扫描比维护额外链表更小且足够。
	for key, route := range p.routes {
		if route.direct || selected != nil && route.lastUsed >= selected.lastUsed {
			continue
		}
		selectedKey, selected = key, route
	}
	return selectedKey, selected, selected != nil
}

func canonicalProxyRoute(proxy string) (string, error) {
	value := strings.TrimSpace(proxy)
	if value == "" {
		return "", nil
	}
	if !strings.Contains(value, "://") {
		value = "http://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Opaque != "" || parsed.Hostname() == "" {
		return "", errors.New("proxy must be an absolute URL with a host")
	}

	parsed.Scheme = strings.ToLower(parsed.Scheme)
	hostname := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if port != "" {
		number, portErr := strconv.Atoi(port)
		if portErr != nil || number < 1 || number > 65_535 {
			return "", errors.New("proxy port must be within 1..65535")
		}
		if (parsed.Scheme == "http" && number == 80) || (parsed.Scheme == "https" && number == 443) {
			port = ""
		} else {
			port = strconv.Itoa(number)
		}
	}
	if port != "" {
		parsed.Host = net.JoinHostPort(hostname, port)
	} else if strings.Contains(hostname, ":") {
		parsed.Host = "[" + hostname + "]"
	} else {
		parsed.Host = hostname
	}
	if parsed.Path == "/" {
		parsed.Path, parsed.RawPath = "", ""
	}
	parsed.ForceQuery = false
	return parsed.String(), nil
}
