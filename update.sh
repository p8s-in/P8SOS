#!/usr/bin/env bash
# Update an installed P8S OS host from this folder (e.g. copied over on a USB stick).
#   sudo bash update.sh
# Your VMs, config and token are kept. p8sd restarts (VMs keep running).
set -euo pipefail
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:$PATH
[[ $EUID -eq 0 ]] || { echo "Run with sudo: sudo bash $0"; exit 1; }
SRC=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
DST=/opt/p8sos
if [[ $SRC != "$DST" ]]; then
  [[ -d $DST ]] && cp -a "$DST" "$DST.bak.$(date +%Y%m%d-%H%M%S)"
  mkdir -p "$DST"
  tar -C "$SRC" --exclude=.git --exclude=dist --exclude=bin -cf - . | tar -C "$DST" -xf -
  find "$DST" -type f \( -name '*.sh' -o -name p8 \) -exec sed -i 's/\r$//' {} +
  echo "copied the new version to $DST"
fi
U=${SUDO_USER:-$(getent passwd | awk -F: '$3 >= 1000 && $3 < 60000 {print $1; exit}')}
SUDO_USER=$U bash "$DST/install.sh"
