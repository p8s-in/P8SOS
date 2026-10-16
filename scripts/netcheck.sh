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

say "==> DNS for guests"
# Home routers often answer some queries wrongly (NXDOMAIN for IPv6/AAAA or
# EDNS queries). dnsmasq then caches "this name doesn't exist" for every
# record type and guests randomly get "DNS address could not be found".
# P8S gives the NAT network's dnsmasq upstream servers that answer correctly
# (public DNS when reachable, else the router) and turns off negative caching.
answers() { local r; r=$(dnsq "$1" "$2"); [[ ${r%% *} == 0 && ${r##* } -gt 0 ]]; }
good_dns=()
for c in 8.8.8.8 1.1.1.1 9.9.9.9; do
  answers "$c" www.google.com && answers "$c" www.msftconnecttest.com && good_dns+=("$c")
  ((${#good_dns[@]} >= 2)) && break
done
if ((${#good_dns[@]} == 0)); then # public DNS blocked: use the router(s) the server uses
  for c in $(awk '/^nameserver/{print $2}' /etc/resolv.conf /run/systemd/resolve/resolv.conf 2>/dev/null | grep -v '^127\.' | sort -u); do
    answers "$c" www.google.com && good_dns+=("$c")
  done
fi
if command -v virsh >/dev/null && virsh net-info default >/dev/null 2>&1; then
  cur=$(virsh net-dumpxml --inactive default 2>/dev/null | sed -n "s/.*<forwarder addr=['\"]\([0-9.]*\)['\"].*/\1/p" | paste -sd' ')
  want=${good_dns[*]}
  negoff=$(virsh net-dumpxml --inactive default 2>/dev/null | grep -c "no-negcache")
  if ((${#good_dns[@]} == 0)); then
    no "no DNS server answered from this server (public DNS and the router) — guests can't resolve names"
  elif [[ -n $cur && $negoff -gt 0 ]] && curok=true && for c in $cur; do answers "$c" www.google.com || curok=false; done && $curok; then
    ok "guests' DNS goes to $cur (no caching of failed lookups)"
  else
    no "guests' DNS goes ${cur:+to $cur }through the server's default resolver and caches failed lookups — names fail now and then"
    if $repair; then
      x=$(virsh net-dumpxml --inactive default)
      x=$(sed -e '/<dns[ >]/,/<\/dns>/d' -e '/<dns\/>/d' -e '/<dnsmasq:options>/,/<\/dnsmasq:options>/d' <<<"$x")
      fw=""; for d in "${good_dns[@]}"; do fw+="    <forwarder addr='$d'/>\n"; done
      ins="  <dns>\n$fw  </dns>\n  <dnsmasq:options>\n    <dnsmasq:option value='no-negcache'/>\n    <dnsmasq:option value='dns-forward-max=500'/>\n  </dnsmasq:options>"
      x=$(sed "s#</network>#$ins\n</network>#" <<<"$x")
      grep -q "xmlns:dnsmasq" <<<"$x" || x=$(sed "0,/<network/s#<network#<network xmlns:dnsmasq='http://libvirt.org/schemas/network/dnsmasq/1.0'#" <<<"$x")
      tmp=$(mktemp)
      printf '%s\n' "$x" >"$tmp"
      if virsh net-define "$tmp" >/dev/null; then
        # restart the network; guests' cables (tap/veth) are plugged back in at once
        ports=$(ls /sys/class/net/virbr0/brif 2>/dev/null | grep -v -e '^virbr0' || true)
        virsh net-destroy default >/dev/null 2>&1
        virsh net-start default >/dev/null
        for p in $ports; do ip link set dev "$p" master virbr0 2>/dev/null; ip link set dev "$p" up 2>/dev/null; done
        systemctl is-active -q p8sd && systemctl restart p8sd # its firewall rules name the guests' ports
        fix "guests' DNS now goes to $want without caching failures (guests stayed connected)"
      else
        hint "libvirt didn't accept the new network settings; left unchanged"
      fi
      rm -f "$tmp"
    fi
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

say "==> From outside"
# Information only (not counted as a problem): can people on the internet
# reach this server? No public IP is needed for the server or its guests to
# go OUT; reaching IN needs one of the remote-access ways below.
info() { say "  · $*"; }
pub=""
for u in https://api.ipify.org https://ifconfig.me/ip https://icanhazip.com; do
  pub=$(curl -4 -fsS --max-time 5 "$u" 2>/dev/null | tr -d '[:space:]')
  [[ $pub =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] && break || pub=""
done
lan=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for (i = 1; i < NF; i++) if ($i == "src") print $(i+1)}')
if [[ -n $pub ]]; then
  if [[ $pub == "$lan" ]]; then
    info "this server has a public address itself: $pub (reachable from the internet if its firewall allows)"
  else
    info "the internet sees this server as $pub (your router's address); the server itself is ${lan:-on a private network}"
  fi
else
  info "couldn't find out the public address (no answer from the IP lookup services)"
fi
# CGNAT: the provider shares one address between many customers (100.64.0.0/10 on the way out)
cg=false
if command -v tracepath >/dev/null; then
  hops=$(timeout 12 tracepath -n -m 4 1.1.1.1 2>/dev/null | awk '{print $2}')
elif command -v traceroute >/dev/null; then
  hops=$(timeout 12 traceroute -n -m 4 -w 1 1.1.1.1 2>/dev/null | awk 'NR > 1 {print $2}')
else
  hops=""
fi
for h in $hops; do
  [[ $h =~ ^100\.(6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])\. ]] && cg=true
done
if $cg; then
  info "your internet provider shares one public address between many customers (CGNAT): opening ports on the router won't work"
elif [[ -n $pub && $pub != "$lan" ]]; then
  info "to reach it from outside without the ways below, forward a port on the router to $lan (not possible with CGNAT)"
fi
ways=()
if command -v tailscale >/dev/null && tailscale status >/dev/null 2>&1; then
  ways+=("Tailscale ($(tailscale ip -4 2>/dev/null | head -1))")
fi
if ip link show p8s-vpn >/dev/null 2>&1; then
  if $cg; then info "the built-in VPN is on, but with CGNAT nothing can connect in to it: use Tailscale instead"
  else ways+=("the built-in VPN (UDP 51830 forwarded on the router)"); fi
fi
pgrep -x cloudflared >/dev/null && ways+=("Cloudflare Tunnel")
if ((${#ways[@]})); then
  info "reachable from anywhere through: $(IFS=,; echo "${ways[*]}" | sed 's/,/, /g')"
else
  info "nothing is set up to reach it from outside: Server → Network → Remote access (Tailscale for you and your team, works with CGNAT; Cloudflare Tunnel for websites and the dashboard)"
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
