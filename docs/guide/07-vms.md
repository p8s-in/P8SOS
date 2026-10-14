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
| More network cards (up to 8) | Manage → Network & firewall → **Add network card** | `p8 nic-add NAME lan --vlan 20`, `p8 nics NAME`, `p8 nic-rm NAME MAC` |
| Add CPUs / RAM while it runs | Manage → Hardware → **Hot-plug** (once, VM off), then Resize while it runs | `p8 hotplug NAME on cpus=8 ram=16384`, `p8 resize NAME cpus=6 ram=8192` |
| Limit disk / network speed | Manage → Hardware → **Speed limits** (applies at once) | `p8 limits NAME read=200 write=100 iops=5000 down=100 up=50`, `p8 limits NAME off` |
| Give it a graphics card, NVMe, USB device (passthrough) | Manage → Hardware → **PCI and USB devices** | `p8 devices NAME`, `p8 dev-add NAME pci 0000:01:00.0`, `p8 dev-add NAME usb 046d:c52b` |
| Snapshot / roll back | Manage → Snapshots | `p8 snap NAME before-upgrade`, `p8 rollback NAME before-upgrade` |
| Password inside the VM | Manage → Login | `p8 passwd NAME [USER]` |
| Firewall | Manage → Firewall | `p8 fw NAME allow tcp 22 from 203.0.113.0/24` |
| Delete | Manage → Delete (type the name) | `p8 delete NAME` |

Snapshots are instant and free until data changes. Take one before every upgrade; roll back undoes everything since.

**Include RAM** (Manage → Snapshots, or `p8 snap NAME before-upgrade --ram`) also saves the running VM's memory: rolling back then resumes the VM exactly where it was, with its programs open, without stopping it first. The VM pauses about 1 s per GB of RAM while its memory is written. The memory file takes as much space as the VM's RAM (in /var/lib/p8s/vmstate) and stays on this server: after a move, the snapshot rolls back the disk only.

## Installing from an ISO, and Windows

1. **ISO images**: upload an ISO, or download one by URL (`p8 iso-download URL`). For Windows also click **Download virtio-win**.
2. **+ Create VM** → Operating system: the ISO → Kind of system → for Windows 11 at least 2 CPUs, 4 GB RAM, 64 GB disk.
3. **Console** → install as usual. The disk is SATA, so Windows sees it without extra drivers.
4. Windows: after installing, run `virtio-win-guest-tools.exe` from the drivers CD (guest agent, balloon, display).
5. Shut down and **Eject CD**.

Windows 11 gets UEFI, Secure Boot and a software TPM 2.0. Setup stuck at "Let's connect you to a network": Console → Keys → Shift+F10, then `start ms-cxh:localonly` (or `OOBE\BYPASSNRO` on older builds). Bring your own Windows licence.

## More operating systems

**Server → OS templates** lists the ready ones (Debian 13/12, Ubuntu 24.04/22.04, Rocky 9, Alma 9): **Add** downloads one and checks its checksum. For anything else press **+ Add another OS (any cloud image)**:

| Field | What to put |
|---|---|
| Name | Short name, e.g. `kali-2026`, `opensuse-15.6` |
| Label | What Create VM shows, e.g. *Kali Linux 2026.3* |
| Image address | The download link of the distribution's **cloud image** (`.qcow2`, `.img`, `.raw`, also `.xz` / `.gz`), e.g. "genericcloud" or "OpenStack" images |
| Login user | The image's default user (`kali`, `opensuse`, `cloud-user`, `fedora`…); empty = root |
| OS type | Optional, e.g. `debian13` |
| Checksum | The address of the publisher's `SHA256SUMS` / `CHECKSUM` file, or `sha256:HASH` (strongly recommended) |

The image must have cloud-init (all official cloud images do), so P8S can set the password, SSH key and network. Command line: `p8 templates add-url kali-2026 https://…/kali.qcow2.xz --label "Kali 2026" --login kali --checksum https://…/SHA256SUMS`. For an OS without a cloud image, install from its ISO and then **Make template** from that VM.

## Clones and templates

- **Clone** (Manage → Options → Clone, or `p8 clone web1 web2 [--start]`): a full, independent copy with all disks, even while the VM runs. Gets a new MAC address.
- **Make template** (Manage → Options → Make template, or `p8 template-from web1 webbase "Web base"`): set up one VM the way you like, then turn it into a template. If the VM is running with the QEMU guest agent, P8S cleans it first (cloud-init, machine id, SSH host keys) and shuts it down, so every VM made from it gets its own identity. The VM itself stays.
- New VMs from it: Create VM → *Made from your VMs* group, or `p8 create web3 --os webbase`.
- Delete with `p8 templates rm webbase` (only when no VM uses it).

## Importing VMs from other platforms

**Storage → Import VMs** (admins): Proxmox backups (`.vma`, `.vma.zst`), **OVA appliances** (VMware / VirtualBox export: CPUs, RAM, firmware, network card and every disk come from it), VMware `.vmdk`, Hyper-V `.vhdx`, VirtualBox `.vdi`, or `.qcow2` / `.raw`. Upload the file (or `scp` big files to `/var/lib/p8s/import/`), click **Check** to see the VM it would make, then **Import**. Keep the original disk type and BIOS/UEFI; keep the MAC only if the old VM is gone for good.

## PCI and USB passthrough

A VM can own one of the server's devices like real hardware: a **graphics card** (gaming, AI, video encoding), a **network card**, an **NVMe disk**, a **USB controller**, or a single **USB device** (stick, dongle, printer, licence key).

1. **PCI needs the CPU's IOMMU.** Manage → Hardware shows if it is off: press **Turn on IOMMU** (or `sudo p8 iommu on`) and reboot the server. If it stays off, turn on **VT-d** (Intel) or **AMD-Vi / IOMMU** (AMD) in the BIOS. USB devices work without it.
2. **Shut the VM down**, choose the device under **PCI and USB devices**, **Give to VM**. Devices that share an IOMMU group (a graphics card and its sound part) go together. USB devices can be given while the VM runs.
3. Start the VM and install the device's own driver inside it (e.g. the NVIDIA driver). The browser console keeps showing the VM's standard screen; a monitor on the card shows the card's output.

| Careful with | Why |
| --- | --- |
| The server's own screen (laptop GPU) | The server's monitor goes dark while the VM runs (the dashboard keeps working) |
| The network card the server uses | The server goes offline |
| A disk controller with the server's disks | The server loses its disks |

The dashboard marks these with ⚠ and asks twice. A device belongs to one VM at a time. VMs with PCI or USB devices can't move to another server (take the devices away first), can't save their RAM in a snapshot, and clones don't get the devices.

