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

**History & experiments live in `docs/` and `tier2/`.** `docs/research-and-design.md` records
the original research and a tiered exploration that went past the proxy. The largest
experiment, `tier2/`, prototypes a transparent L3-routing + host-wide-MagicDNS gateway VM
(Vagrant+QEMU). It was functionally validated but its macOS substrate proved unstable and is
inherently platform-specific — **kept as an unsupported prototype, not part of the product**
(see the bottom section).

## Conventions (read before editing)

- **The repo is sanitized.** Real domains/hosts/users are placeholders: `example.com`,
  `ts.example.com`, `myhost`, `exit-node`, `user`, port `8000`. **Never commit real
  tenant identifiers** — domains, org names, corporate CA names, internal IPs, usernames.
- **Real config lives only in the gitignored `.env`** (and `tier2/corp-ca.pem`). Defaults
  in committed files point at the placeholder domain, so a fresh checkout *requires* a
  populated `.env`.
- **Auth:** Headscale pre-auth keys (`bin/mint-authkey`, mirrors the upstream IaC
  project's preauthkey flow). A persistent tailscale state volume keeps node identity
  across restarts — you normally register once.
- **Portability is a goal.** Prefer cross-platform approaches; call out platform-specific
  dependencies explicitly. The proxy is portable; the `tier2/` gateway is macOS-bound by
  nature (it needs Apple `vmnet` + `/etc/resolver`) — a big reason it's experimental.

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
   corporate root CA trusted; that's handled in `tier2/`, below.)

## Experimental: `tier2/` transparent gateway (unsupported)

`tier2/` prototypes transparent routing (host route for `100.64.0.0/10` via a gateway VM)
plus host-wide MagicDNS (`/etc/resolver`). It was **functionally validated** — routing and
split-DNS both work end to end — but it is **not durable** (see the freeze below) and is
**macOS-specific by nature** (Apple `vmnet`, `/etc/resolver`, macOS `route`). It is kept for
reference only; the portable proxy is the supported path. If you work on it:

- **`/etc/resolver/<domain>` split DNS works on macOS unless an MDM profile overrides DNS.**
  A Configuration Profile DNS payload outranks `/etc/resolver`. Verify: create
  `/etc/resolver/<test-domain>` with a `nameserver`, then check `scutil --dns` lists it
  (Reachable). Look for overrides in `sudo profiles list` / the live NetworkExtension config
  (`com.apple.dnsProxy` / `com.apple.dnsSettings`).
- **All-networks proxy vs. L3 routing.** An SSE/app-proxy set to "include all networks"
  *might* intercept host traffic to `100.64.0.0/10`. The loopback proxy (host apps →
  `127.0.0.1:1055`) sidesteps it; transparent L3 is the case that could be captured —
  **test it** (the `tier2/` probe does: host route + ICMP/TCP to a real peer). In our one
  environment it passed (the proxy did *not* eat CGNAT routing).
- **Trust the corporate root CA in the guest** so its generic HTTPS validates through an
  SSL-decrypt proxy: `security find-certificate -a -p -c "<Org>"
  /Library/Keychains/System.keychain > tier2/corp-ca.pem` (gitignored); provisioning runs
  `update-ca-certificates`.
- **vagrant-qemu defaults to user-mode (SLIRP) networking → no host-routable guest IP.** Use
  **`socket_vmnet`** (`brew install` + `sudo brew services start`) via a `qemu_bin` wrapper,
  as a **second NIC**; keep vagrant-qemu's user-mode NIC as `net0` (Vagrant SSHes over its
  `hostfwd :50022` — remove it and `vagrant up` hangs on `127.0.0.1:50022`). The vmnet NIC
  (`net1`, `fd=3`) gives `192.168.105.x`. DHCP it but strip its default route so egress stays
  on the user-mode NIC.
- **`vagrant plugin list` fails on a stale pin?** `expunge --reinstall` re-hits it; use
  `vagrant plugin expunge --force` then reinstall only what you need.
- **Clamp TCP MSS on the gateway** (`iptables -t mangle ... --clamp-mss-to-pmtu`): tailscale0
  is MTU 1280; without clamping a session TCP-connects but large packets (SSH key exchange)
  blackhole and time out.
- **IPv4-only** (socket_vmnet shared networking is v4); never commit `tier2/.vagrant/`
  (multi-hundred-MB disk images).
- **`TS_MAGICDNS_DOMAIN` must exactly match the Headscale base domain** (often a *private*
  domain — easy to fumble vs. the public one); wrong value ⇒ names fall through to public
  DNS. The gateway's dnsmasq is domain-agnostic (forwards all to `100.100.100.100`); only the
  host's `/etc/resolver/<domain>` filename depends on it. `tailgate-tier2 down` removes
  `/etc/resolver/<current domain>`, so run `down` *before* changing the domain.
- **⚠️ QEMU+HVF guest hard-halt — UNRESOLVED.** The guest silently stops executing (journald
  cuts off mid-line, no panic/OOM, unreachable on every NIC, qemu still alive, Vagrant says
  "running"), so `vagrant ssh` / `tailgate-tier2` hang. It is **not** host sleep (reproduced
  with no sleep). The documented-stable combo `cpu = "cortex-a72"` + `highmem=off` (now in
  the Vagrantfile) did **NOT** fix it — the guest froze again ~15 min in. A durable fix would
  require a Mac-native VM (Apple Virtualization.framework via Lima/vfkit/Tart), deliberately
  **not** adopted to keep the project portable. **Recovery:** `vagrant destroy` hangs on a
  halted guest — `pkill -9 -f qemu-system-aarch64` first, then `vagrant destroy -f` +
  `vagrant global-status --prune`; bound every `vagrant` call with `timeout`. Guest journald
  is made persistent so pre-freeze logs survive.
