#!/usr/bin/env bash
# P8S OS network check for VMs and containers (Server → Network → Check / Repair).
#   netcheck.sh            check and explain what's wrong
#   netcheck.sh --repair   also fix what can be fixed safely (running VMs keep running)
#   netcheck.sh --repair --quiet   boot/after-firewall-reload mode: fix silently
#
# Guests reach the internet through the host: NAT guests (virbr0, 192.168.122.x)
# need IP forwarding, libvirt's NAT rules and its dnsmasq (DHCP + DNS); home
# network guests (br0) need forwarding not to be blocked. The usual ways this
# breaks: a firewall reload (`nft flush ruleset` in /etc/nftables.conf, often
# after Debian updates) wipes libvirt's rules, Docker sets the FORWARD policy to
# DROP, or br_netfilter sends bridged traffic through that DROP policy.
set -uo pipefail
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:$PATH
[[ $EUID -eq 0 ]] || { echo "run as root: sudo $0 $*" >&2; exit 1; }

repair=false quiet=false
for a in "$@"; do
  case $a in
    --repair) repair=true ;;
    --quiet) quiet=true ;;
    *) echo "usage: netcheck.sh [--repair] [--quiet]" >&2; exit 2 ;;
  esac
done

bad=0 fixed=0
say() { $quiet || echo "$@"; }
ok() { say "  ✔ $*"; }
no() { say "  ✘ $*"; bad=$((bad + 1)); }
fix() { say "    → fixed: $*"; fixed=$((fixed + 1)); }
hint() { say "    $*"; }

# dnsq SERVER NAME: one plain DNS A query (no EDNS, like most guests send);
# prints "rcode answers", nothing on timeout. Pure bash: no dig needed.
dnsq() {
  local q='\x12\x34\x01\x00\x00\x01\x00\x00\x00\x00\x00\x00' l
  for l in ${2//./ }; do q+=$(printf '\\x%02x' ${#l})$l; done
  q+='\x00\x00\x01\x00\x01'
  timeout 4 bash -c "exec 3<>/dev/udp/$1/53; printf '$q' >&3; dd bs=512 count=1 status=none <&3" 2>/dev/null |
    od -An -tu1 -N8 | awk 'NF>=8 {print $4 % 16, $7 * 256 + $8}'
}

libvirt_unit() { # the daemon that owns virtual networks
  if systemctl is-active -q virtnetworkd 2>/dev/null; then echo virtnetworkd; else echo libvirtd; fi
}
reload_libvirt_net() { # libvirt re-adds its firewall rules and restarts dnsmasq; VMs keep running
  systemctl restart "$(libvirt_unit)" && sleep 2
}

say "==> Host internet"
if ip -4 route show default | grep -q .; then
  ok "default route: $(ip -4 route show default | head -1 | awk '{print $3" via "$5}')"
else
  no "the server has no default route (no internet itself) — check its cable / Wi-Fi first"
fi
if timeout 5 bash -c 'exec 3<>/dev/tcp/8.8.8.8/443' 2>/dev/null || timeout 5 bash -c 'exec 3<>/dev/tcp/1.1.1.1/443' 2>/dev/null; then
  ok "the server reaches the internet"
else
  no "the server itself can't reach the internet (TCP 443) — guests can't either until it can"
fi
if getent ahosts deb.debian.org >/dev/null 2>&1; then ok "the server's DNS works"; else no "the server's DNS doesn't answer (getent deb.debian.org)"; fi

say "==> Forwarding"
if [[ $(sysctl -n net.ipv4.ip_forward 2>/dev/null) == 1 ]]; then
  ok "IPv4 forwarding is on"
else
  no "IPv4 forwarding is off — the server doesn't pass guest traffic on"
  if $repair; then
    sysctl -q -w net.ipv4.ip_forward=1
    printf '# p8s: guests reach the internet through the server\nnet.ipv4.ip_forward = 1\n' >/etc/sysctl.d/91-p8s-forward.conf
    fix "net.ipv4.ip_forward = 1 (kept after reboot)"
  fi
fi

say "==> NAT network (virbr0, used by NAT VMs and containers)"
if ! command -v virsh >/dev/null; then
  no "libvirt isn't installed"
else
  if [[ $(virsh net-info default 2>/dev/null | awk '/^Active:/{print $2}') == yes ]]; then
    ok "libvirt network 'default' is active"
  else
    no "libvirt network 'default' isn't running"
    if $repair && virsh net-start default >/dev/null 2>&1; then virsh net-autostart default >/dev/null 2>&1; fix "started it (and set it to start at boot)"; fi
  fi
  net=$(virsh net-dumpxml default 2>/dev/null | sed -n "s/.*<ip address='\([0-9.]*\)' netmask='\([0-9.]*\)'.*/\1/p" | head -1)
  sub=${net%.*}. # e.g. 192.168.122.
  rules=$( (nft list ruleset 2>/dev/null; iptables-save -t nat 2>/dev/null) )
  if grep -qi masquerade <<<"$rules" && grep -F -q "$sub" <<<"$(grep -i -B3 -A3 masquerade <<<"$rules")"; then
    ok "NAT rule for ${sub}0/24 is in place"
    natok=true
  else
    no "the NAT rule for ${sub}0/24 is missing — guests get no internet"
    hint "usually a firewall reload removed libvirt's rules (e.g. 'flush ruleset' in /etc/nftables.conf)"
    natok=false
  fi
  if pgrep -f "dnsmasq.*/default.conf" >/dev/null; then
    ok "DHCP/DNS for NAT guests (dnsmasq) is running"
    dnsok=true
  else
    no "dnsmasq for the NAT network isn't running — guests get no address and no DNS"
    dnsok=false
  fi
  gw=${net:-192.168.122.1}
  if $dnsok; then
    r=$(dnsq "$gw" www.msftconnecttest.com)
    if [[ ${r%% *} == 0 && ${r##* } -gt 0 ]]; then
      ok "guests' DNS ($gw) answers (www.msftconnecttest.com, what Windows checks)"
    else
      up=$(awk '/^nameserver/{print $2; exit}' /etc/resolv.conf)
      ru=$( [[ -n $up && $up != 127.* ]] && dnsq "$up" www.msftconnecttest.com )
      no "guests' DNS ($gw) gives no answer${r:+ (rcode ${r%% *})} — guests show 'no internet' even when connected"
      if [[ -n $up && $up != 127.* ]]; then
        if [[ ${ru%% *} == 0 && ${ru##* } -gt 0 ]]; then hint "the server's DNS $up answers, so dnsmasq is the problem (Repair restarts it)"
        else hint "the server's DNS $up doesn't answer plain queries either — set another DNS on your router or in /etc/resolv.conf"; fi
      fi
      dnsok=false
    fi
  fi
  if $repair && { ! $natok || ! $dnsok; }; then
    reload_libvirt_net
    rules=$( (nft list ruleset 2>/dev/null; iptables-save -t nat 2>/dev/null) )
    grep -qi masquerade <<<"$rules" && pgrep -f "dnsmasq.*/default.conf" >/dev/null && fix "restarted $(libvirt_unit): NAT rules and dnsmasq are back (VMs kept running)"
  fi
fi

say "==> Firewall"
fwd_drop=false
if nft list chain ip filter FORWARD 2>/dev/null | grep -q "policy drop"; then fwd_drop=true; fi
if iptables -S FORWARD 2>/dev/null | head -1 | grep -q -- "-P FORWARD DROP"; then fwd_drop=true; fi
bridges=()
for b in virbr0 br0; do [[ -d /sys/class/net/$b ]] && bridges+=("$b"); done
if $fwd_drop; then
  missing=()
  for b in "${bridges[@]}"; do
    iptables -C FORWARD -i "$b" -j ACCEPT 2>/dev/null && iptables -C FORWARD -o "$b" -j ACCEPT 2>/dev/null || missing+=("$b")
  done
  if ((${#missing[@]})); then
    no "the FORWARD policy is DROP (Docker or another firewall set it) and guests on ${missing[*]} aren't allowed"
    if $repair; then
      for b in "${missing[@]}"; do
        iptables -I FORWARD 1 -o "$b" -j ACCEPT
        iptables -I FORWARD 1 -i "$b" -j ACCEPT
      done
      fix "allowed traffic of ${missing[*]} through FORWARD (re-applied at every boot by p8s-net.service)"
    fi
  else
    ok "FORWARD policy is DROP, but P8S guests are allowed"
  fi
else
  ok "nothing blocks forwarded traffic"
fi
if [[ $(cat /proc/sys/net/bridge/bridge-nf-call-iptables 2>/dev/null) == 1 ]] && $fwd_drop; then
  no "br_netfilter sends home-network (br0) traffic through the DROP policy"
  if $repair; then
    sysctl -q -w net.bridge.bridge-nf-call-iptables=0 net.bridge.bridge-nf-call-ip6tables=0 net.bridge.bridge-nf-call-arptables=0 2>/dev/null
    fix "bridge-nf-call-iptables = 0"
  fi
fi
if ! nft list table bridge p8s_fw >/dev/null 2>&1 && systemctl is-active -q p8sd; then
  no "the P8S guest firewall table is gone (someone flushed the firewall)"
  if $repair; then
    systemctl restart p8sd && fix "restarted p8sd, which puts its firewall rules back"
  fi
fi
if systemctl is-enabled -q nftables 2>/dev/null && grep -q "flush ruleset" /etc/nftables.conf 2>/dev/null; then
  hint "note: nftables.service is enabled and /etc/nftables.conf starts with 'flush ruleset';"
  hint "every reload wipes libvirt's rules. P8S re-runs this repair after each reload (nftables drop-in)."
fi

say "==> Guests"
if command -v virsh >/dev/null; then
  leases=$(virsh net-dhcp-leases default 2>/dev/null | awk 'NR>2 && $5 {print $5" "$6}')
  if [[ -n $leases ]]; then ok "NAT DHCP leases: $(echo "$leases" | awk '{print $2"="$1}' | paste -sd' ')"; else say "  · no DHCP leases on the NAT network yet"; fi
fi

say ""
if ((bad == 0)); then
  say "All good: VMs and containers should reach the internet. If one still can't, check its own network settings (DHCP on, gateway ${sub:-192.168.122.}1)."
elif $repair; then
  say "$bad problem(s) found, $fixed fixed. Run the check again; guests may need to renew their address (reboot them or reconnect the network)."
else
  say "$bad problem(s) found. Click Repair (or run: sudo bash /usr/local/share/p8s/netcheck.sh --repair)."
fi
$repair && exit 0
((bad == 0))
