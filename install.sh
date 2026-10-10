#!/usr/bin/env bash
# P8S OS — one-command install on a fresh Debian 13 machine or VM.
#
#   sudo bash install.sh /dev/sdb     # use an empty disk for VM storage (it gets WIPED)
#   sudo bash install.sh              # no spare disk: 64 GB test pool in a file
#
# Safe to run again: finished steps are skipped, your token and config are kept.
# Options (env): BRIDGE=vmbr0 to put VMs on your LAN instead of libvirt NAT (default virbr0)
#                NEW_TOKEN=1 to issue a fresh API token
set -euo pipefail

REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
DEV=${1:-}
BRIDGE=${BRIDGE:-virbr0}
POOL=${POOL:-tank}
CONF=/etc/p8s/p8sd.json
USER_NAME=${SUDO_USER:-root}
USER_HOME=$(getent passwd "$USER_NAME" | cut -d: -f6)
TOKEN_FILE=$USER_HOME/.p8s-token
LOG=/var/log/p8s-install.log

step=0
say()  { step=$((step + 1)); printf '\n\033[1;34m[%d/7] %s\033[0m\n' "$step" "$*"; }
ok()   { printf '      \033[32m✔\033[0m %s\n' "$*"; }
die()  { printf '\n\033[1;31m✘ %s\033[0m\n  Full log: %s\n' "$*" "$LOG" >&2; exit 1; }
trap 'die "install failed at step $step (line $LINENO)"' ERR

[[ $EUID -eq 0 ]] || { echo "Run with sudo:  sudo bash install.sh ${DEV:-/dev/sdX}" >&2; exit 1; }
exec > >(tee -a "$LOG") 2>&1
echo "P8S OS installer — $(date)"

# ---------------------------------------------------------------------------
say "Checking this machine"
. /etc/os-release
[[ ${VERSION_CODENAME:-} == trixie ]] || die "This needs Debian 13 (trixie). Found: ${PRETTY_NAME:-unknown}"
ok "Debian 13"
grep -Eq 'vmx|svm' /proc/cpuinfo || die "No hardware virtualization (VT-x/AMD-V) visible.
  In VMware: tick 'Virtualize Intel VT-x/EPT'. In VirtualBox: 'Enable Nested VT-x/AMD-V'.
  On Windows, Hyper-V/Memory integrity must be off. See USER.md step 1."
ok "CPU virtualization available"
if [[ -n $DEV ]]; then
  [[ -b $DEV ]] || die "$DEV is not a disk. Run 'lsblk' to find the empty one (e.g. /dev/sdb, /dev/nvme0n2)."
  findmnt -rno SOURCE | grep -q "^$DEV" && die "$DEV is in use (mounted). Pick the EMPTY disk."
  ok "storage disk $DEV"
else
  ok "no disk given: will use a 64 GB file pool (testing only)"
fi
free_gb=$(df -BG --output=avail / | tail -1 | tr -dc 0-9)
(( free_gb >= 8 )) || die "Only ${free_gb} GB free on /. Need at least 8 GB."
ok "${free_gb} GB free on /"

# ---------------------------------------------------------------------------
say "Installing KVM, libvirt and ZFS (ZFS builds a kernel module: 3-10 min)"
if zpool list -H "$POOL" >/dev/null 2>&1 && command -v virsh >/dev/null && command -v cloud-localds >/dev/null; then
  ok "already installed, pool '$POOL' exists"
else
  P8S_BRIDGE=$BRIDGE bash "$REPO/scripts/bootstrap-debian13.sh" $DEV
fi
[[ -e /dev/kvm ]] || die "/dev/kvm is missing — nested virtualization is not working. See USER.md 'Problems'."
ok "/dev/kvm present"
zpool list -H "$POOL" >/dev/null || die "ZFS pool '$POOL' was not created"
ok "ZFS pool '$POOL' online"

# ---------------------------------------------------------------------------
say "Setting up the network for VMs ($BRIDGE)"
if [[ $BRIDGE == virbr0 ]]; then
  virsh -q net-info default >/dev/null 2>&1 || die "libvirt 'default' network missing (package libvirt-daemon-config-network)"
  virsh -q net-autostart default >/dev/null
  [[ $(virsh -q net-info default | awk '/^Active/ {print $2}') == yes ]] || virsh -q net-start default >/dev/null
  ok "libvirt NAT network active: VMs get 192.168.122.x"
else
  ip link show "$BRIDGE" >/dev/null 2>&1 || die "Bridge $BRIDGE does not exist. Create it first (docs/host-setup.md) or omit BRIDGE."
  ok "bridge $BRIDGE exists"
fi

# ---------------------------------------------------------------------------
say "Importing the Debian 13 VM template (downloads ~400 MB once)"
POOL=$POOL bash "$REPO/scripts/make-template.sh" >/dev/null
SNAP=$POOL/p8s/templates/debian-13@base
bytes=$(zfs get -Hp -o value volsize "${SNAP%@*}")
MIN_GB=$(( (bytes + 1073741823) / 1073741824 ))
ok "template $SNAP (${MIN_GB} GB)"

# ---------------------------------------------------------------------------
say "Building p8sd"
GO=""
for g in /usr/local/go/bin/go "$(command -v go || true)"; do
  [[ -x $g ]] || continue
  minor=$("$g" env GOVERSION | sed -E 's/^go1\.([0-9]+).*/\1/')
  (( minor >= 24 )) && { GO=$g; break; }
done
if [[ -z $GO ]]; then
  apt-get install -y golang-go >/dev/null 2>&1 || true
  if command -v go >/dev/null && (( $(go env GOVERSION | sed -E 's/^go1\.([0-9]+).*/\1/') >= 24 )); then
    GO=$(command -v go)
  else
    echo "      Debian's Go is too old; installing the latest from go.dev"
    J=$(curl -fsSL 'https://go.dev/dl/?mode=json')
    F=$(jq -r '[.[0].files[] | select(.os=="linux" and .arch=="amd64" and .kind=="archive")][0].filename' <<<"$J")
    SUM=$(jq -r --arg f "$F" '.[0].files[] | select(.filename==$f) | .sha256' <<<"$J")
    curl -fsSL -o "/tmp/$F" "https://go.dev/dl/$F"
    echo "$SUM  /tmp/$F" | sha256sum -c --quiet
    rm -rf /usr/local/go && tar -C /usr/local -xzf "/tmp/$F" && rm "/tmp/$F"
    GO=/usr/local/go/bin/go
  fi
fi
ok "Go $("$GO" env GOVERSION)"
export PATH=$(dirname "$GO"):$PATH GOCACHE=/root/.cache/go-build GOFLAGS=-buildvcs=false
(cd "$REPO" && go test ./... >/dev/null) || die "unit tests failed: cd $REPO && go test ./..."
ok "unit tests pass"
(cd "$REPO" && make -s install >/dev/null)
chown -R "$USER_NAME": "$REPO/bin" 2>/dev/null || true
install -m 0755 "$REPO/scripts/p8" /usr/local/bin/p8
install -d -m 0755 /usr/local/share/p8s
install -m 0755 "$REPO/scripts/smoke-test.sh" /usr/local/share/p8s/smoke-test.sh
ok "installed /usr/local/bin/p8sd and the 'p8' command"

# ---------------------------------------------------------------------------
say "Writing config and API token"
have_cfg=0
if [[ ${NEW_TOKEN:-0} != 1 && -s $TOKEN_FILE && -f $CONF ]] &&
   jq -e '.tokens[0].sha256 | test("^[0-9a-f]{64}$")' "$CONF" >/dev/null 2>&1; then
  have_cfg=1
  ok "keeping existing config and token ($TOKEN_FILE)"
fi
if (( ! have_cfg )); then
  [[ -f $CONF ]] && cp "$CONF" "$CONF.bak.$(date +%s)"
  T=$(p8sd token -name admin -json)
  mem_mib=$(awk '/MemTotal/ {print int($2/1024)}' /proc/meminfo)
  jq -n --argjson t "$T" --arg br "$BRIDGE" --arg snap "$SNAP" --argjson gb "$MIN_GB" \
        --argjson cpus "$(nproc)" --argjson mem "$mem_mib" --arg ds "$POOL/p8s/vms" '{
    listen: "127.0.0.1:8006", data_dir: "/var/lib/p8s", libvirt_uri: "qemu:///system",
    vm_dataset: $ds, bridge: $br,
    max_vcpus: $cpus, max_memory_mib: $mem, max_disk_gb: 200,
    tokens: [{name: $t.name, sha256: $t.sha256}],
    templates: [{name: "debian-13", snapshot: $snap, min_disk_gb: $gb, os_variant: "debian13"}]
  }' > "$CONF"
  chmod 0600 "$CONF"
  ( umask 077; jq -r .token <<<"$T" > "$TOKEN_FILE" )
  chown "$USER_NAME": "$TOKEN_FILE"
  ok "config $CONF"
  ok "token saved to $TOKEN_FILE (only $USER_NAME can read it)"
fi
if [[ ! -f $USER_HOME/.ssh/id_ed25519.pub ]]; then
  sudo -u "$USER_NAME" mkdir -p "$USER_HOME/.ssh"
  sudo -u "$USER_NAME" ssh-keygen -q -t ed25519 -N "" -f "$USER_HOME/.ssh/id_ed25519"
  ok "created SSH key $USER_HOME/.ssh/id_ed25519 (VMs will trust it)"
fi

# ---------------------------------------------------------------------------
say "Starting p8sd"
since=$(date "+%Y-%m-%d %H:%M:%S")
systemctl daemon-reload
systemctl enable p8sd >/dev/null 2>&1
systemctl restart p8sd
for _ in $(seq 20); do curl -fs http://127.0.0.1:8006/api/v1/health >/dev/null && break; sleep 0.5; done
curl -fs http://127.0.0.1:8006/api/v1/health >/dev/null || { journalctl -u p8sd -n 20 --no-pager; die "p8sd did not start"; }
journalctl -u p8sd --since "$since" --no-pager | grep -q "snapshot missing" && die "p8sd can't see the template snapshot"
ok "p8sd running: $(curl -fs http://127.0.0.1:8006/api/v1/health)"

trap - ERR
cat <<DONE

$(printf '\033[1;32m')✔ P8S OS is installed.$(printf '\033[0m')

  Next, as $USER_NAME (no sudo):

    p8 test              run the full automatic test (2-5 min)
    p8 create web1       make a VM
    p8 ssh web1          log into it
    p8 help              all commands

  Install log: $LOG
DONE
