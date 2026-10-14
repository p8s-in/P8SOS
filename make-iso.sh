#!/usr/bin/env bash
# Build the P8S OS installer ISO inside the VirtualBox test VM and save it to
# the shared folder, so it appears on Windows as L:\P8SOS\dist\p8sos-installer-*.iso
#
#   su -c 'mount -t vboxsf P8SOS /mnt || mount -t vboxsf p8sos /mnt; SUDO_USER=vboxuser bash /mnt/make-iso.sh'
#
#   ... make-iso.sh            the P8S installer: ZFS system disk(s), RAID choice (docs/installer.md)
#   ... make-iso.sh classic    the older ISO: Debian's own installer + P8S on first boot
#   ANSWERS=/mnt/answers.json ... make-iso.sh    installs without questions (test VMs)
#
# The shared folder must be WRITABLE (VirtualBox Settings -> Shared Folders ->
# edit P8SOS -> untick Read-only). Otherwise the ISO is saved in the VM's home folder.
set -uo pipefail
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:$PATH
SRC=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
U=${SUDO_USER:-root}
H=$(getent passwd "$U" | cut -d: -f6)
[[ $EUID -eq 0 ]] || { echo "run as root"; exit 1; }

# VMs running inside this VM are what makes VirtualBox freeze: stop them and
# stop them auto-starting. Leftover smoke-test VMs are removed completely.
if command -v virsh >/dev/null; then
  for d in $(virsh list --all --name 2>/dev/null | grep '^p8s-'); do
    virsh destroy "$d" >/dev/null 2>&1
    if [[ $d == p8s-smoke* ]]; then
      virsh undefine "$d" --nvram >/dev/null 2>&1
      zfs destroy -r "tank/p8s/vms/$d-disk0" 2>/dev/null; rm -f "/var/lib/p8s/seeds/$d-seed.iso"
      echo "removed leftover test VM $d"
    else
      virsh autostart --disable "$d" >/dev/null 2>&1
      echo "stopped $d (autostart off; start it again with: p8 start ${d#p8s-})"
    fi
  done
fi

if touch "$SRC/.write-test" 2>/dev/null; then
  rm -f "$SRC/.write-test"
  OUT=$SRC/dist
  echo "ISO will be saved to the shared folder: L:\\P8SOS\\dist"
else
  OUT=$H/dist
  echo "NOTE: shared folder is read-only, so the ISO goes to $OUT inside this VM."
  echo "      To get it on Windows: power off, untick Read-only on the P8SOS share, run this again."
fi

if [[ ${1:-} == classic ]]; then
  bash "$SRC/iso/build-iso.sh" "$OUT"
else
  bash "$SRC/iso/build-installer.sh" "$OUT"
fi
