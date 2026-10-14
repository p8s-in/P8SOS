#!/usr/bin/env bash
# Keep every disk's EFI partition equal to the one mounted at /boot/efi, so the
# server still boots when the first disk dies. The installer puts an EFI
# partition (label p8s-esp0, p8s-esp1, ...) on every disk of a mirror/RAID and
# runs this after every package install (apt hook /etc/apt/apt.conf.d/99p8s-esp-sync).
#   sudo p8s-esp-sync
set -euo pipefail
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
[[ $EUID -eq 0 ]] || { echo "run as root" >&2; exit 1; }
mountpoint -q /boot/efi || exit 0            # first disk gone: nothing to copy from
SRC=$(findmnt -no SOURCE /boot/efi)
SRC=$(readlink -f "$SRC")
TMP=$(mktemp -d); trap 'umount "$TMP" 2>/dev/null; rmdir "$TMP"' EXIT
n=0
for p in /dev/disk/by-partlabel/p8s-esp*; do
  [[ -e $p ]] || continue
  dev=$(readlink -f "$p")
  [[ $dev == "$SRC" ]] && continue
  mount -t vfat "$dev" "$TMP" || { echo "p8s-esp-sync: can't mount $dev" >&2; continue; }
  # copy into place, then drop what's gone from the source
  cp -a /boot/efi/. "$TMP"/
  (cd "$TMP" && find . -depth -mindepth 1 | while read -r f; do [[ -e /boot/efi/$f ]] || rm -rf "${TMP:?}/$f"; done)
  sync; umount "$TMP"; n=$((n + 1))
done
(( n == 0 )) || echo "p8s-esp-sync: $n spare EFI partition(s) updated"
