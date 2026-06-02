#!/usr/bin/env bash
# Provision the Tier 2 gateway VM (runs as root via Vagrant's shell provisioner).
# Two NICs are present:
#   - the user-mode NIC (10.0.2.x) Vagrant manages over, which also carries the
#     VM's internet egress (tailscale's tunnel rides this), and
#   - the socket_vmnet NIC, brought up with a host-reachable 192.168.105.x address
#     so the macOS host can route 100.64.0.0/10 to it.
# The VM joins the tailnet in TUN mode (so MagicDNS at 100.100.100.100 is live),
# NATs host traffic out tailscale0, and runs a dnsmasq forwarder so the host can
# resolve the tailnet MagicDNS domain through this gateway.
set -euxo pipefail

: "${TS_AUTHKEY:?TS_AUTHKEY not provided (set it in ../.env)}"
: "${TS_LOGIN_SERVER:=https://headscale.example.com}"
: "${TS_MAGICDNS_DOMAIN:=ts.example.com}"
HOSTNAME_TS="tailgate-gw"
MAGICDNS_RESOLVER="100.100.100.100"

export DEBIAN_FRONTEND=noninteractive

# Trust the corporate root CA(s) if provided, so HTTPS validates through a
# corporate SSL-decrypt proxy. (Tailscale tolerates the MITM via its Noise
# control protocol, but generic curl/apt require the chain to validate.)
if [ -f /tmp/corp-ca.pem ]; then
  csplit -z -s -f /usr/local/share/ca-certificates/corp- -b '%02d.crt' \
    /tmp/corp-ca.pem '/BEGIN CERTIFICATE/' '{*}' || true
  update-ca-certificates || true
fi

curl -fsSL https://tailscale.com/install.sh | sh
apt-get update -y
apt-get install -y dnsmasq dnsutils

echo 'net.ipv4.ip_forward = 1' >/etc/sysctl.d/99-tailgate.conf
sysctl -p /etc/sysctl.d/99-tailgate.conf

# --- bring up the socket_vmnet NIC -------------------------------------------
# It's the ethernet interface that is NOT the user-mode 10.0.2.x management NIC.
VMNET_IF=""
for iface in /sys/class/net/*; do
  name="$(basename "$iface")"
  case "$name" in lo|tailscale*|docker*|veth*) continue;; esac
  if ip -4 addr show "$name" 2>/dev/null | grep -q '10\.0\.2\.'; then continue; fi
  VMNET_IF="$name"
done
: "${VMNET_IF:?could not identify the socket_vmnet NIC}"

ip link set "$VMNET_IF" up
# DHCP just this interface; don't let it steal the default route from the
# user-mode NIC (that NIC carries internet/tailscale egress).
dhclient -1 "$VMNET_IF" || dhclient "$VMNET_IF" || true
ip route del default dev "$VMNET_IF" 2>/dev/null || true

for _ in $(seq 1 20); do
  ip -4 addr show "$VMNET_IF" | grep -q '192\.168\.105\.' && break
  sleep 1
done
ip -4 addr show "$VMNET_IF" | grep -q '192\.168\.105\.' \
  || { echo "ERROR: $VMNET_IF never got a 192.168.105.x address"; exit 1; }

# --- join the tailnet (TUN) --------------------------------------------------
# --accept-dns=FALSE on purpose: in TUN mode tailscaled serves MagicDNS at
# 100.100.100.100 regardless of this flag (it only controls whether tailscale
# rewrites the *guest's own* resolv.conf, which we don't need — dnsmasq queries
# 100.100.100.100 directly). Setting it true makes tailscale reconfigure
# systemd-resolved mid-provision, which can bounce networking and drop Vagrant's
# management SSH. --reset clears prior state on re-provision.
tailscale up \
  --login-server="${TS_LOGIN_SERVER}" \
  --authkey="${TS_AUTHKEY}" \
  --hostname="${HOSTNAME_TS}" \
  --accept-dns=false \
  --reset

# --- NAT host traffic onto the tailnet ---------------------------------------
iptables -t nat -C POSTROUTING -o tailscale0 -j MASQUERADE 2>/dev/null \
  || iptables -t nat -A POSTROUTING -o tailscale0 -j MASQUERADE
iptables -C FORWARD -i "$VMNET_IF" -o tailscale0 -j ACCEPT 2>/dev/null \
  || iptables -A FORWARD -i "$VMNET_IF" -o tailscale0 -j ACCEPT
iptables -C FORWARD -i tailscale0 -o "$VMNET_IF" -m state --state RELATED,ESTABLISHED -j ACCEPT 2>/dev/null \
  || iptables -A FORWARD -i tailscale0 -o "$VMNET_IF" -m state --state RELATED,ESTABLISHED -j ACCEPT

# --- DNS forwarder ------------------------------------------------------------
# Listen on the vmnet NIC and forward EVERYTHING to tailscaled's MagicDNS
# resolver. We deliberately don't scope by domain here: the host's
# /etc/resolver/<domain> file already decides which queries reach this VM, so the
# gateway needn't know the tailnet domain (avoids a host/VM domain-mismatch bug).
# MagicDNS itself answers tailnet names and forwards the rest to the tailnet's
# global nameservers. bind-dynamic follows the interface address across reboots.
cat >/etc/dnsmasq.d/tailgate.conf <<EOF
interface=${VMNET_IF}
bind-dynamic
no-resolv
server=${MAGICDNS_RESOLVER}
EOF
systemctl enable dnsmasq
systemctl restart dnsmasq

# --- report + self-test ------------------------------------------------------
GW4="$(ip -4 addr show "$VMNET_IF" | awk '/inet /{print $2}' | cut -d/ -f1)"
echo "=== gateway socket_vmnet interface: ${VMNET_IF} (host-reachable IP ${GW4}) ==="
echo "=== gateway tailnet IP ==="
tailscale ip -4 | sed 's/^/  /'
# Confirm the full DNS path works with --accept-dns=false: MagicDNS answers at
# 100.100.100.100, and dnsmasq (which the host's /etc/resolver points at) relays it.
SELF="${HOSTNAME_TS}.${TS_MAGICDNS_DOMAIN}"
echo "=== DNS self-test for ${SELF} ==="
echo "  direct @100.100.100.100 : $(dig +short @100.100.100.100 "$SELF" 2>/dev/null | tr '\n' ' ')"
echo "  via dnsmasq @${GW4}      : $(dig +short @"${GW4}" "$SELF" 2>/dev/null | tr '\n' ' ')"
echo "  (both should show this node's 100.64.x.y; if blank, MagicDNS/dnsmasq need a look)"
echo "=== tailnet peers ==="
tailscale status || true
