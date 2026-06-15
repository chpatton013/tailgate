# tailgate

Run the [Tailscale](https://tailscale.com) client inside a container and reach a private
[Headscale](https://github.com/juanfont/headscale) tailnet from a host that can't have a
VPN client installed.

`tailscaled` runs in **userspace mode** inside the container and exposes a **SOCKS5 + HTTP
proxy** bound to host loopback. Your host tools — `ssh`, `curl`, a browser — reach tailnet
nodes through that proxy, with MagicDNS names resolved at the proxy, so **nothing on the
host has to change** and no VPN/TUN interface is ever created on the host.

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
placeholder domain `headscale.example.com`, so a fresh checkout requires a populated `.env`.
Auth is via Headscale pre-auth keys; `bin/mint-authkey` mints one through the admin API.
A persistent state volume keeps the node's identity across restarts — you register once.

## Layout

```
compose.yaml        the tailgate service: userspace tailscaled + SOCKS5/HTTP proxy
.env.example        config template (copy to .env; .env is gitignored)
bin/tailgate        CLI: up/down/status/logs/ip/ssh/curl/forward/proxy/doctor
bin/mint-authkey    mint a Headscale pre-auth key via the admin API + AWS secret
docs/               usage guide, plus design notes & history
```

## Design notes & history

[`docs/research-and-design.md`](docs/research-and-design.md) records the original research and
a tiered exploration that went beyond this proxy — including an **experimental** transparent
L3 routing + host-wide MagicDNS gateway VM (in [`tier2/`](tier2/)). That transparent approach
was functionally validated but its macOS substrate (QEMU+HVF) proved unstable, and it is
inherently platform-specific; the userspace proxy here is the portable, supported tool.
