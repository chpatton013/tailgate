#!/usr/bin/env bash
# Provision the Tier 2 gateway VM: join the tailnet in TUN mode, enable IP
# forwarding, and masquerade host-originated traffic out tailscale0 so replies
# find their way back. Mirrors the exit-node NAT pattern from the IaC project.
set -euxo pipefail

: "${TS_AUTHKEY:?TS_AUTHKEY not provided (set it in ../.env)}"
: "${TS_LOGIN_SERVER:=https://headscale.example.com}"
HOSTNAME_TS="tailgate-gw-probe"

export DEBIAN_FRONTEND=noninteractive
curl -fsSL https://tailscale.com/install.sh | sh

# Forwarding on.
echo 'net.ipv4.ip_forward = 1' >/etc/sysctl.d/99-tailgate.conf
sysctl -p /etc/sysctl.d/99-tailgate.conf

# Join the tailnet. --accept-dns=false keeps the probe pure L3 (no MagicDNS in
# the guest); --reset clears any prior state on re-provision.
tailscale up \
  --login-server="${TS_LOGIN_SERVER}" \
  --authkey="${TS_AUTHKEY}" \
  --hostname="${HOSTNAME_TS}" \
  --accept-dns=false \
  --reset

# The vmnet interface is the one carrying the host-reachable 192.168.105.x IP
# (also the default route here). Host traffic for 100.64.0.0/10 arrives on it.
VMNET_IF="$(ip -o -4 addr show | awk '/192\.168\.105\./{print $2; exit}')"
: "${VMNET_IF:?could not find the 192.168.105.x vmnet interface}"

# NAT host traffic onto the tailnet; allow forwarding both directions.
iptables -t nat -C POSTROUTING -o tailscale0 -j MASQUERADE 2>/dev/null \
  || iptables -t nat -A POSTROUTING -o tailscale0 -j MASQUERADE
iptables -C FORWARD -i "$VMNET_IF" -o tailscale0 -j ACCEPT 2>/dev/null \
  || iptables -A FORWARD -i "$VMNET_IF" -o tailscale0 -j ACCEPT
iptables -C FORWARD -i tailscale0 -o "$VMNET_IF" -m state --state RELATED,ESTABLISHED -j ACCEPT 2>/dev/null \
  || iptables -A FORWARD -i tailscale0 -o "$VMNET_IF" -m state --state RELATED,ESTABLISHED -j ACCEPT

echo "=== gateway vmnet interface: ${VMNET_IF} ==="
ip -4 addr show "$VMNET_IF" | awk '/inet /{print "  host-reachable IP:", $2}'
echo "=== gateway tailnet IP ==="
tailscale ip -4 | sed 's/^/  /'
echo "=== tailnet peers ==="
tailscale status || true
