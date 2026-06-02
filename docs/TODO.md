# Tailgate — outstanding work

Snapshot of what's left, roughly in priority order. See
[`research-and-design.md`](research-and-design.md) for the tier design and
[`../AGENTS.md`](../AGENTS.md) for the hard-won gotchas behind several of these.

**Where things stand:** Tier 0/1 shipped and in use. Tier 2 (transparent gateway)
is functionally validated — routing to `100.64.0.0/10` and host-wide MagicDNS via
the gateway VM both work end to end. What remains for Tier 2 is *hardening* so it's
not fragile; Tier 3 is the "near-invisible" polish.

---

## Tier 2 hardening (make the working gateway robust)

### 1. Ephemeral pre-auth keys + stale-node cleanup  ⟵ requested
Every `vm-destroy` + `vagrant up` is a brand-new guest (the disk is wiped), so it
registers as a *new* Headscale node. Because the canonical name is already taken by
prior (now-offline) registrations, Headscale hands out suffixed names
(`tailgate-gw`, `tailgate-gw-jwnca3s6`, `tailgate-gw-hyarmgwt`, …) and a new tailnet
IP each time. Result: a pile of dead nodes and IP churn.

- **Fix:** mint the gateway's key as **ephemeral** (`bin/mint-authkey --ephemeral`,
  ideally `--reusable --ephemeral` for iteration) so Headscale auto-removes the node
  shortly after it goes offline. No manual pruning, no accumulation.
- **Also:** prune the existing stale `tailgate-gw*` nodes once (Headplane UI, or the
  Headscale API).
- **Consider:** persisting `/var/lib/tailscale` across rebuilds (a dedicated disk or
  synced dir) so the gateway keeps a *stable identity + IP* instead of re-registering
  — an alternative to ephemeral that also fixes the dynamic-IP item (#4). Pick one.
- Update `tier2/README.md` and the `vm-up` flow to default to the chosen approach.

### 2. Clean dnsmasq startup
Installing `dnsmasq` via apt auto-starts it with the default config *before* our
`/etc/dnsmasq.d/tailgate.conf` drop-in exists, so it fails to bind `:53`
(systemd-resolved holds `127.0.0.53:53`) and leaves the unit in a `failed` state.
Our later `systemctl restart` with `interface=`/`bind-dynamic` succeeds, so it works
— but the provisioning log shows a scary (benign) failure and a reboot could start
from the failed unit.

- **Fix:** in `tier2/provision.sh`, write the drop-in *before* the service ever
  starts (e.g. `apt-get install` with the service masked, then unmask), and/or
  `systemctl reset-failed dnsmasq` before the final `restart`. Goal: zero failed
  units and no alarming log lines.

### 3. Robust VM teardown when the guest is wedged
A QEMU guest that wedged after host sleep can't be cleanly shut down, so
`vagrant destroy` **hangs indefinitely** waiting on it (cost us a 20-minute hang).
Recovery today is manual: force-kill the qemu PID, then destroy.

- **Fix:** add a `tailgate-tier2 vm-kill` (or make `vm-destroy` robust): find the
  gateway's qemu PID, `kill -9` it, then `vagrant destroy -f`, then
  `vagrant global-status --prune`. Bound every `vagrant` call with a timeout so a
  hang is detected, not silent.

### 4. Stable gateway IP
`socket_vmnet` assigns the guest IP by DHCP, so `tailgate-tier2 up` discovers it
each time and the host route/resolver target can drift across reboots.

- **Fix:** pin it — a DHCP reservation (MAC→IP via bootpd) or a static address on
  the vmnet NIC inside the guest. Then the host route + `/etc/resolver` are stable
  and a LaunchDaemon (Tier 3) can assume a fixed target. (Or solve via persistent
  identity in #1.)

---

## Tier 3 — polished, near-invisible

- **VM stability + survive sleep/reboot.** The repeated guest freezes were a
  QEMU+HVF `-cpu host`/`highmem=on` hard-halt, now mitigated with `cortex-a72` +
  `highmem=off` (AGENTS.md #12) — needs a soak test to confirm it's actually fixed.
  Separately, still validate behavior across a *real* host sleep and reboot, and add
  auto-recovery (detect a hung guest, force-kill qemu, recreate, re-join) as a backstop.
- **Persist host wiring.** A macOS **LaunchDaemon** that re-adds the
  `route 100.64.0.0/10 → VM` and ensures `/etc/resolver/<domain>` on boot, after
  wake-from-sleep, and on network change (macOS drops manual routes on these).
- **Autostart the VM** on login via a **LaunchAgent** (`vagrant up` wrapped, with
  retry/self-heal). QEMU's low idle cost makes "always on" cheap.
- **Hands-off keys.** Automated minting/rotation against the Headscale admin API
  (reuse the upstream IaC preauthkey flow), or OAuth client creds, so nothing is
  pasted and nodes re-register themselves.
- **Health + self-heal.** A watchdog that checks VM up / node registered / route
  present / resolver present / a known peer reachable, and repairs what's missing.
- **`tailgate-tier2 doctor`** that runs those checks and prints a clear verdict
  (mirror `tailgate doctor`).

---

## Known caveats / smaller items

- **Loopback-bound node services.** A service bound to `127.0.0.1` on a node (e.g. an
  app gateway UI on `:8000`) is not reachable even via Tier 2 transparent routing.
  Reach it with `tailgate forward`, publish it with `tailscale serve`, or rebind it to
  the node's tailnet IP. Decide on a recommended pattern and document it.
- **IPv6 deferred.** `socket_vmnet` shared networking is IPv4-only, so the tailnet
  IPv6 range (`fd7a:115c:a1e0::/48`) isn't routed. Revisit only if needed.
- **Box pinning.** The Vagrant box defaults to `perk/ubuntu-2204-arm64`; pin/verify a
  known-good version (and document fallbacks) so `vagrant up` is reproducible.
- **Tier 1 polish** (explicitly deprioritized): a proxied browser-launcher / container
  autostart. Skip unless wanted.
