# 7. Virtual machines
Summary: Create, size and run VMs; Windows; clones and templates; importing from other platforms.

## Creating a VM

Dashboard → **VMs → + Create VM**. A VM from a template boots in a few seconds.

| Option | Choices | Advice |
| --- | --- | --- |
| Name | Lowercase letters, digits, hyphens | e.g. `web1`, `db-prod` |
| Operating system | A template (Debian 13, Ubuntu, Rocky…), a template made from your VMs, or an ISO | Templates for Linux servers; ISO for Windows or anything else |
| Kind of system | Linux or other / Windows 11 & Server 2022/2025 / Windows 10 & Server 2019 or older | Set automatically when the ISO name contains "win" |
| Drivers CD | virtio-win ISO | Windows only; optional |
| Network | **Private (NAT)**: reachable from the host, gets internet. **Home network**: an IP from your router, reachable from other PCs | Home network needs `p8 lan-setup` once (Ethernet only) |
| VLAN | 1–4094 | Optional; the switch port must carry it tagged |
| Storage | `local` (ZFS), `ceph`, or a SAN disk | Ceph for VMs that should move instantly / fail over |
| For | A customer | Counts against their plan |
| CPUs / RAM / Disk | e.g. 2 / 2 GB / 20 GB | Disk can grow later, not shrink |
| Root password | Optional | Empty = a strong one is made and shown under Manage → Login |
| SSH public key | Optional | Best way to log in |

**Typical sizes**

| Use | CPUs | RAM | Disk |
| --- | --- | --- | --- |
| Small Linux server, DNS, VPN | 1 | 1 GB | 10 GB |
| Web site, app server | 2 | 2–4 GB | 20–40 GB |
| Database | 4 | 8–16 GB | 50 GB+ |
| Windows 11 | 2–4 | 4–8 GB | 64 GB+ |
| Windows Server | 4 | 8 GB | 80 GB+ |

Command line: `p8 create web1 2 2048 20 --os debian-13 [--lan] [--vlan 20] [--storage ceph]`.

## Using a VM

| Task | Dashboard | Command |
| --- | --- | --- |
| Start / shut down / restart / power off | VM row buttons | `p8 start\|stop\|reboot\|kill NAME` |
| Console (screen, keyboard) | Manage → Console | `p8 console NAME` (serial) |
| Log in | Manage → Login (IP, user, password) | `p8 ssh NAME`, `p8 ip NAME` |
| Change CPUs, RAM, disk | Manage → Options → Resize | `p8 resize NAME cpus=4 ram=4096 disk=40` |
| Extra disks | Manage → Disks | `p8 disk-add NAME 50`, `p8 disk-grow NAME 1 100`, `p8 disk-rm NAME 1` |
| Snapshot / roll back | Manage → Snapshots | `p8 snap NAME before-upgrade`, `p8 rollback NAME before-upgrade` |
| Password inside the VM | Manage → Login | `p8 passwd NAME [USER]` |
| Firewall | Manage → Firewall | `p8 fw NAME allow tcp 22 from 203.0.113.0/24` |
| Delete | Manage → Delete (type the name) | `p8 delete NAME` |

Snapshots are instant and free until data changes. Take one before every upgrade; roll back undoes everything since.

## Installing from an ISO, and Windows

1. **ISO images**: upload an ISO, or download one by URL (`p8 iso-download URL`). For Windows also click **Download virtio-win**.
2. **+ Create VM** → Operating system: the ISO → Kind of system → for Windows 11 at least 2 CPUs, 4 GB RAM, 64 GB disk.
3. **Console** → install as usual. The disk is SATA, so Windows sees it without extra drivers.
4. Windows: after installing, run `virtio-win-guest-tools.exe` from the drivers CD (guest agent, balloon, display).
5. Shut down and **Eject CD**.

Windows 11 gets UEFI, Secure Boot and a software TPM 2.0. Setup stuck at "Let's connect you to a network": Console → Keys → Shift+F10, then `start ms-cxh:localonly` (or `OOBE\BYPASSNRO` on older builds). Bring your own Windows licence.

## Clones and templates

- **Clone** (Manage → Options → Clone, or `p8 clone web1 web2 [--start]`): a full, independent copy with all disks, even while the VM runs. Gets a new MAC address.
- **Make template** (Manage → Options → Make template, or `p8 template-from web1 webbase "Web base"`): set up one VM the way you like, then turn it into a template. If the VM is running with the QEMU guest agent, P8S cleans it first (cloud-init, machine id, SSH host keys) and shuts it down, so every VM made from it gets its own identity. The VM itself stays.
- New VMs from it: Create VM → *Made from your VMs* group, or `p8 create web3 --os webbase`.
- Delete with `p8 templates rm webbase` (only when no VM uses it).

## Importing VMs from other platforms

**Storage → Import VMs** (admins): Proxmox backups (`.vma`, `.vma.zst`), VMware `.vmdk`, Hyper-V `.vhdx`, VirtualBox `.vdi`, or `.qcow2` / `.raw`. Upload the file (or `scp` big files to `/var/lib/p8s/import/`), click **Check** to see the VM it would make, then **Import**. Keep the original disk type and BIOS/UEFI; keep the MAC only if the old VM is gone for good.
