#!/usr/bin/env bash
# Build the P8S OS alpha installer ISO from the official Debian 13 netinst ISO.
# Run as root on any Debian 13 machine with internet (e.g. the p8s-test VM):
#   sudo bash iso/build-iso.sh [output-dir]
#
# How it works: downloads + verifies the Debian netinst ISO, adds
#   /preseed.cfg   installer answers (iso/preseed.cfg)
#   /p8sos/        this repository
# and a boot menu that starts "Install P8S OS (alpha)" by default, then
# repacks it with xorriso, keeping Debian's BIOS+UEFI+USB boot setup intact.
set -euo pipefail
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:$PATH

REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
OUT_DIR=${1:-$REPO/dist}
BASE=https://cdimage.debian.org/debian-cd/current/amd64/iso-cd
CACHE=/var/cache/p8s-iso
WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT
VERSION=$(date +%Y%m%d)
KARGS='preseed/file=/cdrom/preseed.cfg locale=en_US.UTF-8 keyboard-configuration/xkb-keymap=us'

say() { printf '\n\033[1;36m==== %s ====\033[0m\n' "$*"; }
[[ $EUID -eq 0 ]] || { echo "run as root (sudo)"; exit 1; }

say "1/5 Tools"
command -v xorriso >/dev/null || { apt-get update -qq && apt-get install -y -qq xorriso curl >/dev/null; }
echo "xorriso $(xorriso -version 2>/dev/null | head -1 | awk '{print $2}')"

say "2/5 Debian 13 netinst ISO (download once, verified)"
mkdir -p "$CACHE"
curl -4 -fsSL --retry 5 --retry-all-errors --connect-timeout 30 -o "$CACHE/SHA512SUMS" "$BASE/SHA512SUMS"
ISO=$(awk '/amd64-netinst\.iso$/ {print $2; exit}' "$CACHE/SHA512SUMS")
[[ -n $ISO ]] || { echo "could not find the netinst ISO name in SHA512SUMS"; exit 1; }
verify() { (cd "$CACHE" && grep " $ISO\$" SHA512SUMS | sha512sum -c --status - 2>/dev/null); }
if verify; then
  echo "using cached $ISO"
else
  # 1) a copy you already have: in the repo/shared folder (or its dist/), any depth 2
  for f in $(find "$REPO" "$REPO/.." -maxdepth 2 -name "$ISO" 2>/dev/null); do
    echo "trying local copy $f"; cp "$f" "$CACHE/$ISO" && verify && break
  done
  # 2) the ISO attached to this VM's CD drive (VirtualBox/VMware)
  if ! verify; then
    for cd in /dev/sr0 /dev/sr1; do
      [[ -b $cd ]] || continue
      echo "trying the CD drive $cd (the ISO you installed Debian from) ..."
      dd if="$cd" of="$CACHE/$ISO" bs=4M status=none 2>/dev/null && verify && { echo "  matches $ISO"; break; }
    done
  fi
  # 3) download (IPv4, retries, resumable)
  if ! verify; then
    echo "downloading $ISO (750 MB) ..."
    rm -f "$CACHE/$ISO"
    curl -4 -fL --retry 8 --retry-all-errors --retry-delay 5 --connect-timeout 30 -C - -o "$CACHE/$ISO" "$BASE/$ISO" \
      || curl -4 -fL --retry 8 --retry-all-errors --retry-delay 5 --connect-timeout 30 -C - -o "$CACHE/$ISO" "$BASE/$ISO"
  fi
  verify || { echo "Could not get a verified copy of $ISO. Copy it into L:\\P8SOS and run again."; exit 1; }
fi
echo "base: $ISO (sha512 verified)"

say "3/5 P8S OS files"
mkdir -p "$WORK/p8sos"
tar -C "$REPO" --exclude=.git --exclude=bin --exclude=dist --exclude='*.iso' --exclude='*.tar.gz' -cf - . | tar -C "$WORK/p8sos" -xf -
find "$WORK/p8sos" -type f \( -name '*.sh' -o -name p8 -o -name '*.cfg' -o -name '*.service' \) -exec sed -i 's/\r$//' {} +
cp "$WORK/p8sos/iso/preseed.cfg" "$WORK/preseed.cfg"
echo "$(find "$WORK/p8sos" -type f | wc -l) files"

say "4/5 Boot menus (BIOS + UEFI)"
cat > "$WORK/isolinux.cfg" <<EOF
default p8s
prompt 1
timeout 50
say
say   P8S OS alpha installer  -  starting in 5 seconds (type 'debian' for the plain Debian installer)
say
label p8s
  kernel /install.amd/vmlinuz
  append vga=788 initrd=/install.amd/initrd.gz $KARGS --- quiet
label debian
  kernel /install.amd/vmlinuz
  append vga=788 initrd=/install.amd/initrd.gz --- quiet
EOF
xorriso -osirrox on -indev "$CACHE/$ISO" -extract /boot/grub/grub.cfg "$WORK/grub.orig.cfg" >/dev/null 2>&1
chmod u+w "$WORK/grub.orig.cfg"
{
  cat <<EOF
set default=0
set timeout=5
menuentry 'Install P8S OS (alpha)' {
    linux  /install.amd/vmlinuz $KARGS --- quiet
    initrd /install.amd/initrd.gz
}
EOF
  cat "$WORK/grub.orig.cfg"
} > "$WORK/grub.cfg"
echo "default boot entry: Install P8S OS (alpha)"

say "5/5 Build ISO"
mkdir -p "$OUT_DIR"
OUT=$OUT_DIR/p8sos-alpha-$VERSION-amd64.iso
rm -f "$OUT"
xorriso -indev "$CACHE/$ISO" -outdev "$OUT" \
  -volid "P8SOS_ALPHA" \
  -map "$WORK/preseed.cfg" /preseed.cfg \
  -map "$WORK/p8sos" /p8sos \
  -map "$WORK/isolinux.cfg" /isolinux/isolinux.cfg \
  -map "$WORK/grub.cfg" /boot/grub/grub.cfg \
  -boot_image any replay 2>&1 | grep -E 'Writing to|Written to medium|FAILURE|SORRY' || true
[[ -s $OUT ]] || { echo "ISO build failed"; exit 1; }
SUM=$(sha256sum "$OUT" | awk '{print $1}')
echo "$SUM  $(basename "$OUT")" > "$OUT.sha256"
[[ -n ${SUDO_USER:-} ]] && chown "$SUDO_USER": "$OUT" "$OUT.sha256" 2>/dev/null || true

printf '\n\033[1;32m✔ Built %s (%s)\033[0m\nsha256 %s\n' "$OUT" "$(du -h "$OUT" | cut -f1)" "$SUM"
