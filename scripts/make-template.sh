#!/usr/bin/env bash
# Import the official Debian 13 cloud image as a ZFS template snapshot.
#   sudo ./make-template.sh            -> tank/p8s/templates/debian-13@base
set -euo pipefail
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:$PATH  # admin tools even under plain "su"

POOL=${POOL:-tank}
NAME=${NAME:-debian-13}
BASE=https://cloud.debian.org/images/cloud/trixie/latest
IMG=debian-13-genericcloud-amd64.qcow2
DS=$POOL/p8s/templates/$NAME
WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT

[[ $EUID -eq 0 ]] || { echo "run as root" >&2; exit 1; }
if zfs list -H "$DS@base" >/dev/null 2>&1; then echo "$DS@base already exists"; exit 0; fi
# A half-finished earlier attempt (volume but no snapshot) is removed and redone.
zfs list -H "$DS" >/dev/null 2>&1 && zfs destroy -r "$DS"

# Wait for /dev/zvol/<dataset>; on a fresh install udev may not have ZFS's rules loaded yet.
wait_zvol() {
  local i
  for i in $(seq 30); do
    [[ -e /dev/zvol/$1 ]] && return 0
    udevadm trigger --subsystem-match=block --action=add 2>/dev/null
    udevadm settle --timeout=5 2>/dev/null
    sleep 1
  done
  echo "/dev/zvol/$1 did not appear (udev). Try: sudo udevadm control --reload-rules && sudo udevadm trigger" >&2
  return 1
}

echo "==> downloading $IMG"
curl -fL --retry 3 -o "$WORK/$IMG" "$BASE/$IMG"
curl -fsSL -o "$WORK/SHA512SUMS" "$BASE/SHA512SUMS"
(cd "$WORK" && grep " $IMG\$" SHA512SUMS | sha512sum -c -)

BYTES=$(qemu-img info --output=json "$WORK/$IMG" | jq -r '."virtual-size"')
GB=$(( (BYTES + 1073741823) / 1073741824 ))
echo "==> creating ${GB}G zvol $DS"
zfs create -p -V "${GB}G" -o volmode=dev "$DS"
wait_zvol "$DS"
# Write to the real kernel device (/dev/zdN), not the udev symlink: on a fresh
# system udev can briefly remove and re-create /dev/zvol/... while it settles.
for try in 1 2 3 4 5; do
  udevadm settle --timeout=10 2>/dev/null || true
  ZDEV=$(readlink -f "/dev/zvol/$DS" 2>/dev/null)
  if [[ -b $ZDEV ]] && qemu-img convert -p -n -O raw "$WORK/$IMG" "$ZDEV"; then
    break
  fi
  [[ $try == 5 ]] && { echo "could not write the template to $DS" >&2; exit 1; }
  echo "zvol not ready yet, retrying ($try/5)..."
  sleep 3
  wait_zvol "$DS"
done
zfs snapshot "$DS@base"

cat <<MSG

==> template ready. Add to "templates" in /etc/p8s/p8sd.json:
  { "name": "$NAME", "snapshot": "$DS@base", "min_disk_gb": $GB, "os_variant": "debian13" }
MSG
