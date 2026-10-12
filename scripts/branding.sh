#!/usr/bin/env bash
# P8S OS branding on the server itself, like Proxmox does on top of Debian:
# boot menu, console login screen, SSH welcome. Debian stays underneath
# (/etc/os-release is left alone so apt and other tools keep working).
# Safe to run again; install.sh / update.sh run it every time.
set -euo pipefail
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:$PATH
[[ $EUID -eq 0 ]] || { echo "run as root" >&2; exit 1; }
VER=$(p8sd version 2>/dev/null || echo alpha)

# 1. boot menu: "P8S OS GNU/Linux" instead of "Debian GNU/Linux"
if [[ -d /etc/default/grub.d || -f /etc/default/grub ]]; then
  mkdir -p /etc/default/grub.d
  want='GRUB_DISTRIBUTOR="P8S OS"'
  if [[ $(cat /etc/default/grub.d/p8s.cfg 2>/dev/null) != "$want" ]]; then
    echo "$want" > /etc/default/grub.d/p8s.cfg
    command -v update-grub >/dev/null && update-grub >/dev/null 2>&1 || true
  fi
fi

# 2. console login screen (\4 = this server's IPv4 address, filled in by getty)
if [[ ! -f /etc/systemd/system/p8s-firstboot.service || -f /var/lib/p8s/.firstboot-done ]]; then
  cat > /etc/issue <<'ISSUE'

  P8S OS  -  virtual machines and containers
  ------------------------------------------
  Dashboard:  https://\4:8006

  Sign in here or over SSH, then try:  p8 status  |  p8 list  |  p8 ct

ISSUE
  printf 'P8S OS\n' > /etc/issue.net
fi

# 3. SSH / console welcome after login (replaces Debian's legal notice and uname line)
cat > /etc/motd <<'MOTD'
MOTD
chmod -x /etc/update-motd.d/10-uname 2>/dev/null || true
cat > /etc/update-motd.d/05-p8s <<'SH'
#!/bin/sh
ip=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for (i=1;i<NF;i++) if ($i=="src") print $(i+1)}')
ver=$(p8sd version 2>/dev/null || echo alpha)
printf '\n  \033[1mP8S OS\033[0m %s  (Debian %s, kernel %s)\n' "$ver" "$(cat /etc/debian_version 2>/dev/null)" "$(uname -r)"
printf '  Dashboard: https://%s:8006\n' "${ip:-<this-server>}"
printf '  Commands:  p8 status | p8 list | p8 ct | p8 help\n\n'
SH
chmod 0755 /etc/update-motd.d/05-p8s

# 4. what P8S OS version this is (for support and scripts)
printf 'P8S OS %s\nBased on Debian %s\n' "$VER" "$(cat /etc/debian_version 2>/dev/null)" > /etc/p8s-release
echo "branding: P8S OS $VER"
