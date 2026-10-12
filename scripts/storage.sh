#!/usr/bin/env bash
# P8S OS VM storage.
#   p8 storage          how big the pool is and what each disk uses
#   p8 storage grow     file-backed pool: grow it to use the system disk's free
#                       space (keeps room for Debian), and make VM disks thin
#                       (they only take the space the guest has really written)
# Never touches any disk other than the pool's own file.
set -euo pipefail
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:$PATH
POOL=${POOL:-tank}
IMG=/var/lib/p8s/$POOL.img
G=1073741824

show() {
  zpool list -o name,size,alloc,free,cap,health "$POOL"
  echo
  zfs list -r -t volume -o name,volsize,used,refreservation "$POOL/p8s" 2>/dev/null || true
}

# Space reserved by thick zvols counts as "used" even when the guest wrote nothing.
thin() {
  local v r n=0
  for v in $(zfs list -H -r -t volume -o name "$POOL/p8s" 2>/dev/null); do
    r=$(zfs get -H -o value refreservation "$v")
    if [[ $r != none ]]; then zfs set refreservation=none "$v"; n=$((n + 1)); fi
  done
  echo "==> $n disk(s) made thin"
}

grow() {
  [[ $EUID -eq 0 ]] || { echo "run as root: sudo $0 grow" >&2; exit 1; }
  thin
  if [[ ! -f $IMG ]] || ! zpool status -P "$POOL" | grep -qF "$IMG"; then
    echo "==> pool $POOL is on a whole disk, its size is that disk's size (nothing to grow)."
    echo "    For more space: add another empty disk (zpool add) or move to a bigger one."
    show; return
  fi
  local cur used avail fs reserve target
  cur=$(stat -c %s "$IMG")                                  # size the pool sees
  used=$(( $(stat -c %b "$IMG") * $(stat -c %B "$IMG") ))    # what the sparse file really occupies
  avail=$(df -B1 --output=avail "$(dirname "$IMG")" | tail -1)
  fs=$(df -B1 --output=size "$(dirname "$IMG")" | tail -1)
  reserve=$(( fs / 10 )); (( reserve < 20 * G )) && reserve=$(( 20 * G ))   # keep >=20 GB / 10% for Debian
  target=$(( (used + avail - reserve) / G * G ))
  if (( target <= cur )); then
    echo "==> pool file is already $((cur / G)) GB; the system disk has no more room to give it."
    show; return
  fi
  echo "==> growing $IMG: $((cur / G)) GB -> $((target / G)) GB (system disk keeps $((reserve / G)) GB free)"
  truncate -s "$target" "$IMG"
  zpool online -e "$POOL" "$IMG"
  show
}

case ${1:-show} in
  show) show ;;
  grow) grow ;;
  *) echo "usage: $0 [show|grow]" >&2; exit 1 ;;
esac
