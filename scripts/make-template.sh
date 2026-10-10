#!/usr/bin/env bash
# Import the official Debian 13 cloud image as a ZFS template snapshot.
#   sudo ./make-template.sh            -> tank/p8s/templates/debian-13@base
set -euo pipefail

POOL=${POOL:-tank}
NAME=${NAME:-debian-13}
BASE=https://cloud.debian.org/images/cloud/trixie/latest
IMG=debian-13-genericcloud-amd64.qcow2
DS=$POOL/p8s/templates/$NAME
WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT

[[ $EUID -eq 0 ]] || { echo "run as root" >&2; exit 1; }
if zfs list -H "$DS@base" >/dev/null 2>&1; then echo "$DS@base already exists"; exit 0; fi

echo "==> downloading $IMG"
curl -fL --retry 3 -o "$WORK/$IMG" "$BASE/$IMG"
curl -fsSL -o "$WORK/SHA512SUMS" "$BASE/SHA512SUMS"
(cd "$WORK" && grep " $IMG\$" SHA512SUMS | sha512sum -c -)

BYTES=$(qemu-img info --output=json "$WORK/$IMG" | jq -r '."virtual-size"')
GB=$(( (BYTES + 1073741823) / 1073741824 ))
echo "==> creating ${GB}G zvol $DS"
zfs create -p -V "${GB}G" -o volmode=dev "$DS"
udevadm settle
qemu-img convert -p -n -O raw "$WORK/$IMG" "/dev/zvol/$DS"
zfs snapshot "$DS@base"

cat <<MSG

==> template ready. Add to "templates" in /etc/p8s/p8sd.json:
  { "name": "$NAME", "snapshot": "$DS@base", "min_disk_gb": $GB, "os_variant": "debian13" }
MSG
