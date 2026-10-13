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

## Performance tuning

`install.sh` / `update.sh` run `scripts/tune.sh` (safe to run again: `sudo bash /usr/local/share/p8s/tune.sh`):

| Setting | Why |
|---|---|
| ZFS ARC max = 10% of RAM (1-16 GB), `/etc/modprobe.d/zfs.conf` | the disk cache no longer takes the RAM your VMs need (a value you set yourself is kept) |
| KSM on (`/etc/tmpfiles.d/p8s-ksm.conf`) | identical memory pages of similar VMs are shared: more VMs per GB |
| `vhost_net` loaded at boot | VM networking handled in the kernel |
| `vm.swappiness=10`, larger inotify/AIO limits (`/etc/sysctl.d/90-p8s.conf`) | running guests stay in RAM; many guests don't hit limits |

New and re-defined Linux VMs also get an I/O thread, `io=native` and one disk/network queue per vCPU
(up to 8). Existing VMs pick this up when they are powered off and started again.

### Guests have no internet

**Server → Check guest internet** (or `sudo bash /usr/local/share/p8s/netcheck.sh`) checks the
server's own internet and DNS, IP forwarding, libvirt's NAT network (rule + dnsmasq for DHCP/DNS),
a FORWARD policy of DROP (Docker), br_netfilter and the P8S firewall table. **Repair** (`--repair`)
fixes what it finds while guests keep running: it re-enables forwarding, restarts libvirt's network
daemon (which puts its NAT rules back and restarts dnsmasq), allows virbr0/br0 through a DROP policy
and restarts p8sd if its firewall table was wiped. Guests' DNS: many home routers answer some
queries wrongly, and dnsmasq then caches "name doesn't exist", so guests get "DNS address could not
be found" now and then. Repair points the NAT network's dnsmasq at upstream servers that answer
correctly (8.8.8.8 / 1.1.1.1 / 9.9.9.9 when reachable, otherwise the router) with `no-negcache`, and
plugs the guests' network cables straight back in after restarting the network. `p8s-net.service` runs the repair at every boot,
after every `nftables` start/reload (a reload of `/etc/nftables.conf` runs `flush ruleset`) and after
Debian updates.

### Memory shown in the dashboard

Linux reports the ZFS cache (ARC) as "used" memory, so a ZFS server with nothing running can look
nearly full. P8S counts it the way it behaves: the part of the ZFS cache that is given back to VMs
(everything above `zfs_arc_min`) and the normal file cache are shown as cache, not used.
**Server → Memory** shows the split (VMs + containers, system, ZFS cache, file cache, free), the
biggest programs, and lets you set the ZFS cache limit (`tune.sh --arc-max MIB`, or `auto` = 10% of
RAM). `GET /api/v1/node/memory` returns the same data.

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
