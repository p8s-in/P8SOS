#!/usr/bin/env bash
# Prepare a fresh Debian 13 (trixie) host for p8sd: KVM, libvirt, ZFS, cloud-init tools.
#
#   sudo ./bootstrap-debian13.sh /dev/nvme0n1   # real disk -> pool "tank" (DESTROYS the disk)
#   sudo ./bootstrap-debian13.sh                # no disk -> 64G sparse file pool, for testing only
set -euo pipefail
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:$PATH  # admin tools even under plain "su"

POOL=${POOL:-tank}
DEV=${1:-}

[[ $EUID -eq 0 ]] || { echo "run as root" >&2; exit 1; }
. /etc/os-release
[[ ${VERSION_CODENAME:-} == trixie ]] || { echo "expected Debian 13 (trixie), got ${PRETTY_NAME:-unknown}" >&2; exit 1; }
grep -Eq 'vmx|svm' /proc/cpuinfo || { echo "CPU virtualization (VT-x/AMD-V) not available; enable it in BIOS" >&2; exit 1; }

echo "==> enabling contrib (ZFS lives there)"
# Classic format (/etc/apt/sources.list, written by the netinst installer)
if [[ -f /etc/apt/sources.list ]]; then
  sed -i -E '/^deb(-src)?[[:space:]]+https?:/{/[[:space:]]contrib([[:space:]]|$)/! s/[[:space:]]main([[:space:]]|$)/ main contrib\1/}' /etc/apt/sources.list
fi
# deb822 format (/etc/apt/sources.list.d/*.sources)
for f in /etc/apt/sources.list.d/*.sources; do
  [[ -f $f ]] || continue
  sed -i -E '/^Components:/{/[[:space:]]contrib([[:space:]]|$)/! s/[[:space:]]main([[:space:]]|$)/ main contrib\1/}' "$f"
done
grep -rqsE '(^deb .* contrib|^Components:.* contrib)' /etc/apt/sources.list /etc/apt/sources.list.d/ \
  || { echo "could not enable 'contrib' in apt sources" >&2; exit 1; }

echo "==> installing packages"
export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y linux-headers-amd64 zfs-dkms zfsutils-linux \
  qemu-system-x86 qemu-utils libvirt-daemon-system libvirt-clients \
  libvirt-daemon-config-network dnsmasq-base \
  cloud-image-utils ovmf bridge-utils curl jq make git zstd lzop smartmontools iputils-ping openssh-client wireguard-tools nftables lxc uidmap swtpm swtpm-tools
modprobe zfs
udevadm control --reload-rules && udevadm trigger --subsystem-match=block --action=add   # load ZFS's /dev/zvol rules now
systemctl enable --now libvirtd

if ! zpool list -H "$POOL" >/dev/null 2>&1; then
  if [[ -n $DEV ]]; then
    if [[ ${P8S_YES:-0} == 1 ]]; then
      ok=yes   # unattended (ISO first boot); the caller has already checked the disk is empty
    else
      read -r -p "Create pool $POOL on $DEV? ALL DATA ON $DEV WILL BE LOST [type yes]: " ok
    fi
    [[ $ok == yes ]] || exit 1
    zpool create -f -o ashift=12 -O compression=lz4 -O atime=off "$POOL" "$DEV"
  else
    # Use the system disk's free space, keeping 10% (at least 20 GB) for Debian.
    mkdir -p /var/lib/p8s
    avail=$(df -BG --output=avail /var/lib/p8s | tail -1 | tr -dc 0-9)
    fs=$(df -BG --output=size /var/lib/p8s | tail -1 | tr -dc 0-9)
    keep=$(( fs / 10 )); (( keep < 20 )) && keep=20
    size=$(( avail - keep ))
    (( size >= 20 )) || { echo "not enough free space for VM storage (${avail} GB free)" >&2; exit 1; }
    echo "==> no disk given: creating a ${size} GB file pool on the system disk"
    truncate -s "${size}G" /var/lib/p8s/"$POOL".img
    zpool create -o ashift=12 -O compression=lz4 -O atime=off "$POOL" /var/lib/p8s/"$POOL".img
  fi
fi
install -d -m 0750 /etc/p8s
if [[ ${P8S_ENCRYPT:-0} == 1 ]] && ! zfs list -H "$POOL/p8s" >/dev/null 2>&1; then
  # Encryption at rest: everything under $POOL/p8s (VM disks, containers,
  # templates) is AES-256-GCM. The key is on the system disk, so the VM disks
  # alone (stolen, sent back for warranty, sold) can't be read. BACK UP THE KEY:
  # Server page → Storage → Download the key. Without it the data is gone.
  echo "==> encrypting VM storage (key: /etc/p8s/keys/p8s.key)"
  install -d -m 0700 /etc/p8s/keys
  [[ -s /etc/p8s/keys/p8s.key ]] || { head -c 32 /dev/urandom >/etc/p8s/keys/p8s.key; chmod 0400 /etc/p8s/keys/p8s.key; }
  zfs create -o encryption=aes-256-gcm -o keyformat=raw -o keylocation=file:///etc/p8s/keys/p8s.key "$POOL/p8s"
fi
zfs create -p "$POOL/p8s/vms"
zfs create -p "$POOL/p8s/templates"
if [[ $(zfs get -H -o value encryption "$POOL/p8s" 2>/dev/null) != off ]]; then
  # load the key at boot before the VM disks are needed
  cat >/etc/systemd/system/p8s-zfs-keys.service <<UNIT
[Unit]
Description=P8S OS: unlock encrypted VM storage
DefaultDependencies=no
After=zfs-import.target
Before=zfs-mount.service zfs-volumes.target libvirtd.service p8sd.service
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/sh -c 'zfs get -H -o value keystatus $POOL/p8s | grep -q available || zfs load-key $POOL/p8s'
ExecStartPost=-/bin/sh -c 'zfs mount -a; udevadm trigger --subsystem-match=block --action=add'
[Install]
WantedBy=zfs.target
UNIT
  systemctl daemon-reload
  systemctl enable p8s-zfs-keys.service >/dev/null 2>&1 || true
fi
install -d -m 0755 /var/lib/p8s /var/lib/p8s/seeds   # QEMU must be able to read seed ISOs

if [[ ${P8S_BRIDGE:-vmbr0} != virbr0 ]] && ! ip link show vmbr0 >/dev/null 2>&1; then
  cat <<MSG

==> NOTE: bridge vmbr0 does not exist yet. Not creating it automatically
    because that can drop your SSH session. See docs/host-setup.md, or for a
    quick NAT-only test set "bridge": "virbr0" in /etc/p8s/p8sd.json
    (libvirt's default network: virsh net-start default && virsh net-autostart default).
MSG
fi
echo "==> host prepared."
