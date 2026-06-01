# Tailgate — Running the Tailscale client in a VM

**A research report and tiered design for accessing a private Headscale tailnet from a
device that cannot have a VPN client installed.**

Status: design / pre-implementation
Author: Claude (research pass), for user
Date: 2026-05-31

---

## 1. Problem statement

`user` operates a private [Headscale](https://github.com/juanfont/headscale) control
plane (provisioned by the `headscale-infra` CDK project) that gates access to personal
services not exposed to the public internet. Reaching those services normally requires
running the Tailscale client on the client machine.

The client machine is a **managed macOS laptop** whose device policy **prohibits
installing VPN applications**. Installing `tailscaled` natively (which creates a
`utun` interface and is, by every reasonable definition, a VPN client) is therefore
off the table.

The goal: run the Tailscale client **inside a virtual machine / container**, and from
the host be able to:

- **reach tailnet nodes over the terminal** (SSH, `curl`, arbitrary TCP), and
- **reach tailnet web interfaces** in a normal browser, with **MagicDNS name
  resolution** (`*.ts.example.com`, `100.x` addresses).

We want a **maturity ladder**: a quick proof-of-concept that tolerates manual steps,
maturing into something polished that "just works" and is nearly invisible.

### 1.1 A note on the device policy

This design deliberately runs the VPN data plane inside a guest OS rather than on the
host, so it does not install a VPN client on the managed host. That is a meaningful
technical distinction, but it is also clearly adjacent to the intent of a "no VPN"
control. **Before building this, confirm it is consistent with your employer's
acceptable-use / device policy.** Running a VM is normal developer behavior; tunneling
corporate-network traffic or exfiltrating data is not, and nothing here is intended for
that. The services in question are your own personal ones.

> **Confirmed (2026-05-31):** this is not an exfiltration project — purely personal
> access to user's own private services. The rest of this report proceeds on that
> basis.

---

## 2. The target tailnet (facts pulled from `headscale-infra`)

These are the concrete values the client must speak to. Source:
`infra/stacks/headscale_stack.py`, `infra/models/headscale_config.py`, `config.toml`.

| Property | Value |
| --- | --- |
| Headscale control URL (`--login-server`) | `https://headscale.example.com` |
| Headplane admin UI | `https://headplane.example.com` |
| MagicDNS base domain | `ts.example.com` (nodes are `<name>.ts.example.com`) |
| Tailnet IPv4 range (CGNAT) | `100.64.0.0/10` |
| Tailnet IPv6 range | `fd7a:115c:a1e0::/48` |
| Global DNS forwarders | `1.1.1.1`, `9.9.9.9` |
| Existing exit node | `exit-node` (advertises `--advertise-exit-node`) |
| Headscale version | `0.26.1` |
| Auth model | **Pre-auth keys** (created via the admin API key in Secrets Manager, or in Headplane) |

**Auth flow precedent.** The exit node in `headscale-infra` registers exactly the way our
client should. It runs the upstream `tailscale/tailscale` image with:

```
TS_AUTHKey   = <pre-auth key>                     # from headscale/<...>/preauthkey secret
TS_EXTRA_ARGS = --login-server=https://headscale.example.com
TS_HOSTNAME  = <node name>
TS_STATE_DIR = /var/lib/tailscale
```

Pre-auth keys are minted through the Headscale REST API using the admin API key stored
at `headscale/admin-api-key` in AWS Secrets Manager (see the
`headscale_exit_node_preauthkey` Lambda for the canonical
"create user + create preauthkey" call). That same mechanism is what `tailgate` should
reuse to mint keys for the client VM — manually at first, automatically later.

This is the single most important reuse opportunity: **the server side already knows
how to register a containerized Tailscale node with a pre-auth key against this
Headscale. The client VM is just another such node.**

---

## 3. Background: the three things that make this hard

### 3.1 Userspace vs. kernel (TUN) networking

`tailscaled` can run in two modes:

- **Kernel / TUN mode** (`TS_USERSPACE=false`, needs `/dev/net/tun` + `NET_ADMIN`):
  creates a real `tailscale0` interface, sets up routes, and — crucially — brings up
  the **MagicDNS resolver at `100.100.100.100`** and can act as a subnet router /
  gateway for other machines. This is what we need for the transparent endgame.
- **Userspace mode** (`TS_USERSPACE=true`): no TUN device required. `tailscaled`
  instead exposes a **SOCKS5 and/or HTTP proxy** that other processes dial through.
  It does **not** create `100.100.100.100` and cannot route L3 traffic for the host.
  (SOCKS5 since Tailscale v1.8, HTTP proxy since v1.16.)

The two modes map almost one-to-one onto our middle and final tiers.

### 3.2 MagicDNS only resolves at `100.100.100.100`

Names like `app.ts.example.com` are only answered by Tailscale's internal
resolver at `100.100.100.100`, which only exists when `tailscaled` runs in **TUN
mode**. So:

- In **proxy mode**, MagicDNS names work only if the client does *remote* DNS through
  the proxy (`socks5h://` — the `h` means "resolve hostname at the proxy"). Per-app.
- For **host-wide** MagicDNS, we run TUN mode in the VM and forward the `ts.example.com`
  domain from the macOS host to the VM via `/etc/resolver/`.

**macOS gotcha:** A **Configuration Profile / MDM-pushed DNS setting outranks
`/etc/resolver/` files.** On a managed laptop this can silently break split DNS. We
must test this early — it may force a fallback (e.g. point the browser/app at the VM
resolver explicitly, or use a `.local`-style proxy auto-config). This is the single
biggest unknown for the "host-wide MagicDNS" goal and should be validated in Tier 1.

### 3.3 The host has no Tailscale interface — so how does return traffic get back?

When the host sends a packet to `100.64.x.y` via the VM, the VM forwards it out
`tailscale0`. The destination node must be able to send the reply *back*. Tailnet peers
have no route to the host's private VM-network subnet (e.g. `192.168.56.0/24`), so the
VM must **masquerade (SNAT)** host-originated traffic to its own tailnet IP. The reply
then comes back to the VM, which un-NATs it to the host. This is the same trick the
`exit-node` node already uses in reverse (`iptables -t nat -A POSTROUTING -s
100.64.0.0/10 -o $EGRESS_IF -j MASQUERADE`); we apply it on the
host-subnet → `tailscale0` direction. Tailscale enables subnet-route SNAT by default,
so this is mostly automatic, with a small amount of `iptables` for the host subnet.

---

## 4. Platform options

You told me what's available: Docker Desktop (which itself runs a LinuxKit VM),
VirtualBox (installed + working), prior Vagrant experience (and you like its
self-documenting nature), and possibly QEMU.

> **Host is Apple Silicon (confirmed).** VirtualBox is experimental/weak on Apple
> Silicon, so **we skip VBox** and standardize on **Vagrant + QEMU**
> (via the [`vagrant-qemu`](https://github.com/ppggff/vagrant-qemu) plugin, which uses
> Apple's Hypervisor.framework). The table below is kept for completeness, but VBox is
> effectively ruled out for this machine.

| Platform | Strengths | Weaknesses | Best for |
| --- | --- | --- | --- |
| **Docker Desktop** | Trivial to run `tailscale/tailscale`; one-line `-p 1055:1055` to expose a proxy; the exact image `headscale-infra` already uses | Containers run in a hidden LinuxKit VM; container IPs are **not routable from the host**, so transparent L3 / host-wide routing is awkward. Best as a proxy endpoint only. | **Tier 1 (proxy)** |
| **Vagrant + QEMU** (`vagrant-qemu` plugin) | **Chosen for this machine.** Self-documenting `Vagrantfile`; uses Apple's Hypervisor.framework — light, idles near-zero CPU; native on Apple Silicon; `vagrant ssh`, provisioners | Smaller box selection; synced folders + some network modes less mature than VBox; you have no QEMU experience yet; **need to confirm a host-reachable VM IP** (see note) | **Tier 2/3 (transparent)** — the workhorse |
| **Vagrant + VirtualBox** | Stable host-only adapter IP; mature networking; you've used it before | **Experimental/weak on Apple Silicon** — ruled out for this machine | (n/a here) |
| **Raw QEMU / UTM** | Most efficient | Most manual; least self-documenting | Optimization, not first build |

**Recommendation:**
- Prove the proxy tier with **Docker** (fastest to a working demo).
- Build the transparent tiers on **Vagrant + QEMU** (the Apple Silicon choice), with a
  self-documenting `Vagrantfile` + a provider-agnostic shell provisioner so the guest
  setup is identical regardless of hypervisor.

> **QEMU networking wrinkle to resolve in Tier 2.** `vagrant-qemu` defaults to user-mode
> (SLIRP) networking, which does **not** hand the guest a host-reachable IP — that
> breaks the "static route from host → VM" and "DNS forwarder on the VM IP" assumptions
> Tier 2 relies on. Options to validate early: (a) a `vmnet`-based bridged/host network
> (`socket_vmnet` / Apple's vmnet, which Hypervisor.framework supports) to get a stable
> guest IP on a shared subnet; or (b) keep SLIRP and expose the proxy/DNS via QEMU
> **hostfwd** port-forwards to `127.0.0.1` (closer to the Tier-1 model). This is the
> main thing QEMU changes versus the VirtualBox host-only-adapter plan, and it's worth a
> spike before building Tier 2.

---

## 5. The maturity ladder

Each tier is independently useful and builds on the previous one. Capabilities are
cumulative.

### Tier 0 — Proof of concept: "the VM is on the tailnet" (operate from inside)

**Goal:** prove the VM can register against `headscale.example.com` and reach private
services. Accept heavy manual operation. You work *inside* the guest.

**Build:**
- A `tailscale/tailscale` container (Docker) or a one-box `Vagrantfile`, running in
  **TUN mode** (`TS_USERSPACE=false`, `--cap-add=NET_ADMIN`, `--device=/dev/net/tun`).
- Manually mint a pre-auth key (Headplane UI, or the Headscale REST API with the
  `headscale/admin-api-key` secret) and paste it in as `TS_AUTHKEY`.
- `TS_EXTRA_ARGS=--login-server=https://headscale.example.com`,
  `TS_HOSTNAME=tailgate-laptop`, persistent `TS_STATE_DIR`.

**Use:**
- `docker exec -it ts tailscale status` / `vagrant ssh` then `ssh user@100.64.x.y`,
  `curl https://app.ts.example.com` — **all from inside the guest**, where
  MagicDNS already works.

**Manual cost:** key minting by hand; you operate inside the VM; no host integration;
state may not survive cleanly.

**Exit criteria:** node appears in Headplane; can SSH a tailnet node and load a private
web UI from inside the guest. **This validates the auth flow end-to-end** and is the
foundation everything else stands on.

---

### Tier 1 — SOCKS5 / HTTP proxy: host tools reach the tailnet (per-app config)

**Goal:** use host-side tools (`ssh`, `curl`, a browser) against the tailnet without
entering the VM. No host network changes; least invasive.

**Build:**
- Run `tailscaled` in **userspace mode** with a proxy exposed to the host:
  - Docker: `TS_USERSPACE=true`, `TS_SOCKS5_SERVER=0.0.0.0:1055`
    (optionally `TS_OUTBOUND_HTTP_PROXY_LISTEN=0.0.0.0:1056`), published `-p
    1055:1055 -p 1056:1056`.
- The proxy speaks SOCKS5 and HTTP on the same port if you point both flags at it.

**Use (per-tool):**
- **SSH:** `ssh -o ProxyCommand='nc -X 5 -x 127.0.0.1:1055 %h %p' user@nodename` — or a
  `~/.ssh/config` block for `Host *.ts.example.com`. Because the proxy resolves names,
  MagicDNS names work.
- **curl:** `curl --proxy socks5h://127.0.0.1:1055 https://app.ts.example.com`
  (the `h` is essential — DNS resolves at the proxy = MagicDNS works).
- **Browser:** a dedicated Firefox profile (or a Chrome `--proxy-server` launch) set to
  SOCKS5 `127.0.0.1:1055` with **"Proxy DNS when using SOCKS v5"** enabled. Then
  `https://*.ts.example.com` loads in a normal browser tab.

**Manual cost:** every tool needs proxy config; a separate browser profile; no
system-wide name resolution.

**Exit criteria:** from the host, `curl` and a browser profile reach private web UIs by
MagicDNS name, and `ssh` reaches tailnet nodes — all without `vagrant ssh`.

This tier is the **sweet spot for "good enough" daily use** and a clean stepping stone.

---

### Tier 2 — Transparent L3 + host-wide MagicDNS: "just works" for most apps

**Goal:** any host application reaches `100.64.0.0/10` and resolves `*.ts.example.com`
with **no per-app configuration**. The VM is a gateway/relay.

**Build (Vagrant + QEMU):**
1. VM has a **host-reachable IP on a shared/vmnet network** with a fixed address, e.g.
   `192.168.64.10` (resolve the QEMU networking wrinkle from §4 first — vmnet bridged
   networking rather than default SLIRP).
2. Inside the VM, `tailscaled` runs in **TUN mode** (MagicDNS at `100.100.100.100`
   active) and the guest is configured to forward + NAT:
   - `net.ipv4.ip_forward=1`
   - `iptables -t nat -A POSTROUTING -o tailscale0 -j MASQUERADE`
   - accept-forward rules for `tailscale0` (mirror the `exit-node` user-data block).
3. A tiny **DNS forwarder** in the VM (e.g. `dnsmasq`, or just expose `100.100.100.100`
   via socat/forwarding) listening on `192.168.64.10:53`, forwarding `ts.example.com`
   queries into MagicDNS.
4. On the **macOS host**:
   - **Route:** `sudo route -n add -net 100.64.0.0/10 192.168.64.10`
     (and the IPv6 range if wanted).
   - **DNS:** `/etc/resolver/ts.example.com` containing `nameserver 192.168.64.10`
     → split DNS for the tailnet domain only.

**Use:** `ssh user@host.ts.example.com`, open `https://dashboard.ts.example.com` in your
*normal* browser, `curl` a `100.64.x.y` address — all unmodified.

**Manual cost:** one-time `route` + `/etc/resolver` setup (and re-add the route after
reboot until Tier 3 automates it); **validate the MDM-vs-`/etc/resolver` caveat here**.

**Exit criteria:** with zero per-app proxy config, the host resolves and reaches the
tailnet. The route and resolver survive a normal work session.

---

### Tier 3 — Polished, near-invisible: "you hardly know it's in the loop"

**Goal:** the system self-manages. You open your laptop and the tailnet is just there.

**Capabilities to add on top of Tier 2:**

- **Lifecycle automation.** VM auto-starts on login and self-heals.
  - `vagrant up` (QEMU/Hypervisor.framework) wrapped in a **macOS LaunchAgent**;
    `vagrant reload` on failure. QEMU's near-zero idle CPU keeps this cheap to leave
    running.
- **Host integration on boot.** A LaunchDaemon re-adds the `route` and ensures
  `/etc/resolver/ts.example.com` after every boot / network change, and re-checks
  after wake-from-sleep (macOS drops manual routes on sleep/network change).
- **Hands-off auth / key rotation.** Replace pasted pre-auth keys with automated
  minting against the Headscale admin API — reuse the `headscale-infra`
  `headscale_exit_node_preauthkey` Lambda pattern, or call the REST API directly with
  the `headscale/admin-api-key` secret (via `bin/aws-write-secret` / `aws
  secretsmanager`). Use **ephemeral + reusable** keys, or **OAuth client** creds
  (`TS_CLIENT_ID`/`TS_CLIENT_SECRET`) so the node re-registers itself with no human in
  the loop.
- **Resilience.** Persistent `TS_STATE_DIR` on a Vagrant synced folder / Docker volume
  so the node keeps its identity across rebuilds; health checks
  (`TS_ENABLE_HEALTH_CHECK`) and a watchdog that restarts `tailscaled` / re-applies NAT.
- **A thin CLI** (`tailgate up | down | status | doctor`) wrapping the VM lifecycle +
  host route/resolver, so the whole thing is one command and self-diagnosing. `doctor`
  checks: VM running, node registered, route present, resolver present, MagicDNS
  resolves, a known node pings.
- **Self-documenting IaC.** Everything expressed as a committed `Vagrantfile` +
  provisioning shell scripts + the LaunchAgent plist + the CLI, in this repo. No
  undocumented manual steps. (This is the property you said you value about Vagrant.)

**Exit criteria:** cold-boot the laptop, do nothing, and `https://*.ts.example.com`
plus `ssh` to tailnet nodes work within ~a minute, surviving sleep/wake and network
changes, with keys that rotate themselves.

---

## 6. Capability matrix

| Capability | Tier 0 | Tier 1 | Tier 2 | Tier 3 |
| --- | :---: | :---: | :---: | :---: |
| VM registered on tailnet | ✅ | ✅ | ✅ | ✅ |
| Reach services **from inside VM** | ✅ | ✅ | ✅ | ✅ |
| SSH to nodes **from host** | inside only | per-app proxy | ✅ transparent | ✅ |
| Web UIs **from host browser** | inside only | proxy profile | ✅ normal browser | ✅ |
| MagicDNS on host | ❌ | per-app (`socks5h`) | ✅ system-wide | ✅ |
| No per-app config | ❌ | ❌ | ✅ | ✅ |
| Survives reboot/sleep | ❌ | manual restart | route re-add | ✅ automatic |
| Hands-off key management | ❌ | ❌ | ❌ | ✅ |
| `tailscaled` mode | TUN | userspace | TUN (gateway) | TUN (gateway) |
| Platform | Docker | **Docker** | **Vagrant+QEMU** | **Vagrant+QEMU** |

---

## 7. Key risks & open questions

1. **MDM vs `/etc/resolver`** — *spiked and CONFIRMED working (§7.1).* No managed-profile
   DNS override is present, and a live `/etc/resolver` test was honored by the system
   resolver (`scutil --dns` showed the test domain → its nameserver, Reachable). Host-wide
   split DNS for the tailnet domain will work.
2. **Corporate all-networks proxy** — *spiked and CLEARED (§7.1).* The host runs a
   corporate SSE / app-proxy extension capturing all networks, but the `tier2/` probe
   confirmed host→`100.64.0.0/10` traffic **is not** intercepted: with a static route to
   the gateway VM, the host reached a tailnet peer over both ICMP and TCP. Transparent
   Tier 2 routing is viable on this machine.
3. **Policy / compliance.** §1.1 — confirmed personal use.
4. **QEMU host-reachable networking (Apple Silicon)** — *spiked, solved (§7.1):*
   `socket_vmnet` + a vagrant-qemu `qemu_bin` wrapper yields a stable host-reachable
   guest IP (`192.168.105.x`), preserving the route + `/etc/resolver` model. Requires a
   one-time install + a Vagrant plugin repair.
5. **Route persistence on macOS.** Manually added routes are dropped on sleep/network
   change; Tier 3's LaunchDaemon watchdog is what makes this robust.
6. **NAT return path.** Confirm host-subnet → `tailscale0` masquerade works for *all*
   target nodes (the `exit-node` rules are the proven template).
7. **DERP / connectivity.** The tailnet uses Tailscale's default DERP map
   (`HEADSCALE_DERP_SERVER_ENABLED=false`); ensure the VM's egress can reach DERP and
   `controlplane.tailscale.com` for relay when direct connections fail.
8. **State & identity.** Decide ephemeral vs persistent node identity; persistent
   `TS_STATE_DIR` avoids churn in Headplane.

### 7.1 Tier 2 spike results (2026-06-01)

Two spikes were run on the actual managed laptop (Apple Silicon, macOS 26.x).

**DNS / split-DNS feasibility — CONFIRMED working.**
- No MDM/profile DNS override exists (no `com.apple.dnsProxy` / `com.apple.dnsSettings`
  payload, no profile-scoped resolver, clean `scutil --dns`). The classic
  Config-Profile-outranks-`/etc/resolver` failure mode is **absent** here.
- The live test was honored: `/etc/resolver/tailgate-spike` → `nameserver 9.9.9.9`
  appeared in `scutil --dns` (domain `tailgate-spike`, Reachable). So `/etc/resolver`
  split DNS for the tailnet domain will be applied by the system resolver.
- **Caveat (separate from DNS):** the host runs a corporate all-networks app-proxy
  network extension (`IncludeAllNetworks`, overrides primary). It doesn't touch DNS
  resolution, and the routing question it raised is now **settled** — see below.

**Routing through the gateway — PROBE PASSED (the decisive Tier 2 result).**
- With a host route `100.64.0.0/10 → <gateway VM IP>`, the host reached a real tailnet
  peer over **both ICMP and TCP:22** (`ttl` decremented through the gateway hop). The
  corporate all-networks proxy does **not** intercept CGNAT-routed traffic.
- Also surfaced: the corporate proxy does **blanket TLS interception** of all egress
  (verified via `openssl s_client` — every host re-signed by the corporate
  SSL-decrypt CA). Tailscale is unaffected (Noise control plane, WireGuard E2E data), so
  the tunnel works and stays confidential; only generic HTTPS in the guest needed the
  corporate root CA installed (handled by `tier2/`). See `CLAUDE.md` for the general rule.
- **Conclusion: the transparent "just works" endgame is achievable on this machine.**

**QEMU host-reachable IP — SOLVED, needs setup.**
- Installed: QEMU 10.1.1 (vmnet netdevs compiled in), Vagrant 2.4.9, `vagrant-qemu`
  0.3.6. Missing: `socket_vmnet`. Blocker: `vagrant plugin list` currently fails (a
  stale `vagrant-cachier` pin) and must be repaired first.
- Recommended path (preserves the route + resolver design): **`socket_vmnet` + a
  vagrant-qemu `qemu_bin` wrapper** → guest on `192.168.105.x`, host-routable; the
  privileged daemon runs as root via launchd while QEMU stays rootless (the same pattern
  Lima/Colima/minikube use). Native `-netdev vmnet-shared` also works but needs the whole
  QEMU process to run as root — worse IaC. SLIRP+`hostfwd` is the no-install fallback but
  forces a redesign (no L3 route; `/etc/resolver` → `127.0.0.1:<fwd>` only).
- IPv6 (`fd7a:115c:a1e0::/48`): treat **IPv4-only as the pragmatic target**; defer v6.
- One-time setup commands:
  ```sh
  vagrant plugin expunge --reinstall          # repair the broken plugin state first
  brew install socket_vmnet
  sudo brew services start socket_vmnet        # root daemon via launchd
  ```

---

## 8. Recommended build order

1. **Tier 0 on Docker** — paste a pre-auth key, register `tailgate-laptop`, prove
   reachability from inside. (Hours.)
2. **Tier 1 on Docker** — flip to userspace + SOCKS5, wire up `ssh`/`curl`/a browser
   profile. Ship this as the usable PoC. (Hours.)
3. **Spike two questions** — *done (§7.1):* DNS split is clear at the config layer;
   QEMU host-reachable IP is solved via `socket_vmnet`. New blocker surfaced: a corporate
   all-networks proxy may intercept CGNAT routing (risk #2) — settle with an end-to-end
   reachability probe before committing to the transparent model.
4. **Tier 2 on Vagrant+QEMU** — `socket_vmnet` networking, TUN gateway, host route +
   `/etc/resolver`. Gate on the routing-vs-proxy reachability test. (A day or two.)
5. **Tier 3** — LaunchAgent lifecycle, route/resolver watchdog, automated key minting
   (reuse the `headscale-infra` preauthkey Lambda pattern), `tailgate` CLI. (Iterative.)

---

## 9. Sources

- [Userspace networking mode (for containers) — Tailscale](https://tailscale.com/docs/concepts/userspace-networking)
- [`tailscaled` daemon reference — Tailscale](https://tailscale.com/docs/reference/tailscaled)
- [Docker configuration parameters (`TS_*`) — Tailscale](https://tailscale.com/docs/features/containers/docker/docker-params)
- [A deep dive into using Tailscale with Docker — Tailscale blog](https://tailscale.com/blog/docker-tailscale-guide)
- [Subnet routers — Tailscale](https://tailscale.com/docs/features/subnet-routers)
- [Configure a subnet router — Tailscale](https://tailscale.com/docs/features/subnet-routers/how-to/setup)
- [Disable subnet route masquerading — Tailscale](https://tailscale.com/docs/reference/troubleshooting/network-configuration/disable-subnet-route-masquerading)
- [DNS in Tailscale](https://tailscale.com/docs/reference/dns-in-tailscale) /
  [MagicDNS](https://tailscale.com/kb/1081/magicdns) /
  [Why split DNS](https://tailscale.com/learn/why-split-dns)
- [macOS DNS / `/etc/resolver` behavior — tailscale#13461](https://github.com/tailscale/tailscale/issues/13461),
  [#14746](https://github.com/tailscale/tailscale/issues/14746),
  [#5677 (Config Profiles outrank `/etc/resolver`)](https://github.com/tailscale/tailscale/issues/5677)
- [SSH SOCKS proxy but it's Tailscale — Nahum Shalman](https://blog.shalman.org/ssh-socks-proxy-but-its-tailscale/)
- [Using Vagrant with QEMU on macOS — Rene Welches](https://blog.renewelches.com/2026/01/29/vagrant-qemu-mac/) /
  [vagrant-qemu](https://github.com/ppggff/vagrant-qemu)
- Internal: `headscale-infra` — `infra/stacks/headscale_stack.py`,
  `infra/models/headscale_config.py`, `config.toml`,
  `assets/lambda/headscale_exit_node_preauthkey`.
