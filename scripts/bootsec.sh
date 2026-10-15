#!/usr/bin/env bash
# Boot security for P8S OS (p8 secureboot …, p8 boot-unlock …, p8 disk-passphrase).
#
#   secureboot status      Secure Boot state, the module signing key, the boot entry
#   secureboot setup       make this server ready for Secure Boot:
#                          signed shim + GRUB (Debian's), a key (MOK) that signs the
#                          ZFS module (DKMS signs it again on every kernel update),
#                          a new boot entry "P8S OS (Secure Boot)" next to the old one,
#                          and the key queued for enrolment at the next start.
#   unlock status|add KEY|list|remove N
#                          an encrypted system asks for its passphrase at every start;
#                          this lets you type it over SSH (port 2222) instead of on the
#                          server's screen: ssh -p 2222 root@SERVER, then: zfsunlock
#   passphrase             change the disk passphrase (encrypted system)
set -euo pipefail
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
[[ $EUID -eq 0 ]] || { echo "run as root: sudo $0 $*" >&2; exit 1; }

say()  { printf '%s\n' "$*"; }
ok()   { printf '  \033[32m✔\033[0m %s\n' "$*"; }
bad()  { printf '  \033[31m✘\033[0m %s\n' "$*"; }
die()  { printf '\033[31m✘ %s\033[0m\n' "$*" >&2; exit 1; }

MOK_KEY=/var/lib/dkms/mok.key
MOK_PUB=/var/lib/dkms/mok.pub
SB_DIR=/boot/efi/EFI/p8s-sb
SB_LABEL="P8S OS (Secure Boot)"
POOL=tank

sb_state() { mokutil --sb-state 2>/dev/null | head -1 || echo "unknown (mokutil missing)"; }

zfs_signer() {
  local f
  f=$(modinfo -n zfs 2>/dev/null) || { echo "no zfs module"; return; }
  modinfo -F signer "$f" 2>/dev/null | head -1 || true
}

secureboot_status() {
  [[ -d /sys/firmware/efi ]] || { say "This server started in BIOS (legacy) mode: Secure Boot needs UEFI."; return 0; }
  say "Secure Boot:        $(sb_state)"
  if [[ -f $MOK_PUB ]]; then
    if mokutil --test-key "$MOK_PUB" 2>/dev/null | grep -q "is already enrolled"; then ok "module signing key enrolled"
    elif mokutil --list-new 2>/dev/null | grep -q .; then say "  … key waiting for enrolment at the next start (blue MokManager screen)"
    else bad "module signing key not enrolled (run: p8 secureboot setup)"; fi
  else
    bad "no module signing key yet (run: p8 secureboot setup)"
  fi
  s=$(zfs_signer)
  [[ -n $s && $s != "no zfs module" ]] && ok "ZFS module signed by: $s" || bad "ZFS module not signed"
  [[ -f $SB_DIR/shimx64.efi ]] && ok "signed boot loader in $SB_DIR" || bad "signed boot loader not installed"
  efibootmgr 2>/dev/null | grep -q "$SB_LABEL" && ok "boot entry \"$SB_LABEL\"" || bad "no \"$SB_LABEL\" boot entry"
}

secureboot_setup() {
  [[ -d /sys/firmware/efi ]] || die "this server started in BIOS (legacy) mode; Secure Boot needs UEFI"
  mountpoint -q /boot/efi || die "/boot/efi isn't mounted"
  say "==> installing Debian's signed boot loader (shim, GRUB) and tools"
  DEBIAN_FRONTEND=noninteractive apt-get install -y -q shim-signed grub-efi-amd64-signed mokutil dkms openssl >/dev/null ||
    die "could not install shim-signed / grub-efi-amd64-signed (internet needed)"

  say "==> the key that signs the ZFS module (MOK)"
  if [[ ! -f $MOK_KEY || ! -f $MOK_PUB ]]; then
    install -d -m 0700 /var/lib/dkms
    if ! dkms generate_mok >/dev/null 2>&1 || [[ ! -f $MOK_KEY ]]; then
      openssl req -new -x509 -nodes -days 36500 -subj "/CN=P8S OS module signing ($(hostname))/" \
        -newkey rsa:2048 -keyout "$MOK_KEY" -outform DER -out "$MOK_PUB" >/dev/null 2>&1
    fi
    chmod 0600 "$MOK_KEY"
  fi
  ok "$MOK_PUB"

  say "==> signing the ZFS module (rebuilt by DKMS for every installed kernel)"
  v=$(dkms status zfs 2>/dev/null | head -1 | sed -E 's#^zfs/([^,: ]+).*#\1#')
  [[ -n $v ]] || die "the ZFS module isn't built by DKMS here (dkms status zfs is empty)"
  for k in /lib/modules/*; do
    k=${k##*/}
    [[ -d /lib/modules/$k/build ]] || continue
    dkms install --force -m zfs -v "$v" -k "$k" >/dev/null 2>&1 && ok "zfs $v for $k" || bad "zfs $v for $k (build failed; see /var/lib/dkms/zfs/$v/build/make.log)"
  done
  s=$(zfs_signer); [[ -n $s && $s != "no zfs module" ]] || die "the ZFS module still isn't signed; is dkms signing switched off in /etc/dkms/framework.conf?"
  update-initramfs -u -k all >/dev/null

  say "==> the signed boot loader in $SB_DIR"
  install -d "$SB_DIR"
  install -m 0644 /usr/lib/shim/shimx64.efi.signed "$SB_DIR/shimx64.efi"
  [[ -f /usr/lib/shim/mmx64.efi.signed ]] && install -m 0644 /usr/lib/shim/mmx64.efi.signed "$SB_DIR/mmx64.efi"
  [[ -f /usr/lib/shim/fbx64.efi.signed ]] && install -m 0644 /usr/lib/shim/fbx64.efi.signed "$SB_DIR/fbx64.efi"
  install -m 0644 /usr/lib/grub/x86_64-efi-signed/grubx64.efi.signed "$SB_DIR/grubx64.efi"
  # Debian's signed GRUB reads its settings from \EFI\debian\grub.cfg: point it at the real ones
  install -d /boot/efi/EFI/debian
  if [[ $(findmnt -no FSTYPE /boot) == zfs ]]; then
    cat >/boot/efi/EFI/debian/grub.cfg <<'EOF'
# P8S OS: Debian's signed GRUB starts here and loads the real menu from the boot pool
search --no-floppy --file --set=root /BOOT/debian@/grub/grub.cfg
set prefix=($root)/BOOT/debian@/grub
configfile $prefix/grub.cfg
EOF
  else
    cat >/boot/efi/EFI/debian/grub.cfg <<'EOF'
# P8S OS: Debian's signed GRUB starts here and loads the real menu
search --no-floppy --file --set=root /boot/grub/grub.cfg
set prefix=($root)/boot/grub
configfile $prefix/grub.cfg
EOF
  fi
  ok "shim + signed GRUB installed (the old boot entry stays, so turning Secure Boot off again always works)"

  say "==> boot entry \"$SB_LABEL\""
  src=$(findmnt -no SOURCE /boot/efi)
  disk=/dev/$(lsblk -no PKNAME "$src" | head -1)
  part=$(cat "/sys/class/block/$(basename "$(readlink -f "$src")")/partition")
  while read -r num; do efibootmgr -q -b "$num" -B || true; done < <(efibootmgr | awk -v l="$SB_LABEL" 'index($0, l) {sub(/^Boot/, "", $1); sub(/\*$/, "", $1); print $1}')
  efibootmgr -q -c -d "$disk" -p "$part" -L "$SB_LABEL" -l '\EFI\p8s-sb\shimx64.efi'
  ok "first in the boot order: $SB_LABEL ($disk partition $part)"
  [[ -x /usr/local/sbin/p8s-esp-sync ]] && /usr/local/sbin/p8s-esp-sync >/dev/null 2>&1 || true

  say "==> queueing the key for enrolment"
  if mokutil --test-key "$MOK_PUB" 2>/dev/null | grep -q "is already enrolled"; then
    ok "the key is enrolled already"
  else
    pw=$(printf '%08d' $(( $(od -An -N4 -tu4 /dev/urandom) % 100000000 )))
    printf '%s\n%s\n' "$pw" "$pw" | mokutil --import "$MOK_PUB" >/dev/null
    say ""
    say "  At the next start a blue screen (MokManager) appears:"
    say "    Enroll MOK → Continue → Yes → type the password  $pw  → Reboot"
    say "  (it waits about 10 seconds: press a key when it appears; you need the server's screen once)."
  fi
  say ""
  say "Then: reboot, enrol the key, and switch Secure Boot ON in the firmware setup."
  say "Check with: p8 secureboot status   (if it doesn't start: switch Secure Boot off again — the old entry still works)"
}

# --- remote unlock of an encrypted system (dropbear in the initramfs) ---

DROPBEAR_KEYS=/etc/dropbear/initramfs/authorized_keys
DROPBEAR_CONF=/etc/dropbear/initramfs/dropbear.conf

encrypted() { [[ $(zfs get -H -o value keylocation "$POOL" 2>/dev/null) == prompt ]]; }

# initramfs network: the system's static address (on the physical port of a
# bridge), else DHCP
initramfs_ip() {
  local f=/etc/network/interfaces conf=/etc/initramfs-tools/conf.d/p8s-unlock
  local line
  line=$(awk '
    /^iface [^ ]+ inet static/ { dev=$2; inblk=1; addr=""; gw=""; ports=""; next }
    /^(iface|auto|allow-|source)/ { if (inblk && addr != "") { print dev, addr, gw, ports; exit } inblk=0 }
    inblk && $1=="address" { addr=$2 } inblk && $1=="gateway" { gw=$2 } inblk && $1=="bridge_ports" { ports=$2 }
    END { if (inblk && addr != "") print dev, addr, gw, ports }' "$f" /etc/network/interfaces.d/* 2>/dev/null | head -1)
  install -d /etc/initramfs-tools/conf.d
  if [[ -z $line ]]; then
    echo 'IP=dhcp' > "$conf"; ok "the unlock SSH gets its address by DHCP"; return
  fi
  read -r dev addr gw ports <<<"$line"
  [[ -n $ports && $ports != none ]] && dev=${ports%% *}
  local ip=${addr%/*} bits=${addr#*/} mask
  [[ $bits == "$addr" ]] && bits=24
  mask=$(( 0xffffffff << (32 - bits) & 0xffffffff ))
  mask="$(( mask >> 24 & 255 )).$(( mask >> 16 & 255 )).$(( mask >> 8 & 255 )).$(( mask & 255 ))"
  echo "IP=$ip::$gw:$mask::$dev:none" > "$conf"
  ok "the unlock SSH listens on $ip (port $dev)"
}

unlock() {
  local sub=${1:-status}; shift || true
  case $sub in
    status)
      encrypted && say "The system is encrypted: it asks for the passphrase at every start." || say "The system disks aren't encrypted with a passphrase; nothing to unlock."
      if dpkg -s dropbear-initramfs >/dev/null 2>&1 && [[ -s $DROPBEAR_KEYS ]]; then
        ok "unlock over SSH: ssh -p 2222 root@<server address>, then type: zfsunlock   ($(grep -c . "$DROPBEAR_KEYS") key(s))"
      else
        say "Unlock over SSH is off. Turn it on: p8 boot-unlock add \"ssh-ed25519 AAAA… you@pc\""
      fi ;;
    add)
      local key=${1:-}
      [[ $key =~ ^(ssh-(ed25519|rsa)|ecdsa-sha2-nistp(256|384|521))\ [A-Za-z0-9+/=]+(\ .*)?$ ]] || die "usage: p8 boot-unlock add \"ssh-ed25519 AAAA… you@pc\""
      encrypted || say "Note: the system isn't encrypted with a passphrase; this only matters for one that is."
      if ! dpkg -s dropbear-initramfs >/dev/null 2>&1; then
        say "==> installing dropbear-initramfs (a tiny SSH server that runs before the system starts)"
        DEBIAN_FRONTEND=noninteractive apt-get install -y -q dropbear-initramfs >/dev/null || die "could not install dropbear-initramfs (internet needed)"
      fi
      install -d -m 0700 "$(dirname "$DROPBEAR_KEYS")"
      touch "$DROPBEAR_KEYS"; chmod 0600 "$DROPBEAR_KEYS"
      grep -qxF "$key" "$DROPBEAR_KEYS" || echo "$key" >> "$DROPBEAR_KEYS"
      # port 2222 (not 22: the client would see a different host key than the running system's), unlock only
      echo 'DROPBEAR_OPTIONS="-p 2222 -s -j -k -I 600"' > "$DROPBEAR_CONF"
      initramfs_ip
      update-initramfs -u -k all >/dev/null
      ok "at the next start: ssh -p 2222 root@<server address>  →  zfsunlock  →  type the passphrase" ;;
    list)
      [[ -s $DROPBEAR_KEYS ]] && nl -ba "$DROPBEAR_KEYS" | cut -c1-120 || say "no keys" ;;
    remove|rm)
      local n=${1:-}; [[ $n =~ ^[0-9]+$ ]] || die "usage: p8 boot-unlock remove N   (N from p8 boot-unlock list)"
      sed -i "${n}d" "$DROPBEAR_KEYS"
      update-initramfs -u -k all >/dev/null
      ok "removed key $n" ;;
    *) die "usage: p8 boot-unlock status|add KEY|list|remove N" ;;
  esac
}

passphrase() {
  encrypted || die "the system isn't encrypted with a passphrase"
  say "New disk passphrase for $POOL (at least 8 characters; asked at every start from now on)."
  read -r -s -p "New passphrase: " p1; echo
  read -r -s -p "Again: " p2; echo
  [[ $p1 == "$p2" ]] || die "the two are different"
  (( ${#p1} >= 8 )) || die "at least 8 characters"
  printf '%s\n' "$p1" | zfs change-key -o keyformat=passphrase -o keylocation=prompt "$POOL"
  ok "passphrase changed (the data isn't re-encrypted: ZFS re-wraps its key)"
}

case ${1:-} in
  secureboot) case ${2:-status} in status) secureboot_status ;; setup) secureboot_setup ;; *) die "usage: p8 secureboot status|setup" ;; esac ;;
  unlock) shift; unlock "$@" ;;
  passphrase) passphrase ;;
  *) die "usage: $0 secureboot status|setup | unlock status|add KEY|list|remove N | passphrase" ;;
esac
