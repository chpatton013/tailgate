# tailgate

Run the [Tailscale](https://tailscale.com) client inside a container/VM and reach a
private [Headscale](https://github.com/juanfont/headscale) tailnet from a host that
can't have a VPN client installed.

This repo targets the `headscale-infra` tailnet (control plane
`https://headscale.example.com`, MagicDNS domain `ts.example.com`).

## Status

| Tier | What | State |
| --- | --- | --- |
| 0 | Tailscale in a container (TUN), operate from inside | ✅ PoC |
| 1 | Userspace + SOCKS5/HTTP proxy; host `ssh`/`curl`/browser reach the tailnet | ✅ PoC |
| 2 | Transparent L3 + host-wide MagicDNS via a gateway VM (Vagrant+QEMU) | 🔬 routing validated; building out |
| 3 | Polished, auto-managed, near-invisible | 📋 designed |

## Quick start

```sh
cp .env.example .env
./bin/mint-authkey --write-env     # or paste a key from https://headplane.example.com
./bin/tailgate up                  # Tier 1: proxy on 127.0.0.1:1055 (SOCKS5) / :1056 (HTTP)
./bin/tailgate doctor
./bin/tailgate curl http://myhost.ts.example.com:<port>   # a web UI on a tailnet node
```

## Docs

- [`docs/poc-usage.md`](docs/poc-usage.md) — hands-on Tier 0 / Tier 1 walkthrough.
- [`docs/research-and-design.md`](docs/research-and-design.md) — research, the full
  maturity ladder, platform decisions, and open risks.

## Layout

```
compose.yaml        Tier 1: userspace tailscaled + SOCKS5/HTTP proxy (default)
compose.tun.yaml    Tier 0 override: TUN mode (docker compose -f compose.yaml -f compose.tun.yaml ...)
.env.example        config template (copy to .env; .env is gitignored)
bin/tailgate        CLI: up/down/status/logs/ip/ssh/curl/proxy/doctor
bin/mint-authkey    mint a Headscale pre-auth key via the admin API + AWS secret
docs/               usage + design
```
