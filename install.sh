#!/usr/bin/env bash
# P8S OS — one-command install on a fresh Debian 13 machine or VM.
#
#   sudo bash install.sh /dev/sdb     # use an empty disk for VM storage (it gets WIPED)
#   sudo bash install.sh              # no spare disk: 64 GB test pool in a file
#   sudo bash install.sh --encrypt /dev/sdb   # VM storage encrypted at rest (new pools only)
#
# Safe to run again: finished steps are skipped, your token and config are kept.
# Options (env): BRIDGE=vmbr0 to put VMs on your LAN instead of libvirt NAT (default virbr0)
#                NEW_TOKEN=1 to issue a fresh API token
set -euo pipefail
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:$PATH  # admin tools even under plain "su"

REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
if [[ ${1:-} == --encrypt ]]; then export P8S_ENCRYPT=1; shift; fi
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
  mounted=$(findmnt -rno SOURCE)   # capture first: `cmd | grep -q` misfires under pipefail
  grep -q "^$DEV" <<<"$mounted" && die "$DEV is in use (mounted). Pick the EMPTY disk."
  ok "storage disk $DEV"
elif zpool list -H "$POOL" >/dev/null 2>&1; then
  ok "VM storage: ZFS pool '$POOL' ($(zpool list -H -o size "$POOL"))"
else
  ok "no disk given: will use a 64 GB file pool (testing only)"
fi
free_gb=$(df -BG --output=avail / | tail -1 | tr -dc 0-9)
(( free_gb >= 8 )) || die "Only ${free_gb} GB free on /. Need at least 8 GB."
ok "${free_gb} GB free on /"

# ---------------------------------------------------------------------------
say "Installing KVM, libvirt and ZFS (ZFS builds a kernel module: 3-10 min)"
if zpool list -H "$POOL" >/dev/null 2>&1 && zfs list -H "$POOL/p8s/vms" >/dev/null 2>&1 && command -v virsh >/dev/null && command -v cloud-localds >/dev/null; then
  ok "already installed, pool '$POOL' exists"
  command -v zstd >/dev/null || apt-get install -y zstd >/dev/null 2>&1 || true  # backups compress with it (newer versions)
  command -v ssh >/dev/null || apt-get install -y openssh-client >/dev/null 2>&1 || true  # off-site copies to another server
  command -v qemu-img >/dev/null || apt-get install -y qemu-utils >/dev/null 2>&1 || true  # importing disk images (VMware, Hyper-V, ...)
  command -v lzop >/dev/null || apt-get install -y lzop >/dev/null 2>&1 || true  # importing .vma.lzo Proxmox backups
  command -v smartctl >/dev/null || apt-get install -y smartmontools >/dev/null 2>&1 || true  # disk health alerts
  command -v ping >/dev/null || apt-get install -y iputils-ping >/dev/null 2>&1 || true  # high availability: is the router reachable
  command -v tracepath >/dev/null || apt-get install -y iputils-tracepath >/dev/null 2>&1 || true  # network check: CGNAT on the way out
  command -v lspci >/dev/null || apt-get install -y pciutils usbutils >/dev/null 2>&1 || true  # PCI/USB passthrough: device names
  [[ -x /usr/sbin/dnsmasq ]] || apt-get install -y dnsmasq-base >/dev/null 2>&1 || true  # virtual networks: DHCP and DNS on their gateway
  command -v qemu-storage-daemon >/dev/null || apt-get install -y qemu-utils >/dev/null 2>&1 || apt-get install -y qemu-system-common >/dev/null 2>&1 || true  # P8S Mirror: serves mirror disks
else
  P8S_BRIDGE=$BRIDGE bash "$REPO/scripts/bootstrap-debian13.sh" $DEV
fi
# cloudflared: used only if public access is switched on (dashboard → Settings).
if ! command -v cloudflared >/dev/null; then
  { mkdir -p --mode=0755 /usr/share/keyrings &&
    curl -fsSL https://pkg.cloudflare.com/cloudflare-main.gpg -o /usr/share/keyrings/cloudflare-main.gpg &&
    echo 'deb [signed-by=/usr/share/keyrings/cloudflare-main.gpg] https://pkg.cloudflare.com/cloudflared any main' > /etc/apt/sources.list.d/cloudflared.list &&
    apt-get update -qq && apt-get install -y -qq cloudflared >/dev/null; } 2>/dev/null &&
    ok "cloudflared installed (for public access)" || echo "  note: cloudflared not installed; public access won't work until it is"
fi
# Windows 11 VMs: software TPM 2.0 chip (swtpm) and UEFI firmware with Secure Boot (ovmf).
if ! command -v swtpm >/dev/null || [[ ! -e /usr/share/OVMF/OVMF_CODE_4M.ms.fd && ! -e /usr/share/OVMF/OVMF_CODE_4M.secboot.fd ]]; then
  apt-get install -y -qq swtpm swtpm-tools ovmf >/dev/null 2>&1 && ok "TPM and UEFI firmware installed (Windows 11 VMs)" || echo "  note: swtpm not installed; Windows 11 VMs won't be available"
fi
# Network storage: NFS shares (NAS) and iSCSI disks (SAN).
if ! command -v mount.nfs >/dev/null || ! command -v iscsiadm >/dev/null; then
  apt-get install -y -qq nfs-common open-iscsi >/dev/null 2>&1 && ok "NFS and iSCSI tools installed (network storage)" || echo "  note: nfs-common/open-iscsi not installed; network storage won't be available"
fi
systemctl enable --now iscsid.socket >/dev/null 2>&1 || true
# Clusters: servers talk over a WireGuard tunnel (kernel module is built in; this is the wg tool).
if ! command -v wg >/dev/null; then
  apt-get install -y -qq wireguard-tools >/dev/null 2>&1 && ok "WireGuard tools installed (clusters)" || echo "  note: wireguard-tools not installed; clusters won't be available"
fi
# Containers (LXC, unprivileged): ids 100000-165535 belong to containers' root.
if ! command -v lxc-start >/dev/null || ! command -v newuidmap >/dev/null; then
  apt-get install -y -qq lxc uidmap >/dev/null 2>&1 && ok "LXC installed (containers)" || echo "  note: LXC not installed; containers won't be available"
fi
if command -v lxc-start >/dev/null; then
  # VMs and containers share the same bridges; LXC's own lxcbr0 isn't used
  [[ -f /etc/default/lxc-net ]] && sed -i 's/^USE_LXC_BRIDGE=.*/USE_LXC_BRIDGE="false"/' /etc/default/lxc-net
  grep -q '^USE_LXC_BRIDGE=' /etc/default/lxc-net 2>/dev/null || echo 'USE_LXC_BRIDGE="false"' >> /etc/default/lxc-net
  systemctl disable --now lxc-net >/dev/null 2>&1 || true
  grep -q '^root:100000:65536$' /etc/subuid 2>/dev/null || echo 'root:100000:65536' >> /etc/subuid
  grep -q '^root:100000:65536$' /etc/subgid 2>/dev/null || echo 'root:100000:65536' >> /etc/subgid
  install -d -m 0755 /var/lib/p8s /var/lib/p8s/ct /var/lib/p8s/ct-templates /var/lib/lxc
  chmod o+x /var/lib/p8s   # containers' root (uid 100000) must reach its files
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
# The installer ISO carries p8sd and its tools pre-built for the exact source
# it ships (prebuilt/, with the source's fingerprint): then no Go, no build.
PRE=$REPO/prebuilt
if [[ -x $PRE/p8sd && -f $PRE/source.sha256 ]] && [[ $(bash "$REPO/scripts/srchash.sh" "$REPO") == "$(tr -d ' \r\n' < "$PRE/source.sha256")" ]]; then
  install -m 0755 "$PRE/p8sd" /usr/local/bin/p8sd
  install -m 0644 "$REPO/deploy/p8sd.service" /etc/systemd/system/p8sd.service
  install -d -m 0750 /etc/p8s /var/lib/p8s/seeds
  [[ -f /etc/p8s/p8sd.json ]] || install -m 0600 "$REPO/deploy/p8sd.example.json" /etc/p8s/p8sd.json
  if [[ -x $PRE/p8s-licensed ]]; then install -m 0755 "$PRE/p8s-licensed" /usr/local/bin/p8s-licensed; fi
  install -m 0644 "$REPO/deploy/p8s-licensed.service" /etc/systemd/system/p8s-licensed.service
  install -d -m 0755 /usr/local/share/p8s/witness
  for A in amd64 arm64; do
    if [[ -x $PRE/p8s-witness-$A ]]; then install -m 0755 "$PRE/p8s-witness-$A" "/usr/local/share/p8s/witness/p8s-witness-$A"; fi
  done
  ok "p8sd $(cat "$PRE/version" 2>/dev/null) pre-built on the installer ISO (no build needed)"
else
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
  if ! (cd "$REPO" && go test ./... >/tmp/p8s-tests.out 2>&1); then
    { echo "--- unit test failures ---"; grep -v -E '^ok|no test files' /tmp/p8s-tests.out; } | tee -a "$LOG" | tail -60
    die "unit tests failed (above; also in the log): cd $REPO && go test ./..."
  fi
  ok "unit tests pass"
  V=$(tr -d ' \r\n' < "$REPO/VERSION" 2>/dev/null || true)  # release bundles carry their version
  (cd "$REPO" && make -s install ${V:+VERSION=$V} >/dev/null)
  # licence server (selling P8S OS subscriptions); installed but off until you enable it
  (cd "$REPO" && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/p8s-licensed ./cmd/p8s-licensed) || die "building p8s-licensed failed"
  install -m 0755 "$REPO/bin/p8s-licensed" /usr/local/bin/p8s-licensed
  install -m 0644 "$REPO/deploy/p8s-licensed.service" /etc/systemd/system/p8s-licensed.service
  # cluster witness (tie-breaker for 2 servers): copy to a Pi/NAS/VPS and run sudo ./p8s-witness install
  install -d -m 0755 /usr/local/share/p8s/witness
  for A in amd64 arm64; do
    (cd "$REPO" && CGO_ENABLED=0 GOOS=linux GOARCH=$A go build -trimpath -ldflags "-s -w" -o "bin/p8s-witness-$A" ./cmd/p8s-witness) || die "building p8s-witness ($A) failed"
    install -m 0755 "$REPO/bin/p8s-witness-$A" "/usr/local/share/p8s/witness/p8s-witness-$A"
  done
fi
systemctl daemon-reload
chown -R "$USER_NAME": "$REPO/bin" 2>/dev/null || true
install -m 0755 "$REPO/scripts/p8" /usr/local/bin/p8
install -d -m 0755 /usr/local/share/p8s
# files to import (Proxmox backups, disk images): the admin user can scp them here
install -d -m 0775 -o "$USER_NAME" -g root /var/lib/p8s/import
install -m 0755 "$REPO/scripts/smoke-test.sh" /usr/local/share/p8s/smoke-test.sh
install -m 0755 "$REPO/scripts/setup-bridge.sh" /usr/local/share/p8s/setup-bridge.sh
install -m 0755 "$REPO/scripts/make-template.sh" /usr/local/share/p8s/make-template.sh
install -m 0755 "$REPO/scripts/storage.sh" /usr/local/share/p8s/storage.sh
install -m 0755 "$REPO/scripts/os-upgrade.sh" /usr/local/share/p8s/os-upgrade.sh
install -m 0755 "$REPO/scripts/make-ct-template.sh" /usr/local/share/p8s/make-ct-template.sh
install -m 0755 "$REPO/scripts/rename.sh" /usr/local/share/p8s/rename.sh
install -m 0755 "$REPO/scripts/tune.sh" /usr/local/share/p8s/tune.sh
install -m 0755 "$REPO/scripts/iommu.sh" /usr/local/share/p8s/iommu.sh
install -m 0755 "$REPO/scripts/acme.sh" /usr/local/share/p8s/acme.sh
install -m 0755 "$REPO/scripts/netcheck.sh" /usr/local/share/p8s/netcheck.sh
install -m 0755 "$REPO/scripts/tailscale.sh" /usr/local/share/p8s/tailscale.sh
install -m 0755 "$REPO/scripts/bootsec.sh" /usr/local/share/p8s/bootsec.sh
install -m 0755 "$REPO/scripts/vlans.sh" /usr/local/share/p8s/vlans.sh
install -m 0755 "$REPO/scripts/sdn.sh" /usr/local/share/p8s/sdn.sh
install -m 0755 "$REPO/scripts/setup-bond.sh" /usr/local/share/p8s/setup-bond.sh
install -m 0755 "$REPO/scripts/ipv6.sh" /usr/local/share/p8s/ipv6.sh
install -m 0644 "$REPO/deploy/p8s-vlans.service" /etc/systemd/system/p8s-vlans.service
install -m 0755 "$REPO/scripts/storages-up.sh" /usr/local/share/p8s/storages-up.sh
install -m 0755 "$REPO/scripts/libvirt-migrate.sh" /usr/local/share/p8s/libvirt-migrate.sh
install -m 0644 "$REPO/deploy/p8s-storages.service" /etc/systemd/system/p8s-storages.service
install -m 0755 "$REPO/scripts/ceph-map.sh" /usr/local/share/p8s/ceph-map.sh   # VM disks on Ceph, opened at boot
install -m 0644 "$REPO/deploy/p8s-rbd.service" /etc/systemd/system/p8s-rbd.service
# guest internet: repair at boot and after every firewall reload (which can wipe libvirt's NAT rules)
install -m 0644 "$REPO/deploy/p8s-net.service" /etc/systemd/system/p8s-net.service
install -d -m 0755 /etc/systemd/system/nftables.service.d
printf '[Service]\n# P8S: loading /etc/nftables.conf flushes the ruleset; put libvirt NAT + P8S rules back\nExecStartPost=-/bin/systemctl --no-block start p8s-net.service\nExecReload=-/bin/systemctl --no-block start p8s-net.service\n' \
  >/etc/systemd/system/nftables.service.d/p8s.conf
# libvirtd must stay up: Debian starts it with "--timeout 120", so it quits after
# 2 idle minutes when no VM runs (and, before this, took p8sd down with it)
install -d -m 0755 /etc/systemd/system/libvirtd.service.d
printf '[Service]\n# P8S: no idle timeout; the host always manages VMs\nEnvironment=LIBVIRTD_ARGS=\nRestart=on-failure\n' \
  >/etc/systemd/system/libvirtd.service.d/p8s.conf
# system logs survive reboots and are kept long (CERT-In: 180 days); journald trims by size
install -d -m 0755 /etc/systemd/journald.conf.d
printf '[Journal]\n# P8S: keep system logs on disk (Activity log: see audit_retain_days)\nStorage=persistent\nSystemMaxUse=4G\n' >/etc/systemd/journald.conf.d/p8s.conf
systemctl restart systemd-journald >/dev/null 2>&1 || true
timedatectl set-ntp true >/dev/null 2>&1 || true  # synced clock for logs and 2FA
ok "installed /usr/local/bin/p8sd and the 'p8' command"

# ---------------------------------------------------------------------------
# VM disks (ZFS volumes, also mirror copies) can hold the guests' own LVM:
# the host must never pick those up
if [[ -d /etc/lvm ]] && ! grep -q 'p8s: VM disks' /etc/lvm/lvmlocal.conf 2>/dev/null &&
   ! grep -Eqs '^[[:space:]]*global_filter' /etc/lvm/lvm.conf /etc/lvm/lvmlocal.conf; then
  cp -a /etc/lvm/lvmlocal.conf /etc/lvm/lvmlocal.conf.p8s-old 2>/dev/null || true
  printf '\n# p8s: VM disks are the guests'"'"', not the host'"'"'s\ndevices {\n\tglobal_filter = [ "r|^/dev/zd.*|", "r|^/dev/zvol/.*|" ]\n}\n' >>/etc/lvm/lvmlocal.conf
  if command -v lvmconfig >/dev/null && ! lvmconfig --validate >/dev/null 2>&1; then  # never leave LVM broken
    if [[ -f /etc/lvm/lvmlocal.conf.p8s-old ]]; then mv /etc/lvm/lvmlocal.conf.p8s-old /etc/lvm/lvmlocal.conf; else sed -i '/p8s: VM disks/,$d' /etc/lvm/lvmlocal.conf; fi
  fi
fi

say "Writing config and API token"
if [[ ! -f $USER_HOME/.ssh/id_ed25519.pub ]]; then
  sudo -u "$USER_NAME" mkdir -p "$USER_HOME/.ssh"
  sudo -u "$USER_NAME" ssh-keygen -q -t ed25519 -N "" -f "$USER_HOME/.ssh/id_ed25519"
  ok "created SSH key $USER_HOME/.ssh/id_ed25519 (VMs will trust it)"
fi
PUBKEY_LINE=$(cat "$USER_HOME/.ssh/id_ed25519.pub")
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
        --argjson cpus "$(nproc)" --argjson mem "$mem_mib" --arg ds "$POOL/p8s/vms" --arg key "$PUBKEY_LINE" '{
    listen: "0.0.0.0:8006", tls_auto: true, data_dir: "/var/lib/p8s", libvirt_uri: "qemu:///system",
    vm_dataset: $ds, bridge: $br, default_ssh_keys: [$key],
    max_vcpus: $cpus, max_memory_mib: $mem, max_disk_gb: 200,
    tokens: [{name: $t.name, sha256: $t.sha256}],
    templates: [{name: "debian-13", snapshot: $snap, min_disk_gb: $gb, os_variant: "debian13", label: "Debian 13 (trixie)", default_user: "debian"}]
  }' > "$CONF"
  chmod 0600 "$CONF"
  ( umask 077; jq -r .token <<<"$T" > "$TOKEN_FILE" )
  chown "$USER_NAME": "$TOKEN_FILE"
  ok "config $CONF"
  ok "token saved to $TOKEN_FILE (only $USER_NAME can read it)"
fi
# Upgrade older configs: dashboard on the network over HTTPS, host user's key as default.
if ! jq -e '.tls_auto == true and .listen == "0.0.0.0:8006" and (.default_ssh_keys | length > 0)' "$CONF" >/dev/null 2>&1; then
  tmp=$(mktemp)
  jq --arg key "$PUBKEY_LINE" '.listen = "0.0.0.0:8006" | .tls_auto = true
      | .default_ssh_keys = ((.default_ssh_keys // []) + [$key] | unique)' "$CONF" > "$tmp" && mv "$tmp" "$CONF"
  chmod 0600 "$CONF"
  ok "web dashboard enabled (HTTPS on port 8006)"
fi

# Home-network bridge made earlier (sudo p8 lan-setup) but not in the config yet?
if [[ -d /sys/class/net/br0/bridge && $BRIDGE != br0 ]] && ! jq -e '.lan_bridge == "br0"' "$CONF" >/dev/null 2>&1; then
  tmp=$(mktemp)
  jq '.lan_bridge = "br0"' "$CONF" > "$tmp" && mv "$tmp" "$CONF" && chmod 0600 "$CONF"
  ok "home network (br0) available for VMs"
fi

# ---------------------------------------------------------------------------
say "Starting p8sd"
since=$(date "+%Y-%m-%d %H:%M:%S")
systemctl daemon-reload
systemctl enable p8sd >/dev/null 2>&1
systemctl restart p8sd
for _ in $(seq 20); do curl -kfs https://127.0.0.1:8006/api/v1/health >/dev/null && break; sleep 0.5; done
curl -kfs https://127.0.0.1:8006/api/v1/health >/dev/null || { journalctl -u p8sd -n 20 --no-pager; die "p8sd did not start"; }
plog=$(journalctl -u p8sd --since "$since" --no-pager)
grep -q "snapshot missing" <<<"$plog" && die "p8sd can't see the template snapshot"
ok "p8sd running: $(curl -kfs https://127.0.0.1:8006/api/v1/health)"

# Guest internet: check and repair now (VMs keep running), and at every boot
systemctl enable p8s-net.service >/dev/null 2>&1 || true
systemctl enable p8s-vlans.service >/dev/null 2>&1 || true
systemctl enable p8s-storages.service >/dev/null 2>&1 || true
systemctl enable p8s-rbd.service >/dev/null 2>&1 || true
bash /usr/local/share/p8s/netcheck.sh --repair | sed 's/^/  /' || true

# Host tuning: ZFS cache limit, KSM, vhost_net, low swappiness
bash "$REPO/scripts/tune.sh" | sed 's/^/  /' || echo "  note: tuning not applied"

# P8S OS name on the boot menu, login screen and SSH welcome (Debian stays underneath)
bash "$REPO/scripts/branding.sh" >/dev/null 2>&1 && ok "P8S logo on the boot menu, boot screen and login" || echo "  note: branding not applied"

# Installed from the ISO? Mark first-boot setup as done (also when it was finished by hand).
if [[ -f /etc/systemd/system/p8s-firstboot.service ]]; then
  touch /var/lib/p8s/.firstboot-done
  systemctl disable p8s-firstboot.service >/dev/null 2>&1 || true
  bash "$REPO/scripts/branding.sh" >/dev/null 2>&1 || true   # login screen now that setup is done
fi

trap - ERR
HOST_IP=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for (i=1;i<NF;i++) if ($i=="src") print $(i+1)}')
cat <<DONE

$(printf '\033[1;32m')✔ P8S OS is installed.$(printf '\033[0m')

  Web dashboard:  https://${HOST_IP:-<this-host-ip>}:8006
                  Accept the certificate warning once. The first visit asks you to create
                  your admin account, with this one-time setup code:  cat ~/.p8s-token
                  Everything else (users, public access, ...) is in the dashboard.

  Next, as $USER_NAME (no sudo):

    p8 test              run the full automatic test (2-5 min)
    p8 create web1       make a VM
    p8 ssh web1          log into it
    p8 help              all commands

  Install log: $LOG
DONE
