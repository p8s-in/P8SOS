#!/usr/bin/env bash
# P8S OS host tuning for VM/container workloads (safe to run again).
#  - ZFS ARC limited to 10% of RAM (1-16 GiB) so the cache never fights VMs for memory
#  - KSM merges identical memory pages between VMs (more VMs per GB of RAM)
#  - vhost_net loaded at boot (kernel-side virtio networking)
#  - low swappiness so running guests stay in RAM
# A setting someone else wrote (no "p8s-tune" marker) is left alone.
set -euo pipefail
[[ $EUID -eq 0 ]] || { echo "run as root" >&2; exit 1; }

mark="# p8s-tune"
mem_kb=$(awk '/^MemTotal:/{print $2}' /proc/meminfo)
mem=$((mem_kb * 1024))
gib=$((1024 * 1024 * 1024))

# --- ZFS ARC -----------------------------------------------------------------
# `tune.sh --arc-max MIB` (Server page → Memory) sets the limit by hand;
# `tune.sh --arc-max auto` goes back to 10% of RAM.
conf=/etc/modprobe.d/zfs.conf
umark="# p8s-dashboard: ZFS cache limit"
set_arc() { # bytes, comment
  { grep -v -e zfs_arc_max -e "$mark" -e "$umark" "$conf" 2>/dev/null || true
    echo "$2"
    echo "options zfs zfs_arc_max=$1"; } >"$conf.new"
  mv "$conf.new" "$conf"
  if [[ -w /sys/module/zfs/parameters/zfs_arc_max ]]; then
    min=$(cat /sys/module/zfs/parameters/zfs_arc_min 2>/dev/null || echo 0)
    if ((min > 0 && $1 <= min)); then echo "$((min / 2))" >/sys/module/zfs/parameters/zfs_arc_min || true; fi
    echo "$1" >/sys/module/zfs/parameters/zfs_arc_max || true
  fi
  # the root pool reads this from the initramfs on boot
  command -v update-initramfs >/dev/null && update-initramfs -u >/dev/null 2>&1 || true
}
auto_arc=$((mem / 10))
((auto_arc < gib)) && auto_arc=$gib
((auto_arc > 16 * gib)) && auto_arc=$((16 * gib))
# never more than half of RAM on small machines
((auto_arc > mem / 2)) && auto_arc=$((mem / 2))

if [[ ${1:-} == --arc-max ]]; then
  case ${2:-} in
    auto) set_arc "$auto_arc" "$mark: ARC max = 10% of RAM"
          echo "ZFS cache limit: $((auto_arc >> 20)) MiB (10% of RAM)" ;;
    ''|*[!0-9]*) echo "usage: tune.sh --arc-max MIB|auto" >&2; exit 2 ;;
    *) b=$(($2 << 20))
       ((b > mem * 3 / 4)) && { echo "that's more than 3/4 of the RAM ($((mem >> 20)) MiB)" >&2; exit 2; }
       set_arc "$b" "$umark"
       echo "ZFS cache limit: $2 MiB" ;;
  esac
  [[ -r /proc/spl/kstat/zfs/arcstats ]] && echo "ZFS cache now: $(awk '$1=="size"{print int($3/1048576)}' /proc/spl/kstat/zfs/arcstats) MiB (shrinks within a minute)"
  exit 0
fi

if [[ ! -f $conf ]] || ! grep -q zfs_arc_max "$conf" || grep -q "$mark" "$conf"; then
  set_arc "$auto_arc" "$mark: ARC max = 10% of RAM"
  echo "ZFS ARC max: $((auto_arc / 1024 / 1024)) MiB"
else
  echo "ZFS ARC max: set by hand in $conf, left alone"
fi

# --- KSM -----------------------------------------------------------------------
if [[ -e /sys/kernel/mm/ksm/run ]]; then
  cat >/etc/tmpfiles.d/p8s-ksm.conf <<EOF
$mark: share identical memory pages between VMs
w /sys/kernel/mm/ksm/run - - - - 1
w /sys/kernel/mm/ksm/pages_to_scan - - - - 1000
w /sys/kernel/mm/ksm/sleep_millisecs - - - - 200
EOF
  systemd-tmpfiles --create /etc/tmpfiles.d/p8s-ksm.conf 2>/dev/null || true
  echo "KSM: on"
fi

# --- vhost_net -------------------------------------------------------------------
printf '%s\nvhost_net\n' "$mark" >/etc/modules-load.d/p8s.conf
modprobe vhost_net 2>/dev/null && echo "vhost_net: loaded" || echo "vhost_net: not available"

# --- sysctl ------------------------------------------------------------------------
cat >/etc/sysctl.d/90-p8s.conf <<EOF
$mark
vm.swappiness = 10
# many VMs/containers open many files and inotify watches
fs.inotify.max_user_instances = 1024
fs.inotify.max_user_watches = 1048576
fs.aio-max-nr = 1048576
EOF
sysctl -q -p /etc/sysctl.d/90-p8s.conf 2>/dev/null || true
echo "sysctl: swappiness 10, larger inotify/aio limits"
