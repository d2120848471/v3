package httpclient

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewTransportDirectIgnoresEnvironmentProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://environment.invalid:8080")
	t.Setenv("HTTPS_PROXY", "http://environment.invalid:8080")
	transport, proxied, err := NewTransport("", 1)
	if err != nil {
		t.Fatal(err)
	}
	if proxied || transport.Proxy != nil {
		t.Fatal("direct transport must ignore environment proxies")
	}
}

func TestSOCKS5HHandshakeUsesProxyDNSAndCredentials(t *testing.T) {
	address, wait := startMockSOCKSProxy(t, func(connection net.Conn) error {
		greeting := make([]byte, 3)
		if _, err := io.ReadFull(connection, greeting); err != nil {
			return err
		}
		if !bytes.Equal(greeting, []byte{5, 1, 2}) {
			return fmt.Errorf("unexpected greeting %v", greeting)
		}
		if _, err := connection.Write([]byte{5, 2}); err != nil {
			return err
		}

		authHeader := make([]byte, 2)
		if _, err := io.ReadFull(connection, authHeader); err != nil {
			return err
		}
		username := make([]byte, int(authHeader[1]))
		if _, err := io.ReadFull(connection, username); err != nil {
			return err
		}
		passwordLength := make([]byte, 1)
		if _, err := io.ReadFull(connection, passwordLength); err != nil {
			return err
		}
		password := make([]byte, int(passwordLength[0]))
		if _, err := io.ReadFull(connection, password); err != nil {
			return err
		}
		if authHeader[0] != 1 || string(username) != "alice" || string(password) != "secret" {
			return errors.New("unexpected SOCKS5 credentials")
		}
		if _, err := connection.Write([]byte{1, 0}); err != nil {
			return err
		}

		host, port, addressType, err := readSOCKS5Target(connection)
		if err != nil {
			return err
		}
		if addressType != 3 || host != "target.invalid" || port != 443 {
			return fmt.Errorf("unexpected target type=%d host=%s port=%d", addressType, host, port)
		}
		_, err = connection.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 80})
		return err
	})

	transport, proxied, err := NewTransport("socks5h://alice:secret@"+address, 2)
	if err != nil || !proxied {
		t.Fatalf("NewTransport: proxied=%v err=%v", proxied, err)
	}
	connection, err := transport.DialContext(context.Background(), "tcp", "target.invalid:443")
	if err != nil {
		t.Fatal(err)
	}
	_ = connection.Close()
	wait()
}

func TestSOCKS5ResolvesDomainLocally(t *testing.T) {
	address, wait := startMockSOCKSProxy(t, func(connection net.Conn) error {
		greeting := make([]byte, 3)
		if _, err := io.ReadFull(connection, greeting); err != nil {
			return err
		}
		if !bytes.Equal(greeting, []byte{5, 1, 0}) {
			return fmt.Errorf("unexpected greeting %v", greeting)
		}
		if _, err := connection.Write([]byte{5, 0}); err != nil {
			return err
		}
		host, port, addressType, err := readSOCKS5Target(connection)
		if err != nil {
			return err
		}
		if addressType != 1 || host != "127.0.0.1" || port != 80 {
			return fmt.Errorf("SOCKS5 must send locally resolved IPv4, got type=%d host=%s port=%d", addressType, host, port)
		}
		_, err = connection.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 80})
		return err
	})

	transport, _, err := NewTransport("socks5://"+address, 2)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := transport.DialContext(context.Background(), "tcp", "localhost:80")
	if err != nil {
		t.Fatal(err)
	}
	_ = connection.Close()
	wait()
}

func TestSOCKS4ResolvesDomainLocallyAndSendsUserID(t *testing.T) {
	address, wait := startMockSOCKSProxy(t, func(connection net.Conn) error {
		header := make([]byte, 8)
		if _, err := io.ReadFull(connection, header); err != nil {
			return err
		}
		if header[0] != 4 || header[1] != 1 || binary.BigEndian.Uint16(header[2:4]) != 80 || !bytes.Equal(header[4:8], net.IPv4(127, 0, 0, 1).To4()) {
			return fmt.Errorf("unexpected SOCKS4 header %v", header)
		}
		userID, err := readNullTerminated(connection, 256)
		if err != nil {
			return err
		}
		if userID != "alice" {
			return fmt.Errorf("unexpected SOCKS4 user ID %q", userID)
		}
		_, err = connection.Write([]byte{0, 0x5a, 0, 80, 127, 0, 0, 1})
		return err
	})

	transport, _, err := NewTransport("socks4://alice@"+address, 2)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := transport.DialContext(context.Background(), "tcp", "localhost:80")
	if err != nil {
		t.Fatal(err)
	}
	_ = connection.Close()
	wait()
}

func TestSOCKSHandshakeHonorsContextDeadline(t *testing.T) {
	address, wait := startMockSOCKSProxy(t, func(connection net.Conn) error {
		greeting := make([]byte, 3)
		if _, err := io.ReadFull(connection, greeting); err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, connection)
		return nil
	})
	transport, _, err := NewTransport("socks5://"+address, 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = transport.DialContext(ctx, "tcp", "127.0.0.1:80")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline, got %v", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("context cancellation took %s", elapsed)
	}
	wait()
}

func TestSOCKSAuthenticationErrorDoesNotLeakPassword(t *testing.T) {
	address, wait := startMockSOCKSProxy(t, func(connection net.Conn) error {
		greeting := make([]byte, 3)
		if _, err := io.ReadFull(connection, greeting); err != nil {
			return err
		}
		if _, err := connection.Write([]byte{5, 2}); err != nil {
			return err
		}
		header := make([]byte, 2)
		if _, err := io.ReadFull(connection, header); err != nil {
			return err
		}
		credentialsLength := int(header[1])
		username := make([]byte, credentialsLength)
		if _, err := io.ReadFull(connection, username); err != nil {
			return err
		}
		passwordLength := make([]byte, 1)
		if _, err := io.ReadFull(connection, passwordLength); err != nil {
			return err
		}
		password := make([]byte, int(passwordLength[0]))
		if _, err := io.ReadFull(connection, password); err != nil {
			return err
		}
		_, err := connection.Write([]byte{1, 1})
		return err
	})
	transport, _, err := NewTransport("socks5://alice:super-secret@"+address, 1)
	if err != nil {
		t.Fatal(err)
	}
	_, err = transport.DialContext(context.Background(), "tcp", "127.0.0.1:80")
	if err == nil {
		t.Fatal("expected authentication failure")
	}
	if strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("password leaked in error: %v", err)
	}
	wait()
}

func TestNewTransportRejectsInvalidProxyParameters(t *testing.T) {
	for _, test := range []struct {
		name        string
		proxy       string
		connections int
	}{
		{name: "zero connections", connections: 0},
		{name: "too many connections", connections: 33},
		{name: "unsupported scheme", proxy: "ftp://proxy.invalid:21", connections: 1},
		{name: "missing host", proxy: "socks5://:1080", connections: 1},
		{name: "zero port", proxy: "socks5://proxy.invalid:0", connections: 1},
		{name: "large port", proxy: "socks5://proxy.invalid:65536", connections: 1},
		{name: "nonnumeric port", proxy: "socks5://proxy.invalid:nope", connections: 1},
		{name: "path", proxy: "socks5://proxy.invalid:1080/path", connections: 1},
		{name: "query", proxy: "socks5://proxy.invalid:1080?x=1", connections: 1},
		{name: "socks4 password", proxy: "socks4://user:password@proxy.invalid:1080", connections: 1},
		{name: "socks5 missing password", proxy: "socks5://user@proxy.invalid:1080", connections: 1},
		{name: "socks5 empty password", proxy: "socks5://user:@proxy.invalid:1080", connections: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := NewTransport(test.proxy, test.connections); err == nil {
				t.Fatal("expected invalid proxy error")
			}
		})
	}

	_, _, err := NewTransport("socks5://user:top-secret@proxy.invalid:nope", 1)
	if err == nil || strings.Contains(err.Error(), "top-secret") {
		t.Fatalf("invalid URL error leaked password: %v", err)
	}
}

func startMockSOCKSProxy(t *testing.T, handler func(net.Conn) error) (string, func()) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			result <- acceptErr
			return
		}
		defer connection.Close()
		result <- handler(connection)
	}()

	var once sync.Once
	wait := func() {
		once.Do(func() {
			_ = listener.Close()
			if proxyErr := <-result; proxyErr != nil {
				t.Errorf("mock SOCKS proxy: %v", proxyErr)
			}
		})
	}
	t.Cleanup(wait)
	return listener.Addr().String(), wait
}

func readSOCKS5Target(connection net.Conn) (string, int, byte, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(connection, header); err != nil {
		return "", 0, 0, err
	}
	if !bytes.Equal(header[:3], []byte{5, 1, 0}) {
		return "", 0, 0, fmt.Errorf("unexpected connect header %v", header)
	}
	addressType := header[3]
	var host string
	switch addressType {
	case 1:
		value := make([]byte, net.IPv4len)
		if _, err := io.ReadFull(connection, value); err != nil {
			return "", 0, 0, err
		}
		host = net.IP(value).String()
	case 3:
		length := make([]byte, 1)
		if _, err := io.ReadFull(connection, length); err != nil {
			return "", 0, 0, err
		}
		value := make([]byte, int(length[0]))
		if _, err := io.ReadFull(connection, value); err != nil {
			return "", 0, 0, err
		}
		host = string(value)
	case 4:
		value := make([]byte, net.IPv6len)
		if _, err := io.ReadFull(connection, value); err != nil {
			return "", 0, 0, err
		}
		host = net.IP(value).String()
	default:
		return "", 0, 0, fmt.Errorf("unexpected address type %d", addressType)
	}
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(connection, portBytes); err != nil {
		return "", 0, 0, err
	}
	return host, int(binary.BigEndian.Uint16(portBytes)), addressType, nil
}

func readNullTerminated(connection net.Conn, limit int) (string, error) {
	value := make([]byte, 0, limit)
	for len(value) < limit {
		character := make([]byte, 1)
		if _, err := io.ReadFull(connection, character); err != nil {
			return "", err
		}
		if character[0] == 0 {
			return string(value), nil
		}
		value = append(value, character[0])
	}
	return "", errors.New("unterminated value")
}
