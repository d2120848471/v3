package challenge

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestTransportPoolCanonicalizesEquivalentRoutes(t *testing.T) {
	pool, err := NewTransportPool(8, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.CloseIdleConnections()

	tests := []struct {
		name   string
		first  string
		second string
	}{
		{name: "http", first: "HTTP://alice:secret@EXAMPLE.COM:080/", second: "http://alice:secret@example.com"},
		{name: "https", first: "HTTPS://EXAMPLE.COM:0443/", second: "https://example.com"},
		{name: "socks5h", first: "SOCKS5H://alice:secret@PROXY.EXAMPLE:01080/", second: "socks5h://alice:secret@proxy.example:1080"},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			first, proxied, err := pool.Get(test.first)
			if err != nil || !proxied {
				t.Fatalf("first route: proxied=%v err=%v", proxied, err)
			}
			second, proxied, err := pool.Get(test.second)
			if err != nil || !proxied || first != second {
				t.Fatalf("equivalent route was not reused: proxied=%v err=%v", proxied, err)
			}
			if got := pool.RouteCount(); got != index+1 {
				t.Fatalf("routes=%d, want %d", got, index+1)
			}
		})
	}
}

func TestTransportPoolPreservesSOCKSDNSSemantics(t *testing.T) {
	pool, err := NewTransportPool(2, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.CloseIdleConnections()

	localDNS, _, err := pool.Get("socks5://proxy.example:1080")
	if err != nil {
		t.Fatal(err)
	}
	remoteDNS, _, err := pool.Get("socks5h://proxy.example:1080")
	if err != nil {
		t.Fatal(err)
	}
	if localDNS == remoteDNS || pool.RouteCount() != 2 {
		t.Fatal("socks5 and socks5h routes must remain isolated")
	}
}

func TestTransportPoolEvictsLeastRecentlyUsedProxyAndPinsDirect(t *testing.T) {
	pool, err := NewTransportPool(3, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.CloseIdleConnections()

	direct, proxied, err := pool.Get("")
	if err != nil || proxied {
		t.Fatalf("direct route: proxied=%v err=%v", proxied, err)
	}
	proxyA, _, err := pool.Get("http://127.0.0.1:18080")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = pool.Get("http://127.0.0.1:18081")
	if err != nil {
		t.Fatal(err)
	}
	if reused, _, err := pool.Get("http://127.0.0.1:18080"); err != nil || reused != proxyA {
		t.Fatal("proxy A was not reused before eviction")
	}
	if _, _, err := pool.Get("http://127.0.0.1:18082"); err != nil {
		t.Fatal(err)
	}

	canonicalB, err := canonicalProxyRoute("http://127.0.0.1:18081")
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := pool.routes[sha256.Sum256([]byte(canonicalB))]; exists {
		t.Fatal("least recently used proxy B was not evicted")
	}
	if reused, proxied, err := pool.Get(""); err != nil || proxied || reused != direct {
		t.Fatalf("direct route was evicted: proxied=%v err=%v", proxied, err)
	}
	if pool.RouteCount() != 3 {
		t.Fatalf("routes=%d, want 3", pool.RouteCount())
	}
}

func TestTransportPoolCapacityErrorOnlyWhenDirectIsPinned(t *testing.T) {
	t.Run("pinned-direct", func(t *testing.T) {
		pool, err := NewTransportPool(1, 1)
		if err != nil {
			t.Fatal(err)
		}
		defer pool.CloseIdleConnections()
		direct, _, err := pool.Get("")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := pool.Get("http://127.0.0.1:18080"); !errors.Is(err, ErrTransportRouteCapacity) {
			t.Fatalf("capacity error=%v", err)
		}
		if _, _, err := pool.Get("not a valid proxy://"); err == nil || errors.Is(err, ErrTransportRouteCapacity) {
			t.Fatalf("invalid proxy returned wrong error: %v", err)
		}
		if reused, _, err := pool.Get(""); err != nil || reused != direct {
			t.Fatal("pinned direct route changed")
		}
	})

	t.Run("direct-replaces-proxy", func(t *testing.T) {
		pool, err := NewTransportPool(1, 1)
		if err != nil {
			t.Fatal(err)
		}
		defer pool.CloseIdleConnections()
		if _, _, err := pool.Get("http://127.0.0.1:18080"); err != nil {
			t.Fatal(err)
		}
		if _, proxied, err := pool.Get(""); err != nil || proxied {
			t.Fatalf("direct route did not replace proxy: proxied=%v err=%v", proxied, err)
		}
	})
}

func TestTransportPoolKeepsDirectAfterThirtyTwoUnreachableProxies(t *testing.T) {
	pool, err := NewTransportPool(32, 32)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.CloseIdleConnections()
	direct, _, err := pool.Get("")
	if err != nil {
		t.Fatal(err)
	}
	for index := range 32 {
		if _, proxied, err := pool.Get(fmt.Sprintf("http://127.0.0.1:%d", 20_000+index)); err != nil || !proxied {
			t.Fatalf("proxy %d: proxied=%v err=%v", index, proxied, err)
		}
	}
	if pool.RouteCount() != 32 {
		t.Fatalf("routes=%d, want 32", pool.RouteCount())
	}
	if reused, proxied, err := pool.Get(""); err != nil || proxied || reused != direct {
		t.Fatalf("direct route unavailable after proxy churn: proxied=%v err=%v", proxied, err)
	}
}

func TestTransportPoolDoesNotRetainCredentialsInMapKeysOrFormatting(t *testing.T) {
	pool, err := NewTransportPool(2, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.CloseIdleConnections()
	if _, _, err := pool.Get("HTTP://alice:super-secret@PROXY.EXAMPLE:80/"); err != nil {
		t.Fatal(err)
	}

	wantKey := sha256.Sum256([]byte("http://alice:super-secret@proxy.example"))
	if _, exists := pool.routes[wantKey]; !exists {
		t.Fatal("canonical SHA-256 route key is missing")
	}
	formatted := []string{
		fmt.Sprintf("%+v", pool), fmt.Sprintf("%#v", pool),
		fmt.Sprintf("%+v", pool.routes), fmt.Sprintf("%#v", pool.routes),
	}
	for _, output := range formatted {
		for _, secret := range []string{"alice", "super-secret", "proxy.example"} {
			if strings.Contains(strings.ToLower(output), secret) {
				t.Fatalf("pool formatting leaked route material %q", secret)
			}
		}
	}
}

func TestTransportPoolCloseClearsRoutes(t *testing.T) {
	pool, err := NewTransportPool(2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := pool.Get(""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := pool.Get("http://127.0.0.1:18080"); err != nil {
		t.Fatal(err)
	}
	pool.CloseIdleConnections()
	pool.CloseIdleConnections()
	if pool.RouteCount() != 0 || len(pool.routes) != 0 {
		t.Fatalf("closed pool retained %d routes", pool.RouteCount())
	}
	if _, _, err := pool.Get(""); err == nil {
		t.Fatal("closed pool accepted a route")
	}
}

func TestTransportPoolConcurrentRouteChurn(t *testing.T) {
	pool, err := NewTransportPool(32, 4)
	if err != nil {
		t.Fatal(err)
	}
	direct, _, err := pool.Get("")
	if err != nil {
		t.Fatal(err)
	}

	var workers sync.WaitGroup
	errorsFound := make(chan error, 16)
	for worker := range 16 {
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			for iteration := range 64 {
				port := 20_000 + (worker*64+iteration)%64
				if _, _, err := pool.Get(fmt.Sprintf("http://127.0.0.1:%d", port)); err != nil {
					errorsFound <- err
					return
				}
				if iteration%8 == 0 {
					reused, proxied, err := pool.Get("")
					if err != nil || proxied || reused != direct {
						errorsFound <- fmt.Errorf("direct route changed: proxied=%v err=%v", proxied, err)
						return
					}
				}
				_ = pool.RouteCount()
			}
		}(worker)
	}
	workers.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Fatal(err)
	}
	if count := pool.RouteCount(); count < 1 || count > 32 {
		t.Fatalf("routes=%d", count)
	}
	pool.CloseIdleConnections()
}
