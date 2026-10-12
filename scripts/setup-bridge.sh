#!/usr/bin/env bash
# setup-bridge.sh — put P8S OS VMs on your home network (run as root: sudo p8 lan-setup).
#
# Turns the host's Ethernet port into a bridge "br0" so VMs can plug into it and
# get an IP from your router, like any other computer in the house.
#   * The host keeps its address: br0 copies the Ethernet port's MAC.
#   * If the network doesn't work within 60 s, the old settings are restored.
#   * Wi-Fi cannot be bridged (a Wi-Fi card only talks for its own MAC), so this
#     needs an Ethernet cable.
# Options (env): P8S_YES=1 skip the question, BR=br0 bridge name.
set -euo pipefail
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

BR=${BR:-br0}
IFACES=/etc/network/interfaces
CONF=/etc/p8s/p8sd.json
STATE=/var/lib/p8s/bridge
RESULT=$STATE/result

say() { printf '\033[1m==> %s\033[0m\n' "$*"; }
ok()  { printf '  \033[32m✔\033[0m %s\n' "$*"; }
die() { printf '  \033[31m✘ %s\033[0m\n' "$*" >&2; exit 1; }

[[ $EUID == 0 ]] || die "run as root: sudo p8 lan-setup"
mkdir -p "$STATE"

use_bridge() { # tell p8sd about the bridge
  if [[ -f $CONF ]]; then
    local tmp; tmp=$(mktemp)
    jq --arg br "$BR" '.lan_bridge = $br' "$CONF" >"$tmp" && cat "$tmp" >"$CONF" && rm -f "$tmp"
    systemctl restart p8sd
    ok "p8sd: new VMs can now choose network \"lan\""
  fi
}

finish_msg() {
  local ip; ip=$(ip -4 -o addr show dev "$BR" | awk '{print $4}' | cut -d/ -f1 | head -1)
  echo
  printf '\033[32mDone.\033[0m Host address: %s (dashboard: https://%s:8006)\n' "$ip" "$ip"
  echo "Make a VM on your home network:"
  echo "    p8 create web2 2 2048 20 --lan"
  echo "or tick \"Home network\" in the dashboard's New VM dialog."
}

# Already done?
if [[ -d /sys/class/net/$BR/bridge && $(ip -4 -o addr show dev "$BR" 2>/dev/null) == *inet* ]]; then
  ok "bridge $BR already exists"
  use_bridge
  finish_msg
  exit 0
fi

say "Checking the network"
IF=$(ip -4 route show default | awk '{for (i=1;i<NF;i++) if ($i=="dev") {print $(i+1); exit}}')
[[ -n $IF ]] || die "no default route — is the host online?"
is_wifi() { [[ -d /sys/class/net/$1/wireless || -e /sys/class/net/$1/phy80211 ]]; }
MODE=move   # move: the host's own connection moves onto br0
if is_wifi "$IF"; then
  # Host is online over Wi-Fi. Wi-Fi itself can't be bridged, but a plugged-in
  # Ethernet port can: br0 goes on the cable, the host stays on Wi-Fi too.
  WIFI=$IF; IF=""
  for d in /sys/class/net/*; do
    n=${d##*/}
    [[ -e $d/device ]] && ! is_wifi "$n" && [[ $(cat "$d/carrier" 2>/dev/null) == 1 ]] && { IF=$n; break; }
  done
  if [[ -z $IF ]]; then
    cat >&2 <<EOF
  ✘ The host is online through Wi-Fi ($WIFI) and no Ethernet cable is plugged in.
    Wi-Fi cannot be bridged (a Wi-Fi card only sends traffic for its own address).
    Plug an Ethernet cable from the laptop into the router, then run again:
        sudo p8 lan-setup
    VMs keep working on the private network ("nat") meanwhile.
EOF
    exit 1
  fi
  grep -Eq "^iface[[:space:]]+$IF[[:space:]]" "$IFACES" &&
    die "$IF has its own settings in $IFACES — remove them or make the cable the main connection first"
  MODE=add
  ok "host stays on Wi-Fi ($WIFI); VMs use the cable ($IF)"
else
  [[ -e /sys/class/net/$IF/device ]] || die "$IF is not a physical network port (virtual? already bridged?)"
  ok "Ethernet port: $IF"
  if systemctl is-active -q NetworkManager 2>/dev/null && ! grep -Eq "^iface[[:space:]]+$IF[[:space:]]" "$IFACES"; then
    die "$IF is managed by NetworkManager. This script supports the standard Debian setup (/etc/network/interfaces) only."
  fi
  grep -Eq "^iface[[:space:]]+$IF[[:space:]]+inet[[:space:]]" "$IFACES" ||
    die "$IF is not configured in $IFACES — don't know how to move its settings to $BR"
fi
if ! command -v brctl >/dev/null || [[ ! -x /lib/bridge-utils/ifupdown.sh && ! -e /etc/network/if-pre-up.d/bridge ]]; then
  apt-get install -y bridge-utils >/dev/null
fi
ok "uses $IFACES"

MAC=$(cat "/sys/class/net/$IF/address")
OLDIP=$(ip -4 -o addr show dev "$IF" | awk '{print $4}' | head -1)
GW=$(ip -4 route show default | awk '{print $3; exit}')

# New interfaces file: every stanza for $IF moves to $BR; everything else stays.
NEW=$STATE/interfaces.new
if [[ $MODE == add ]]; then
  { cat "$IFACES"; printf '\n# P8S OS: %s is the bridge VMs plug into; %s (cable) is its uplink.\n' "$BR" "$IF"
    printf 'iface %s inet manual\n\nauto %s\niface %s inet dhcp\n' "$IF" "$BR" "$BR"
    printf '    bridge_ports %s\n    bridge_stp off\n    bridge_fd 0\n    bridge_hw %s\n' "$IF" "$MAC"
  } >"$NEW"
else
awk -v IF="$IF" -v BR="$BR" -v MAC="$MAC" '
  /^[[:space:]]*(auto|allow-hotplug)[[:space:]]/ {   # drop IF from auto lines
    out = $1; n = 0
    for (i = 2; i <= NF; i++) if ($i != IF) { out = out " " $i; n++ }
    if (n) print out
    inblk = 0; next
  }
  $1 == "iface" && $2 == IF { nb++; fam[nb] = $3; meth[nb] = $4; inblk = 1; next }
  inblk && /^[[:space:]]+[^[:space:]]/ { opts[nb] = opts[nb] $0 "\n"; next }
  { inblk = 0; print }
  END {
    printf "\n# P8S OS: %s is the bridge VMs plug into; %s is its uplink.\n", BR, IF
    printf "iface %s inet manual\n\nauto %s\n", IF, BR
    for (b = 1; b <= nb; b++) {
      printf "iface %s %s %s\n%s", BR, fam[b], meth[b], opts[b]
      if (fam[b] == "inet" && !done) {
        printf "    bridge_ports %s\n    bridge_stp off\n    bridge_fd 0\n    bridge_hw %s\n", IF, MAC; done = 1
      }
      print ""
    }
  }' "$IFACES" | cat -s >"$NEW"
fi
grep -q "bridge_ports $IF" "$NEW" || die "could not build the new network config (left at $NEW)"

echo
if [[ $MODE == add ]]; then
  echo "  Host: stays on Wi-Fi $WIFI"
  echo "  New:  $BR on the cable $IF — VMs get addresses from your router through it"
  echo "  If $BR gets no address within 60 s, nothing is changed."
else
  echo "  Now:  $IF  $OLDIP  (gateway $GW)"
  echo "  After: $BR  (bridge on $IF, same MAC, so normally the same address)"
  echo "  The network drops for a few seconds. If it isn't back within 60 s, the old"
  echo "  settings come back by themselves."
fi
if [[ ${P8S_YES:-0} != 1 ]]; then
  read -r -p "  Continue? [y/N] " a
  [[ $a == [yY]* ]] || { echo "cancelled"; exit 0; }
fi

# Apply in a separate systemd unit so an SSH disconnect can't stop it halfway.
BACKUP=$STATE/interfaces.before-bridge
cp -a "$IFACES" "$BACKUP"
cat >"$STATE/apply.sh" <<EOF
#!/bin/bash
export PATH=$PATH
exec >"$STATE/apply.log" 2>&1
set -x
online() {
  for _ in \$(seq 60); do
    if [[ $MODE == add ]]; then ip -4 -o addr show dev $BR | grep -q inet && return 0; sleep 1; continue; fi
    ip -4 -o addr show dev $BR | grep -q inet && ip -4 route show default | grep -q "dev $BR" &&
      ping -c1 -W1 "\$(ip -4 route show default | awk '{print \$3; exit}')" >/dev/null && return 0
    sleep 1
  done
  return 1
}
if [[ $MODE == move ]]; then
  ifdown --force $IF || true
  ip -4 addr flush dev $IF || true
fi
cp "$NEW" "$IFACES"
ifup $IF || true
if ifup $BR && online; then
  echo ok >"$RESULT"
else
  ifdown --force $BR || true
  ip link del $BR 2>/dev/null || true
  [[ $MODE == move ]] && { ifdown --force $IF || true; }  # forget the "manual" state
  cp "$BACKUP" "$IFACES"
  if [[ $MODE == move ]]; then ifup $IF || true; fi
  echo failed >"$RESULT"
fi
EOF
chmod 0700 "$STATE/apply.sh"
rm -f "$RESULT"
say "Switching $IF to bridge $BR"
systemd-run --quiet --unit "p8s-bridge-$(date +%s)" --collect /bin/bash "$STATE/apply.sh"

for _ in $(seq 120); do [[ -f $RESULT ]] && break; sleep 1; done
case $(cat "$RESULT" 2>/dev/null) in
  ok)
    ok "bridge $BR is up"
    # libvirt re-attaches running NAT VMs on its own; nothing else to restart.
    use_bridge
    finish_msg ;;
  failed)
    die "the network did not come back on $BR, so the old settings were restored. Details: $STATE/apply.log" ;;
  *)
    die "still working... check in a minute: cat $RESULT ; log: $STATE/apply.log" ;;
esac
