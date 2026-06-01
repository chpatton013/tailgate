# Tier 2 — routing probe

Before building the full transparent-routing gateway, this settles one question:

> With a host route for `100.64.0.0/10` pointing at a gateway VM, can the host
> actually reach the tailnet — or does the corporate all-networks proxy
> (network extension) intercept CGNAT-range traffic?

If it works, the same VM is the foundation of real Tier 2. If it's blocked, we
pivot to the proxy-everywhere approach instead of fighting the corporate proxy.

See [`../docs/research-and-design.md` §7.1](../docs/research-and-design.md) for context.

## One-time prerequisites

```sh
# 1. Repair Vagrant (a stale vagrant-cachier pin breaks `vagrant plugin list`):
vagrant plugin expunge --reinstall

# 2. Install socket_vmnet (gives the QEMU guest a host-reachable 192.168.105.x IP):
brew install socket_vmnet
sudo brew services start socket_vmnet      # privileged daemon via launchd

# 3. Mint a FRESH pre-auth key for the VM (the Tier 1 container consumed the last
#    single-use one) and write it to ../.env:
../bin/mint-authkey --write-env
```

> The Vagrantfile defaults to the `perk/ubuntu-2204-arm64` box. If `vagrant up`
> can't fetch it, set another arm64/QEMU box, e.g.
> `TIER2_BOX=perk/ubuntu-2004-arm64 vagrant up` (or `gyptazy/ubuntu22.04-arm64`).

## Run the probe

```sh
cd tier2
vagrant up                  # boots the gateway, joins the tailnet, sets up NAT
../bin/tier2-probe          # adds the host route, tests reachability, cleans up
```

`tier2-probe` discovers the VM's `192.168.105.x` address and a tailnet peer,
adds `route 100.64.0.0/10 -> VM` (needs `sudo`), then runs ICMP + TCP:22 tests
to the peer and prints a verdict:

- **ROUTING WORKS** → transparent Tier 2 is viable; the proxy isn't eating CGNAT.
- **PARTIAL** (ping but no TCP) → firewall/ACL or TCP steering; investigate.
- **ROUTING BLOCKED** → the corporate proxy is almost certainly intercepting
  `100.64.0.0/10`; favor the proxy-everywhere path over transparent routing.

The route is removed automatically when the probe exits.

## Teardown

```sh
cd tier2 && vagrant destroy -f
```

## Notes / caveats

- **IPv4 only.** `socket_vmnet` shared networking is v4; the tailnet v6 range is
  deferred.
- **Dynamic IP.** socket_vmnet assigns the guest IP via DHCP, so the probe
  discovers it rather than pinning it. Real Tier 2 will want a DHCP reservation
  or a static in-guest address.
- **Gateway NAT** mirrors the exit-node pattern: `ip_forward` + `MASQUERADE` out
  `tailscale0` + FORWARD accepts. See `provision.sh`.
