#!/usr/bin/env bash
# Connect the network storages at boot (p8s-storages.service, before libvirt
# starts the VMs): NFS shares are mounted, iSCSI disks logged in and their
# ZFS pools (p8s-<name>, VM disks) imported. The list is kept by p8sd in
# /var/lib/p8s/storages.json. A storage that doesn't answer is skipped, so
# the server still boots; VMs on it then fail to start until it is back
# (Server → Network storage → Connect).
set -uo pipefail
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
STOP=0
[[ ${1:-} == --stop ]] && { STOP=1; shift; }
LIST=${1:-/var/lib/p8s/storages.json}
ROOT=${MOUNT_ROOT:-/mnt/p8s}
[[ -s $LIST ]] || exit 0
command -v jq >/dev/null || { echo "jq missing"; exit 0; }

# At shutdown (after libvirt stopped the VMs, before the network goes): close
# the VM-disk pools that live in files on NFS shares, so the share can unmount.
if (( STOP )); then
  jq -r '.[] | select(.type == "nfs" and (.vm_disks // false)) | .name' "$LIST" | while read -r name; do
    [[ $name =~ ^[a-z][a-z0-9-]*$ ]] || continue
    zpool list -H "p8s-$name" >/dev/null 2>&1 && { zpool export "p8s-$name" && echo "$name: pool closed" || echo "$name: pool busy"; }
  done
  exit 0
fi

jq -r '.[] | [.name, .type, (.portal // ""), (.target // ""), (.lun // 0 | tostring), (.vm_disks // false | tostring)] | @tsv' "$LIST" |
while IFS=$'\t' read -r name type portal target lun vmdisks; do
  [[ $name =~ ^[a-z][a-z0-9-]*$ ]] || continue
  case $type in
  nfs)
    dir=$ROOT/$name
    mkdir -p "$dir"
    if ! mountpoint -q "$dir"; then
      for i in 1 2 3; do
        timeout 40 mount "$dir" && break
        sleep 5
      done
    fi
    if ! mountpoint -q "$dir"; then echo "$name: NFS share not reachable"; continue; fi
    echo "$name: mounted"
    if [[ $vmdisks == true ]]; then # its ZFS pool for VM disks is a file on the share
      pool=p8s-$name
      if zpool list -H "$pool" >/dev/null 2>&1; then echo "$name: pool $pool already imported"
      else zpool import -d "$dir/p8s" -N -o cachefile=none "$pool" && echo "$name: pool $pool imported" || echo "$name: could not import $pool"; fi
    fi
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
