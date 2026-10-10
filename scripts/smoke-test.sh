#!/usr/bin/env bash
# End-to-end lifecycle test against a running p8sd.
# Run it ON the p8sd host: it also checks libvirt and ZFS directly (via sudo).
#
#   export P8S_TOKEN=p8s_...           # the token, not the hash
#   scripts/smoke-test.sh
#
# Optional env: P8S_URL, TEMPLATE, PUBKEY, DATASET, SEED_DIR, KEEP=1 (don't delete the VM),
#               SLOW=1 (3x timeouts, for nested VirtualBox/VMware), IP_TIMEOUT, SSH_TIMEOUT, STOP_TIMEOUT
set -uo pipefail

URL=${P8S_URL:-http://127.0.0.1:8006/api/v1}
TOK=${P8S_TOKEN:?set P8S_TOKEN to your API token}
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
  out=$(curl "${args[@]}" "$URL$2")
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
trap cleanup EXIT

for bin in curl jq sudo; do command -v $bin >/dev/null || { echo "missing $bin" >&2; exit 2; }; done
sudo -v || exit 2
echo "p8sd smoke test: url=$URL vm=$NAME template=$TEMPLATE timeouts(ip/ssh/stop)=${IP_TIMEOUT}/${SSH_TIMEOUT}/${STOP_TIMEOUT}s"

step "1. health and auth"
CODE=$(curl -s -o /dev/null -w '%{http_code}' "$URL/health"); expect "GET /health (no token)" 200
CODE=$(curl -s -o /dev/null -w '%{http_code}' "$URL/vms"); expect "GET /vms without token" 401
CODE=$(curl -s -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer wrong' "$URL/vms"); expect "GET /vms with wrong token" 401
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

step "3. create $NAME"
keys='[]'
if [[ -f $PUBKEY ]]; then keys=$(jq -Rc '[.]' <"$PUBKEY"); else echo "  (no $PUBKEY; SSH check will be skipped)"; fi
t0=$(date +%s)
api POST /vms '{"name":"'"$NAME"'","template":"'"$TEMPLATE"'","vcpus":2,"memory_mib":1024,"disk_gb":10,"ssh_keys":'"$keys"'}'
expect "create VM" 201
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

step "5. guest boots and cloud-init runs"
ip=""
for ((i = 0; i < IP_TIMEOUT; i += 5)); do
  ip=$( { sudo virsh -q domifaddr "$DOM" 2>/dev/null; sudo virsh -q domifaddr "$DOM" --source arp 2>/dev/null; } \
        | awk '$3 == "ipv4" {sub(/\/.*/, "", $4); print $4; exit}')
  [[ -n $ip ]] && break
  sleep 5
done
[[ -n $ip ]] && ok "guest got IP $ip" || bad "no IP after ${IP_TIMEOUT}s (try: sudo virsh console $DOM)"
if [[ -n $ip && $keys != '[]' ]]; then
  ssh_opts=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 -o BatchMode=yes -o LogLevel=ERROR)
  out=""
  for ((i = 0; i < SSH_TIMEOUT; i += 5)); do
    out=$(ssh "${ssh_opts[@]}" "debian@$ip" 'hostname; df -BG --output=size / | tail -1; cloud-init status --wait >/dev/null 2>&1; systemctl is-active qemu-guest-agent' 2>/dev/null) && break
    sleep 5
  done
  if [[ -n $out ]]; then
    ok "SSH as debian@$ip works"
    mapfile -t g <<<"$out"
    [[ ${g[0]} == "$NAME" ]] && ok "hostname is $NAME" || bad "hostname is '${g[0]}'"
    sz=${g[1]//[^0-9]/}; [[ $sz -ge 9 ]] && ok "root fs grew to ${sz}G" || bad "root fs is ${sz}G, expected ~10G"
    [[ ${g[2]} == active ]] && ok "qemu-guest-agent active" || bad "qemu-guest-agent: ${g[2]:-unknown}"
  else
    bad "SSH to debian@$ip failed for ${SSH_TIMEOUT}s"
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

echo
echo "== result: $pass passed, $fail failed"
[[ $fail -eq 0 ]]
