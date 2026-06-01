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
apt-get install -y dnsmasq

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

# --- join the tailnet (TUN, MagicDNS on) -------------------------------------
# --accept-dns=true ensures tailscaled pulls the MagicDNS config and serves it
# at 100.100.100.100. --reset clears prior state on re-provision.
tailscale up \
  --login-server="${TS_LOGIN_SERVER}" \
  --authkey="${TS_AUTHKEY}" \
  --hostname="${HOSTNAME_TS}" \
  --accept-dns=true \
  --reset

# --- NAT host traffic onto the tailnet ---------------------------------------
iptables -t nat -C POSTROUTING -o tailscale0 -j MASQUERADE 2>/dev/null \
  || iptables -t nat -A POSTROUTING -o tailscale0 -j MASQUERADE
iptables -C FORWARD -i "$VMNET_IF" -o tailscale0 -j ACCEPT 2>/dev/null \
  || iptables -A FORWARD -i "$VMNET_IF" -o tailscale0 -j ACCEPT
iptables -C FORWARD -i tailscale0 -o "$VMNET_IF" -m state --state RELATED,ESTABLISHED -j ACCEPT 2>/dev/null \
  || iptables -A FORWARD -i tailscale0 -o "$VMNET_IF" -m state --state RELATED,ESTABLISHED -j ACCEPT

# --- DNS forwarder for the tailnet domain ------------------------------------
# Listen on the vmnet NIC and forward the MagicDNS domain to tailscaled's
# resolver. bind-dynamic follows the interface's address across reboots; the
# host's /etc/resolver scopes only this domain to us, so no-resolv is safe.
cat >/etc/dnsmasq.d/tailgate.conf <<EOF
interface=${VMNET_IF}
bind-dynamic
no-resolv
server=/${TS_MAGICDNS_DOMAIN}/${MAGICDNS_RESOLVER}
EOF
systemctl enable dnsmasq
systemctl restart dnsmasq

# --- report ------------------------------------------------------------------
echo "=== gateway socket_vmnet interface: ${VMNET_IF} ==="
ip -4 addr show "$VMNET_IF" | awk '/inet /{print "  host-reachable IP:", $2}'
echo "=== gateway tailnet IP ==="
tailscale ip -4 | sed 's/^/  /'
echo "=== MagicDNS self-test (resolve ${HOSTNAME_TS}.${TS_MAGICDNS_DOMAIN} via dnsmasq) ==="
getent hosts "${HOSTNAME_TS}.${TS_MAGICDNS_DOMAIN}" || \
  dig +short "@127.0.0.1" "${HOSTNAME_TS}.${TS_MAGICDNS_DOMAIN}" || \
  echo "  (could not resolve — check 'tailscale status' and dnsmasq)"
echo "=== tailnet peers ==="
tailscale status || true
