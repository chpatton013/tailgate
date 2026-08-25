package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestTailscaleResolverParsesCapturedAResponse(t *testing.T) {
	t.Parallel()

	fixture, err := os.ReadFile("testdata/tailscale-dns-query-a.json")
	if err != nil {
		t.Fatal(err)
	}
	resolver := tailscaleResolver{query: func(_ context.Context, name, queryType string) ([]byte, error) {
		if name != "mail.ts.example.com" || queryType != "A" {
			t.Fatalf("query = %s %s, want captured A query", name, queryType)
		}
		return fixture, nil
	}}
	got, err := resolver.Resolve(context.Background(), "mail.ts.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if want := netip.MustParseAddr("100.100.50.25"); got != want {
		t.Fatalf("Resolve() = %v, want %v", got, want)
	}
}

func TestCommandQueryHonorsCancellation(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "tailscale")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 10\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := commandQuery(ctx, "mail.ts.example.com", "A")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("commandQuery() error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("commandQuery() cancellation took %v", elapsed)
	}
}

func TestFragmentedGreetingAndRequest(t *testing.T) {
	t.Parallel()

	backend := startFakeBackend(t)
	server := startShim(t, Server{
		Suffix: "ts.example.com",
		Resolver: resolverFunc(func(context.Context, string) (netip.Addr, error) {
			return netip.MustParseAddr("100.100.50.25"), nil
		}),
		HandshakeTimeout: time.Second,
		DialBackend: func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", backend.addr)
		},
	})
	conn, err := net.Dial("tcp", server)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	writeFragmented(t, conn, []byte{socksVersion, 1, authNone})
	auth := make([]byte, 2)
	if _, err := io.ReadFull(conn, auth); err != nil {
		t.Fatal(err)
	}
	request, err := encodeConnectRequest("mail.ts.example.com", 443)
	if err != nil {
		t.Fatal(err)
	}
	writeFragmented(t, conn, request)
	readSuccessReply(t, conn)
	got := <-backend.requests
	if got.host != "100.100.50.25" || got.port != 443 {
		t.Fatalf("backend request = %+v", got)
	}
}

func TestMalformedClientRequestsReceiveFailures(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		request []byte
		want    byte
	}{
		{name: "invalid version", request: []byte{4, commandConnect, 0, addressIPv4}, want: replyGeneralFailure},
		{name: "invalid reserved", request: []byte{socksVersion, commandConnect, 1, addressIPv4}, want: replyGeneralFailure},
		{name: "unsupported address type", request: []byte{socksVersion, commandConnect, 0, 9}, want: replyAddressNotSupported},
		{name: "truncated header", request: []byte{socksVersion, commandConnect}, want: replyGeneralFailure},
		{name: "truncated IPv4 address", request: []byte{socksVersion, commandConnect, 0, addressIPv4, 100, 64}, want: replyGeneralFailure},
		{name: "truncated IPv6 port", request: append([]byte{socksVersion, commandConnect, 0, addressIPv6}, make([]byte, net.IPv6len+1)...), want: replyGeneralFailure},
		{name: "truncated domain", request: []byte{socksVersion, commandConnect, 0, addressDomain, 5, 'm'}, want: replyGeneralFailure},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := startShim(t, Server{HandshakeTimeout: 40 * time.Millisecond})
			conn := dialNoAuth(t, server)
			defer conn.Close()
			if _, err := conn.Write(test.request); err != nil {
				t.Fatal(err)
			}
			if got := readReplyCode(t, conn); got != test.want {
				t.Fatalf("reply = %d, want %d", got, test.want)
			}
		})
	}
}

func TestInvalidGreetingVersionClosesWithoutSOCKS5Reply(t *testing.T) {
	t.Parallel()

	server := startShim(t, Server{HandshakeTimeout: 100 * time.Millisecond})
	conn, err := net.Dial("tcp", server)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	if _, err := conn.Write([]byte{4, 1, authNone}); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	if n, err := conn.Read(buf); n != 0 || err == nil {
		t.Fatalf("Read() = %d, %v, want closed connection without SOCKS5 reply", n, err)
	}
}

func TestTruncatedGreetingGetsNoAcceptableWhenVersionKnown(t *testing.T) {
	t.Parallel()

	server := startShim(t, Server{HandshakeTimeout: 30 * time.Millisecond})
	conn, err := net.Dial("tcp", server)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	if _, err := conn.Write([]byte{socksVersion}); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 2)
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatal(err)
	}
	if got[0] != socksVersion || got[1] != authNoAcceptable {
		t.Fatalf("response = %v", got)
	}
}

func TestBackendFailuresAreBoundedAndReported(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name        string
		dial        func(context.Context) (net.Conn, error)
		want        byte
		maxDuration time.Duration
	}{
		{
			name: "dial failure",
			dial: func(context.Context) (net.Conn, error) {
				return nil, errors.New("backend unavailable")
			},
			want: replyGeneralFailure,
		},
		{
			name: "dial cancellation",
			dial: func(ctx context.Context) (net.Conn, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			},
			want:        replyGeneralFailure,
			maxDuration: time.Second,
		},
		{
			name: "auth rejection",
			dial: pipeBackend(t, func(conn net.Conn) {
				greeting := make([]byte, 3)
				_, _ = io.ReadFull(conn, greeting)
				_, _ = conn.Write([]byte{socksVersion, 2})
			}),
			want: replyGeneralFailure,
		},
		{
			name: "malformed reply",
			dial: pipeBackend(t, func(conn net.Conn) {
				acceptBackendGreeting(conn)
				_, _, _ = readConnectRequest(conn)
				_, _ = conn.Write([]byte{4, replySucceeded, 0, addressIPv4, 0, 0, 0, 0, 0, 0})
			}),
			want: replyGeneralFailure,
		},
		{
			name: "malformed reply reserved byte",
			dial: pipeBackend(t, func(conn net.Conn) {
				acceptBackendGreeting(conn)
				_, _, _ = readConnectRequest(conn)
				_, _ = conn.Write([]byte{socksVersion, replySucceeded, 1, addressIPv4, 0, 0, 0, 0, 0, 0})
			}),
			want: replyGeneralFailure,
		},
		{
			name: "malformed reply address type",
			dial: pipeBackend(t, func(conn net.Conn) {
				acceptBackendGreeting(conn)
				_, _, _ = readConnectRequest(conn)
				_, _ = conn.Write([]byte{socksVersion, replySucceeded, 0, 9})
			}),
			want: replyGeneralFailure,
		},
		{
			name: "backend failure propagation",
			dial: pipeBackend(t, func(conn net.Conn) {
				acceptBackendGreeting(conn)
				_, _, _ = readConnectRequest(conn)
				_, _ = conn.Write([]byte{socksVersion, replyHostUnreachable, 0, addressIPv4, 0, 0, 0, 0, 0, 0})
			}),
			want: replyHostUnreachable,
		},
		{
			name: "stalled auth",
			dial: pipeBackend(t, func(conn net.Conn) {
				greeting := make([]byte, 3)
				_, _ = io.ReadFull(conn, greeting)
				_, _ = io.Copy(io.Discard, conn)
			}),
			want:        replyGeneralFailure,
			maxDuration: time.Second,
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := startShim(t, Server{HandshakeTimeout: 50 * time.Millisecond, DialBackend: test.dial})
			conn := dialNoAuth(t, server)
			defer conn.Close()
			started := time.Now()
			writeConnectDomain(t, conn, "example.com", 443)
			if got := readReplyCode(t, conn); got != test.want {
				t.Fatalf("reply = %d, want %d", got, test.want)
			}
			if test.maxDuration > 0 && time.Since(started) > test.maxDuration {
				t.Fatalf("failure took %v", time.Since(started))
			}
		})
	}
}

func TestServeContextCancelsStalledClients(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- (Server{HandshakeTimeout: 10 * time.Second}).ServeContext(ctx, listener) }()

	const count = 24
	clients := make([]net.Conn, 0, count)
	for range count {
		conn, err := net.Dial("tcp", listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, conn)
		_, _ = conn.Write([]byte{socksVersion})
	}
	cancel()
	select {
	case <-serveDone:
	case <-time.After(time.Second):
		t.Fatal("ServeContext did not stop")
	}
	var wg sync.WaitGroup
	for _, conn := range clients {
		wg.Add(1)
		go func(conn net.Conn) {
			defer wg.Done()
			defer conn.Close()
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			_, _ = io.ReadAll(conn)
		}(conn)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stalled client handlers did not finish after cancellation")
	}
}

func TestRelayPreservesCleanHalfClose(t *testing.T) {
	t.Parallel()

	client, relayClient := tcpPair(t)
	backend, relayBackend := tcpPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		relay(ctx, relayClient, relayBackend, time.Second)
		close(done)
	}()

	if _, err := client.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	if err := client.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	request := make([]byte, len("request"))
	if _, err := io.ReadFull(backend, request); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Write([]byte("response")); err != nil {
		t.Fatal(err)
	}
	if err := backend.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	response, err := io.ReadAll(client)
	if err != nil {
		t.Fatal(err)
	}
	if string(response) != "response" {
		t.Fatalf("response = %q", response)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("relay did not finish after clean half-closes")
	}
}

func TestRelayResetTerminatesIdleOppositeDirection(t *testing.T) {
	t.Parallel()

	client, relayClient := tcpPair(t)
	backend, relayBackend := tcpPair(t)
	done := make(chan struct{})
	go func() {
		relay(context.Background(), relayClient, relayBackend, 5*time.Second)
		close(done)
	}()
	if err := client.SetLinger(0); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	_ = backend.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 1)
	_, _ = backend.Read(buf)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("relay waited for idle opposite direction after reset")
	}
}

func TestRelayAllowsActiveOppositeDirectionBeyondIdleTimeout(t *testing.T) {
	client, relayClient := tcpPair(t)
	backend, relayBackend := tcpPair(t)
	const (
		idleTimeout = 500 * time.Millisecond
		chunkCount  = 12
		chunkDelay  = 50 * time.Millisecond
	)
	done := make(chan struct{})
	go func() {
		relay(context.Background(), relayClient, relayBackend, idleTimeout)
		close(done)
	}()
	if err := client.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	for i := range chunkCount {
		if _, err := backend.Write([]byte{byte('a' + i)}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(chunkDelay)
	}
	if err := backend.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if err := client.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	response, err := io.ReadAll(client)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(response), "abcdefgh"; got != want {
		t.Fatalf("response = %q, want %q", got, want)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("relay did not finish after active response completed")
	}
}

func TestRelayBoundsIdleOppositeDirectionAfterHalfClose(t *testing.T) {
	t.Parallel()

	client, relayClient := tcpPair(t)
	backend, relayBackend := tcpPair(t)
	const idleTimeout = 40 * time.Millisecond
	done := make(chan struct{})
	started := time.Now()
	go func() {
		relay(context.Background(), relayClient, relayBackend, idleTimeout)
		close(done)
	}()
	if err := client.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		if elapsed := time.Since(started); elapsed < idleTimeout/2 {
			t.Fatalf("relay stopped before the idle timeout: %v", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("relay waited forever for idle opposite direction")
	}
	_ = backend.Close()
}

func writeFragmented(t *testing.T, writer io.Writer, data []byte) {
	t.Helper()
	for _, value := range data {
		if _, err := writer.Write([]byte{value}); err != nil {
			t.Fatal(err)
		}
	}
}

func pipeBackend(t *testing.T, serve func(net.Conn)) func(context.Context) (net.Conn, error) {
	t.Helper()
	return func(context.Context) (net.Conn, error) {
		client, backend := net.Pipe()
		go func() {
			defer backend.Close()
			serve(backend)
		}()
		t.Cleanup(func() { _ = client.Close() })
		return client, nil
	}
}

func acceptBackendGreeting(conn net.Conn) {
	greeting := make([]byte, 3)
	_, _ = io.ReadFull(conn, greeting)
	_, _ = conn.Write([]byte{socksVersion, authNone})
}

func tcpPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan *net.TCPConn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		conn, err := listener.AcceptTCP()
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- conn
	}()
	client, err := net.DialTCP("tcp", nil, listener.Addr().(*net.TCPAddr))
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	var server *net.TCPConn
	select {
	case server = <-accepted:
	case err := <-acceptErr:
		t.Fatal(err)
	}
	_ = listener.Close()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	return client, server
}
