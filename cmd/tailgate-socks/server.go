package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"
)

const (
	socksVersion = 5

	authNone         = 0
	authNoAcceptable = 0xff

	commandConnect = 1
	commandBind    = 2

	addressIPv4   = 1
	addressDomain = 3
	addressIPv6   = 4

	replySucceeded           = 0
	replyGeneralFailure      = 1
	replyHostUnreachable     = 4
	replyCommandNotSupported = 7
	replyAddressNotSupported = 8

	defaultHandshakeTimeout = 5 * time.Second
	defaultResolveTimeout   = 3 * time.Second
	defaultRelayIdleTimeout = 2 * time.Minute
)

var (
	cgNATPrefix  = netip.MustParsePrefix("100.64.0.0/10")
	tailscaleULA = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
)

type resolver interface {
	Resolve(context.Context, string) (netip.Addr, error)
}

type resolverFunc func(context.Context, string) (netip.Addr, error)

func (f resolverFunc) Resolve(ctx context.Context, name string) (netip.Addr, error) {
	return f(ctx, name)
}

type queryFunc func(context.Context, string, string) ([]byte, error)

type tailscaleResolver struct {
	query queryFunc
}

type dnsAnswer struct {
	Type string `json:"Type"`
	Body string `json:"Body"`
}

type dnsQueryResponse struct {
	ResponseCode string      `json:"ResponseCode"`
	Answers      []dnsAnswer `json:"Answers"`
}

func (r tailscaleResolver) Resolve(ctx context.Context, name string) (netip.Addr, error) {
	if r.query == nil {
		return netip.Addr{}, errors.New("DNS query function is not configured")
	}
	for _, queryType := range []string{"A", "AAAA"} {
		output, err := r.query(ctx, name, queryType)
		if err != nil {
			return netip.Addr{}, fmt.Errorf("querying %s %s: %w", name, queryType, err)
		}
		var response dnsQueryResponse
		if err := json.Unmarshal(output, &response); err != nil {
			return netip.Addr{}, fmt.Errorf("decoding %s %s response: %w", name, queryType, err)
		}
		if response.ResponseCode != "RCodeSuccess" {
			return netip.Addr{}, fmt.Errorf("querying %s %s: response code %s", name, queryType, response.ResponseCode)
		}
		wantType := "Type" + queryType
		for _, answer := range response.Answers {
			if answer.Type != wantType {
				continue
			}
			addr, err := netip.ParseAddr(answer.Body)
			if err != nil {
				return netip.Addr{}, fmt.Errorf("querying %s %s: invalid address", name, queryType)
			}
			if !isTailnetAddress(addr) {
				return netip.Addr{}, fmt.Errorf("querying %s %s: answer is outside the Tailnet ranges", name, queryType)
			}
			return addr, nil
		}
	}
	return netip.Addr{}, fmt.Errorf("querying %s: no Tailnet A or AAAA answer", name)
}

func commandQuery(ctx context.Context, name, queryType string) ([]byte, error) {
	output, err := exec.CommandContext(ctx, "tailscale", "dns", "query", "--json", name, queryType).Output()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	return output, err
}

func matchesSuffix(name, suffix string) bool {
	name = canonicalDNSName(name)
	suffix = canonicalDNSName(suffix)
	if name == "" || suffix == "" {
		return false
	}
	return name == suffix || strings.HasSuffix(name, "."+suffix)
}

func canonicalDNSName(name string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
}

func validDNSName(name string) bool {
	name = canonicalDNSName(name)
	if name == "" || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
				return false
			}
		}
	}
	return true
}

func resolveConfiguredName(ctx context.Context, name, suffix string, configured resolver) (netip.Addr, error) {
	if !matchesSuffix(name, suffix) {
		return netip.Addr{}, fmt.Errorf("%q is outside the configured Tailnet suffix", name)
	}
	if !validDNSName(name) {
		return netip.Addr{}, fmt.Errorf("%q is not a valid DNS name", name)
	}
	if configured == nil {
		return netip.Addr{}, errors.New("resolver is not configured")
	}
	addr, err := configured.Resolve(ctx, name)
	if err != nil {
		return netip.Addr{}, err
	}
	if !isTailnetAddress(addr) {
		return netip.Addr{}, fmt.Errorf("resolved address %s is outside the Tailnet ranges", addr)
	}
	return addr, nil
}

func isTailnetAddress(addr netip.Addr) bool {
	return cgNATPrefix.Contains(addr) || tailscaleULA.Contains(addr)
}

// Server accepts SOCKS5 CONNECT requests and rewrites configured Tailnet names
// to addresses obtained from tailscaled's internal DNS resolver.
type Server struct {
	Suffix           string
	Resolver         resolver
	HandshakeTimeout time.Duration
	ResolveTimeout   time.Duration
	RelayIdleTimeout time.Duration
	DialBackend      func(context.Context) (net.Conn, error)
	Logf             func(string, ...any)
}

func (s Server) Serve(listener net.Listener) error {
	return s.ServeContext(context.Background(), listener)
}

func (s Server) ServeContext(parent context.Context, listener net.Listener) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		go s.handle(ctx, conn)
	}
}

func (s Server) handle(parent context.Context, client net.Conn) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	defer client.Close()
	stopClient := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stopClient()

	handshakeTimeout := durationOrDefault(s.HandshakeTimeout, defaultHandshakeTimeout)
	if err := client.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		s.logf("client deadline failed: %v", err)
		return
	}
	if err := s.negotiate(client); err != nil {
		s.logf("client negotiation failed: %v", err)
		return
	}
	if err := client.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		s.logf("client deadline failed: %v", err)
		return
	}

	host, port, err := readRequest(client)
	if err != nil {
		var requestErr requestError
		if errors.As(err, &requestErr) {
			_ = writeFailureReply(client, requestErr.reply)
		}
		s.logf("client request failed: %v", err)
		return
	}

	backendHost := host
	if _, err := netip.ParseAddr(host); err != nil && matchesSuffix(host, s.Suffix) {
		resolveCtx, resolveCancel := context.WithTimeout(ctx, durationOrDefault(s.ResolveTimeout, defaultResolveTimeout))
		addr, resolveErr := resolveConfiguredName(resolveCtx, host, s.Suffix, s.Resolver)
		resolveCancel()
		if resolveErr != nil {
			_ = writeFailureReply(client, replyHostUnreachable)
			s.logf("internal resolution failed for %q: %v", host, resolveErr)
			return
		}
		backendHost = addr.String()
	}

	if s.DialBackend == nil {
		_ = writeFailureReply(client, replyGeneralFailure)
		s.logf("backend connection failed for %q: backend dialer is not configured", host)
		return
	}
	backendCtx, backendCancel := context.WithTimeout(ctx, handshakeTimeout)
	backend, err := s.DialBackend(backendCtx)
	backendCancel()
	if err != nil {
		_ = writeFailureReply(client, replyGeneralFailure)
		s.logf("backend connection failed for %q: %v", host, err)
		return
	}
	defer backend.Close()
	stopBackend := context.AfterFunc(ctx, func() { _ = backend.Close() })
	defer stopBackend()
	if err := backend.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		_ = writeFailureReply(client, replyGeneralFailure)
		s.logf("backend deadline failed for %q: %v", host, err)
		return
	}

	if err := negotiateBackend(backend); err != nil {
		_ = writeFailureReply(client, replyGeneralFailure)
		s.logf("backend negotiation failed for %q: %v", host, err)
		return
	}
	request, err := encodeConnectRequest(backendHost, port)
	if err != nil {
		_ = writeFailureReply(client, replyAddressNotSupported)
		s.logf("backend request failed for %q: %v", host, err)
		return
	}
	if _, err := backend.Write(request); err != nil {
		_ = writeFailureReply(client, replyGeneralFailure)
		s.logf("backend request failed for %q: %v", host, err)
		return
	}
	reply, code, err := readReply(backend)
	if err != nil {
		_ = writeFailureReply(client, replyGeneralFailure)
		s.logf("backend reply failed for %q: %v", host, err)
		return
	}
	if err := client.SetWriteDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return
	}
	if _, err := client.Write(reply); err != nil {
		return
	}
	if code != replySucceeded {
		s.logf("backend rejected %q with SOCKS reply %d", host, code)
		return
	}
	if err := client.SetDeadline(time.Time{}); err != nil {
		return
	}
	if err := backend.SetDeadline(time.Time{}); err != nil {
		return
	}

	relay(ctx, client, backend, durationOrDefault(s.RelayIdleTimeout, defaultRelayIdleTimeout))
}

func durationOrDefault(value, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return fallback
}

func (s Server) negotiate(conn net.Conn) error {
	header := make([]byte, 2)
	n, err := io.ReadFull(conn, header)
	if err != nil {
		if n > 0 && header[0] == socksVersion {
			_ = writeAuthReply(conn, authNoAcceptable)
		}
		return err
	}
	if header[0] != socksVersion {
		return fmt.Errorf("unsupported SOCKS version %d", header[0])
	}
	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		_ = writeAuthReply(conn, authNoAcceptable)
		return err
	}
	for _, method := range methods {
		if method == authNone {
			return writeAuthReply(conn, authNone)
		}
	}
	if err := writeAuthReply(conn, authNoAcceptable); err != nil {
		return err
	}
	return errors.New("client did not offer no-auth authentication")
}

func negotiateBackend(conn net.Conn) error {
	if _, err := conn.Write([]byte{socksVersion, 1, authNone}); err != nil {
		return err
	}
	response := make([]byte, 2)
	if _, err := io.ReadFull(conn, response); err != nil {
		return err
	}
	if response[0] != socksVersion {
		return fmt.Errorf("backend returned unsupported SOCKS version %d", response[0])
	}
	if response[1] != authNone {
		return fmt.Errorf("backend selected unsupported authentication %d", response[1])
	}
	return nil
}

type requestError struct {
	reply byte
	err   error
}

func (e requestError) Error() string { return e.err.Error() }
func (e requestError) Unwrap() error { return e.err }

func readRequest(reader io.Reader) (string, uint16, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(reader, header); err != nil {
		return "", 0, requestError{reply: replyGeneralFailure, err: fmt.Errorf("reading request header: %w", err)}
	}
	if header[0] != socksVersion {
		return "", 0, requestError{reply: replyGeneralFailure, err: fmt.Errorf("unsupported SOCKS version %d", header[0])}
	}
	if header[1] != commandConnect {
		return "", 0, requestError{reply: replyCommandNotSupported, err: fmt.Errorf("unsupported command %d", header[1])}
	}
	if header[2] != 0 {
		return "", 0, requestError{reply: replyGeneralFailure, err: errors.New("invalid reserved byte")}
	}
	switch header[3] {
	case addressIPv4, addressIPv6, addressDomain:
		// Recognized address types that cannot be read are malformed requests.
	default:
		return "", 0, requestError{
			reply: replyAddressNotSupported,
			err:   fmt.Errorf("unsupported address type %d", header[3]),
		}
	}
	host, port, err := readAddress(reader, header[3])
	if err != nil {
		return "", 0, requestError{reply: replyGeneralFailure, err: err}
	}
	return host, port, nil
}

func readAddress(reader io.Reader, addressType byte) (string, uint16, error) {
	var size int
	switch addressType {
	case addressIPv4:
		size = net.IPv4len
	case addressIPv6:
		size = net.IPv6len
	case addressDomain:
		length := []byte{0}
		if _, err := io.ReadFull(reader, length); err != nil {
			return "", 0, err
		}
		if length[0] == 0 {
			return "", 0, errors.New("empty domain")
		}
		size = int(length[0])
	default:
		return "", 0, fmt.Errorf("unsupported address type %d", addressType)
	}
	body := make([]byte, size+2)
	if _, err := io.ReadFull(reader, body); err != nil {
		return "", 0, err
	}
	var host string
	if addressType == addressDomain {
		host = string(body[:size])
	} else {
		host = net.IP(body[:size]).String()
	}
	return host, uint16(body[size])<<8 | uint16(body[size+1]), nil
}

func encodeConnectRequest(host string, port uint16) ([]byte, error) {
	request := []byte{socksVersion, commandConnect, 0}
	if addr, err := netip.ParseAddr(host); err == nil {
		if addr.Is4() {
			bytes := addr.As4()
			request = append(request, addressIPv4)
			request = append(request, bytes[:]...)
		} else {
			bytes := addr.As16()
			request = append(request, addressIPv6)
			request = append(request, bytes[:]...)
		}
	} else {
		if len(host) == 0 || len(host) > 255 {
			return nil, errors.New("domain length is outside the SOCKS range")
		}
		request = append(request, addressDomain, byte(len(host)))
		request = append(request, host...)
	}
	return append(request, byte(port>>8), byte(port)), nil
}

func readReply(reader io.Reader) ([]byte, byte, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(reader, header); err != nil {
		return nil, 0, err
	}
	if header[0] != socksVersion || header[2] != 0 {
		return nil, 0, errors.New("malformed backend reply")
	}
	body, err := readRawAddress(reader, header[3])
	if err != nil {
		return nil, 0, err
	}
	return append(header, body...), header[1], nil
}

func readRawAddress(reader io.Reader, addressType byte) ([]byte, error) {
	var size int
	prefix := []byte(nil)
	switch addressType {
	case addressIPv4:
		size = net.IPv4len
	case addressIPv6:
		size = net.IPv6len
	case addressDomain:
		length := []byte{0}
		if _, err := io.ReadFull(reader, length); err != nil {
			return nil, err
		}
		if length[0] == 0 {
			return nil, errors.New("empty backend reply domain")
		}
		prefix = length
		size = int(length[0])
	default:
		return nil, fmt.Errorf("unsupported address type %d", addressType)
	}
	body := make([]byte, size+2)
	if _, err := io.ReadFull(reader, body); err != nil {
		return nil, err
	}
	return append(prefix, body...), nil
}

func writeAuthReply(conn net.Conn, method byte) error {
	_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	_, err := conn.Write([]byte{socksVersion, method})
	return err
}

func writeFailureReply(writer io.Writer, code byte) error {
	if conn, ok := writer.(interface{ SetWriteDeadline(time.Time) error }); ok {
		_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	}
	_, err := writer.Write([]byte{socksVersion, code, 0, addressIPv4, 0, 0, 0, 0, 0, 0})
	return err
}

type relayResult struct {
	err error
}

func relay(ctx context.Context, client, backend net.Conn, idleTimeout time.Duration) {
	results := make(chan relayResult, 2)
	var lastProgress atomic.Int64
	copyHalf := func(dst, src net.Conn) {
		err := copyWithProgress(dst, src, func() {
			lastProgress.Store(time.Now().UnixNano())
		})
		if err == nil {
			if closer, ok := dst.(interface{ CloseWrite() error }); ok {
				_ = closer.CloseWrite()
			}
		}
		results <- relayResult{err: err}
	}
	go copyHalf(backend, client)
	go copyHalf(client, backend)

	var first relayResult
	select {
	case first = <-results:
	case <-ctx.Done():
		closeRelay(client, backend)
		<-results
		<-results
		return
	}

	if first.err != nil {
		closeRelay(client, backend)
		<-results
		return
	}

	lastProgress.Store(time.Now().UnixNano())
	timer := time.NewTimer(idleTimeout)
	defer timer.Stop()
	for {
		select {
		case second := <-results:
			if second.err != nil {
				closeRelay(client, backend)
			}
			return
		case <-ctx.Done():
			closeRelay(client, backend)
			<-results
			return
		case now := <-timer.C:
			last := time.Unix(0, lastProgress.Load())
			remaining := idleTimeout - now.Sub(last)
			if remaining > 0 {
				timer.Reset(remaining)
				continue
			}
			closeRelay(client, backend)
			<-results
			return
		}
	}
}

func copyWithProgress(dst io.Writer, src io.Reader, progress func()) error {
	buffer := make([]byte, 32*1024)
	for {
		n, readErr := src.Read(buffer)
		if n > 0 {
			written := 0
			for written < n {
				count, writeErr := dst.Write(buffer[written:n])
				if count > 0 {
					written += count
					progress()
				}
				if writeErr != nil {
					return writeErr
				}
				if count == 0 {
					return io.ErrNoProgress
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}

func closeRelay(client, backend net.Conn) {
	_ = client.Close()
	_ = backend.Close()
}

func (s Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}
