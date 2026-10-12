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
  cloud-image-utils ovmf bridge-utils curl jq make git zstd nftables
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
    echo "==> no disk given: creating a sparse 64G file pool (testing only)"
    mkdir -p /var/lib/p8s
    truncate -s 64G /var/lib/p8s/"$POOL".img
    zpool create -o ashift=12 -O compression=lz4 -O atime=off "$POOL" /var/lib/p8s/"$POOL".img
  fi
fi
zfs create -p "$POOL/p8s/vms"
zfs create -p "$POOL/p8s/templates"
install -d -m 0750 /etc/p8s
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
