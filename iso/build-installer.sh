#!/usr/bin/env bash
# Build the P8S OS installer ISO: a small Debian 13 live system that starts the
# P8S OS installer (cmd/p8s-install). It installs with the system on ZFS — one
# disk, mirror, RAID 10 or RAID-Z1/2/3 — or on ext4, from the ISO itself (no
# internet needed to install; the first start downloads the VM template).
#
#   sudo bash iso/build-installer.sh [output-dir]
#
# Run on Debian 13 with internet (the p8s-test VM). Needs ~15 GB free in
# /var/cache/p8s-live (set P8S_LIVE_WORK=/somewhere/else) and 30-60 minutes the
# first time (packages are cached, later builds are faster).
#
# Options (env):
#   ANSWERS=/path/answers.json   bake an answers file in: the ISO then installs
#                                without questions (for test VMs; see docs/installer.md)
set -euo pipefail
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/usr/local/go/bin:$PATH

REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
OUT_DIR=${1:-$REPO/dist}
WORK=${P8S_LIVE_WORK:-/var/cache/p8s-live}
VERSION=$(date +%Y%m%d)
BRAND=$REPO/assets/brand
say() { printf '\n\033[1;36m==== %s ====\033[0m\n' "$*"; }
die() { printf '\n\033[1;31m✘ %s\033[0m\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "run as root (sudo)"
. /etc/os-release
[[ ${VERSION_CODENAME:-} == trixie ]] || die "build on Debian 13 (trixie); found ${PRETTY_NAME:-unknown}"
[[ -z ${ANSWERS:-} || -f $ANSWERS ]] || die "ANSWERS=$ANSWERS: no such file"

say "1/6 Tools"
need=()
for p in live-build xorriso squashfs-tools debootstrap; do dpkg -s "$p" >/dev/null 2>&1 || need+=("$p"); done
((${#need[@]} == 0)) || { apt-get update -qq && apt-get install -y -qq "${need[@]}" >/dev/null; }
command -v go >/dev/null || { apt-get install -y -qq golang-go >/dev/null; }
echo "live-build $(dpkg-query -W -f='${Version}' live-build), $(go version | awk '{print $3}')"
mkdir -p "$WORK"
free=$(df -BG --output=avail "$WORK" | tail -1 | tr -dc 0-9)
((free >= 15)) || die "only ${free} GB free in $WORK; the build needs about 15 GB (set P8S_LIVE_WORK=/a/bigger/disk)"

say "2/6 Installer program"
(cd "$REPO" && go test ./internal/installer >/dev/null) || die "installer unit tests failed: cd $REPO && go test ./internal/installer"
(cd "$REPO" && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$WORK/p8s-install" ./cmd/p8s-install)
echo "built p8s-install"

say "3/6 Live system recipe"
B=$WORK/build
mkdir -p "$B"
cd "$B"
lb clean >/dev/null 2>&1 || true          # keeps the package cache
rm -rf config auto local
lb config \
  --distribution trixie \
  --archive-areas "main contrib non-free-firmware" \
  --debian-installer none \
  --binary-image iso-hybrid \
  --bootloaders grub-efi,syslinux \
  --linux-packages "linux-image linux-headers" \
  --security true --updates true \
  --firmware-chroot true --firmware-binary false \
  --memtest none \
  --iso-volume "P8SOS_INSTALL" \
  --iso-application "P8S OS installer" \
  --iso-publisher "P8S" \
  --bootappend-live "boot=live components quiet hostname=p8s-installer username=installer p8s.installer" \
  --bootappend-live-failsafe "boot=live components nomodeset hostname=p8s-installer username=installer p8s.installer" \
  >/dev/null

# Everything a P8S server runs, so the first start doesn't download it, plus
# what the installer itself needs. ZFS is compiled here once (DKMS).
cat > config/package-lists/p8s.list.chroot <<'EOF'
zfs-dkms
zfsutils-linux
zfs-initramfs
dkms
grub-efi-amd64-bin
grub-pc-bin
efibootmgr
mokutil
dosfstools
gdisk
parted
squashfs-tools
whiptail
ifupdown
wpasupplicant
openssh-server
sudo
curl
jq
git
make
golang-go
ca-certificates
nftables
zstd
lzop
smartmontools
iputils-ping
openssh-client
wireguard-tools
lxc
uidmap
swtpm
swtpm-tools
qemu-system-x86
qemu-utils
libvirt-daemon-system
libvirt-clients
libvirt-daemon-config-network
dnsmasq-base
cloud-image-utils
ovmf
bridge-utils
nfs-common
open-iscsi
plymouth
plymouth-themes
console-setup
locales
less
nano
vim-tiny
bash-completion
pciutils
usbutils
ethtool
rsync
EOF

mkdir -p config/includes.chroot_after_packages/usr/local/sbin \
         config/includes.chroot_after_packages/etc/systemd/system \
         config/includes.chroot_after_packages/opt/p8sos
install -m 0755 "$WORK/p8s-install" config/includes.chroot_after_packages/usr/local/sbin/p8s-install
install -m 0644 "$REPO/iso/p8s-installer.service" config/includes.chroot_after_packages/etc/systemd/system/
tar -C "$REPO" --exclude=.git --exclude=bin --exclude=dist --exclude='*.iso' --exclude='*.tar.gz' -cf - . \
  | tar -C config/includes.chroot_after_packages/opt/p8sos -xf -
find config/includes.chroot_after_packages/opt/p8sos -type f \( -name '*.sh' -o -name p8 -o -name '*.service' -o -name '*.cfg' \) -exec sed -i 's/\r$//' {} +

cat > config/hooks/normal/9000-p8s.hook.chroot <<'EOF'
#!/bin/sh
# Runs inside the live system while it is built.
set -e
# 1. ZFS must be compiled for every kernel in the image
for k in $(ls /lib/modules); do
  if ! find "/lib/modules/$k" -name 'zfs.ko*' | grep -q .; then dkms autoinstall -k "$k"; fi
  find "/lib/modules/$k" -name 'zfs.ko*' | grep -q . || { echo "P8S: ZFS module missing for kernel $k" >&2; exit 1; }
done
# 2. Boot loader packages the installer puts on the new system (UEFI or BIOS;
#    the two can't be installed together, so the live system has only the -bin parts)
mkdir -p /opt/p8s-debs/efi/partial /opt/p8s-debs/bios/partial
apt-get install -y --download-only -o Dir::Cache::archives=/opt/p8s-debs/efi grub-efi-amd64
apt-get install -y --download-only -o Dir::Cache::archives=/opt/p8s-debs/bios grub-pc
rm -rf /opt/p8s-debs/efi/partial /opt/p8s-debs/bios/partial /opt/p8s-debs/*/lock
ls /opt/p8s-debs/efi/grub-efi-amd64_*.deb /opt/p8s-debs/bios/grub-pc_*.deb >/dev/null
# 3. The installer on the first screen; no SSH into the live system (the new
#    system turns SSH back on)
chmod +x /opt/p8sos/install.sh /opt/p8sos/update.sh /opt/p8sos/iso/*.sh /opt/p8sos/scripts/*.sh /opt/p8sos/scripts/p8
systemctl enable p8s-installer.service
systemctl disable ssh.service ssh.socket 2>/dev/null || true
echo "P8S OS installer" > /etc/p8s-installer-release
EOF
chmod +x config/hooks/normal/9000-p8s.hook.chroot
echo "recipe in $B/config"

say "4/6 Building the live system (30-60 min the first time; log: $WORK/build.log)"
if ! lb build >"$WORK/build.log" 2>&1; then
  tail -40 "$WORK/build.log"
  die "live-build failed; full log: $WORK/build.log"
fi
ISO=$(ls -1 "$B"/*.hybrid.iso 2>/dev/null | head -1)
[[ -s $ISO ]] || { tail -40 "$WORK/build.log"; die "no ISO came out; full log: $WORK/build.log"; }
echo "live system: $(du -h "$ISO" | cut -f1)"

say "5/6 Boot menus: P8S names and logo"
P=$WORK/post
rm -rf "$P"; mkdir -p "$P"
MAP=()
CFGS=$(for d in /isolinux /boot/grub; do xorriso -indev "$ISO" -find "$d" -type f -name '*.cfg' 2>/dev/null; done | tr -d "'")
for f in $CFGS; do
  dst=$P/$(echo "$f" | tr / _)
  xorriso -osirrox on -indev "$ISO" -extract "$f" "$dst" >/dev/null 2>&1 || continue
  chmod u+w "$dst"
  cp "$dst" "$dst.orig"
  sed -i -E \
    -e 's/Live system \(([^)]*) fail-safe mode\)/Install P8S OS (safe graphics)/g' \
    -e 's/Live system \(([^)]*)\)/Install P8S OS/g' \
    -e 's/Live system/P8S OS installer/g' \
    "$dst"
  [[ -n ${ANSWERS:-} ]] && sed -i 's/ p8s\.installer/ p8s.installer p8s.answers=\/answers.json/g' "$dst"
  cmp -s "$dst" "$dst.orig" || MAP+=(-map "$dst" "$f")
done
[[ -f $BRAND/iso/splash.png ]] && xorriso -indev "$ISO" -find /isolinux/splash.png 2>/dev/null | grep -q . \
  && MAP+=(-map "$BRAND/iso/splash.png" /isolinux/splash.png)
[[ -f $BRAND/grub/background.png ]] && xorriso -indev "$ISO" -find /boot/grub/splash.png 2>/dev/null | grep -q . \
  && MAP+=(-map "$BRAND/grub/background.png" /boot/grub/splash.png)
[[ -n ${ANSWERS:-} ]] && { MAP+=(-map "$ANSWERS" /answers.json); echo "answers file baked in: installs WITHOUT questions"; }
echo "$((${#MAP[@]} / 3)) file(s) replaced"

say "6/6 ISO"
mkdir -p "$OUT_DIR"
NAME=p8sos-installer-$VERSION-amd64.iso
[[ -n ${ANSWERS:-} ]] && NAME=p8sos-installer-$VERSION-auto-amd64.iso
OUT=$OUT_DIR/$NAME
rm -f "$OUT"
if ((${#MAP[@]})); then
  xorriso -indev "$ISO" -outdev "$OUT" "${MAP[@]}" -boot_image any replay 2>&1 | grep -E 'Written to medium|FAILURE|SORRY' || true
else
  cp "$ISO" "$OUT"
fi
[[ -s $OUT ]] || die "ISO build failed"
SUM=$(sha256sum "$OUT" | awk '{print $1}')
echo "$SUM  $NAME" > "$OUT.sha256"
[[ -n ${SUDO_USER:-} ]] && chown "$SUDO_USER": "$OUT" "$OUT.sha256" 2>/dev/null || true
printf '\n\033[1;32m✔ Built %s (%s)\033[0m\nsha256 %s\n' "$OUT" "$(du -h "$OUT" | cut -f1)" "$SUM"
