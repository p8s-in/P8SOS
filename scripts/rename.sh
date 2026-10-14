#!/usr/bin/env bash
# Give this server a new name: /etc/hosts first (so sudo and others resolve it
# at once), then the hostname, then p8sd. VMs keep running. In a cluster the
# master shows the new name within a few seconds.
#   sudo bash rename.sh NEWNAME
set -euo pipefail
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:$PATH
n=${1:-}
[[ $n =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$ ]] || { echo "usage: $0 NEWNAME   (lowercase letters, digits, hyphens)" >&2; exit 1; }
[[ $EUID -eq 0 ]] || { echo "run as root: sudo $0 $n" >&2; exit 1; }
old=$(hostname)
[[ $n == "$old" ]] && { echo "==> this server is already called $n"; exit 0; }
echo "==> renaming $old to $n"
if grep -qw -- "$old" /etc/hosts; then
  sed -i "s/\b$old\b/$n/g" /etc/hosts
else
  printf '127.0.1.1\t%s\n' "$n" >> /etc/hosts
fi
hostnamectl set-hostname "$n"
echo "==> /etc/hosts and the hostname say $n"
# the login screen's "ready" line and the prompt pick it up on the next login
systemctl restart p8sd
echo "==> done: this server is now $n (log out and in again to see it in the prompt)"
