#!/usr/bin/env bash
# Runs inside the freshly installed system at the end of the install (the P8S
# installer, or the classic ISO's Debian installer). Arms the one-time P8S OS
# first-boot setup.
set -eu
install -m 0644 /opt/p8sos/iso/p8s-firstboot.service /etc/systemd/system/p8s-firstboot.service
chmod +x /opt/p8sos/iso/firstboot.sh /opt/p8sos/install.sh /opt/p8sos/scripts/*.sh /opt/p8sos/scripts/p8
systemctl enable p8s-firstboot.service
# "encrypted VM storage": the classic ISO's boot menu entry, or the installer (P8S_ENCRYPT=1)
if [[ ${P8S_ENCRYPT:-0} == 1 ]] || grep -qw 'p8s.encrypt=1' /proc/cmdline 2>/dev/null; then install -d -m 0750 /etc/p8s; touch /etc/p8s/encrypt-storage; fi

cat > /etc/issue <<'EOF'

  P8S OS (alpha) — finishing setup on first boot.
  This takes a few minutes and needs internet. Progress is shown on screen.

EOF
echo "P8S OS alpha" > /etc/p8s-release
