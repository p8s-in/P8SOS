#!/usr/bin/env bash
# Recreate the virtual-network devices VMs use (p8s-vlans.service, at boot
# before libvirt starts the VMs). p8sd keeps /var/lib/p8s/sdn.list
# ("vni local-address" per line) and adds the rest (peers, gateway) itself.
set -uo pipefail
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
LIST=${1:-/var/lib/p8s/sdn.list}
[[ -f $LIST ]] || exit 0
while read -r vni local _; do
  [[ -z ${vni:-} || $vni == \#* ]] && continue
  [[ $vni =~ ^[0-9]+$ ]] || continue
  br=vxb$vni dev=vxl$vni
  if [[ ! -e /sys/class/net/$br ]]; then
    ip link add "$br" type bridge stp_state 0 forward_delay 0
    sysctl -qw "net.ipv6.conf.$br.disable_ipv6=1" 2>/dev/null
  fi
  ip link set "$br" mtu 1230
  ip link set "$br" up
  if [[ $local != - && ! -e /sys/class/net/$dev ]]; then
    ip link add "$dev" type vxlan id "$vni" local "$local" dstport 4789 &&
      ip link set "$dev" mtu 1230 && ip link set "$dev" master "$br" && ip link set "$dev" up
  fi
  echo "virtual network $vni: $br"
done <"$LIST"
exit 0
