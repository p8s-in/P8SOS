#!/usr/bin/env bash
# Runs inside the freshly installed system (in-target) at the end of the
# Debian installer. Arms the one-time P8S OS first-boot setup.
set -eu
install -m 0644 /opt/p8sos/iso/p8s-firstboot.service /etc/systemd/system/p8s-firstboot.service
chmod +x /opt/p8sos/iso/firstboot.sh /opt/p8sos/install.sh /opt/p8sos/scripts/*.sh /opt/p8sos/scripts/p8
systemctl enable p8s-firstboot.service
# "Install P8S OS, encrypted VM storage" in the boot menu
if grep -qw 'p8s.encrypt=1' /proc/cmdline 2>/dev/null; then install -d -m 0750 /etc/p8s; touch /etc/p8s/encrypt-storage; fi

cat > /etc/issue <<'EOF'

  P8S OS (alpha) — finishing setup on first boot.
  This takes 10-20 minutes and needs internet. Progress is shown on screen.

EOF
echo "P8S OS alpha" > /etc/p8s-release
