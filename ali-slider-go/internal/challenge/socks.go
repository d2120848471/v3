package challenge

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	socks4Version = 4
	socks5Version = 5
)

type socksProxy struct {
	scheme      string
	address     string
	username    string
	password    string
	remoteDNS   bool
	dialer      *net.Dialer
	resolver    *net.Resolver
	hasUserInfo bool
}

func newSOCKSDialContext(proxyURL *url.URL, dialer *net.Dialer) (func(context.Context, string, string) (net.Conn, error), error) {
	port := proxyURL.Port()
	if port == "" {
		port = "1080"
	}
	proxy := &socksProxy{
		scheme:    proxyURL.Scheme,
		address:   net.JoinHostPort(proxyURL.Hostname(), port),
		remoteDNS: proxyURL.Scheme == "socks5h",
		dialer:    dialer,
		resolver:  net.DefaultResolver,
	}
	if proxyURL.User != nil {
		proxy.hasUserInfo = true
		proxy.username = proxyURL.User.Username()
		proxy.password, _ = proxyURL.User.Password()
	}

	switch proxy.scheme {
	case "socks4":
		if proxyURL.User != nil {
			if _, hasPassword := proxyURL.User.Password(); hasPassword {
				return nil, errors.New("socks4 proxy userinfo supports a username only")
			}
		}
		if len(proxy.username) > 255 || strings.ContainsRune(proxy.username, '\x00') {
			return nil, errors.New("socks4 proxy username is invalid")
		}
	case "socks5", "socks5h":
		if proxy.hasUserInfo {
			_, hasPassword := proxyURL.User.Password()
			if !hasPassword || len(proxy.username) < 1 || len(proxy.username) > 255 || len(proxy.password) < 1 || len(proxy.password) > 255 {
				return nil, errors.New("socks5 proxy userinfo requires a 1..255 byte username and password")
			}
		}
	default:
		return nil, errors.New("unsupported SOCKS proxy scheme")
	}
	return proxy.dialContext, nil
}

func (proxy *socksProxy) dialContext(ctx context.Context, network, targetAddress string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, errors.New("SOCKS proxy supports TCP connections only")
	}
	targetHost, targetPortText, err := net.SplitHostPort(targetAddress)
	if err != nil || targetHost == "" {
		return nil, errors.New("SOCKS target must contain a host and port")
	}
	targetPort, err := strconv.Atoi(targetPortText)
	if err != nil || targetPort < 1 || targetPort > 65_535 {
		return nil, errors.New("SOCKS target port must be within 1..65535")
	}

	connection, err := proxy.dialer.DialContext(ctx, "tcp", proxy.address)
	if err != nil {
		return nil, fmt.Errorf("dial SOCKS proxy: %w", err)
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = connection.Close()
		}
	}()

	deadline := time.Now().Add(proxy.dialer.Timeout)
	contextDeadline, hasContextDeadline := ctx.Deadline()
	if hasContextDeadline && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := connection.SetDeadline(deadline); err != nil {
		return nil, errors.New("set SOCKS handshake deadline")
	}
	stopContextWatch := interruptConnectionOnContext(ctx, connection)

	switch proxy.scheme {
	case "socks4":
		err = proxy.handshake4(ctx, connection, targetHost, targetPort)
	case "socks5", "socks5h":
		err = proxy.handshake5(ctx, connection, targetHost, targetPort)
	}
	stopContextWatch()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		// net.Conn 截止时钟可能比 context timer 回调先醒来数微秒。
		if hasContextDeadline && !time.Now().Before(contextDeadline) {
			return nil, context.DeadlineExceeded
		}
		return nil, err
	}
	if err := connection.SetDeadline(time.Time{}); err != nil {
		return nil, errors.New("clear SOCKS handshake deadline")
	}
	closeOnError = false
	return connection, nil
}

func interruptConnectionOnContext(ctx context.Context, connection net.Conn) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
			_ = connection.SetDeadline(time.Now())
		case <-stop:
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

func (proxy *socksProxy) handshake4(ctx context.Context, connection net.Conn, targetHost string, targetPort int) error {
	targetIP, err := proxy.resolveIP(ctx, targetHost, true)
	if err != nil {
		return err
	}
	ipv4 := targetIP.To4()
	if ipv4 == nil {
		return errors.New("socks4 target must resolve to IPv4")
	}
	request := make([]byte, 9+len(proxy.username))
	request[0] = socks4Version
	request[1] = 1
	binary.BigEndian.PutUint16(request[2:4], uint16(targetPort))
	copy(request[4:8], ipv4)
	copy(request[8:], proxy.username)
	if err := writeAll(connection, request); err != nil {
		return errors.New("write SOCKS4 handshake")
	}

	response := make([]byte, 8)
	if _, err := io.ReadFull(connection, response); err != nil {
		return errors.New("read SOCKS4 handshake")
	}
	if response[0] != 0 && response[0] != socks4Version {
		return errors.New("SOCKS4 proxy returned an invalid response")
	}
	if response[1] != 0x5a {
		return fmt.Errorf("SOCKS4 proxy rejected connection (code 0x%02x)", response[1])
	}
	return nil
}

func (proxy *socksProxy) handshake5(ctx context.Context, connection net.Conn, targetHost string, targetPort int) error {
	method := byte(0)
	if proxy.hasUserInfo {
		method = 2
	}
	if err := writeAll(connection, []byte{socks5Version, 1, method}); err != nil {
		return errors.New("write SOCKS5 greeting")
	}
	greeting := make([]byte, 2)
	if _, err := io.ReadFull(connection, greeting); err != nil {
		return errors.New("read SOCKS5 greeting")
	}
	if greeting[0] != socks5Version || greeting[1] != method {
		return errors.New("SOCKS5 proxy rejected authentication method")
	}
	if method == 2 {
		credentials := make([]byte, 0, 3+len(proxy.username)+len(proxy.password))
		credentials = append(credentials, 1, byte(len(proxy.username)))
		credentials = append(credentials, proxy.username...)
		credentials = append(credentials, byte(len(proxy.password)))
		credentials = append(credentials, proxy.password...)
		if err := writeAll(connection, credentials); err != nil {
			return errors.New("write SOCKS5 authentication")
		}
		authentication := make([]byte, 2)
		if _, err := io.ReadFull(connection, authentication); err != nil {
			return errors.New("read SOCKS5 authentication")
		}
		if authentication[0] != 1 || authentication[1] != 0 {
			return errors.New("SOCKS5 proxy authentication failed")
		}
	}

	target, err := proxy.socks5Target(ctx, targetHost)
	if err != nil {
		return err
	}
	request := append([]byte{socks5Version, 1, 0}, target...)
	request = binary.BigEndian.AppendUint16(request, uint16(targetPort))
	if err := writeAll(connection, request); err != nil {
		return errors.New("write SOCKS5 connect request")
	}

	header := make([]byte, 4)
	if _, err := io.ReadFull(connection, header); err != nil {
		return errors.New("read SOCKS5 connect response")
	}
	if header[0] != socks5Version || header[2] != 0 {
		return errors.New("SOCKS5 proxy returned an invalid response")
	}
	if header[1] != 0 {
		return fmt.Errorf("SOCKS5 proxy rejected connection (code 0x%02x)", header[1])
	}
	if err := discardSOCKS5Address(connection, header[3]); err != nil {
		return err
	}
	return nil
}

func (proxy *socksProxy) socks5Target(ctx context.Context, host string) ([]byte, error) {
	if parsedIP := net.ParseIP(host); parsedIP != nil {
		return encodeSOCKS5IP(parsedIP), nil
	}
	if proxy.remoteDNS {
		domain := []byte(host)
		if len(domain) < 1 || len(domain) > 255 {
			return nil, errors.New("SOCKS5 target domain must be within 1..255 bytes")
		}
		return append([]byte{3, byte(len(domain))}, domain...), nil
	}
	resolved, err := proxy.resolveIP(ctx, host, false)
	if err != nil {
		return nil, err
	}
	return encodeSOCKS5IP(resolved), nil
}

func (proxy *socksProxy) resolveIP(ctx context.Context, host string, ipv4Only bool) (net.IP, error) {
	if parsedIP := net.ParseIP(host); parsedIP != nil {
		if ipv4Only && parsedIP.To4() == nil {
			return nil, errors.New("SOCKS4 does not support IPv6 targets")
		}
		return parsedIP, nil
	}
	addresses, err := proxy.resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, errors.New("resolve SOCKS target host")
	}
	for _, address := range addresses {
		if address.IP.To4() != nil {
			return address.IP, nil
		}
	}
	if !ipv4Only {
		for _, address := range addresses {
			if address.IP.To16() != nil {
				return address.IP, nil
			}
		}
	}
	return nil, errors.New("SOCKS target host has no supported IP address")
}

func encodeSOCKS5IP(ip net.IP) []byte {
	if ipv4 := ip.To4(); ipv4 != nil {
		return append([]byte{1}, ipv4...)
	}
	return append([]byte{4}, ip.To16()...)
}

func discardSOCKS5Address(connection net.Conn, addressType byte) error {
	length := 0
	switch addressType {
	case 1:
		length = net.IPv4len
	case 4:
		length = net.IPv6len
	case 3:
		domainLength := make([]byte, 1)
		if _, err := io.ReadFull(connection, domainLength); err != nil {
			return errors.New("read SOCKS5 bound address")
		}
		length = int(domainLength[0])
	default:
		return errors.New("SOCKS5 proxy returned an invalid address type")
	}
	if _, err := io.CopyN(io.Discard, connection, int64(length+2)); err != nil {
		return errors.New("read SOCKS5 bound address")
	}
	return nil
}

func writeAll(connection net.Conn, payload []byte) error {
	for len(payload) > 0 {
		written, err := connection.Write(payload)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrUnexpectedEOF
		}
		payload = payload[written:]
	}
	return nil
}
