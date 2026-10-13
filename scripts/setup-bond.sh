#!/usr/bin/env bash
# setup-bond.sh — give the home-network bridge a second (third…) cable:
#   sudo p8 bond-setup enp4s0                    # failover: one cable works, the other waits
#   sudo p8 bond-setup enp4s0 --mode 802.3ad     # both at once (switch must have LACP on those ports)
#
# br0's cable becomes part of "bond0" together with the new port(s); br0 then
# sits on bond0. VMs keep their addresses. If the network isn't back within
# 60 s, the old settings are restored by themselves.
# Options (env): P8S_YES=1 skip the question, BR=br0.
set -euo pipefail
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

BR=${BR:-br0}
BOND=bond0
IFACES=/etc/network/interfaces
STATE=/var/lib/p8s/bond
RESULT=$STATE/result
MODE=active-backup
NEWPORTS=()

ok()  { printf '  \033[32m✔\033[0m %s\n' "$*"; }
die() { printf '  \033[31m✘ %s\033[0m\n' "$*" >&2; exit 1; }
say() { printf '\033[1m==> %s\033[0m\n' "$*"; }

while (($#)); do
  case $1 in
    --mode) MODE=${2:-}; shift 2 ;;
    --mode=*) MODE=${1#*=}; shift ;;
    -*) die "unknown option $1" ;;
    *) NEWPORTS+=("$1"); shift ;;
  esac
done
case $MODE in active-backup|802.3ad|balance-alb) ;; *) die "mode must be active-backup, 802.3ad or balance-alb" ;; esac
[[ $EUID == 0 ]] || die "run as root: sudo p8 bond-setup PORT"
((${#NEWPORTS[@]})) || die "which port? e.g. sudo p8 bond-setup enp4s0   (ports: $(ls /sys/class/net | tr '\n' ' '))"
mkdir -p "$STATE"

[[ -d /sys/class/net/$BR/bridge ]] || die "the home network ($BR) isn't set up yet: run sudo p8 lan-setup first"
[[ -d /sys/class/net/$BOND ]] && die "$BOND already exists (cat /proc/net/bonding/$BOND)"
grep -Eq "^iface[[:space:]]+$BR[[:space:]]" "$IFACES" || die "$BR is not configured in $IFACES"
CUR=$(awk -v BR="$BR" '$1=="iface" {inb = ($2==BR)} inb && $1=="bridge_ports" {print $2; exit}' "$IFACES")
[[ -n $CUR && $CUR != none ]] || die "can't find $BR's cable (bridge_ports) in $IFACES"
[[ -e /sys/class/net/$CUR/device ]] || die "$BR's port $CUR is not a physical port"
for p in "${NEWPORTS[@]}"; do
  [[ $p != "$CUR" ]] || die "$p is already $BR's cable"
  [[ -e /sys/class/net/$p/device ]] || die "$p is not a physical network port (ls /sys/class/net)"
  [[ -d /sys/class/net/$p/wireless || -e /sys/class/net/$p/phy80211 ]] && die "$p is Wi-Fi; only cables can be bonded"
  [[ -e /sys/class/net/$p/master ]] && die "$p is already in use (part of $(basename "$(readlink /sys/class/net/$p/master)"))"
  [[ -z $(ip -o addr show dev "$p" scope global) ]] || die "$p has its own address; it must be unused"
  grep -Eq "^iface[[:space:]]+$p[[:space:]]+inet[[:space:]]+(dhcp|static)" "$IFACES" && die "$p is configured in $IFACES; remove that first"
done
command -v ifenslave >/dev/null || [[ -e /etc/network/if-pre-up.d/ifenslave ]] || apt-get install -y ifenslave >/dev/null
modprobe bonding 2>/dev/null || true
ok "$BR's cable: $CUR; adding: ${NEWPORTS[*]} (mode $MODE)"
[[ $MODE == 802.3ad ]] && echo "  Note: 802.3ad needs LACP (a 'port channel' / 'LAG') on those switch ports, or the network stays down and is rolled back."

SLAVES="$CUR ${NEWPORTS[*]}"
NEW=$STATE/interfaces.new
{
  awk -v BR="$BR" -v CUR="$CUR" -v BOND="$BOND" '
    $1=="iface" {inb = ($2==BR)}
    inb && $1=="bridge_ports" {sub(CUR, BOND)}
    {print}' "$IFACES"
  printf '\n# P8S OS: %s carries %s (cables: %s)\n' "$BOND" "$BR" "$SLAVES"
  for p in "${NEWPORTS[@]}"; do printf 'iface %s inet manual\n' "$p"; done
  printf '\nauto %s\niface %s inet manual\n    bond-slaves %s\n    bond-mode %s\n    bond-miimon 100\n' "$BOND" "$BOND" "$SLAVES" "$MODE"
  [[ $MODE == active-backup ]] && printf '    bond-primary %s\n' "$CUR"
  [[ $MODE == 802.3ad ]] && printf '    bond-lacp-rate fast\n    bond-xmit-hash-policy layer3+4\n'
} | cat -s >"$NEW"
grep -q "bridge_ports $BOND" "$NEW" || die "could not build the new config (left at $NEW)"

echo "  The network drops for a few seconds; VMs stay connected afterwards."
if [[ ${P8S_YES:-0} != 1 ]]; then
  read -r -p "  Continue? [y/N] " a
  [[ $a == [yY]* ]] || { echo "cancelled"; exit 0; }
fi

BACKUP=$STATE/interfaces.before-bond
cp -a "$IFACES" "$BACKUP"
cat >"$STATE/apply.sh" <<EOF
#!/bin/bash
export PATH=$PATH
exec >"$STATE/apply.log" 2>&1
set -x
ports=\$(ls /sys/class/net/$BR/brif 2>/dev/null | grep -v -x -e "$CUR" || true)   # VMs and VLAN uplinks stay plugged
vlans=\$(ls -d /sys/class/net/p8sv* 2>/dev/null | xargs -rn1 basename || true)
online() {
  for _ in \$(seq 60); do
    ip -4 -o addr show dev $BR | grep -q inet && ip -4 route show default | grep -q "dev $BR" &&
      ping -c1 -W1 "\$(ip -4 route show default | awk '{print \$3; exit}')" >/dev/null && return 0
    sleep 1
  done
  return 1
}
replug() { for p in \$ports; do ip link set dev "\$p" master $BR 2>/dev/null; ip link set dev "\$p" up 2>/dev/null; done; }
ifdown --force $BR || true
cp "$NEW" "$IFACES"
if ifup $BOND && ifup $BR && online; then
  replug
  # VLAN devices move from the old cable to the bond
  for v in \$vlans; do ip link del "\$v" 2>/dev/null; done
  [[ -f /var/lib/p8s/vlans.list ]] && sed -i -E "s/^$CUR /$BOND /" /var/lib/p8s/vlans.list && /usr/local/share/p8s/vlans.sh
  echo ok >"$RESULT"
else
  ifdown --force $BR || true
  ifdown --force $BOND || true
  ip link del $BOND 2>/dev/null || true
  cp "$BACKUP" "$IFACES"
  ifup $BR || true
  replug
  echo failed >"$RESULT"
fi
EOF
chmod 0700 "$STATE/apply.sh"
rm -f "$RESULT"
say "Moving $BR onto $BOND"
systemd-run --quiet --unit "p8s-bond-$(date +%s)" --collect /bin/bash "$STATE/apply.sh"
for _ in $(seq 150); do [[ -f $RESULT ]] && break; sleep 1; done
case $(cat "$RESULT" 2>/dev/null) in
  ok) ok "$BR now runs on $BOND ($SLAVES, $MODE). Pull one cable to try: the server stays online."
      grep -E "^(Bonding Mode|Currently Active Slave|Slave Interface|MII Status)" /proc/net/bonding/$BOND | sed 's/^/    /' || true ;;
  failed) die "the network didn't come back on $BOND, so the old settings were restored. Details: $STATE/apply.log" ;;
  *) die "still working... check in a minute: cat $RESULT ; log: $STATE/apply.log" ;;
esac
