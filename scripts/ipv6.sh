#!/usr/bin/env bash
# ipv6.sh — IPv6 for VMs and containers on the private (NAT) network.
#   ipv6.sh on | off | status
# "on" gives the NAT network a private IPv6 range (fdXX:…/64, the same on
# every run of this server) and NATs it to the server's own IPv6, like IPv4.
# Needs IPv6 internet on the server. VMs on the home network get IPv6 from
# the router already (if it offers it).
set -uo pipefail
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
STATEF=/var/lib/p8s/ipv6-nat
NET=default

prefix() { # stable ULA /64 from the machine id
  local h; h=$(sha256sum /etc/machine-id | cut -c1-10)
  printf 'fd%s:%s:%s' "${h:0:2}" "${h:2:4}" "${h:6:4}"
}

replace_net() { # $1 = new XML; restart the network keeping guests plugged in
  local tmp ports; tmp=$(mktemp)
  printf '%s\n' "$1" >"$tmp"
  virsh net-define "$tmp" >/dev/null || { rm -f "$tmp"; echo "libvirt didn't accept the network settings"; return 1; }
  rm -f "$tmp"
  ports=$(ls /sys/class/net/virbr0/brif 2>/dev/null || true)
  virsh net-destroy $NET >/dev/null 2>&1
  virsh net-start $NET >/dev/null || return 1
  for p in $ports; do ip link set dev "$p" master virbr0 2>/dev/null; ip link set dev "$p" up 2>/dev/null; done
  systemctl is-active -q p8sd && systemctl restart p8sd   # firewall rules name the guests' ports
  return 0
}

strip6() { sed -e "/<ip family=['\"]ipv6['\"]/,/<\/ip>/d" -e "s/<nat ipv6=['\"]yes['\"]>/<nat>/"; }

case ${1:-status} in
on)
  ip -6 route show default | grep -q . || { echo "This server has no IPv6 internet (no IPv6 default route): ask your provider / turn IPv6 on in the router first."; exit 1; }
  p=$(prefix)
  x=$(virsh net-dumpxml --inactive $NET | strip6)
  grep -q "<nat" <<<"$x" || x=$(sed "s#<forward mode=['\"]nat['\"]/>#<forward mode='nat'><nat/></forward>#" <<<"$x")
  x=$(sed -e "s#<nat>#<nat ipv6='yes'>#" -e "s#<nat/>#<nat ipv6='yes'/>#" <<<"$x")
  ins="  <ip family='ipv6' address='$p::1' prefix='64'>\n    <dhcp>\n      <range start='$p::100' end='$p::1ff'/>\n    </dhcp>\n  </ip>"
  x=$(sed "s#</network>#$ins\n</network>#" <<<"$x")
  sysctl -qw net.ipv6.conf.all.forwarding=1
  echo 'net.ipv6.conf.all.forwarding=1' >/etc/sysctl.d/90-p8s-ipv6.conf
  # forwarding turns off router advertisements on the uplink unless accept_ra=2
  dev=$(ip -6 route show default | awk '{for(i=1;i<NF;i++) if($i=="dev"){print $(i+1); exit}}')
  [[ -n $dev ]] && { sysctl -qw "net.ipv6.conf.$dev.accept_ra=2"; echo "net.ipv6.conf.$dev.accept_ra=2" >>/etc/sysctl.d/90-p8s-ipv6.conf; }
  replace_net "$x" || exit 1
  mkdir -p "$(dirname "$STATEF")"; echo "$p::/64" >"$STATEF"
  echo "==> IPv6 on: the private network is $p::/64 (gateway $p::1), NATed to this server's IPv6."
  echo "    Running VMs get an address after a network restart inside them (or a reboot)."
  ;;
off)
  x=$(virsh net-dumpxml --inactive $NET | strip6)
  replace_net "$x" || exit 1
  rm -f "$STATEF" /etc/sysctl.d/90-p8s-ipv6.conf
  echo "==> IPv6 off for the private network"
  ;;
status)
  if virsh net-dumpxml $NET 2>/dev/null | grep -q "family=['\"]ipv6"; then echo "on: $(cat "$STATEF" 2>/dev/null)"; else echo off; fi
  ip -6 route show default | grep -q . && echo "server IPv6 internet: yes" || echo "server IPv6 internet: no"
  ;;
*) echo "usage: $0 on|off|status"; exit 2 ;;
esac
