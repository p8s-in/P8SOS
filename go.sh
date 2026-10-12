#!/usr/bin/env bash
# P8S OS: one-shot "install + test" inside a VirtualBox/VMware test VM.
# Run inside the VM:
#   sudo mount -t vboxsf p8sos /mnt && sudo bash /mnt/go.sh
set -uo pipefail
U=${SUDO_USER:-root}
H=$(getent passwd "$U" | cut -d: -f6)
SRC=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
LOG=$H/p8s-run.log
exec > >(tee -a "$LOG") 2>&1

line() { printf '\n\033[1;36m==== %s ====\033[0m\n' "$*"; }
fail() { printf '\n\033[1;31m✘ %s\033[0m\n' "$*"; exit 1; }

[[ $EUID -eq 0 ]] || { echo "Run with sudo: sudo bash $0"; exit 1; }

line "1. Can this VM run VMs?"
n=$(grep -cE 'vmx|svm' /proc/cpuinfo)
echo "virtualization flags: $n"
(( n > 0 )) || fail "No VT-x inside the VM. Power the VM OFF fully (not save state) and start again. On Windows run windows\\check-mode.ps1 — it must say P8S TEST MODE."

line "2. Copy P8S OS into $H/p8sos"
rm -rf "$H/p8sos"
mkdir -p "$H/p8sos"
tar -C "$SRC" --exclude=.git --exclude='*.tar.gz' --exclude=bin -cf - . | tar -C "$H/p8sos" -xf -
sed -i 's/\r$//' "$H/p8sos/install.sh" "$H/p8sos/scripts/"*.sh "$H/p8sos/scripts/p8"
chown -R "$U": "$H/p8sos"
echo "copied: $(find "$H/p8sos" -type f | wc -l) files"

line "3. Find the empty storage disk"
lsblk -dno NAME,SIZE,TYPE | grep disk
ROOTDISK=$(lsblk -no PKNAME "$(findmnt -no SOURCE /)" | head -1)
DEV=""
for d in $(lsblk -dno NAME,TYPE | awk '$2=="disk"{print $1}'); do
  [[ $d == "$ROOTDISK" ]] && continue
  [[ -z $(lsblk -no FSTYPE "/dev/$d" | tr -d '[:space:]') ]] && [[ $(lsblk -no NAME "/dev/$d" | wc -l) -eq 1 ]] && { DEV=/dev/$d; break; }
done
[[ -n $DEV ]] || fail "No empty second disk found. Add a 64 GB disk in VirtualBox Settings → Storage."
echo "Debian is on /dev/$ROOTDISK. Empty disk for VM storage: $DEV ($(lsblk -dno SIZE "$DEV"))"

line "4. Install P8S OS (10-20 min). Type yes when asked to use $DEV."
bash "$H/p8sos/install.sh" "$DEV" || fail "install.sh failed — see the red message above"

line "5. Full automatic test (3-8 min)"
sudo -u "$U" -H env SLOW=1 /usr/local/bin/p8 test
rc=$?

line "Done"
if (( rc == 0 )); then
  printf '\033[1;32m✔ P8S OS installed and every test passed.\033[0m\n'
else
  printf '\033[1;31m✘ Some tests failed.\033[0m Run:  p8 report   and send p8s-report.txt\n'
fi
echo "Full log: $LOG"
echo "Try it:  p8 create web1   then   p8 ssh web1"
