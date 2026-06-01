# Tier 2 — transparent gateway

A small QEMU + socket_vmnet Linux VM joins the tailnet in TUN mode and acts as a
**gateway**: the macOS host routes `100.64.0.0/10` to it and forwards the tailnet
MagicDNS domain to it, so a *normal* browser/terminal reaches tailnet nodes by name
with **no per-tool config**. The gateway NATs host traffic out `tailscale0` and runs a
`dnsmasq` forwarder to tailscaled's MagicDNS resolver (`100.100.100.100`).

Routing through this gateway was validated by the probe (see
[`../docs/research-and-design.md` §7.1](../docs/research-and-design.md)) — the corporate
all-networks proxy does not intercept CGNAT-routed traffic on this host.

Driven by `../bin/tailgate-tier2` (run from anywhere):

```
tailgate-tier2 vm-up      boot + provision the gateway VM
tailgate-tier2 up         wire the host (route + /etc/resolver)   [sudo]
tailgate-tier2 status     show VM / route / resolver / DNS state
tailgate-tier2 down       remove the host route + resolver        [sudo]
tailgate-tier2 vm-destroy destroy the VM
tailgate-tier2 probe      run the standalone reachability probe
```

## One-time prerequisites

```sh
# 1. Repair Vagrant if `vagrant plugin list` is broken by a stale pin:
vagrant plugin expunge --force && vagrant plugin install vagrant-qemu

# 2. socket_vmnet — gives the guest a host-reachable 192.168.105.x IP:
brew install socket_vmnet
sudo brew services start socket_vmnet      # privileged daemon via launchd

# 3. A FRESH pre-auth key for the VM (it's a new node), and set TS_MAGICDNS_DOMAIN
#    to your tailnet's MagicDNS base domain in ../.env:
../bin/mint-authkey --write-env
```

### If your host egress is TLS-intercepted (corporate SSL-decrypt proxy)

The VM's HTTPS (install script, apt) validates against the web PKI, so a corporate MITM
breaks it unless the VM trusts the corporate root CA. Export your host's trust anchors to
`tier2/corp-ca.pem` (gitignored); the Vagrantfile ships it in and `provision.sh` installs
it automatically:

```sh
security find-certificate -a -p -c "<OrgName>" /Library/Keychains/System.keychain > tier2/corp-ca.pem
```

(Tailscale's own control/data plane tolerates the MITM via Noise, so this is only needed
for generic HTTPS during provisioning. Detect interception with
`openssl s_client -connect <host>:443 | openssl x509 -noout -issuer`.)

> Box: defaults to `perk/ubuntu-2204-arm64`. If `vagrant up` can't fetch it, pick another
> arm64/QEMU box, e.g. `TIER2_BOX=perk/ubuntu-2004-arm64`.

## Bring it up

```sh
tailgate-tier2 vm-up      # boots, joins the tailnet (MagicDNS on), NAT + dnsmasq
tailgate-tier2 up         # host route 100.64.0.0/10 -> VM, and /etc/resolver split DNS
```

Then, from the host with no proxy config:

```sh
ssh user@somenode.<your-domain>          # resolves via the gateway, routes transparently
open http://somenode.<your-domain>:PORT  # a normal browser tab
```

Check state any time with `tailgate-tier2 status`. Tear the wiring down with
`tailgate-tier2 down` (leaves the VM running); destroy the VM with
`tailgate-tier2 vm-destroy`.

## Caveats

- **Loopback-bound node services still need a forward.** Transparent routing reaches
  services on a node's *tailnet* IP. A service bound to `127.0.0.1` on the node (e.g. an
  app gateway UI) is not reachable this way — use `tailgate forward`, `tailscale serve`,
  or rebind it. (Same as Tier 1.)
- **Dynamic VM IP.** socket_vmnet assigns the guest IP via DHCP; `tailgate-tier2 up`
  discovers it rather than pinning it. After a reboot that changes the IP, re-run `up`.
  A DHCP reservation / static IP and auto-re-wiring on sleep/boot are Tier 3.
- **IPv4 only.** socket_vmnet shared networking is v4; the tailnet v6 range is deferred.
- **NAT** mirrors the exit-node pattern: `ip_forward` + `MASQUERADE` out `tailscale0` +
  FORWARD accepts (see `provision.sh`).
