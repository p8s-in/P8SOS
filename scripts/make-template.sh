#!/usr/bin/env bash
# Import an official cloud image as a P8S OS template (a ZFS snapshot VMs are cloned from).
#
#   sudo ./make-template.sh                       -> debian-13 (what install.sh uses)
#   sudo ./make-template.sh ubuntu-24.04 --register
#                                                 -> import + add to /etc/p8s/p8sd.json + restart p8sd
#   ./make-template.sh --list                     -> what can be imported (and others added)
#   sudo ./make-template.sh --spec FILE --register
#                                                 -> any cloud image: FILE is JSON
#      {"name","label","url","login","variant","checksum"}; checksum = the URL of a
#      SHA256SUMS / SHA512SUMS / CHECKSUM file, "sha256:HEX" / "sha512:HEX", or "" (not checked)
#
# Every image is checked against its publisher's checksum before use.
set -euo pipefail
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:$PATH  # admin tools even under plain "su"

CATALOG="debian-13 debian-12 ubuntu-24.04 ubuntu-22.04 rocky-9 alma-9"

# catalog NAME -> sets URL SUMS ALGO LOGIN VARIANT LABEL
catalog() {
  case $1 in
    debian-13) URL=https://cloud.debian.org/images/cloud/trixie/latest/debian-13-genericcloud-amd64.qcow2
               SUMS=https://cloud.debian.org/images/cloud/trixie/latest/SHA512SUMS ALGO=sha512
               LOGIN=debian VARIANT=debian13 LABEL="Debian 13 (trixie)" ;;
    debian-12) URL=https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2
               SUMS=https://cloud.debian.org/images/cloud/bookworm/latest/SHA512SUMS ALGO=sha512
               LOGIN=debian VARIANT=debian12 LABEL="Debian 12 (bookworm)" ;;
    ubuntu-24.04) URL=https://cloud-images.ubuntu.com/releases/noble/release/ubuntu-24.04-server-cloudimg-amd64.img
               SUMS=https://cloud-images.ubuntu.com/releases/noble/release/SHA256SUMS ALGO=sha256
               LOGIN=ubuntu VARIANT=ubuntu24.04 LABEL="Ubuntu 24.04 LTS" ;;
    ubuntu-22.04) URL=https://cloud-images.ubuntu.com/releases/jammy/release/ubuntu-22.04-server-cloudimg-amd64.img
               SUMS=https://cloud-images.ubuntu.com/releases/jammy/release/SHA256SUMS ALGO=sha256
               LOGIN=ubuntu VARIANT=ubuntu22.04 LABEL="Ubuntu 22.04 LTS" ;;
    rocky-9)   URL=https://dl.rockylinux.org/pub/rocky/9/images/x86_64/Rocky-9-GenericCloud-Base.latest.x86_64.qcow2
               SUMS=$URL.CHECKSUM ALGO=sha256
               LOGIN=rocky VARIANT=rocky9 LABEL="Rocky Linux 9" ;;
    alma-9)    URL=https://repo.almalinux.org/almalinux/9/cloud/x86_64/images/AlmaLinux-9-GenericCloud-latest.x86_64.qcow2
               SUMS=https://repo.almalinux.org/almalinux/9/cloud/x86_64/images/CHECKSUM ALGO=sha256
               LOGIN=almalinux VARIANT=almalinux9 LABEL="AlmaLinux 9" ;;
    *) return 1 ;;
  esac
}

REGISTER=0; NAME=${NAME:-debian-13}; SPEC=""; HASH=""
CONF=${CONF:-/etc/p8s/p8sd.json}
args=("$@")
for ((i = 0; i < ${#args[@]}; i++)); do
  a=${args[i]}
  case $a in
    --register) REGISTER=1 ;;
    --list)
      for n in $CATALOG; do catalog "$n"; printf '  %-14s %s\n' "$n" "$LABEL"; done
      # others added from an image address (--spec)
      [[ -r $CONF ]] && jq -r '.templates[]? | select(.source_url != null) | "  \(.name) \(.label // .name)"' "$CONF" 2>/dev/null |
        while read -r n l; do [[ " $CATALOG " == *" $n "* ]] || printf '  %-14s %s\n' "$n" "$l"; done
      exit 0 ;;
    --spec) SPEC=${args[i+1]:-}; i=$((i + 1)) ;;
    -*) echo "unknown option $a" >&2; exit 1 ;;
    *) NAME=$a ;;
  esac
done
if [[ -n $SPEC ]]; then
  [[ -r $SPEC ]] || { echo "no spec file $SPEC" >&2; exit 1; }
  NAME=$(jq -r '.name // ""' "$SPEC"); URL=$(jq -r '.url // ""' "$SPEC")
  LABEL=$(jq -r '.label // ""' "$SPEC"); LOGIN=$(jq -r '.login // ""' "$SPEC")
  VARIANT=$(jq -r '.variant // ""' "$SPEC"); CHECK=$(jq -r '.checksum // ""' "$SPEC")
  [[ $NAME =~ ^[a-z][a-z0-9.-]{1,30}$ ]] || { echo "bad template name '$NAME'" >&2; exit 1; }
  [[ $URL =~ ^https?://[^[:space:]\"\']+$ ]] || { echo "bad image address '$URL'" >&2; exit 1; }
  [[ -n $LABEL ]] || LABEL=$NAME
  [[ $LOGIN =~ ^[a-z_][a-z0-9_-]{0,31}$ ]] || LOGIN=root
  [[ $VARIANT =~ ^[a-z0-9.]{1,40}$ ]] || VARIANT=linux2022
  SUMS="" ALGO=sha256
  case $CHECK in
    sha256:*) HASH=${CHECK#sha256:} ;;
    sha512:*) HASH=${CHECK#sha512:}; ALGO=sha512 ;;
    http://*|https://*) SUMS=$CHECK; [[ $CHECK == *512* ]] && ALGO=sha512 ;;
    "") ;;
    *) echo "checksum must be a URL, sha256:HEX or sha512:HEX" >&2; exit 1 ;;
  esac
  HASH=${HASH,,}
  [[ -z $HASH || ( $ALGO == sha256 && $HASH =~ ^[0-9a-f]{64}$ ) || ( $ALGO == sha512 && $HASH =~ ^[0-9a-f]{128}$ ) ]] ||
    { echo "the $ALGO checksum is not the right length" >&2; exit 1; }
else
  catalog "$NAME" || { echo "unknown template '$NAME'. Available:" >&2; "$0" --list >&2; exit 1; }
fi

POOL=${POOL:-tank}
IMG=${URL##*/}; IMG=${IMG%%\?*}; [[ $IMG =~ ^[A-Za-z0-9._+-]+$ ]] || IMG=image
DS=$POOL/p8s/templates/$NAME
WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT

[[ $EUID -eq 0 ]] || { echo "run as root: sudo $0 $*" >&2; exit 1; }

register() { # add/refresh the template in p8sd's config, then restart p8sd
  (( REGISTER )) || return 0
  [[ -f $CONF ]] || { echo "no $CONF (run install.sh first)" >&2; exit 1; }
  local bytes gb tmp
  bytes=$(zfs get -Hp -o value volsize "$DS"); gb=$(( (bytes + 1073741823) / 1073741824 ))
  tmp=$(mktemp)
  local src=""; [[ -n $SPEC ]] && src=$URL
  jq --arg n "$NAME" --arg s "$DS@base" --argjson gb "$gb" --arg v "$VARIANT" --arg l "$LABEL" --arg u "$LOGIN" --arg src "$src" '
    .templates = ([.templates[] | select(.name != $n)] +
      [{name: $n, snapshot: $s, min_disk_gb: $gb, os_variant: $v, label: $l, default_user: $u} +
       (if $src == "" then {} else {source_url: $src} end)])' "$CONF" > "$tmp"
  cat "$tmp" > "$CONF"; rm -f "$tmp"
  systemctl restart p8sd
  echo "==> $LABEL is now in the dashboard's template list (log in as '$LOGIN' or root)."
}

if zfs list -H "$DS@base" >/dev/null 2>&1; then echo "$DS@base already exists"; register; exit 0; fi
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

echo "==> downloading $LABEL ($IMG)"
curl -fL --retry 3 -o "$WORK/$IMG" "$URL"
LEN=64; [[ $ALGO == sha512 ]] && LEN=128
want=$HASH
if [[ -n $SUMS ]]; then
  curl -fsSL --retry 3 -o "$WORK/SUMS" "$SUMS"
  # Checksum files come in several formats ("HASH  file", "HASH *file", "SHA256 (file) = HASH");
  # take the hash on the line that names our file (or the only hash, for a one-file .sha256).
  want=$(grep -F "$IMG" "$WORK/SUMS" | grep -oiE "[0-9a-f]{$LEN}" | head -1 || true)
  [[ -n $want ]] || [[ $(grep -ciE "[0-9a-f]{$LEN}" "$WORK/SUMS") != 1 ]] || want=$(grep -oiE "[0-9a-f]{$LEN}" "$WORK/SUMS")
  [[ -n $want ]] || { echo "no $ALGO checksum for $IMG in $SUMS" >&2; exit 1; }
fi
if [[ -n $want ]]; then
  got=$("${ALGO}sum" "$WORK/$IMG" | cut -d' ' -f1)
  [[ $got == "${want,,}" ]] || { echo "CHECKSUM MISMATCH for $IMG (download corrupted or tampered)" >&2; exit 1; }
  echo "==> checksum OK ($ALGO)"
else
  echo "==> WARNING: no checksum given, so the download is not checked"
fi
# compressed images: unpack first
case $IMG in
  *.xz) xz -d "$WORK/$IMG"; IMG=${IMG%.xz} ;;
  *.gz) gzip -d "$WORK/$IMG"; IMG=${IMG%.gz} ;;
esac
qemu-img info "$WORK/$IMG" >/dev/null 2>&1 || { echo "$IMG is not a disk image qemu-img can read (use a .qcow2, .img or .raw cloud image)" >&2; exit 1; }

BYTES=$(qemu-img info --output=json "$WORK/$IMG" | jq -r '."virtual-size"')
GB=$(( (BYTES + 1073741823) / 1073741824 ))
echo "==> creating ${GB}G zvol $DS"
zfs create -p -s -V "${GB}G" -o volmode=dev "$DS"
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
echo "==> template $NAME ready: $DS@base (${GB} GB)"
register
