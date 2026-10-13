#!/usr/bin/env bash
# libvirt-migrate.sh on TUNNEL_IP | off
# Lets the other servers of the cluster move running VMs here (live moves):
# libvirt listens on this server's cluster-tunnel address only (TCP 16509,
# no password: only cluster servers can reach the tunnel), and QEMU's
# migration ports (49152-49215) are open on the tunnel only. p8sd runs this
# whenever the tunnel comes up or goes away.
set -uo pipefail
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
MARK=/var/lib/p8s/libvirt-migrate
IFACE=p8s-wg
NET=10.250.0.0/24

if systemctl is-active -q virtqemud.service 2>/dev/null || systemctl is-active -q virtqemud.socket 2>/dev/null; then
  SOCK=virtproxyd-tcp.socket; SVC=virtproxyd.service; CONF=/etc/libvirt/virtproxyd.conf   # modular daemons
else
  SOCK=libvirtd-tcp.socket; SVC=libvirtd.service; CONF=/etc/libvirt/libvirtd.conf
fi
DROP=/etc/systemd/system/$SOCK.d/p8s.conf

nft_on() {
  nft -f - <<NFT
table inet p8s_migrate
delete table inet p8s_migrate
table inet p8s_migrate {
	chain input {
		type filter hook input priority -4; policy accept;
		iifname "$IFACE" ip saddr $NET tcp dport { 16509, 49152-49215 } accept
		tcp dport { 16509, 49152-49215 } counter drop comment "live moves: cluster tunnel only"
	}
}
NFT
}

case ${1:-} in
on)
  ip=${2:-}
  [[ $ip =~ ^10\.250\.0\.[0-9]{1,3}$ ]] || { echo "usage: $0 on 10.250.0.N"; exit 2; }
  nft_on || echo "note: firewall for live moves not set"
  if [[ $(cat "$MARK" 2>/dev/null) == "$ip" ]] && systemctl is-active -q "$SOCK"; then exit 0; fi
  [[ -f $CONF ]] || touch "$CONF"
  if grep -Eq '^[#[:space:]]*auth_tcp[[:space:]]*=' "$CONF"; then
    sed -i -E 's/^[#[:space:]]*auth_tcp[[:space:]]*=.*/auth_tcp = "none"/' "$CONF"
  else
    echo 'auth_tcp = "none"' >>"$CONF"
  fi
  mkdir -p "$(dirname "$DROP")"
  printf '[Socket]\nListenStream=\nListenStream=%s:16509\nFreeBind=yes\n' "$ip" >"$DROP"
  systemctl daemon-reload
  # the daemon must restart to take its sockets from systemd; VMs keep running
  systemctl stop "$SVC" 2>/dev/null
  systemctl enable "$SOCK" >/dev/null 2>&1
  systemctl restart "$SOCK" || { echo "could not start $SOCK"; exit 1; }
  systemctl start "$SVC"
  mkdir -p "$(dirname "$MARK")"; echo "$ip" >"$MARK"
  echo "live moves: libvirt listens on $ip:16509 ($SOCK)"
  ;;
off)
  [[ -f $MARK ]] || exit 0
  nft delete table inet p8s_migrate 2>/dev/null
  systemctl disable --now "$SOCK" >/dev/null 2>&1
  rm -f "$DROP" "$MARK"
  systemctl daemon-reload
  echo "live moves: off"
  ;;
*) echo "usage: $0 on TUNNEL_IP | off"; exit 2 ;;
esac
