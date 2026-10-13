#!/usr/bin/env bash
# Recreate the VLAN bridges VMs use (p8s-vlans.service, at boot before libvirt).
# The list is kept by p8sd in /var/lib/p8s/vlans.list: "uplink vlan bridge".
set -uo pipefail
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
LIST=${1:-/var/lib/p8s/vlans.list}
[[ -f $LIST ]] || exit 0
while read -r up vid br _; do
  [[ -z ${up:-} || $up == \#* ]] && continue
  [[ $vid =~ ^[0-9]+$ ]] || continue
  for i in $(seq 30); do [[ -e /sys/class/net/$up ]] && break; sleep 1; done  # the port may come up late
  [[ -e /sys/class/net/$up ]] || { echo "VLAN $vid: port $up not found"; continue; }
  dev=p8sv$vid
  [[ -e /sys/class/net/$dev ]] || ip link add link "$up" name "$dev" type vlan id "$vid"
  if [[ ! -e /sys/class/net/$br ]]; then
    ip link add "$br" type bridge stp_state 0 forward_delay 0
    sysctl -qw "net.ipv6.conf.$br.disable_ipv6=1"
  fi
  ip link set "$dev" master "$br"
  ip link set "$dev" up
  ip link set "$br" up
  echo "VLAN $vid: $br on $up"
done <"$LIST"
exit 0
