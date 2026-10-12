#!/usr/bin/env bash
# Add a container template: an official linuxcontainers.org "cloud" image
# (cloud-init inside) unpacked once on ZFS, owned by the unprivileged id range;
# every container is then an instant ZFS clone of it.
#
#   sudo ./make-ct-template.sh debian-13      add one
#   ./make-ct-template.sh --list              what can be added
#
# The download is checked against the image server's SHA256SUMS.
set -euo pipefail
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:$PATH

CATALOG="debian-13 debian-12 ubuntu-24.04 ubuntu-22.04 alpine-3.24 rocky-9 alma-9 fedora-44 archlinux"
SERVER=${SERVER:-https://images.linuxcontainers.org}
BASE=100000

# catalog NAME -> sets DIST REL LABEL
catalog() {
  case $1 in
    debian-13)    DIST=debian      REL=trixie   LABEL="Debian 13 (trixie)" ;;
    debian-12)    DIST=debian      REL=bookworm LABEL="Debian 12 (bookworm)" ;;
    ubuntu-24.04) DIST=ubuntu      REL=noble    LABEL="Ubuntu 24.04 LTS" ;;
    ubuntu-22.04) DIST=ubuntu      REL=jammy    LABEL="Ubuntu 22.04 LTS" ;;
    alpine-3.24)  DIST=alpine      REL=3.24     LABEL="Alpine Linux 3.24" ;;
    rocky-9)      DIST=rockylinux  REL=9        LABEL="Rocky Linux 9" ;;
    alma-9)       DIST=almalinux   REL=9        LABEL="AlmaLinux 9" ;;
    fedora-44)    DIST=fedora      REL=44       LABEL="Fedora 44" ;;
    archlinux)    DIST=archlinux   REL=current  LABEL="Arch Linux" ;;
    *) return 1 ;;
  esac
}

NAME=""
for a in "$@"; do
  case $a in
    --list) for n in $CATALOG; do catalog "$n"; printf '  %-14s %s\n' "$n" "$LABEL"; done; exit 0 ;;
    -*) echo "unknown option $a" >&2; exit 1 ;;
    *) NAME=$a ;;
  esac
done
[[ -n $NAME ]] || { echo "usage: $0 NAME   (see: $0 --list)" >&2; exit 1; }
catalog "$NAME" || { echo "unknown container template '$NAME'. Available:" >&2; "$0" --list >&2; exit 1; }
[[ $EUID -eq 0 ]] || { echo "run as root: sudo $0 $*" >&2; exit 1; }
command -v lxc-usernsexec >/dev/null || { echo "lxc is not installed (sudo bash update.sh installs it)" >&2; exit 1; }

POOL=${POOL:-tank}
DS=$POOL/p8s/ct-templates/$NAME
MNT=/var/lib/p8s/ct-templates/$NAME
if zfs list -H "$DS@base" >/dev/null 2>&1; then echo "==> $NAME is already added"; exit 0; fi

# IPv4 first: many networks hand out IPv6 addresses without a working IPv6 route
get() { curl -4 "$@" 2>/dev/null || curl "$@"; }

echo "==> finding the newest $LABEL image"
LINE=$(get -fsSL --retry 3 "$SERVER/meta/1.0/index-system" | grep "^$DIST;$REL;amd64;cloud;" | tail -1 || true)
[[ -n $LINE ]] || { echo "no amd64 cloud image for $DIST $REL on $SERVER" >&2; exit 1; }
DIR=$(cut -d';' -f6 <<<"$LINE")
URL=$SERVER$DIR
echo "    $URL"

WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT
echo "==> downloading (usually 50-200 MB)"
get -fSL --retry 3 -o "$WORK/rootfs.tar.xz" "${URL}rootfs.tar.xz"
get -fsSL --retry 3 -o "$WORK/SHA256SUMS" "${URL}SHA256SUMS"
echo "==> checking the checksum"
want=$(awk '$2 == "rootfs.tar.xz" || $2 == "*rootfs.tar.xz" {print $1}' "$WORK/SHA256SUMS")
have=$(sha256sum "$WORK/rootfs.tar.xz" | cut -d' ' -f1)
[[ -n $want && $want == "$have" ]] || { echo "checksum mismatch: the download is damaged or not genuine" >&2; exit 1; }

# root ids 100000-165535 (unprivileged containers) for root
grep -q '^root:100000:65536$' /etc/subuid 2>/dev/null || echo 'root:100000:65536' >> /etc/subuid
grep -q '^root:100000:65536$' /etc/subgid 2>/dev/null || echo 'root:100000:65536' >> /etc/subgid

echo "==> unpacking into $DS"
zfs destroy -r "$DS" 2>/dev/null || true
zfs create -p -o mountpoint="$MNT" "$DS"
chown "$BASE:$BASE" "$MNT"
chmod 0755 "$MNT"
chmod o+x /var/lib/p8s /var/lib/p8s/ct-templates 2>/dev/null || true
cp "$WORK/rootfs.tar.xz" "$MNT/.rootfs.tar.xz"
# unpack as the container's root (ids shifted to 100000+), so it's unprivileged-ready
lxc-usernsexec -m "b:0:$BASE:65536" -- tar --numeric-owner -xJpf "$MNT/.rootfs.tar.xz" -C "$MNT" --exclude='./dev/*'
rm -f "$MNT/.rootfs.tar.xz"
# no machine-id / ssh host keys in the template: each container makes its own
: > "$MNT/etc/machine-id" 2>/dev/null || true
rm -f "$MNT"/etc/ssh/ssh_host_* 2>/dev/null || true
zfs set "p8s:label=$LABEL" "$DS"
zfs set "p8s:source=$URL" "$DS"
zfs snapshot "$DS@base"
echo "==> $LABEL is ready: new containers are made from it in a second"
