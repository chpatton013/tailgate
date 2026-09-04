# tailgate

[![ci](https://github.com/chpatton013/tailgate/actions/workflows/ci.yml/badge.svg)](https://github.com/chpatton013/tailgate/actions/workflows/ci.yml)

Run the [Tailscale](https://tailscale.com) client inside a container and reach a private
[Headscale](https://github.com/juanfont/headscale) tailnet from a host that can't have a
VPN client installed.

`tailscaled` runs in **userspace mode** inside the container and exposes a **SOCKS5 + HTTP
proxy** bound to host loopback. A small SOCKS shim resolves both registered node names and
Headscale `dns.extra_records` aliases through Tailscale's internal DNS, then delegates the
connection to tailscaled. Your host tools — `ssh`, `curl`, a browser — therefore need no
host DNS or VPN changes.

## Quick start

```sh
cp .env.example .env
./bin/mint-authkey --write-env     # mint a Headscale pre-auth key (or paste one into .env)
./bin/tailgate up                  # start; proxy on 127.0.0.1:1055 (SOCKS5) / :1056 (HTTP)
./bin/tailgate doctor              # verify container + registration + proxy
```

Then reach the tailnet from the host:

```sh
./bin/tailgate curl https://service.ts.example.com         # SOCKS5h → MagicDNS resolves
./bin/tailgate ssh user@node.ts.example.com                # ssh through the proxy
./bin/tailgate forward node 8000                           # loopback-bound service → localhost:8000
./bin/tailgate proxy                                       # config snippets for your own tools
```

`./bin/tailgate proxy` prints ready-to-paste settings for a browser profile, `~/.ssh/config`,
and shell `ALL_PROXY`. Full walkthrough: [`docs/usage.md`](docs/usage.md).

## Configuration

All config lives in `.env` (gitignored — it holds a secret). Defaults point at the
placeholder domains `headscale.example.com` and `ts.example.com`, so a fresh checkout
requires a populated `.env`. Set `TAILGATE_TAILNET_SUFFIX` to the Headscale MagicDNS suffix.
`TAILGATE_DNS_TEST_NAME` optionally gives `tailgate doctor` one existing alias to verify.

Auth is via Headscale pre-auth keys; `bin/mint-authkey` mints one through the admin API.
A persistent state volume keeps the node's identity across restarts — you register once.
Networks where symmetric NAT or policy prevents direct WireGuard can opt into
`TS_DEBUG_ALWAYS_USE_DERP=true`. That Tailscale debug setting adds relay latency and should
remain off when direct connectivity works.

The HTTP proxy on port `1056` is tailscaled's original proxy. It does not use the DNS-aware
shim, so use SOCKS5 on port `1055` for Headscale extra-record aliases. `tailgate proxy`
unsets inherited `HTTPS_PROXY` variables before exporting `ALL_PROXY`, because HTTPS-specific
proxy variables otherwise bypass the shim.

## Layout

```text
compose.yaml        tailscaled, DNS-aware SOCKS shim, and loopback port wiring
cmd/                Go SOCKS shim and process supervisor
.env.example        config template (copy to .env; .env is gitignored)
bin/tailgate        CLI: up/down/status/logs/ip/ssh/curl/forward/proxy/doctor
bin/mint-authkey    mint a Headscale pre-auth key via the admin API + AWS secret
docs/               usage guide, plus design notes & history
```

## Design notes & history

[`docs/research-and-design.md`](docs/research-and-design.md) records the original research and
a tiered exploration that went beyond this proxy — including a transparent L3 routing +
host-wide MagicDNS gateway VM. That approach was functionally validated but its macOS
substrate (QEMU+HVF) proved unstable and is inherently platform-specific, so its code was
removed (§10 of that doc keeps the implementation notes). The userspace proxy here is the
portable, supported tool.

## License

`tailgate` is licensed under the terms of the MIT License, as described in
[LICENSE.md](LICENSE.md).

## Contributing

Contributions are welcome in the form of bug reports, feature requests, or pull
requests.

Contribution to `tailgate` is organized under the terms of the [Contributor
Covenant](CONTRIBUTOR_COVENANT.md).
