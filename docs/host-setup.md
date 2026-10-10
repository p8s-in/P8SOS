# Host setup — Phase 1 test box (e.g. the FX505GT laptop)

Install **plain Debian 13** (no Proxmox) — netinst, "SSH server" + "standard
system utilities" only. Enable VT-x in BIOS.

## 1. Packages, ZFS pool, datasets

```bash
git clone <repo> p8sos && cd p8sos
sudo scripts/bootstrap-debian13.sh /dev/sdb   # spare disk → pool "tank" (wipes it)
# or, no spare disk:
sudo scripts/bootstrap-debian13.sh            # 64G sparse file pool, testing only
```

## 2. Network bridge

VMs attach to `vmbr0`. Do this from the physical console, not over SSH.
Replace `enp3s0` with your NIC (`ip -br link`):

```
# /etc/network/interfaces
auto lo
iface lo inet loopback

iface enp3s0 inet manual

auto vmbr0
iface vmbr0 inet dhcp
    bridge_ports enp3s0
    bridge_stp off
    bridge_fd 0
```

`sudo systemctl restart networking`. VMs now get DHCP from your router.

**Laptop on Wi-Fi?** Wi-Fi can't be bridged. Use libvirt's NAT network instead:
`sudo virsh net-start default && sudo virsh net-autostart default` and set
`"bridge": "virbr0"` in the config.

## 3. Template

```bash
sudo scripts/make-template.sh       # downloads + verifies Debian 13 cloud image
```

It prints the `templates` entry to paste into the config.

## 4. Build and install p8sd

Needs Go 1.24+ (`sudo apt install golang-go` on trixie, or go.dev/dl).

```bash
make test
sudo make install
sudo p8sd token -name admin          # copy the sha256 line into /etc/p8s/p8sd.json
sudoedit /etc/p8s/p8sd.json          # tokens, templates, bridge
sudo systemctl enable --now p8sd
journalctl -u p8sd -f
```

## 5. Smoke test

Follow the example session in [api.md](api.md). Then:

```bash
sudo virsh list --all                       # p8s-web1 running
sudo zfs list -t all -r tank/p8s            # clone of debian-13@base
sudo virsh domifaddr p8s-web1 --source arp  # find the IP
ssh debian@<ip>
```

From another machine, tunnel instead of exposing the API:
`ssh -L 8006:127.0.0.1:8006 you@laptop`.

## Troubleshooting

| Symptom | Fix |
|---------|-----|
| `template ... snapshot ... is missing` | Run `make-template.sh`; check the `snapshot` string. |
| `Cannot get interface MTU on 'vmbr0'` | Bridge doesn't exist — step 2. |
| `Permission denied` opening `/dev/zvol/...` | AppArmor: check `journalctl -k | grep apparmor`; libvirt normally allows disks named in the XML. |
| VM boots but no SSH | `sudo virsh console p8s-web1`; check cloud-init: `cloud-init status --long`. |
