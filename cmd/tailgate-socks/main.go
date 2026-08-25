package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
)

func main() {
	listenAddr := flag.String("listen", envOrDefault("TAILGATE_SOCKS_LISTEN", "0.0.0.0:1055"), "SOCKS5 listen address")
	backendAddr := flag.String("backend", envOrDefault("TAILGATE_BACKEND_SOCKS_ADDR", "127.0.0.1:1057"), "tailscaled SOCKS5 backend address")
	tailnetSuffix := flag.String("tailnet-suffix", envOrDefault("TAILGATE_TAILNET_SUFFIX", "ts.example.com"), "Tailnet DNS suffix resolved through tailscaled")
	handshakeTimeout := flag.Duration("handshake-timeout", defaultHandshakeTimeout, "maximum client and backend SOCKS handshake duration")
	resolveTimeout := flag.Duration("resolve-timeout", defaultResolveTimeout, "maximum internal DNS query duration")
	relayIdleTimeout := flag.Duration("relay-idle-timeout", defaultRelayIdleTimeout, "maximum wait after one relay direction closes cleanly")
	resolveName := flag.String("resolve", "", "resolve one configured Tailnet name and exit")
	flag.Parse()

	logger := log.New(os.Stderr, "tailgate-socks: ", log.LstdFlags)
	configuredResolver := tailscaleResolver{query: commandQuery}
	if *resolveName != "" {
		ctx, cancel := context.WithTimeout(context.Background(), *resolveTimeout)
		defer cancel()
		addr, err := resolveConfiguredName(ctx, *resolveName, *tailnetSuffix, configuredResolver)
		if err != nil {
			logger.Fatalf("resolve %q: %v", *resolveName, err)
		}
		fmt.Println(addr)
		return
	}
	listener, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		logger.Fatalf("listen on %s: %v", *listenAddr, err)
	}
	defer listener.Close()

	server := Server{
		Suffix:           *tailnetSuffix,
		Resolver:         configuredResolver,
		HandshakeTimeout: *handshakeTimeout,
		ResolveTimeout:   *resolveTimeout,
		RelayIdleTimeout: *relayIdleTimeout,
		DialBackend: func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", *backendAddr)
		},
		Logf: logger.Printf,
	}
	logger.Printf("listening on %s with backend %s", *listenAddr, *backendAddr)
	if err := server.Serve(listener); err != nil {
		logger.Fatalf("serve: %v", err)
	}
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
