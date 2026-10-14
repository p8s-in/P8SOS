#!/usr/bin/env bash
# Open (map) this server's Ceph disks at boot, before the VMs start, and close
# them at shutdown, after the VMs stopped. p8sd keeps the list: /etc/p8s/rbd-maps
# (one pool/image per line: the disks of the VMs defined on this server).
#   ceph-map.sh map | unmap | list
set -uo pipefail
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
LIST=${P8S_RBD_MAPS:-/etc/p8s/rbd-maps}

images() { [[ -f $LIST ]] && grep -v -e '^[[:space:]]*#' -e '^[[:space:]]*$' "$LIST"; }

case ${1:-map} in
map)
  [[ -f /etc/ceph/ceph.conf ]] && images >/dev/null || exit 0
  modprobe rbd 2>/dev/null || true
  # the monitors may still be starting (this server's, or the others after a power cut)
  for _ in $(seq 60); do
    timeout 15 ceph --connect-timeout 5 health >/dev/null 2>&1 && break
    sleep 2
  done
  n=0 bad=0
  while read -r img; do
    [[ -e /dev/rbd/$img ]] && continue
    if timeout 90 rbd device map --options ms_mode=secure "$img" >/dev/null; then
      n=$((n + 1))
    else
      echo "p8s: could not open Ceph disk $img (its VM won't start until Ceph is reachable)" >&2
      bad=$((bad + 1))
    fi
  done < <(images)
  udevadm settle --timeout=30 || true
  echo "p8s: $n Ceph disk(s) opened, $bad failed"
  ;;
unmap)
  for d in /dev/rbd/*/*; do
    [[ -e $d ]] || continue
    timeout 60 rbd device unmap "$d" 2>/dev/null || timeout 30 rbd device unmap -o force "$d" || echo "p8s: could not close $d" >&2
  done
  ;;
list)
  images
  ;;
*)
  echo "usage: $0 map|unmap|list" >&2
  exit 1
  ;;
esac
exit 0
