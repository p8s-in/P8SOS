#!/bin/bash
# P8S OS: turn the CPU's IOMMU on or off for PCI passthrough (graphics cards,
# network cards, NVMe to VMs). Needs a reboot. The firmware (BIOS/UEFI) must
# also have it on: Intel "VT-d", AMD "AMD-Vi"/"IOMMU"/"SVM".
#   iommu.sh on | off | status
set -euo pipefail
CFG=/etc/default/grub.d/p8s-iommu.cfg
MODS=/etc/modules-load.d/p8s-vfio.conf
groups() { ls /sys/kernel/iommu_groups 2>/dev/null | wc -l; }
vendor() { grep -m1 -o 'GenuineIntel\|AuthenticAMD' /proc/cpuinfo || true; }

case ${1:-status} in
  status)
    n=$(groups)
    if (( n > 0 )); then echo "IOMMU is on ($n groups): PCI devices can be given to VMs"
    elif [[ -f $CFG ]]; then echo "IOMMU is set up but not active: reboot; if it stays off, turn on VT-d / AMD-Vi in the BIOS"
    else echo "IOMMU is off: run 'p8 iommu on' (then reboot)"; fi ;;
  on)
    case $(vendor) in
      GenuineIntel) opt="intel_iommu=on iommu=pt" ;;
      AuthenticAMD) opt="amd_iommu=on iommu=pt" ;;
      *) echo "unknown CPU vendor; set the kernel option by hand" >&2; exit 1 ;;
    esac
    install -d -m 0755 /etc/default/grub.d
    printf '# P8S OS: IOMMU for PCI passthrough (p8 iommu off removes it)\nGRUB_CMDLINE_LINUX_DEFAULT="$GRUB_CMDLINE_LINUX_DEFAULT %s"\n' "$opt" >"$CFG"
    printf '# P8S OS: VFIO for PCI passthrough\nvfio\nvfio_iommu_type1\nvfio_pci\n' >"$MODS"
    echo "→ kernel option: $opt"
    update-grub 2>&1 | tail -2
    [[ -x /usr/local/sbin/p8s-esp-sync ]] && /usr/local/sbin/p8s-esp-sync || true
    modprobe vfio_pci 2>/dev/null || true
    if (( $(groups) > 0 )); then echo "✔ IOMMU is already active"
    else echo "✔ done. Reboot the server (Server page → Reboot) to switch it on."
      echo "  If it is still off after the reboot, turn on VT-d (Intel) or AMD-Vi/IOMMU (AMD) in the BIOS."; fi ;;
  off)
    rm -f "$CFG" "$MODS"
    update-grub 2>&1 | tail -2
    [[ -x /usr/local/sbin/p8s-esp-sync ]] && /usr/local/sbin/p8s-esp-sync || true
    echo "✔ IOMMU option removed; reboot to finish" ;;
  *) echo "usage: iommu.sh on|off|status" >&2; exit 2 ;;
esac
