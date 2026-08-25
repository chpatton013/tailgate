package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"
)

func TestMatchesSuffix(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name   string
		suffix string
		want   bool
	}{
		{name: "mail.ts.example.com", suffix: "ts.example.com", want: true},
		{name: "MAIL.TS.EXAMPLE.COM", suffix: "ts.example.com", want: true},
		{name: "ts.example.com", suffix: "TS.EXAMPLE.COM", want: true},
		{name: "mail.other.example.com", suffix: "ts.example.com", want: false},
		{name: "mail.notts.example.com", suffix: "ts.example.com", want: false},
		{name: "mail.ts.example.com.evil", suffix: "ts.example.com", want: false},
		{name: "", suffix: "ts.example.com", want: false},
		{name: "mail.ts.example.com", suffix: "", want: false},
	} {
		test := test
		t.Run(test.name+"/"+test.suffix, func(t *testing.T) {
			t.Parallel()
			if got := matchesSuffix(test.name, test.suffix); got != test.want {
				t.Fatalf("matchesSuffix(%q, %q) = %v, want %v", test.name, test.suffix, got, test.want)
			}
		})
	}
}

func TestTailnetAddressValidation(t *testing.T) {
	t.Parallel()

	for raw, want := range map[string]bool{
		"100.64.0.1":          true,
		"100.127.255.254":     true,
		"100.63.255.255":      false,
		"100.128.0.1":         false,
		"fd7a:115c:a1e0::1":   true,
		"fd7a:115c:a1e0:1::1": true,
		"fd7a:115c:a1e1::1":   false,
		"not-an-ip":           false,
	} {
		addr, err := netip.ParseAddr(raw)
		got := err == nil && isTailnetAddress(addr)
		if got != want {
			t.Errorf("isTailnetAddress(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestTailscaleResolverUsesAThenAAAA(t *testing.T) {
	t.Parallel()

	var queries []string
	resolver := tailscaleResolver{
		query: func(_ context.Context, name, queryType string) ([]byte, error) {
			queries = append(queries, name+"/"+queryType)
			if queryType == "A" {
				return dnsResponse(t, "RCodeSuccess"), nil
			}
			return dnsResponse(t, "RCodeSuccess", dnsAnswer{Type: "TypeAAAA", Body: "fd7a:115c:a1e0::42"}), nil
		},
	}

	got, err := resolver.Resolve(context.Background(), "mail.ts.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if want := netip.MustParseAddr("fd7a:115c:a1e0::42"); got != want {
		t.Fatalf("Resolve() = %v, want %v", got, want)
	}
	if len(queries) != 2 || queries[0] != "mail.ts.example.com/A" || queries[1] != "mail.ts.example.com/AAAA" {
		t.Fatalf("queries = %v, want A then AAAA", queries)
	}
}

func TestTailscaleResolverRejectsUnsafeResponses(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		response []byte
		queryErr error
	}{
		{name: "query error", queryErr: errors.New("query failed")},
		{name: "malformed JSON", response: []byte("not-json")},
		{name: "non-success response", response: dnsResponse(t, "RCodeNameError")},
		{name: "public answer", response: dnsResponse(t, "RCodeSuccess", dnsAnswer{Type: "TypeA", Body: "203.0.113.8"})},
		{name: "wrong answer type", response: dnsResponse(t, "RCodeSuccess", dnsAnswer{Type: "TypeAAAA", Body: "fd7a:115c:a1e0::42"})},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			resolver := tailscaleResolver{query: func(_ context.Context, _, queryType string) ([]byte, error) {
				if queryType == "AAAA" && test.name == "wrong answer type" {
					return dnsResponse(t, "RCodeSuccess"), nil
				}
				return test.response, test.queryErr
			}}
			if _, err := resolver.Resolve(context.Background(), "mail.ts.example.com"); err == nil {
				t.Fatal("Resolve() succeeded, want error")
			}
		})
	}
}

func TestResolveConfiguredNameUsesExactTailnetValidation(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name       string
		queryName  string
		resolved   string
		want       string
		wantErr    bool
		resolverOK bool
	}{
		{name: "IPv4", queryName: "mail.ts.example.com", resolved: "100.64.0.7", want: "100.64.0.7", resolverOK: true},
		{name: "IPv6", queryName: "mail.ts.example.com", resolved: "fd7a:115c:a1e0::7", want: "fd7a:115c:a1e0::7", resolverOK: true},
		{name: "public suffix", queryName: "example.com", resolved: "100.64.0.7", wantErr: true},
		{name: "malformed name", queryName: "bad name.ts.example.com", resolved: "100.64.0.7", wantErr: true},
		{name: "public answer", queryName: "mail.ts.example.com", resolved: "203.0.113.7", wantErr: true, resolverOK: true},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			called := false
			resolver := resolverFunc(func(context.Context, string) (netip.Addr, error) {
				called = true
				return netip.MustParseAddr(test.resolved), nil
			})
			got, err := resolveConfiguredName(context.Background(), test.queryName, "ts.example.com", resolver)
			if (err != nil) != test.wantErr {
				t.Fatalf("resolveConfiguredName() error = %v, wantErr %v", err, test.wantErr)
			}
			if !test.wantErr && got.String() != test.want {
				t.Fatalf("resolveConfiguredName() = %v, want %s", got, test.want)
			}
			if called != test.resolverOK {
				t.Fatalf("resolver called = %v, want %v", called, test.resolverOK)
			}
		})
	}
}

func TestConnectRewritesTailnetAliasAndRelaysPayload(t *testing.T) {
	t.Parallel()

	backend := startFakeBackend(t)
	resolver := resolverFunc(func(_ context.Context, name string) (netip.Addr, error) {
		if name != "mail.ts.example.com" {
			return netip.Addr{}, fmt.Errorf("Resolve(%q), want mail.ts.example.com", name)
		}
		return netip.MustParseAddr("100.100.50.25"), nil
	})
	server := startShim(t, Server{
		Suffix:         "ts.example.com",
		Resolver:       resolver,
		ResolveTimeout: time.Second,
		DialBackend: func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", backend.addr)
		},
	})

	conn := dialNoAuth(t, server)
	writeConnectDomain(t, conn, "mail.ts.example.com", 443)
	readSuccessReply(t, conn)
	payload := []byte("preserve TLS SNI and application bytes")
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("relayed payload = %q, want %q", got, payload)
	}
	conn.Close()

	request := <-backend.requests
	if request.host != "100.100.50.25" || request.port != 443 {
		t.Fatalf("backend request = %s:%d, want 100.100.50.25:443", request.host, request.port)
	}
}

func TestConnectLeavesLiteralAndPublicDestinationsUnchanged(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		host string
	}{
		{name: "literal IPv4", host: "100.100.50.25"},
		{name: "literal IPv6", host: "fd7a:115c:a1e0::42"},
		{name: "public domain", host: "example.com"},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			backend := startFakeBackend(t)
			resolver := resolverFunc(func(_ context.Context, name string) (netip.Addr, error) {
				t.Fatalf("Resolve(%q) called for unchanged destination", name)
				return netip.Addr{}, errors.New("unreachable")
			})
			server := startShim(t, Server{
				Suffix:   "ts.example.com",
				Resolver: resolver,
				DialBackend: func(ctx context.Context) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "tcp", backend.addr)
				},
			})
			conn := dialNoAuth(t, server)
			writeConnectHost(t, conn, test.host, 8443)
			readSuccessReply(t, conn)
			conn.Close()
			request := <-backend.requests
			if request.host != test.host || request.port != 8443 {
				t.Fatalf("backend request = %s:%d, want %s:8443", request.host, request.port, test.host)
			}
		})
	}
}

func TestTailnetResolutionFailureDoesNotReachBackend(t *testing.T) {
	t.Parallel()

	backendCalled := make(chan struct{}, 1)
	server := startShim(t, Server{
		Suffix: "ts.example.com",
		Resolver: resolverFunc(func(ctx context.Context, _ string) (netip.Addr, error) {
			<-ctx.Done()
			return netip.Addr{}, ctx.Err()
		}),
		ResolveTimeout: 10 * time.Millisecond,
		DialBackend: func(context.Context) (net.Conn, error) {
			backendCalled <- struct{}{}
			return nil, errors.New("must not be called")
		},
	})
	conn := dialNoAuth(t, server)
	writeConnectDomain(t, conn, "mail.ts.example.com", 443)
	if got := readReplyCode(t, conn); got != replyHostUnreachable {
		t.Fatalf("reply code = %d, want host unreachable", got)
	}
	select {
	case <-backendCalled:
		t.Fatal("backend was called after internal DNS failure")
	default:
	}
}

func TestRejectsUnsupportedAuthenticationAndCommand(t *testing.T) {
	t.Parallel()

	server := startShim(t, Server{
		Suffix:   "ts.example.com",
		Resolver: resolverFunc(func(context.Context, string) (netip.Addr, error) { return netip.Addr{}, nil }),
		DialBackend: func(context.Context) (net.Conn, error) {
			return nil, errors.New("must not be called")
		},
	})

	t.Run("authentication", func(t *testing.T) {
		conn, err := net.Dial("tcp", server)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.Write([]byte{socksVersion, 1, 2}); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, 2)
		if _, err := io.ReadFull(conn, got); err != nil {
			t.Fatal(err)
		}
		if got[0] != socksVersion || got[1] != authNoAcceptable {
			t.Fatalf("auth response = %v", got)
		}
	})

	t.Run("command", func(t *testing.T) {
		conn := dialNoAuth(t, server)
		defer conn.Close()
		if _, err := conn.Write([]byte{socksVersion, commandBind, 0, addressIPv4, 127, 0, 0, 1, 0, 80}); err != nil {
			t.Fatal(err)
		}
		if got := readReplyCode(t, conn); got != replyCommandNotSupported {
			t.Fatalf("reply code = %d, want command not supported", got)
		}
	})
}

func dnsResponse(t *testing.T, code string, answers ...dnsAnswer) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"ResponseCode": code, "Answers": answers})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

type backendRequest struct {
	host string
	port uint16
}

type fakeBackend struct {
	addr     string
	requests chan backendRequest
}

func startFakeBackend(t *testing.T) fakeBackend {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	backend := fakeBackend{addr: listener.Addr().String(), requests: make(chan backendRequest, 1)}
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		greeting := make([]byte, 3)
		if _, err := io.ReadFull(conn, greeting); err != nil {
			return
		}
		_, _ = conn.Write([]byte{socksVersion, authNone})
		host, port, err := readConnectRequest(conn)
		if err != nil {
			return
		}
		backend.requests <- backendRequest{host: host, port: port}
		_, _ = conn.Write([]byte{socksVersion, replySucceeded, 0, addressIPv4, 127, 0, 0, 1, 0, 0})
		_, _ = io.Copy(conn, conn)
	}()
	return backend
}

func startShim(t *testing.T, server Server) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() { _ = server.Serve(listener) }()
	return listener.Addr().String()
}

func dialNoAuth(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte{socksVersion, 1, authNone}); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, 2)
	if _, err := io.ReadFull(conn, response); err != nil {
		t.Fatal(err)
	}
	if response[0] != socksVersion || response[1] != authNone {
		t.Fatalf("auth response = %v", response)
	}
	return conn
}

func writeConnectHost(t *testing.T, conn net.Conn, host string, port uint16) {
	t.Helper()
	if addr, err := netip.ParseAddr(host); err == nil {
		if addr.Is4() {
			bytes := addr.As4()
			request := append([]byte{socksVersion, commandConnect, 0, addressIPv4}, bytes[:]...)
			request = append(request, byte(port>>8), byte(port))
			_, err = conn.Write(request)
		} else {
			bytes := addr.As16()
			request := append([]byte{socksVersion, commandConnect, 0, addressIPv6}, bytes[:]...)
			request = append(request, byte(port>>8), byte(port))
			_, err = conn.Write(request)
		}
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	writeConnectDomain(t, conn, host, port)
}

func writeConnectDomain(t *testing.T, conn net.Conn, host string, port uint16) {
	t.Helper()
	request := append([]byte{socksVersion, commandConnect, 0, addressDomain, byte(len(host))}, []byte(host)...)
	request = append(request, byte(port>>8), byte(port))
	if _, err := conn.Write(request); err != nil {
		t.Fatal(err)
	}
}

func readSuccessReply(t *testing.T, conn net.Conn) {
	t.Helper()
	if got := readReplyCode(t, conn); got != replySucceeded {
		t.Fatalf("reply code = %d, want success", got)
	}
}

func readReplyCode(t *testing.T, conn net.Conn) byte {
	t.Helper()
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		t.Fatal(err)
	}
	if header[0] != socksVersion {
		t.Fatalf("reply version = %d", header[0])
	}
	if _, _, err := readAddressBody(conn, header[3]); err != nil {
		t.Fatal(err)
	}
	return header[1]
}

func readConnectRequest(conn net.Conn) (string, uint16, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return "", 0, err
	}
	if header[0] != socksVersion || header[1] != commandConnect {
		return "", 0, errors.New("unexpected request")
	}
	return readAddressBody(conn, header[3])
}

func readAddressBody(reader io.Reader, addressType byte) (string, uint16, error) {
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
		size = int(length[0])
	default:
		return "", 0, errors.New("unsupported address type")
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
