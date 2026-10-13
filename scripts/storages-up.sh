#!/usr/bin/env bash
# Connect the network storages at boot (p8s-storages.service, before libvirt
# starts the VMs): NFS shares are mounted, iSCSI disks logged in and their
# ZFS pools (p8s-<name>, VM disks) imported. The list is kept by p8sd in
# /var/lib/p8s/storages.json. A storage that doesn't answer is skipped, so
# the server still boots; VMs on it then fail to start until it is back
# (Server → Network storage → Connect).
set -uo pipefail
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
LIST=${1:-/var/lib/p8s/storages.json}
ROOT=${MOUNT_ROOT:-/mnt/p8s}
[[ -s $LIST ]] || exit 0
command -v jq >/dev/null || { echo "jq missing"; exit 0; }

jq -r '.[] | [.name, .type, (.portal // ""), (.target // ""), (.lun // 0 | tostring)] | @tsv' "$LIST" |
while IFS=$'\t' read -r name type portal target lun; do
  [[ $name =~ ^[a-z][a-z0-9-]*$ ]] || continue
  case $type in
  nfs)
    dir=$ROOT/$name
    mkdir -p "$dir"
    if mountpoint -q "$dir"; then echo "$name: mounted"; continue; fi
    for i in 1 2 3; do
      timeout 40 mount "$dir" && break
      sleep 5
    done
    mountpoint -q "$dir" && echo "$name: mounted" || echo "$name: NFS share not reachable"
    ;;
  iscsi)
    pool=p8s-$name
    if zpool list -H "$pool" >/dev/null 2>&1; then echo "$name: pool $pool already imported"; continue; fi
    dev=/dev/disk/by-path/ip-$portal-iscsi-$target-lun-$lun
    timeout 30 iscsiadm -m node -T "$target" -p "$portal" --login >/dev/null 2>&1 || true
    for i in $(seq 60); do [[ -e $dev ]] && break; sleep 1; done
    if [[ ! -e $dev ]]; then echo "$name: iSCSI disk not reachable ($portal $target lun $lun)"; continue; fi
    udevadm settle --timeout=10 || true
    zpool import -d "$dev" -N -o cachefile=none "$pool" && echo "$name: pool $pool imported" || echo "$name: could not import $pool"
    ;;
  esac
done
exit 0
