#!/usr/bin/env bash
# P8S OS remote access with Tailscale: works from anywhere, also behind
# CGNAT (Jio, Airtel) where nothing from outside can reach this server.
#   tailscale.sh up [lan]   install if needed, connect (prints a sign-in link); lan = also reach the home network
#   tailscale.sh down       disconnect
set -uo pipefail
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
case "${1:-up}" in
up)
  if ! command -v tailscale >/dev/null; then
    echo "==> installing Tailscale (from pkgs.tailscale.com)"
    curl -fsSL https://tailscale.com/install.sh | sh || { echo "install failed (internet?)"; exit 1; }
  fi
  systemctl enable --now tailscaled
  ROUTES=()
  if [[ ${2:-} == lan ]]; then
    dev=$(ip -4 route show default | awk '{print $5; exit}')
    net=$(ip -4 route show dev "$dev" scope link proto kernel | awk '{print $1; exit}')
    if [[ -n $net ]]; then
      ROUTES=(--advertise-routes="$net")
      sysctl -qw net.ipv4.ip_forward=1
      echo 'net.ipv4.ip_forward=1' >/etc/sysctl.d/90-p8s-tailscale.conf
      echo "==> offering the home network $net (approve it once in the Tailscale admin page → Machines → Edit route settings)"
    fi
  fi
  if tailscale status >/dev/null 2>&1; then
    [[ ${#ROUTES[@]} -gt 0 ]] && tailscale set "${ROUTES[@]}"
    echo "==> already connected"
  else
    echo "==> open the link below in a browser and sign in to Tailscale (free for personal use)."
    echo "    This task waits up to 15 minutes for you."
    tailscale up --accept-dns=false --hostname="$(hostname)" "${ROUTES[@]}" --timeout=15m || exit 1
  fi
  ip4=$(tailscale ip -4 | head -1)
  echo "==> connected as $ip4"
  echo "    On your phone/laptop: install Tailscale, sign in with the same account, then open https://$ip4:8006"
  ;;
down)
  tailscale logout 2>/dev/null || true
  systemctl disable --now tailscaled 2>/dev/null || true
  echo "==> Tailscale is off"
  ;;
*) echo "usage: $0 up [lan] | down"; exit 2 ;;
esac
