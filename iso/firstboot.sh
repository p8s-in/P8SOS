#!/usr/bin/env bash
# P8S OS first boot: runs install.sh unattended, then shows the result on the
# login screen. Retries on the next boot if anything fails.
set -uo pipefail
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
LOG=/var/log/p8s-firstboot.log
exec > >(tee -a "$LOG") 2>&1
say() { printf '\n[P8S OS] %s\n' "$*"; }

say "First-boot setup starting $(date)"

# 1. Internet (packages, ZFS, template download)
for i in $(seq 60); do
  curl -fsS -o /dev/null --max-time 5 https://deb.debian.org/debian/dists/trixie/Release && break
  say "waiting for internet ($i/60)..."; sleep 5
done

# 2. The admin user = first normal account created in the installer
USER_NAME=$(getent passwd | awk -F: '$3 >= 1000 && $3 < 60000 {print $1; exit}')
[[ -n $USER_NAME ]] || USER_NAME=root
usermod -aG sudo "$USER_NAME" 2>/dev/null || true

# 3. VM storage: the pool the P8S installer made; else an EMPTY second disk if
#    there is one; else a file pool on the system disk.
ROOTDISK=$(lsblk -no PKNAME "$(findmnt -no SOURCE /)" 2>/dev/null | head -1)
DEV=""
if zpool list -H tank >/dev/null 2>&1; then
  say "VM storage: ZFS pool 'tank' made by the installer ($(zpool list -H -o size tank), $(cat /etc/p8s/installed.json 2>/dev/null | jq -r .layout 2>/dev/null || echo zfs))"
  ROOTDISK=all   # don't look for spare disks
fi
for d in $(lsblk -dno NAME,TYPE | awk '$2=="disk"{print $1}'); do
  [[ $ROOTDISK == all ]] && break
  [[ $d == "$ROOTDISK" ]] && continue
  [[ $(lsblk -no NAME "/dev/$d" | wc -l) -eq 1 ]] || continue          # has partitions -> not empty
  [[ -z $(blkid -p "/dev/$d" 2>/dev/null) ]] || continue               # has a filesystem/signature -> not empty
  DEV=/dev/$d; break
done
if [[ $ROOTDISK == all ]]; then
  :
elif [[ -n $DEV ]]; then
  say "VM storage: empty disk $DEV ($(lsblk -dno SIZE "$DEV")) will hold the ZFS pool"
else
  say "VM storage: no empty second disk found -> using a file-backed pool on /dev/$ROOTDISK (fine for alpha)"
fi

# 4. Install everything
ENC=""
[[ -f /etc/p8s/encrypt-storage ]] && { ENC=--encrypt; say "VM storage will be encrypted (key in /etc/p8s/keys — back it up!)"; }
if SUDO_USER=$USER_NAME P8S_YES=1 bash /opt/p8sos/install.sh $ENC $DEV; then
  touch /var/lib/p8s/.firstboot-done
  systemctl disable p8s-firstboot.service >/dev/null 2>&1
  cat > /etc/issue <<EOF

  P8S OS (alpha) is ready.   Host IP: \4

  Log in as '$USER_NAME', then try:
      p8 status          is everything running?
      p8 create web1     make a VM
      p8 ssh web1        log into it
      p8 test            full self-test

EOF
  say "DONE. Log in as $USER_NAME and run: p8 status"
else
  cat > /etc/issue <<EOF

  P8S OS (alpha): first-boot setup FAILED. It will retry on the next reboot.
  Details: /var/log/p8s-firstboot.log   (log in as '$USER_NAME', then: sudo tail -50 /var/log/p8s-firstboot.log)

EOF
  say "FAILED — see $LOG. Reboot to retry."
  exit 1
fi
