# Agent guide — working in the tailgate repo

Guidance for Claude (and humans) contributing to or operating **tailgate**: running the
Tailscale client in a container to reach a private Headscale tailnet from a host that
can't install a VPN client (typically a locked-down, MDM-managed machine).

## Orientation

**tailgate is a userspace-proxy tool — that's the product.** `tailscaled` runs in a
container in userspace mode and exposes a SOCKS5 + HTTP proxy on host loopback; host tools
(`ssh`/`curl`/`forward`/browser) reach the tailnet through it, with MagicDNS resolved at the
proxy. No VPN/TUN interface is created on the host. Core files: `compose.yaml`,
`bin/tailgate` (up/down/status/logs/ip/ssh/curl/forward/proxy/doctor), `bin/mint-authkey`
(Headscale pre-auth key via the admin API), `docs/usage.md`. Treat the proxy as the finished
deliverable — not a "tier 1 stepping stone."

**History lives in `docs/`.** `docs/research-and-design.md` records the original research and
a tiered exploration that went past the proxy — including a transparent L3-routing +
host-wide-MagicDNS gateway VM (Vagrant+QEMU) that was functionally validated but whose macOS
substrate proved unstable (and is inherently platform-specific). **That code was removed**;
§10 of the design doc keeps the implementation notes. Don't reintroduce it without
re-platforming onto a Mac-native VM — a portability tradeoff that was deliberately declined.

## Conventions (read before editing)

- **The repo is sanitized.** Real domains/hosts/users are placeholders: `example.com`,
  `ts.example.com`, `myhost`, `exit-node`, `user`, port `8000`. **Never commit real
  tenant identifiers** — domains, org names, corporate CA names, internal IPs, usernames.
- **Real config lives only in the gitignored `.env`.** Defaults in committed files point
  at the placeholder domain, so a fresh checkout *requires* a populated `.env`.
- **Auth:** Headscale pre-auth keys (`bin/mint-authkey`, mirrors the upstream IaC
  project's preauthkey flow). A persistent tailscale state volume keeps node identity
  across restarts — you normally register once.
- **Portability is a goal.** Prefer cross-platform approaches; call out platform-specific
  dependencies explicitly. The proxy is portable; transparent L3 routing on macOS is
  intrinsically platform-bound (needs Apple `vmnet` + `/etc/resolver`) — which is why that
  exploration was dropped.

## Environment gotchas (hard-won — these will bite again)

1. **Docker "address pools fully subnetted".** If the Docker daemon's
   `default-address-pools` is scoped to a single `/24` (sometimes set deliberately to
   avoid colliding with corporate network ranges), Compose can't allocate a per-project
   network. Fix: `network_mode: bridge` (already in `compose.yaml`) — tailgate only needs
   outbound + loopback-published ports, so it joins the default bridge instead.

2. **macOS system `curl` is LibreSSL.** It treats a server's `unrecognized_name` TLS
   warning as fatal (`error:...:tlsv1 unrecognized name`). Use `http://`, add `-k`, or
   `brew install curl` (OpenSSL-backed).

3. **MagicDNS only answers at `100.100.100.100`, which exists only in TUN mode.** In
   userspace/proxy mode there's no `100.100.100.100`, so resolve names *at the proxy*:
   `curl` via `socks5h://`, and `ssh` via `nc -X 5 -x <proxy> %h %p`. macOS `nc` *does* do
   remote DNS over SOCKS5, so MagicDNS names work through the wrappers without any host DNS
   changes. (`tailgate ssh`/`forward` inject this ProxyCommand themselves — no
   `~/.ssh/config` `ProxyCommand` needed; a `Host` alias is optional convenience.)

4. **Loopback-bound services aren't reachable over the tailnet.** A service bound to
   `127.0.0.1` on a node (common for app gateways/UIs) is only reachable *on* that node —
   the proxy can't reach it, and neither could the experimental L3 gateway. Options:
   `tailgate forward <node> <port>` (SSH local-forward through the proxy), publish it with
   `tailscale serve`, or rebind it to the node's tailnet IP.

5. **Corporate TLS interception (SSL-decrypt proxy).** Many managed machines blanket-decrypt
   HTTPS and re-sign with a corporate root CA — for *all* egress, including Docker and QEMU.
   Detect:
   ```sh
   openssl s_client -connect <host>:443 -servername <host> </dev/null 2>/dev/null \
     | openssl x509 -noout -issuer        # a corporate issuer == interception
   ```
   **Tailscale itself is unaffected** — its control plane is secured by the Noise protocol
   (not web PKI) and the data plane is WireGuard end-to-end, so the tunnel works *and stays
   confidential* even through the MITM. That's why the proxy works with a stock image.
   (Generic HTTPS *inside a guest VM* — install scripts, `apt` — does fail and needs the
   corporate root CA trusted; see the archived gateway notes in
   `docs/research-and-design.md` §10.)
