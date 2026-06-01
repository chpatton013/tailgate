#!/usr/bin/env bash
# Provision the Tier 2 gateway VM (runs as root via Vagrant's shell provisioner).
# Two NICs are present:
#   - the user-mode NIC (10.0.2.x) Vagrant manages over, and which provides the
#     VM's own internet egress (tailscale's tunnel rides this), and
#   - the socket_vmnet NIC, which must come up with a host-reachable 192.168.105.x
#     address so the macOS host can route 100.64.0.0/10 to it.
# It then joins the tailnet in TUN mode and NATs host traffic out tailscale0.
set -euxo pipefail

: "${TS_AUTHKEY:?TS_AUTHKEY not provided (set it in ../.env)}"
: "${TS_LOGIN_SERVER:=https://headscale.example.com}"
HOSTNAME_TS="tailgate-gw-probe"

export DEBIAN_FRONTEND=noninteractive

# Trust the corporate root CA(s) if provided, so HTTPS validates through the
# corporate SSL-decrypt proxy. (Tailscale itself tolerates the MITM via its Noise
# control protocol, but generic curl/apt require the chain to validate.)
if [ -f /tmp/corp-ca.pem ]; then
  csplit -z -s -f /usr/local/share/ca-certificates/corp- -b '%02d.crt' \
    /tmp/corp-ca.pem '/BEGIN CERTIFICATE/' '{*}' || true
  update-ca-certificates || true
fi

curl -fsSL https://tailscale.com/install.sh | sh

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

# Wait for the host-reachable address to appear.
for _ in $(seq 1 20); do
  ip -4 addr show "$VMNET_IF" | grep -q '192\.168\.105\.' && break
  sleep 1
done
ip -4 addr show "$VMNET_IF" | grep -q '192\.168\.105\.' \
  || { echo "ERROR: $VMNET_IF never got a 192.168.105.x address"; exit 1; }

# --- join the tailnet (TUN) --------------------------------------------------
# --accept-dns=false keeps the probe pure L3; --reset clears prior state.
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

echo "=== gateway socket_vmnet interface: ${VMNET_IF} ==="
ip -4 addr show "$VMNET_IF" | awk '/inet /{print "  host-reachable IP:", $2}'
echo "=== gateway tailnet IP ==="
tailscale ip -4 | sed 's/^/  /'
echo "=== tailnet peers ==="
tailscale status || true
