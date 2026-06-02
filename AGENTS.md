# Agent guide — working in the tailgate repo

Guidance for Claude (and humans) contributing to or operating **tailgate**: running the
Tailscale client in a container/VM to reach a private Headscale tailnet from a host that
can't install a VPN client (typically a locked-down, MDM-managed machine).

## Orientation

Maturity ladder (see `docs/research-and-design.md` for the full design):

| Tier | What | Status |
| --- | --- | --- |
| 0 | Tailscale in a container (TUN), operate from inside | ✅ PoC |
| 1 | Userspace + SOCKS5/HTTP proxy; host `ssh`/`curl`/`forward`/browser reach the tailnet | ✅ usable |
| 2 | Transparent L3 routing + host-wide MagicDNS via a Vagrant+QEMU gateway VM | 🔬 routing validated; building out |
| 3 | Polished, auto-managed, near-invisible | 📋 designed |

Layout: `compose.yaml` / `compose.tun.yaml` (Tier 0/1), `bin/tailgate` (Tier 0/1 CLI),
`bin/mint-authkey` (Headscale pre-auth key via admin API), `tier2/` + `bin/tailgate-tier2`
(Tier 2 gateway VM + host route/resolver wiring; `bin/tier2-probe` is the standalone
reachability probe), `docs/` (design + usage).

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
   userspace/proxy mode (Tier 1) there's no `100.100.100.100`, so resolve names *at the
   proxy*: `curl` via `socks5h://`, and `ssh` via `nc -X 5 -x <proxy> %h %p`. macOS `nc`
   *does* do remote DNS over SOCKS5, so MagicDNS names work through the wrappers without
   any host DNS changes. (`tailgate ssh`/`forward` inject this ProxyCommand themselves —
   no `~/.ssh/config` `ProxyCommand` needed; a `Host` alias is optional convenience.)

4. **Loopback-bound services are unreachable over the tailnet — even with Tier 2.** A
   service bound to `127.0.0.1` on a node (common for app gateways/UIs) is only reachable
   *on* that node. Options: `tailgate forward <node> <port>` (SSH local-forward through
   the proxy), publish it with `tailscale serve`, or rebind it to the node's tailnet IP.

5. **macOS `/etc/resolver/<domain>` split DNS works — unless an MDM profile overrides
   DNS.** A Configuration Profile DNS payload outranks `/etc/resolver`. Verify on the
   target machine: create `/etc/resolver/<test-domain>` with a `nameserver`, then check
   `scutil --dns` lists it (Reachable). Check for overrides with `sudo profiles list` /
   live NetworkExtension config (look for `com.apple.dnsProxy` / `com.apple.dnsSettings`).

## Locked-down / MDM-managed hosts (tailgate's core use case)

6. **Corporate TLS interception (SSL-decrypt proxy).** Many managed machines blanket-
   decrypt HTTPS and re-sign with a corporate root CA — for *all* egress, including from
   Docker and QEMU. Detect:
   ```sh
   openssl s_client -connect <host>:443 -servername <host> </dev/null 2>/dev/null \
     | openssl x509 -noout -issuer        # a corporate issuer == interception
   ```
   **Tailscale itself is unaffected** — its control plane is secured by the Noise protocol
   (not web PKI) and the data plane is WireGuard end-to-end, so the tunnel works *and stays
   confidential* even through the MITM. That's why Tier 1 works with a stock image.
   **But generic HTTPS inside a guest VM fails** (install scripts, `apt`) because it
   validates against the web PKI. Fix: trust the corporate root CA in the guest. Export it
   from the host and let provisioning install it:
   ```sh
   security find-certificate -a -p -c "<Org>" /Library/Keychains/System.keychain > tier2/corp-ca.pem
   ```
   `tier2/` ships this gitignored bundle into the VM and runs `update-ca-certificates`
   before fetching anything.

7. **All-networks proxy vs. L3 routing.** An SSE/app-proxy configured to "include all
   networks" *might* intercept host traffic to the tailnet CGNAT range (`100.64.0.0/10`).
   Loopback-based approaches (Tier 1: host apps → `127.0.0.1:1055`) sidestep it entirely.
   Don't assume transparent L3 routing (Tier 2) is captured — **test it**: stand up the
   gateway, add the host route, and probe a real peer IP over ICMP + TCP (`tier2/` does
   exactly this). In at least one such environment it passed cleanly (the proxy did *not*
   eat CGNAT routing), so the transparent endgame was viable despite blanket TLS decrypt.

## QEMU on Apple Silicon (Tier 2 substrate)

8. **vagrant-qemu defaults to user-mode (SLIRP) networking → no host-routable guest IP.**
   For the route + `/etc/resolver` design you need a stable host-reachable address. Use
   **`socket_vmnet`** (`brew install socket_vmnet` + `sudo brew services start
   socket_vmnet`) wired via a `qemu_bin` wrapper. Critically, attach it as a **second NIC**
   and keep vagrant-qemu's user-mode NIC as `net0` — Vagrant SSHes in over that user-mode
   NIC's `hostfwd :50022`; if you remove it (`net_device = nil`), `vagrant up` hangs at
   "Connection refused" on `127.0.0.1:50022`. The socket_vmnet NIC (`net1`, `fd=3`) gives
   the guest a host-reachable `192.168.105.x`. Bring it up via DHCP in provisioning but
   strip its default route so the user-mode NIC keeps internet/tailscale egress.

9. **`vagrant plugin list` fails on a stale pin?** `vagrant plugin expunge --reinstall`
   re-hits the broken pin. Use `vagrant plugin expunge --force` (clean wipe) then
   reinstall only what you need (`vagrant plugin install vagrant-qemu`).

10. **IPv4-only is pragmatic.** `socket_vmnet` shared networking is v4; defer the tailnet
    IPv6 range.

11. **Never commit `tier2/.vagrant/`** — it holds multi-hundred-MB VM disk images
    (gitignored). `git add -A` will try to and GitHub will reject the push.

12. **QEMU+HVF guest hard-halt (the freeze we hit repeatedly).** Symptom: the guest
    silently stops executing — journald cuts off mid-line with no panic/OOM, the serial
    console goes quiet, the VM is unreachable on *every* NIC, yet the qemu process is
    still alive and Vagrant reports "running". So `vagrant ssh` (and thus
    `tailgate-tier2 up`/`status`) hangs. **Root cause: `-cpu host` + `highmem=on` under
    Apple HVF is unstable** (a known QEMU bug class). **Fix: use `cpu = "cortex-a72"` and
    `machine = "virt,accel=hvf,highmem=off"`** (already set in `tier2/Vagrantfile`). It is
    NOT host sleep (we wrongly assumed that at first; it reproduced with no sleep).
    - **Recovering a hung guest:** `vagrant destroy` *hangs* on a halted guest (it waits
      to shut it down gracefully). Force-kill first: `pkill -9 -f qemu-system-aarch64`
      (or kill the specific PID), then `vagrant destroy -f` + `vagrant global-status
      --prune`. Bound every `vagrant` call with `timeout` so a hang is visible, not silent.
    - Make journald persistent in the guest (`/var/log/journal`) so a freeze's pre-death
      logs survive the reboot — the default volatile journal loses them.

## Tier 2 wiring reminders

- `TS_MAGICDNS_DOMAIN` in `.env` must be the **exact** Headscale base domain (often a
  *private* domain — easy to fumble vs. the public one). Wrong value ⇒ names fall through
  to public DNS and you get a wildcard IP with `Connection refused` on the service port.
  The gateway's dnsmasq is domain-agnostic (forwards all to `100.100.100.100`); only the
  host's `/etc/resolver/<domain>` filename depends on it.
- `tailgate-tier2 down` removes `/etc/resolver/<current $TS_MAGICDNS_DOMAIN>` — if you
  change the domain in `.env`, run `down` **before** the change (or remove the stale
  `/etc/resolver/*` file by hand), else it's orphaned.
