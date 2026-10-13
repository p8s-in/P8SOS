#!/usr/bin/env bash
# Debian updates for a P8S OS server (the dashboard's Server page runs this).
#   os-upgrade.sh check     refresh the package lists and show what can be upgraded
#   os-upgrade.sh upgrade   install all updates (VMs keep running)
# Writes /var/lib/p8s/os-updates.json for the dashboard.
set -euo pipefail
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:$PATH
export DEBIAN_FRONTEND=noninteractive
[[ $EUID -eq 0 ]] || { echo "run as root: sudo $0 ${1:-check}" >&2; exit 1; }
STATE=/var/lib/p8s/os-updates.json
APT=(apt-get -y -q -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold)

status() { # count upgradable packages and note whether a reboot is needed
  local n security reboot=false
  n=$(apt-get -s -q dist-upgrade 2>/dev/null | grep -c '^Inst ' || true)
  security=$(apt-get -s -q dist-upgrade 2>/dev/null | grep '^Inst ' | grep -ci security || true)
  [[ -f /var/run/reboot-required ]] && reboot=true
  printf '{"checked":"%s","upgradable":%d,"security":%d,"reboot_required":%s,"kernel":"%s"}\n' \
    "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$n" "$security" "$reboot" "$(uname -r)" > "$STATE.tmp"
  mv "$STATE.tmp" "$STATE"
  echo "==> $n update(s) available ($security security)$([[ $reboot == true ]] && echo '; a reboot is needed to finish earlier updates')"
}

case ${1:-check} in
  check)
    echo "==> refreshing package lists"
    apt-get -q update
    echo "==> what would be upgraded"
    apt-get -s -q dist-upgrade | grep '^Inst ' | sed 's/^Inst /  /' || echo "  nothing"
    status ;;
  upgrade)
    echo "==> refreshing package lists"
    apt-get -q update
    echo "==> installing updates (VMs keep running)"
    "${APT[@]}" dist-upgrade
    "${APT[@]}" autoremove --purge
    # package scripts may reload the firewall or restart libvirt: make sure guests still reach the internet
    if [[ -x /usr/local/share/p8s/netcheck.sh ]]; then
      echo "==> checking the guest network"
      /usr/local/share/p8s/netcheck.sh --repair | sed 's/^/  /' || true
    fi
    if [[ -f /var/run/reboot-required ]]; then
      echo "==> done. A reboot is needed to use the new kernel/libraries (Server page -> Reboot)."
    else
      echo "==> done, no reboot needed."
    fi
    status ;;
  *) echo "usage: $0 check|upgrade" >&2; exit 1 ;;
esac
