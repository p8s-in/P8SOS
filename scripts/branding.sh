#!/usr/bin/env bash
# P8S OS branding on the server itself, like Proxmox does on top of Debian:
# boot menu, console login screen, SSH welcome. Debian stays underneath
# (/etc/os-release is left alone so apt and other tools keep working).
# Safe to run again; install.sh / update.sh run it every time.
set -euo pipefail
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:$PATH
[[ $EUID -eq 0 ]] || { echo "run as root" >&2; exit 1; }
VER=$(p8sd version 2>/dev/null || echo alpha)

REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." 2>/dev/null && pwd)
BRAND=${P8S_BRAND_DIR:-$REPO/assets/brand}
[[ -d $BRAND ]] || BRAND=/usr/local/share/p8s/brand
# Boot splash (Plymouth) is on unless this file exists: touch /etc/p8s/no-splash
SPLASH=1; [[ -e /etc/p8s/no-splash ]] && SPLASH=0

# the boot splash program (installed first, so the boot menu below can turn it on)
if ((SPLASH)) && { ! command -v plymouth >/dev/null || [[ ! -d /usr/share/plymouth/themes/spinner ]]; }; then
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq plymouth plymouth-themes >/dev/null 2>&1 || true
fi

# 1. boot menu: "P8S OS GNU/Linux" on the P8S background (GRUB theme)
if [[ -d /etc/default/grub.d || -f /etc/default/grub ]]; then
  mkdir -p /etc/default/grub.d
  theme_ok=0
  if [[ -f $BRAND/grub/theme.txt ]]; then
    T=/boot/grub/themes/p8s
    mkdir -p "$T"
    cp "$BRAND/grub/theme.txt" "$BRAND/grub/background.png" "$BRAND/grub/select_c.png" "$T/"
    # fonts for the theme (GRUB uses its own font when these can't be made)
    if command -v grub-mkfont >/dev/null; then
      for f in "DejaVuSans.ttf:dejavu_16:16" "DejaVuSans.ttf:dejavu_20:20" "DejaVuSansMono.ttf:dejavu_mono_16:16"; do
        IFS=: read -r ttf name size <<<"$f"
        src=/usr/share/fonts/truetype/dejavu/$ttf
        [[ -f $src && ! -f $T/$name.pf2 ]] && grub-mkfont -s "$size" -o "$T/$name.pf2" "$src" >/dev/null 2>&1 || true
      done
    fi
    theme_ok=1
  fi
  want='GRUB_DISTRIBUTOR="P8S OS"'
  if ((theme_ok)); then
    want+=$'\nGRUB_THEME=/boot/grub/themes/p8s/theme.txt\nGRUB_GFXMODE=1920x1080,1280x1024,1024x768,auto'
    want+=$'\nGRUB_BACKGROUND=/boot/grub/themes/p8s/background.png'
  fi
  if ((SPLASH)) && command -v plymouth >/dev/null; then
    want+=$'\nGRUB_CMDLINE_LINUX_DEFAULT="quiet splash"'
  fi
  if [[ $(cat /etc/default/grub.d/p8s.cfg 2>/dev/null) != "$want" || ! -f /boot/grub/.p8s-theme-$(md5sum "$BRAND/grub/background.png" 2>/dev/null | cut -c1-8) ]]; then
    echo "$want" > /etc/default/grub.d/p8s.cfg
    rm -f /boot/grub/.p8s-theme-*; touch "/boot/grub/.p8s-theme-$(md5sum "$BRAND/grub/background.png" 2>/dev/null | cut -c1-8)"
    command -v update-grub >/dev/null && update-grub >/dev/null 2>&1 || true
  fi
fi

# 1b. boot splash: the logo instead of scrolling text while the server starts
#     (Esc shows the messages). Debian's "spinner" theme with the P8S logo.
if ((SPLASH)) && [[ -f $BRAND/plymouth/p8s.plymouth ]]; then
  P=/usr/share/plymouth/themes/p8s
  if [[ -d /usr/share/plymouth/themes/spinner ]] && command -v plymouth-set-default-theme >/dev/null; then
    sum=$(cat "$BRAND/plymouth/p8s.plymouth" "$BRAND/plymouth/watermark.png" | md5sum | cut -c1-12)
    if [[ $(cat $P/.p8s-version 2>/dev/null) != "$sum" || $(plymouth-set-default-theme 2>/dev/null) != p8s ]]; then
      mkdir -p "$P"
      cp /usr/share/plymouth/themes/spinner/*.png "$P/" 2>/dev/null || true
      cp "$BRAND/plymouth/watermark.png" "$BRAND/plymouth/p8s.plymouth" "$P/"
      echo "$sum" > "$P/.p8s-version"
      # rebuilds the initramfs (about a minute); only when the splash changed
      plymouth-set-default-theme -R p8s >/dev/null 2>&1 || echo "  note: boot splash not set (plymouth)"
      command -v update-grub >/dev/null && update-grub >/dev/null 2>&1 || true
    fi
  fi
elif ((!SPLASH)) && command -v plymouth-set-default-theme >/dev/null && [[ $(plymouth-set-default-theme 2>/dev/null) == p8s ]]; then
  plymouth-set-default-theme -R text >/dev/null 2>&1 || true
fi

# 2. console login screen (\4 = this server's IPv4 address, filled in by getty)
if [[ ! -f /etc/systemd/system/p8s-firstboot.service || -f /var/lib/p8s/.firstboot-done ]]; then
  cat > /etc/issue <<'ISSUE'

   ____   ___   ____     ___   ____
  |  _ \\ ( _ ) / ___|   / _ \\ / ___|
  | |_) |/ _ \\ \\___ \\  | | | |\\___ \\
  |  __/| (_) | ___) | | |_| | ___) |
  |_|    \\___/ |____/   \\___/ |____/

  Problems. Infinite Solutions.  -  virtual machines and containers
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
printf '\n\033[1;38;5;31m'
cat <<'ART'
   ____   ___   ____     ___   ____
  |  _ \ ( _ ) / ___|   / _ \ / ___|
  | |_) |/ _ \ \___ \  | | | |\___ \
  |  __/| (_) | ___) | | |_| | ___) |
  |_|    \___/ |____/   \___/ |____/
ART
printf '\033[0m\n  \033[1mP8S OS\033[0m %s  (Debian %s, kernel %s)\n' "$ver" "$(cat /etc/debian_version 2>/dev/null)" "$(uname -r)"
printf '  Dashboard: https://%s:8006\n' "${ip:-<this-server>}"
printf '  Commands:  p8 status | p8 list | p8 ct | p8 help\n\n'
SH
chmod 0755 /etc/update-motd.d/05-p8s

# 4. what P8S OS version this is (for support and scripts)
printf 'P8S OS %s\nBased on Debian %s\n' "$VER" "$(cat /etc/debian_version 2>/dev/null)" > /etc/p8s-release
echo "branding: P8S OS $VER"
