#!/usr/bin/env bash
# End-to-end lifecycle test against a running p8sd.
# Run it ON the p8sd host: it also checks libvirt and ZFS directly (via sudo).
#
#   p8 test                            # after install.sh (token read from ~/.p8s-token)
#   P8S_TOKEN=p8s_... scripts/smoke-test.sh
#
# Optional env: P8S_URL, TEMPLATE, PUBKEY, DATASET, SEED_DIR, KEEP=1 (don't delete the VM),
#               SLOW=1 (3x timeouts, for nested VirtualBox/VMware), IP_TIMEOUT, SSH_TIMEOUT, STOP_TIMEOUT
set -uo pipefail

URL=${P8S_URL:-https://127.0.0.1:8006/api/v1}  # self-signed cert: curl -k
TOK=${P8S_TOKEN:-$(cat "$HOME/.p8s-token" 2>/dev/null || true)}
[[ -n $TOK ]] || { echo "No API token: run install.sh, or export P8S_TOKEN=p8s_..." >&2; exit 2; }
TEMPLATE=${TEMPLATE:-debian-13}
PUBKEY=${PUBKEY:-$HOME/.ssh/id_ed25519.pub}
DATASET=${DATASET:-tank/p8s/vms}
SEED_DIR=${SEED_DIR:-/var/lib/p8s/seeds}
NAME=smoke$RANDOM
X=1; [[ ${SLOW:-0} == 1 ]] && X=3
IP_TIMEOUT=${IP_TIMEOUT:-$((120 * X))}
SSH_TIMEOUT=${SSH_TIMEOUT:-$((180 * X))}
STOP_TIMEOUT=${STOP_TIMEOUT:-$((120 * X))}
DOM=p8s-$NAME

pass=0 fail=0
ok()   { echo "  PASS  $*"; pass=$((pass + 1)); }
bad()  { echo "  FAIL  $*"; fail=$((fail + 1)); }
step() { echo; echo "== $*"; }

# api METHOD PATH [JSON]  -> sets CODE and BODY
api() {
  local args=(-s -X "$1" -H "Authorization: Bearer $TOK" -w '\n%{http_code}')
  [[ -n ${3:-} ]] && args+=(-H 'Content-Type: application/json' -d "$3")
  local out
  out=$(curl -k "${args[@]}" "$URL$2")
  CODE=${out##*$'\n'}
  BODY=${out%$'\n'*}
}
expect() { [[ $CODE == "$2" ]] && ok "$1 -> $CODE" || bad "$1 -> got $CODE, want $2: $BODY"; }
state()  { api GET "/vms/$NAME"; jq -r '.state // empty' <<<"$BODY"; }
wait_state() { # want timeout_s
  local i
  for ((i = 0; i < $2; i += 3)); do [[ $(state) == "$1" ]] && return 0; sleep 3; done
  return 1
}

cleanup() {
  [[ ${KEEP:-0} == 1 ]] && return
  api GET "/vms/$NAME"
  [[ $CODE == 200 ]] && { echo; echo "(cleanup) deleting $NAME"; api DELETE "/vms/$NAME"; }
}
LAN_NAME=${NAME}lan
cleanup_lan() {
  [[ ${KEEP:-0} == 1 ]] && return
  api GET "/vms/$LAN_NAME"
  [[ $CODE == 200 ]] && { echo "(cleanup) deleting $LAN_NAME"; api DELETE "/vms/$LAN_NAME"; }
}
trap 'cleanup; cleanup_lan' EXIT

for bin in curl jq sudo; do command -v $bin >/dev/null || { echo "missing $bin" >&2; exit 2; }; done
sudo -v || exit 2
echo "p8sd smoke test: url=$URL vm=$NAME template=$TEMPLATE timeouts(ip/ssh/stop)=${IP_TIMEOUT}/${SSH_TIMEOUT}/${STOP_TIMEOUT}s"

step "1. health and auth"
CODE=$(curl -ks -o /dev/null -w '%{http_code}' "$URL/health"); expect "GET /health (no token)" 200
CODE=$(curl -ks -o /dev/null -w '%{http_code}' "$URL/vms"); expect "GET /vms without token" 401
CODE=$(curl -ks -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer wrong' "$URL/vms"); expect "GET /vms with wrong token" 401
api GET /templates; expect "GET /templates" 200
jq -e --arg t "$TEMPLATE" '.templates[] | select(.name == $t)' <<<"$BODY" >/dev/null \
  && ok "template $TEMPLATE is configured" || { bad "template $TEMPLATE not in config: $BODY"; exit 1; }

step "2. validation (nothing should be created)"
api POST /vms '{"name":"Bad_Name","template":"'"$TEMPLATE"'"}';           expect "invalid name" 400
api POST /vms '{"name":"x'"$NAME"'","template":"no-such-template"}';     expect "unknown template" 400
api POST /vms '{"name":"x'"$NAME"'","template":"'"$TEMPLATE"'","vcpus":999}'; expect "vcpus over limit" 400
api POST /vms '{"name":"x'"$NAME"'","template":"'"$TEMPLATE"'","colour":"red"}'; expect "unknown field" 400
api POST /vms '{not json';                                               expect "malformed JSON" 400
api GET /vms/does-not-exist;                                             expect "get missing VM" 404

api GET /templates
LOGIN=$(jq -r --arg t "$TEMPLATE" '.templates[] | select(.name == $t) | .default_user // "debian"' <<<"$BODY")
[[ -n $LOGIN ]] && ok "template $TEMPLATE is installed (login user: $LOGIN)" || { bad "template $TEMPLATE is not installed"; LOGIN=debian; }

step "3. create $NAME"
keys='[]'
if [[ -f $PUBKEY ]]; then keys=$(jq -Rc '[.]' <"$PUBKEY"); else echo "  (no $PUBKEY; SSH check will be skipped)"; fi
t0=$(date +%s)
api POST /vms '{"name":"x'"$NAME"'","template":"'"$TEMPLATE"'","root_password":"short"}'; expect "too-short root password" 400
api POST /vms '{"name":"'"$NAME"'","template":"'"$TEMPLATE"'","vcpus":2,"memory_mib":1024,"disk_gb":10,"ssh_keys":'"$keys"',"root_password":"Smoke-root-pw-1"}'
expect "create VM (with root password)" 201
[[ $BODY != *Smoke-root-pw-1* ]] && ok "password not echoed by the API" || bad "API response contains the password"
echo "        took $(( $(date +%s) - t0 ))s"
[[ $(jq -r .state <<<"$BODY") == running ]] && ok "state is running" || bad "state: $BODY"
api POST /vms '{"name":"'"$NAME"'","template":"'"$TEMPLATE"'"}'; expect "duplicate name" 409
api GET /vms; jq -e --arg n "$NAME" '.vms[] | select(.name == $n)' <<<"$BODY" >/dev/null \
  && ok "listed in GET /vms" || bad "not listed: $BODY"

step "4. host-side checks"
[[ $(sudo virsh -q domstate "$DOM" 2>/dev/null) == running ]] && ok "libvirt: $DOM running" || bad "libvirt: $DOM not running"
sudo zfs list -H "$DATASET/$DOM-disk0" >/dev/null 2>&1 && ok "zfs: $DATASET/$DOM-disk0 exists" || bad "zfs: disk missing"
origin=$(sudo zfs get -H -o value origin "$DATASET/$DOM-disk0" 2>/dev/null)
[[ $origin == *"@"* ]] && ok "zfs: disk is a clone of $origin" || bad "zfs: disk has no origin snapshot"
vs=$(sudo zfs get -H -o value volsize "$DATASET/$DOM-disk0" 2>/dev/null)
[[ $vs == 10G ]] && ok "zfs: volsize 10G" || bad "zfs: volsize is '$vs', want 10G"
sudo test -f "$SEED_DIR/$DOM-seed.iso" && ok "seed ISO exists" || bad "seed ISO missing in $SEED_DIR"
# (capture first: `cmd | grep -q` fails under pipefail when grep exits early)
info=$(sudo virsh dominfo "$DOM" 2>/dev/null)
grep -Eq '^Autostart:[[:space:]]+enable' <<<"$info" && ok "autostart on (starts with the host)" || bad "autostart not enabled"
api GET "/vms/$NAME"; [[ $(jq -r .autostart <<<"$BODY") == true ]] && ok "API reports autostart" || bad "API autostart: $BODY"

step "5. guest boots and cloud-init runs"
ip=""
for ((i = 0; i < IP_TIMEOUT; i += 5)); do
  api GET "/vms/$NAME"; ip=$(jq -r '.ip // empty' <<<"$BODY")   # the API's own IP lookup
  [[ -n $ip ]] && break
  sleep 5
done
[[ -n $ip ]] && ok "API reports guest IP $ip" || bad "no IP after ${IP_TIMEOUT}s (boot log: p8 bootlog $NAME)"
if [[ -n $ip && $keys != '[]' ]]; then
  ssh_opts=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 -o BatchMode=yes -o LogLevel=ERROR)
  out=""
  for ((i = 0; i < SSH_TIMEOUT; i += 5)); do
    out=$(ssh "${ssh_opts[@]}" "$LOGIN@$ip" 'hostname; df -BG --output=size / | tail -1; cloud-init status --wait >/dev/null 2>&1; systemctl is-active qemu-guest-agent' 2>/dev/null) && break
    sleep 5
  done
  if [[ -n $out ]]; then
    ok "SSH as $LOGIN@$ip works"
    mapfile -t g <<<"$out"
    [[ ${g[0]} == "$NAME" ]] && ok "hostname is $NAME" || bad "hostname is '${g[0]}'"
    sz=${g[1]//[^0-9]/}; [[ $sz -ge 9 ]] && ok "root fs grew to ${sz}G" || bad "root fs is ${sz}G, expected ~10G"
    [[ ${g[2]} == active ]] && ok "qemu-guest-agent active" || bad "qemu-guest-agent: ${g[2]:-unknown}"
    # VM guard: a VM must not reach this server or the home network, but must reach the internet.
    hostlan=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for (i=1;i<NF;i++) if ($i=="src") print $(i+1)}')
    langw=$(ip -4 route show default | awk '{print $3; exit}')
    g=$(ssh "${ssh_opts[@]}" "$LOGIN@$ip" "for t in ${ip%.*}.1:8006 ${ip%.*}.1:22 ${hostlan:-127.0.0.1}:8006 ${langw:-127.0.0.1}:80; do timeout 4 bash -c \"</dev/tcp/\${t%:*}/\${t#*:}\" 2>/dev/null && echo \"\$t OPEN\" || echo \"\$t closed\"; done; getent hosts deb.debian.org >/dev/null && echo DNS-OK" 2>/dev/null)
    if [[ -z $g ]]; then bad "VM guard: could not run the check inside the VM"
    else grep -q "OPEN" <<<"$g" && bad "VM guard: a VM can reach $(grep OPEN <<<"$g" | tr '\n' ' ')" || ok "VM guard: VM can't reach this server or the home network"; fi
    grep -q DNS-OK <<<"$g" && ok "VM still has DNS/internet" || bad "VM has no DNS: $g"
    r=$(ssh "${ssh_opts[@]}" "$LOGIN@$ip" 'sudo getent shadow root | cut -d: -f2 | cut -c1-3; sudo sshd -T 2>/dev/null | grep -i "^permitrootlogin"' 2>/dev/null)
    [[ $(head -1 <<<"$r") == '$6$' ]] && ok "root password set (SHA-512 hash in /etc/shadow)" || bad "root password not set: $r"
    [[ $(tail -1 <<<"$r") == "permitrootlogin yes" ]] && ok "SSH allows root password login" || bad "sshd: $(tail -1 <<<"$r")"

    # Reboot: the guest's boot_id changes once it has really restarted.
    b1=$(ssh "${ssh_opts[@]}" "$LOGIN@$ip" cat /proc/sys/kernel/random/boot_id 2>/dev/null)
    api POST "/vms/$NAME/reboot"; expect "reboot" 202
    b2=""
    for ((i = 0; i < SSH_TIMEOUT; i += 5)); do
      sleep 5
      b2=$(ssh "${ssh_opts[@]}" "$LOGIN@$ip" cat /proc/sys/kernel/random/boot_id 2>/dev/null) && [[ -n $b2 && $b2 != "$b1" ]] && break
    done
    [[ -n $b2 && $b2 != "$b1" ]] && ok "guest rebooted and is reachable again" || bad "guest did not come back after reboot"

    # Snapshot a running guest (filesystems frozen via the guest agent), then change the file.
    ssh "${ssh_opts[@]}" "$LOGIN@$ip" 'echo one | sudo tee /var/p8s-marker >/dev/null; sync' 2>/dev/null
    api POST "/vms/$NAME/snapshots" '{"name":"smoke1"}'; expect "snapshot running VM" 201
    ssh "${ssh_opts[@]}" "$LOGIN@$ip" 'echo two | sudo tee /var/p8s-marker >/dev/null; sync' 2>/dev/null
    MARKER=1
  else
    bad "SSH to $LOGIN@$ip failed for ${SSH_TIMEOUT}s"
  fi
fi

step "5a. usage charts"
for ((i = 0; i < 45; i += 5)); do  # samples every 10 s; the first rate needs two
  api GET "/vms/$NAME/metrics"; n=$(jq '.points | length' <<<"$BODY" 2>/dev/null); [[ ${n:-0} -ge 1 ]] && break; sleep 5
done
[[ ${n:-0} -ge 1 ]] && ok "usage samples collected ($n, last CPU $(jq -r '.points[-1].cpu' <<<"$BODY")%)" || bad "no usage samples yet: $BODY"
api GET /host/metrics
[[ $(jq '.points | length' <<<"$BODY") -ge 1 ]] && ok "host usage samples collected" || bad "no host samples: $BODY"

step "5b. browser console"
api POST "/vms/$NAME/console"; expect "console ticket" 201
path=$(jq -r '.path // empty' <<<"$BODY")
out=$(curl -ks --http1.1 -i -N --max-time 4 -H "Connection: Upgrade" -H "Upgrade: websocket" -H "Sec-WebSocket-Version: 13" \
  -H "Sec-WebSocket-Key: MDEyMzQ1Njc4OWFiY2RlZg==" -H "Sec-WebSocket-Protocol: binary" "${URL%/api/v1}$path" 2>/dev/null | tr -d '\0')
[[ $out == *"101 Switching Protocols"* ]] && ok "console WebSocket opens" || bad "console WebSocket: $(head -1 <<<"$out")"
[[ $out == *"RFB 003.008"* ]] && ok "VM screen answers (VNC)" || bad "no VNC greeting from the VM screen"
code=$(curl -ks --http1.1 -o /dev/null -w '%{http_code}' -H "Connection: Upgrade" -H "Upgrade: websocket" -H "Sec-WebSocket-Version: 13" \
  -H "Sec-WebSocket-Key: MDEyMzQ1Njc4OWFiY2RlZg==" "${URL%/api/v1}$path")
[[ $code == 401 ]] && ok "console ticket works only once" || bad "reused ticket -> $code"
if [[ ${MARKER:-0} == 1 ]]; then  # guest agent is up (SSH worked)
  api POST "/vms/$NAME/password" '{"user":"debian","password":"smoke-test-pw-1"}'; expect "set guest password" 204
fi

step "5c. VM firewall"
api GET "/vms/$NAME/firewall"; expect "firewall settings" 200
[[ $(jq -r .enabled <<<"$BODY") == false ]] && ok "firewall is off by default" || bad "firewall default: $BODY"
if [[ ${MARKER:-0} == 1 ]]; then
  mac=$(api GET "/vms/$NAME"; jq -r .mac <<<"$BODY")
  api PUT "/vms/$NAME/firewall" '{"enabled":true,"rules":[{"proto":"tcp","ports":"2222"}]}'; expect "firewall on, SSH not allowed" 200
  sudo nft list table bridge p8s_fw 2>/dev/null | grep -q "ether daddr $mac" && ok "nftables rules loaded for $mac" || bad "no p8s_fw rules for $mac: $BODY"
  ssh "${ssh_opts[@]}" -o ConnectTimeout=4 "$LOGIN@$ip" true 2>/dev/null && bad "firewall did not block SSH" || ok "firewall blocks SSH (port 22 not allowed)"
  api PUT "/vms/$NAME/firewall" '{"enabled":true,"rules":[{"proto":"tcp","ports":"22","comment":"SSH"}]}'; expect "allow SSH" 200
  g=""
  for _ in 1 2 3; do g=$(ssh "${ssh_opts[@]}" "$LOGIN@$ip" 'getent hosts deb.debian.org >/dev/null && timeout 8 bash -c "</dev/tcp/deb.debian.org/80" && echo NET-OK' 2>/dev/null) && break; sleep 2; done
  [[ -n $g ]] && ok "SSH allowed again by the rule" || bad "SSH still blocked with port 22 allowed"
  grep -q NET-OK <<<"$g" && ok "VM's own connections still work with the firewall on" || bad "VM lost internet with firewall on: $g"
  api PUT "/vms/$NAME/firewall" '{"enabled":false,"rules":[{"proto":"tcp","ports":"22"}]}'; expect "firewall off" 200
  sudo nft list table bridge p8s_fw 2>/dev/null | grep -q "$mac" && bad "rules left after switching off" || ok "rules removed when off"
else
  echo "  skipped the traffic checks (no SSH to the VM)"
fi

step "5d. containers (LXC)"
api GET /containers
if ! jq -e .enabled <<<"$BODY" >/dev/null 2>&1; then
  echo "  skipped (lxc not installed)"
else
  api GET /ct-templates; CTT=$(jq -r '.templates[0].name // empty' <<<"$BODY")
  if [[ -z $CTT ]]; then
    echo "  skipped (no container template: dashboard -> Server -> Container templates -> Add)"
  else
    CTN=${NAME}ct; api DELETE "/containers/$CTN"
    api POST /containers '{"name":"'"$CTN"'","template":"'"$CTT"'","vcpus":1,"memory_mib":256,"disk_gb":4,"root_password":"smoke-ct-pass-1"}'
    expect "create container from $CTT" 201
    [[ $(jq -r '.state // .container.state' <<<"$BODY") == running ]] && ok "container is running" || bad "container state: $BODY"
    sudo lxc-attach -n "$CTN" -- /bin/true && ok "lxc-attach works" || bad "lxc-attach failed"
    [[ $(sudo lxc-attach -n "$CTN" -- id -u 2>/dev/null) == 0 ]] && ok "root inside the container" || bad "not root inside"
    pid=$(sudo lxc-info -n "$CTN" -pH 2>/dev/null)
    [[ -n $pid && $(awk '/^Uid:/ {print $2}' /proc/$pid/status 2>/dev/null) == 100000 ]] && ok "unprivileged: container root is uid 100000 on the host" || bad "container root is not mapped to 100000"
    api PATCH "/containers/$CTN" '{"memory_mib":512}'; expect "resize while running" 200
    api POST "/containers/$CTN/snapshots" '{"name":"s1"}'; expect "snapshot" 201
    api POST "/containers/$CTN/stop" '{"force":true}'; expect "force off" 200
    api DELETE "/containers/$CTN"; expect "delete container" 204
  fi
fi

step "6. power operations"
api POST "/vms/$NAME/stop"; expect "ACPI stop" 202
wait_state stopped "$STOP_TIMEOUT" && ok "guest shut down cleanly" || bad "still $(state) after ${STOP_TIMEOUT}s of ACPI shutdown"
api POST "/vms/$NAME/stop"; expect "stop again (idempotent)" 202
api POST "/vms/$NAME/start"; expect "start" 200
wait_state running 30 && ok "running again" || bad "state $(state)"
api POST "/vms/$NAME/start"; expect "start again (idempotent)" 200
api POST "/vms/$NAME/stop" '{"force":true}'; expect "force stop" 202
[[ $(state) == stopped ]] && ok "stopped immediately" || bad "state $(state)"
api POST "/vms/$NAME/reboot"; expect "reboot a stopped VM is refused" 409

step "6b. snapshots and resize"
api POST "/vms/$NAME/snapshots" '{"name":"smoke2"}'; expect "snapshot stopped VM" 201
api POST "/vms/$NAME/snapshots" '{"name":"smoke2"}'; expect "duplicate snapshot" 409
sudo zfs list -H -t snapshot "$DATASET/$DOM-disk0@smoke2" >/dev/null 2>&1 && ok "zfs: snapshot exists" || bad "zfs: snapshot missing"
api GET "/vms/$NAME/snapshots"
[[ $(jq -r '[.snapshots[].name] | join(",")' <<<"$BODY") == *smoke2 ]] && ok "snapshots listed" || bad "snapshots: $BODY"
api PATCH "/vms/$NAME" '{"vcpus":2,"memory_mib":1536,"disk_gb":12}'; expect "resize" 200
api PATCH "/vms/$NAME" '{"disk_gb":5}'; expect "shrink disk is refused" 400
vs=$(sudo zfs get -H -o value volsize "$DATASET/$DOM-disk0" 2>/dev/null)
[[ $vs == 12G ]] && ok "zfs: disk grew to 12G" || bad "zfs: volsize $vs, want 12G"
info=$(sudo virsh dominfo "$DOM" 2>/dev/null)
grep -Eq '^CPU\(s\):[[:space:]]+2$' <<<"$info" && ok "libvirt: 2 CPUs" || bad "libvirt CPUs: $(grep CPU <<<"$info")"
grep -Eq '^Max memory:[[:space:]]+1572864 KiB' <<<"$info" && ok "libvirt: 1536 MB" || bad "libvirt memory: $(grep -i 'max memory' <<<"$info")"
grep -Eq '^Autostart:[[:space:]]+enable' <<<"$info" && ok "autostart kept after resize" || bad "autostart lost after resize"
if [[ ${MARKER:-0} == 1 ]]; then
  api POST "/vms/$NAME/snapshots/smoke1/rollback"; expect "rollback past a newer snapshot needs confirmation" 409
  api POST "/vms/$NAME/snapshots/smoke1/rollback" '{"destroy_newer":true}'; expect "rollback" 200
  vs=$(sudo zfs get -H -p -o value volsize "$DATASET/$DOM-disk0" 2>/dev/null)
  [[ $(jq -r .disk_gb <<<"$BODY") == $((vs >> 30)) ]] && ok "API disk size matches the disk after rollback ($((vs >> 30)) GB)" \
    || bad "disk_gb after rollback: $(jq -r .disk_gb <<<"$BODY"), disk is $((vs >> 30)) GB"
  api POST "/vms/$NAME/start"; expect "start after rollback" 200
  m=""
  for ((i = 0; i < SSH_TIMEOUT; i += 5)); do
    sleep 5; m=$(ssh "${ssh_opts[@]}" "$LOGIN@$ip" 'cat /var/p8s-marker' 2>/dev/null) && [[ -n $m ]] && break
  done
  [[ $m == one ]] && ok "rollback restored the guest's files (marker=one)" || bad "marker after rollback is '$m', want 'one'"
  api POST "/vms/$NAME/stop" '{"force":true}'
else
  api POST "/vms/$NAME/snapshots/smoke2/rollback"; expect "rollback (no SSH: disk only)" 200
fi
api DELETE "/vms/$NAME/snapshots/nope"; expect "delete missing snapshot" 404

step "6c. backup and restore"
# wait_job ID TIMEOUT -> sets JOB (final job JSON)
wait_job() {
  local i
  for ((i = 0; i < $2; i += 3)); do
    api GET "/jobs/$1"; JOB=$BODY
    [[ $(jq -r .state <<<"$JOB") != running ]] && return 0
    sleep 3
  done
  return 1
}
BACKUP_TIMEOUT=${BACKUP_TIMEOUT:-$((300 * X))}
api POST "/vms/$NAME/backups"; expect "start backup" 202
jid=$(jq -r .id <<<"$BODY")
api POST "/vms/$NAME/backups"; [[ $CODE == 409 || $CODE == 202 ]] && ok "second backup while one runs -> $CODE" || bad "second backup -> $CODE"
if wait_job "$jid" "$BACKUP_TIMEOUT" && [[ $(jq -r .state <<<"$JOB") == done ]]; then
  ok "backup finished: $(jq -r .message <<<"$JOB")"
else
  bad "backup job: $JOB"
fi
api GET "/vms/$NAME/backups"
bid=$(jq -r '.backups[0].id // empty' <<<"$BODY")
[[ -n $bid ]] && ok "backup $bid listed" || bad "no backup listed: $BODY"
left=$(sudo zfs list -H -t snapshot -o name -r "$DATASET/$DOM-disk0" 2>/dev/null | grep -c p8s-backup- || true)
[[ $left == 0 ]] && ok "zfs: no backup snapshot left behind" || bad "zfs: $left backup snapshot(s) left"
if [[ -n $bid ]]; then
  COPY=${NAME}r
  api POST "/backups/$NAME/$bid/restore" '{"name":"'"$COPY"'"}'; expect "restore as new VM" 202
  if wait_job "$(jq -r .id <<<"$BODY")" "$BACKUP_TIMEOUT" && [[ $(jq -r .state <<<"$JOB") == done ]]; then
    ok "restore finished: $(jq -r .message <<<"$JOB")"
  else
    bad "restore job: $JOB"
  fi
  api GET "/vms/$COPY"
  [[ $(jq -r .state <<<"$BODY") == running ]] && ok "restored copy $COPY is running" || bad "copy: $BODY"
  if [[ ${MARKER:-0} == 1 ]]; then
    cip=""
    for ((i = 0; i < IP_TIMEOUT; i += 5)); do api GET "/vms/$COPY"; cip=$(jq -r '.ip // empty' <<<"$BODY"); [[ -n $cip ]] && break; sleep 5; done
    m=""
    for ((i = 0; i < SSH_TIMEOUT && ${#cip} > 0; i += 5)); do
      m=$(ssh "${ssh_opts[@]}" "$LOGIN@$cip" 'cat /var/p8s-marker; hostname' 2>/dev/null) && [[ -n $m ]] && break; sleep 5
    done
    [[ $(head -1 <<<"$m") == one ]] && ok "copy has the backed-up files (marker=one)" || bad "copy marker: '$m'"
    [[ $(tail -1 <<<"$m") == "$COPY" ]] && ok "copy got its own hostname ($COPY)" || bad "copy hostname: '$(tail -1 <<<"$m")'"
  fi
  api DELETE "/vms/$COPY"; expect "delete restored copy" 204
  api DELETE "/backups/$NAME/$bid"; expect "delete backup" 204
  api DELETE "/backups/$NAME/$bid"; expect "delete backup again" 404
fi

step "6d. install from ISO"
ISOD=$(mktemp -d); ISOF=$ISOD/p8s-smoke.iso
# a tiny BOOTABLE CD (El Torito, no-emulation): P8S refuses ISOs that can't start a computer
mkdir -p "$ISOD/cd"; head -c 2048 /dev/zero > "$ISOD/cd/boot.img"; printf '\x55\xaa' | dd of="$ISOD/cd/boot.img" bs=1 seek=510 conv=notrunc 2>/dev/null
mkiso=""; for t in genisoimage mkisofs xorrisofs; do command -v $t >/dev/null && { mkiso=$t; break; }; done
[[ -z $mkiso ]] && command -v xorriso >/dev/null && mkiso="xorriso -as mkisofs"
if [[ -n $mkiso ]] && $mkiso -quiet -o "$ISOF" -V P8STEST -b boot.img -no-emul-boot -boot-load-size 4 "$ISOD/cd" 2>/dev/null; then
  code=$(curl -ks -o /dev/null -w '%{http_code}' -X PUT -H "Authorization: Bearer $TOK" -T "$ISOF" "$URL/isos/p8s-smoke.iso")
  [[ $code == 201 ]] && ok "ISO upload -> 201" || bad "ISO upload -> $code"
fi
if [[ ${code:-} == 201 ]]; then
  api GET /isos; [[ $(jq -r '[.isos[].name] | index("p8s-smoke.iso") != null' <<<"$BODY") == true ]] && ok "ISO listed" || bad "ISO not listed: $BODY"
  for os in linux windows; do
    ISONAME=${NAME}i${os:0:1}
    api POST /vms '{"name":"'"$ISONAME"'","iso":"p8s-smoke.iso","os_type":"'"$os"'","vcpus":1,"memory_mib":512,"disk_gb":2}'
    expect "create $os VM from ISO" 201
    [[ $(jq -r .state <<<"$BODY") == running && $(jq -r .iso <<<"$BODY") == p8s-smoke.iso ]] && ok "$os VM boots with the CD inserted" || bad "$os ISO VM: $BODY"
    api POST "/vms/$ISONAME/console"; expect "console for the ISO VM" 201
    api DELETE /isos/p8s-smoke.iso; expect "ISO in use can't be deleted" 409
    api POST "/vms/$ISONAME/stop" '{"force":true}'
    api POST "/vms/$ISONAME/eject"; expect "eject CD" 200
    [[ $(jq -r '.iso // ""' <<<"$BODY") == "" ]] && ok "CD gone after eject" || bad "eject: $BODY"
    api DELETE "/vms/$ISONAME"; expect "delete ISO VM" 204
  done
  api DELETE /isos/p8s-smoke.iso; expect "delete ISO" 204
else
  echo "  skipped the rest (no test ISO)"
fi
rm -rf "$(dirname "$ISOF")"

step "6e. user accounts (customers see only their own VMs)"
CU=smoke-cust; CPW=smoke-cust-pass-$RANDOM
api DELETE "/users/$CU"   # leftover from an interrupted run
api POST /users '{"name":"'"$CU"'","role":"customer","password":"'"$CPW"'","quota":{"vms":1,"vcpus":1,"memory_mib":1024,"disk_gb":20}}'
if [[ $CODE == 402 ]]; then
  ok "P8S OS Home: user limit reached -> 402 (skipping the customer tests; a subscription removes the limit)"
else
expect "add customer" 201
out=$(curl -ks -X POST -H 'Content-Type: application/json' -d '{"user":"'"$CU"'","password":"'"$CPW"'"}' "$URL/login")
CT=$(jq -r '.token // empty' <<<"$out")
[[ -n $CT ]] && ok "customer signs in" || bad "customer sign-in: $out"
capi() { local T=$TOK; TOK=$CT; api "$@"; TOK=$T; }
capi GET /vms
[[ $(jq -r --arg n "$NAME" '[.vms[].name] | index($n)' <<<"$BODY") == null ]] && ok "customer doesn't see the admin's VM" || bad "customer sees $NAME: $BODY"
capi GET "/vms/$NAME";               expect "admin's VM looks like no VM" 404
capi POST "/vms/$NAME/stop" '{"force":true}'; expect "customer can't stop the admin's VM" 404
capi GET /host;                      expect "host details are admin-only" 403
capi POST /vms '{"name":"'"${NAME}c"'","template":"debian-13","vcpus":2,"start":false}'; expect "over the plan (2 CPUs of 1)" 403
capi POST /vms '{"name":"'"${NAME}c"'","template":"debian-13","vcpus":1,"memory_mib":512,"start":false}'; expect "customer creates a VM in the plan" 201
[[ $(jq -r .owner <<<"$BODY") == "$CU" ]] && ok "VM belongs to the customer" || bad "owner: $BODY"
capi POST /vms '{"name":"'"${NAME}d"'","template":"debian-13","vcpus":1,"start":false}'; expect "second VM is over the plan" 403
api GET "/vms/${NAME}c"; [[ $(jq -r .owner <<<"$BODY") == "$CU" ]] && ok "admin sees the customer's VM" || bad "admin view: $BODY"
api PUT "/vms/$NAME/owner" '{"owner":"'"$CU"'"}'; expect "give the test VM to the customer" 200
capi GET "/vms/$NAME"; expect "customer now sees it" 200
api PUT "/vms/$NAME/owner" '{"owner":""}'; expect "take it back" 200
capi GET "/vms/$NAME"; expect "customer can't see it again" 404
capi DELETE "/vms/${NAME}c"; expect "customer deletes own VM" 204
api PATCH "/users/$CU" '{"disabled":true}'; expect "disable customer" 200
capi GET /vms; expect "disabled customer is signed out" 401
api DELETE "/users/$CU"; expect "delete customer" 204
api GET "/audit?limit=50"
jq -e --arg u "$CU" '[.entries[] | select(.user == $u)] | length > 0' <<<"$BODY" >/dev/null && ok "activity log records the customer's actions" || bad "audit: $BODY"
fi

step "6f. public IP pool (only with the home-network bridge)"
api GET /host
if jq -e '.networks | index("lan")' <<<"$BODY" >/dev/null 2>&1; then
  PN=smoketest; PV=${NAME}p
  api DELETE "/ippools/$PN"   # leftover
  # TEST-NET-1 addresses (never used on real networks): the VM gets no internet, which is fine here.
  api POST /ippools '{"name":"'"$PN"'","bridge":"br0","cidr":"192.0.2.0/30","gateway":"192.0.2.1"}'; expect "add IP pool" 201
  [[ $(jq -r .size <<<"$BODY") == 1 ]] && ok "pool has 1 usable address (/30 minus network, broadcast, gateway)" || bad "pool: $BODY"
  api POST /vms '{"name":"'"$PV"'","template":"debian-13","ip_pool":"'"$PN"'","memory_mib":512}'; expect "create VM with a public IP" 201
  [[ $(jq -r .network <<<"$BODY") == public && $(jq -r .public.ip <<<"$BODY") == 192.0.2.2 && $(jq -r .ip <<<"$BODY") == 192.0.2.2 ]] && ok "VM got 192.0.2.2" || bad "public VM: $BODY"
  pmac=$(jq -r .mac <<<"$BODY")
  api POST /vms '{"name":"'"${PV}x"'","template":"debian-13","ip_pool":"'"$PN"'","start":false}'; expect "pool full" 400
  port=$(sudo virsh -q domiflist "p8s-$PV" | awk '{print $1; exit}')
  [[ $(sudo virsh -q domiflist "p8s-$PV" | awk '{print $3; exit}') == br0 ]] && ok "VM is on br0" || bad "VM bridge: $(sudo virsh -q domiflist "p8s-$PV")"
  r=$(sudo nft list chain bridge p8s_fw spoof 2>/dev/null)
  grep -q "iifname \"$port\" ether saddr != $pmac" <<<"$r" && grep -q "ip saddr != 192.0.2.2" <<<"$r" && ok "address pinned to the VM's port $port (anti-spoofing)" || bad "no spoof rules for $port: $r"
  api DELETE "/ippools/$PN"; expect "pool in use can't be deleted" 409
  api DELETE "/vms/$PV"; expect "delete public VM" 204
  api GET /ippools; [[ $(jq -r --arg n "$PN" '.pools[] | select(.name==$n) | .free' <<<"$BODY") == 1 ]] && ok "address returned to the pool" || bad "pool after delete: $BODY"
  sudo nft list chain bridge p8s_fw spoof 2>/dev/null | grep -q "192.0.2.2" && bad "spoof rules left" || ok "spoof rules removed"
  api DELETE "/ippools/$PN"; expect "delete pool" 204
else
  echo "  skipped (no home-network bridge; run: sudo p8 lan-setup)"
fi

step "6g. P8S Cloud (plans, billing, support, API tokens)"
api GET /billing; expect "billing view" 200
api PUT /plans/smoketest '{"name":"Smoke","vcpus":1,"memory_mib":512,"disk_gb":4,"price_monthly":73000,"hidden":true}'
if [[ $CODE == 402 ]]; then
  ok "priced plans need a Hosting subscription -> 402"
else
  expect "add a plan" 200
  [[ $(jq -r .hourly <<<"$BODY") == 100 ]] && ok "hourly price = monthly / 730 (rounded up)" || bad "plan: $BODY"
  api DELETE /plans/smoketest; expect "delete the plan" 204
fi
api POST /tickets '{"subject":"smoke test","text":"please ignore"}'; expect "open a support request" 201
tid=$(jq -r .id <<<"$BODY")
api POST "/tickets/$tid/close"; expect "close it" 200
api GET /cloud/settings; expect "cloud settings (admin)" 200
api GET /cloud/outbox; expect "sent-email log" 200

step "6h. subscription"
api GET /license; expect "subscription status (admin)" 200
[[ $(jq -r .server_id <<<"$BODY") =~ ^P8S(-[0-9A-F]{4}){4}$ ]] && ok "server ID $(jq -r .server_id <<<"$BODY"), tier $(jq -r .tier <<<"$BODY")" || bad "license: $BODY"
api PUT /license '{"key":"P8S1-not.a-key"}'; expect "a fake key is refused" 400
api GET /updates; expect "update status (admin)" 200
api GET /node; expect "Server page (admin)" 200
[[ $(jq -r .storage.health <<<"$BODY") == ONLINE ]] && ok "storage pool ONLINE" || bad "storage: $(jq -c .storage <<<"$BODY")"
jq -e '.catalog | length > 0' <<<"$BODY" >/dev/null && ok "OS template list ($(jq '.catalog | length' <<<"$BODY") images)" || bad "template catalog empty"

step "7. delete"
if [[ ${KEEP:-0} == 1 ]]; then
  echo "  KEEP=1: leaving $NAME in place"
else
  api DELETE "/vms/$NAME"; expect "delete" 204
  api GET "/vms/$NAME";    expect "get after delete" 404
  sudo virsh -q domstate "$DOM" >/dev/null 2>&1 && bad "libvirt domain still defined" || ok "libvirt domain gone"
  sudo zfs list -H "$DATASET/$DOM-disk0" >/dev/null 2>&1 && bad "zfs disk still exists" || ok "zfs disk destroyed"
  sudo test -f "$SEED_DIR/$DOM-seed.iso" && bad "seed ISO still exists" || ok "seed ISO removed"
  sudo zfs list -H "${origin%@*}" >/dev/null 2>&1 && ok "template survived" || bad "template dataset missing!"
fi

step "8. home network (only after: sudo p8 lan-setup)"
api GET /host
if jq -e '.networks | index("lan")' <<<"$BODY" >/dev/null 2>&1; then
  api POST /vms '{"name":"'"$LAN_NAME"'","template":"'"$TEMPLATE"'","network":"lan","ssh_keys":'"$keys"'}'
  expect "create VM on the home network" 201
  [[ $(jq -r .network <<<"$BODY") == lan ]] && ok "API reports network lan" || bad "network: $BODY"
  br=$(sudo virsh domiflist "p8s-$LAN_NAME" 2>/dev/null | awk 'NR>2 && $3 {print $3; exit}')
  [[ $br == br0 ]] && ok "attached to br0" || bad "attached to '$br', want br0"
  lip=""
  for ((i = 0; i < IP_TIMEOUT; i += 5)); do
    api GET "/vms/$LAN_NAME"; lip=$(jq -r '.ip // empty' <<<"$BODY"); [[ -n $lip ]] && break; sleep 5
  done
  if [[ -n $lip ]]; then
    [[ $lip != 192.168.122.* ]] && ok "got home-network IP $lip from the router" || bad "IP $lip is from the NAT network"
    out=""
    for ((i = 0; i < SSH_TIMEOUT; i += 5)); do
      out=$(ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 -o BatchMode=yes \
        -o LogLevel=ERROR "$LOGIN@$lip" 'ip -4 route show default' 2>/dev/null) && break
      sleep 5
    done
    [[ -n $out ]] && ok "SSH to $lip works (gateway: $(awk '{print $3; exit}' <<<"$out"))" || bad "SSH to $lip failed"
  else
    bad "LAN VM got no IP in ${IP_TIMEOUT}s (does your router hand out DHCP? p8 bootlog $LAN_NAME)"
  fi
  if [[ ${KEEP:-0} != 1 ]]; then api DELETE "/vms/$LAN_NAME"; expect "delete LAN VM" 204; fi
  # VLAN: a VM on tagged VLAN 4000 (no DHCP there, so only the wiring is checked)
  if [[ $(jq -r '.vlans // false' <<<"$(curl -ks -H "Authorization: Bearer $TOK" "$URL/host")") == true ]]; then
    VN=${NAME}v
    api POST /vms '{"name":"'"$VN"'","template":"'"$TEMPLATE"'","network":"lan","vlan":4000,"vcpus":1,"memory_mib":512}'
    expect "create VM on VLAN 4000" 201
    [[ $(jq -r .vlan <<<"$BODY") == 4000 ]] && ok "API reports VLAN 4000" || bad "vlan: $BODY"
    br=$(sudo virsh domiflist "p8s-$VN" 2>/dev/null | awk 'NR>2 && $3 {print $3; exit}')
    [[ $br == br0v4000 ]] && ok "attached to br0v4000" || bad "attached to '$br', want br0v4000"
    ip -d link show p8sv4000 2>/dev/null | grep -q "vlan protocol 802.1Q id 4000" && ok "p8sv4000 tags VLAN 4000 on the uplink" || bad "p8sv4000 missing"
    sudo grep -q " 4000 br0v4000" /var/lib/p8s/vlans.list 2>/dev/null && ok "VLAN kept for the next boot" || bad "vlans.list has no VLAN 4000"
    api DELETE "/vms/$VN"; expect "delete VLAN VM" 204
  fi
else
  echo "  skipped: home network not set up on this host"
fi

echo
echo "== result: $pass passed, $fail failed"
[[ $fail -eq 0 ]]
