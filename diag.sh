#!/usr/bin/env bash
# P8S OS: diagnose why a new VM does not boot / get an IP.
# Run inside the test VM (same way as go.sh):
#   su -c 'mount -t vboxsf P8SOS /mnt || mount -t vboxsf p8sos /mnt; SUDO_USER=vboxuser bash /mnt/diag.sh'
set -uo pipefail
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:$PATH
U=${SUDO_USER:-root}
H=$(getent passwd "$U" | cut -d: -f6)
SRC=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
OUT=$H/p8s-diag.txt
exec > >(tee "$OUT") 2>&1
line() { printf '\n\033[1;36m==== %s ====\033[0m\n' "$*"; }
[[ $EUID -eq 0 ]] || { echo "run as root"; exit 1; }

line "Update P8S OS from the shared folder and rebuild p8sd"
rm -rf "$H/p8sos"; mkdir -p "$H/p8sos"
tar -C "$SRC" --exclude=.git --exclude='*.tar.gz' --exclude=bin -cf - . | tar -C "$H/p8sos" -xf -
sed -i 's/\r$//' "$H/p8sos/install.sh" "$H/p8sos/scripts/"*.sh "$H/p8sos/scripts/p8"
chown -R "$U": "$H/p8sos"
DEV=$(zpool list -vHP tank 2>/dev/null | awk '$1 ~ /^\/dev\// {print $1; exit}')
bash "$H/p8sos/install.sh" ${DEV:-} >/dev/null 2>&1 && echo "p8sd rebuilt and restarted" || { echo "install.sh failed:"; tail -20 /var/log/p8s-install.log; exit 1; }

line "Clean up old test VMs"
for d in $(virsh list --all --name | grep '^p8s-smoke'); do
  virsh destroy "$d" >/dev/null 2>&1; virsh undefine "$d" --nvram >/dev/null 2>&1
  zfs destroy -r "tank/p8s/vms/$d-disk0" 2>/dev/null; rm -f "/var/lib/p8s/seeds/$d-seed.iso"
  echo "removed $d"
done

line "Host facts"
echo "CPU flags in this VM: $(grep -cE 'vmx|svm' /proc/cpuinfo)   /dev/kvm: $(ls /dev/kvm 2>&1)"
echo "kvm modules: $(lsmod | grep -E '^kvm' | awk '{print $1}' | xargs)"
cat /sys/module/kvm_intel/parameters/nested 2>/dev/null | sed 's/^/kvm_intel nested: /'
virsh net-list --all
free -m | head -2

line "Create test VM diag1"
sudo -u "$U" -H p8 delete diag1 -y >/dev/null 2>&1
sudo -u "$U" -H p8 create diag1 1 1024 || exit 1
DOM=p8s-diag1
LOGF=/var/log/libvirt/qemu/$DOM-console.log

line "Watching boot for up to 6 minutes"
prev=0
for i in $(seq 1 18); do
  sleep 20
  cpu=$(virsh domstats "$DOM" --cpu-total 2>/dev/null | awk -F= '/cpu.time/ {print int($2/1e9)}')
  ip=$(virsh -q domifaddr "$DOM" 2>/dev/null | awk '$3=="ipv4"{print $4}')
  lines=$(wc -l < "$LOGF" 2>/dev/null || echo 0)
  printf '[%3ss] state=%s  guest-cpu=%ss (+%s)  console-lines=%s  ip=%s\n' $((i*20)) "$(virsh -q domstate $DOM)" "${cpu:-?}" $(( ${cpu:-0} - prev )) "$lines" "${ip:--}"
  prev=${cpu:-0}
  [[ -n $ip ]] && break
done

line "Last 40 lines of the VM's console (boot messages)"
tail -n 40 "$LOGF" 2>/dev/null || echo "(no console log at $LOGF)"

line "DHCP leases on the NAT network"
virsh net-dhcp-leases default

line "QEMU log (errors)"
tail -n 15 "/var/log/libvirt/qemu/$DOM.log"

line "Done"
echo "Saved to $OUT. Send me a screenshot of this window (scroll up if needed)."
echo "The VM diag1 is left running. Remove it later with: p8 delete diag1"
