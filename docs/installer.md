# P8S OS installer

The installer ISO puts P8S OS on the disks you pick, with the **system itself on ZFS** — like
Proxmox: one disk, a mirror, striped mirrors (RAID 10) or RAID-Z. VMs live in the same pool
(`tank/p8s/vms`), so they get the same protection. Everything comes from the ISO: no internet is
needed to install (the first start downloads the Debian VM template and finishes setup).

## Layouts

| Layout | Disks | Survives | Space (4 × 1 TB) |
|---|---|---|---|
| ZFS, one disk | 1 | nothing | — (1 TB with 1 disk) |
| ZFS mirror (RAID 1) | 2+ | all disks but one | 1 TB |
| ZFS striped mirrors (RAID 10) | 4, 6, 8 … (even) | 1 disk (more if in different pairs) | 2 TB |
| ZFS RAID-Z1 (like RAID 5) | 3+ | 1 disk | 3 TB |
| ZFS RAID-Z2 (like RAID 6) | 4+ | 2 disks | 2 TB |
| ZFS RAID-Z3 | 5+ | 3 disks | — (2 TB with 5) |
| ext4 + ZFS for VMs (classic) | 1 | nothing | system partition (default 32 GB) + the rest for VMs |

Every disk counts as the smallest one. Mirrors and RAID 10 are fastest for VMs; RAID-Z gives more
space. **Every disk can boot the server**: each has its own boot partitions, so pulling any one
disk of a mirror or RAID still boots.

What ends up where (ZFS layouts):

| | |
|---|---|
| `tank/ROOT/debian` | the system (`/`) |
| `bpool/BOOT/debian` | `/boot` (a small pool GRUB can read, mirrored on every disk) |
| `tank/p8s/vms`, `tank/p8s/templates` | VM disks and templates (made on the first start; encrypted if you chose that) |
| partition 2 of every disk (`p8s-esp0`, `p8s-esp1` …) | EFI boot partition; `p8s-esp-sync` keeps the copies equal after updates |

`/etc/p8s/installed.json` records the layout and the disks.

## 1. Build the ISO (in the p8s-test VM)

Same as before (docs/iso.md): shared folder writable, then

```
su -c 'mount -t vboxsf P8SOS /mnt || mount -t vboxsf p8sos /mnt; SUDO_USER=vboxuser bash /mnt/make-iso.sh'
```

The first build takes **30–60 minutes** (it downloads ~1.5 GB of packages once and compiles ZFS)
and needs **15 GB free** in the VM. Later builds reuse the downloads. Result:
`L:\P8SOS\dist\p8sos-installer-YYYYMMDD-amd64.iso` (about 2 GB) + `.sha256`.

The older ISO (Debian's installer, ext4 only, P8S set up on first boot) still builds with
`bash /mnt/make-iso.sh classic`.

## 2. USB stick

Rufus → the installer ISO → **DD Image** mode (everything on the stick is erased).

## 3. Install

1. Firmware setup (F2 on the ASUS laptop): **Secure Boot off** (the ZFS module isn't signed),
   **Virtualization on**. The installer says so if Secure Boot is still on.
2. Boot the stick (Esc on the ASUS → the UEFI entry). **Install P8S OS** starts after a few seconds.
3. The installer asks:
   - **Disks** — tick them with Space. Each line shows the size, model, serial number and what's on
     it now: `empty`, or `HAS DATA: 2 partition(s), ntfs DATA`. The USB stick itself and disks in
     use are not offered.
   - **Layout** — only the ones that fit the number of disks are shown.
   - Server name, network port, automatic (DHCP) or fixed address, Wi-Fi name if it's a Wi-Fi port.
   - Time zone, keyboard layout.
   - Admin user + password (gets sudo; root login stays off, as before).
   - Encrypt the VM disks? (same as before: the key is on the system disk; back it up.)
4. A summary shows everything that will be **ERASED**. If a disk isn't empty you're asked again,
   then you **type ERASE**. Nothing is written before that.
5. 3–10 minutes. Remove the stick, Enter → restart. The first start finishes the setup like before
   (`[P8S OS] ...` lines, internet needed) and the login screen shows the address.

> **The laptop (FX505GT):** the 1 TB hard disk holds your files (`p8s/`, `project/`). It shows up
> as `HAS DATA` with its partitions — **don't tick it**. Install on the SSD only.

Check afterwards:

```
zpool status            # tank and bpool ONLINE, every disk listed
p8 status
```

## Command line / unattended installs

The installer is `p8s-install` (from `cmd/p8s-install`). In the installer's menu, *Command line*
gives a shell; there:

```
p8s-install --list                          # disks and network ports it sees
p8s-install --answers a.json --dry-run      # print every command, change nothing
p8s-install --answers a.json                # install without questions
```

Answers file:

```json
{
  "disks": ["sdb", "sdc"],
  "layout": "mirror",
  "hostname": "p8s1.example.com",
  "timezone": "Europe/Berlin",
  "keyboard": "de",
  "user": "admin",
  "password": "change-me-please",
  "network": {"interface": "auto", "mode": "static", "address": "192.168.1.21/24",
              "gateway": "192.168.1.1", "dns": ["1.1.1.1"]},
  "encrypt_vm_storage": false,
  "erase": true
}
```

- `disks`: names (`sdb`, `nvme0n1`), `/dev/...` paths or `/dev/disk/by-id/...` paths (safest: they
  include the serial number). `"erase": true` is required.
- `layout`: `single`, `mirror`, `raid10`, `raidz1`, `raidz2`, `raidz3`, `ext4` (`root_gb` sets the
  ext4 system partition, default 32).
- `network.interface`: `auto` = the first port with a cable. `mode`: `dhcp` or `static`.
  `wifi_ssid` / `wifi_password` for a Wi-Fi port.

To make a stick that installs with no questions at all (for test VMs or many identical servers):

```
su -c '... ANSWERS=/mnt/answers.json SUDO_USER=vboxuser bash /mnt/make-iso.sh'
```

→ `p8sos-installer-YYYYMMDD-auto-amd64.iso`. **It erases the listed disks as soon as it boots**
(after 10 seconds) — keep it away from machines it isn't meant for.

## Test it in VirtualBox

1. New VM "p8s-inst": Linux / Debian 64-bit, 4 GB RAM, 2 CPUs, **Settings → System → Enable EFI**,
   Processor → **Enable Nested VT-x/AMD-V**.
2. Storage: two new 20 GB disks on the SATA controller, the installer ISO in the CD drive.
3. Start → Install P8S OS → tick both disks → **mirror** → … → type ERASE.
4. After the restart and first-start setup: `zpool status` shows `tank` and `bpool` as mirrors.
5. Power off, **remove disk 1** (Storage → remove the first disk), start: it still boots from disk 2,
   `zpool status` says DEGRADED. Put it back: `sudo zpool online tank ...` / it resilvers.
6. Once more without EFI (untick Enable EFI) to test BIOS booting.

## Replacing a failed disk (mirror / RAID)

`zpool status` names the failed disk. With the new, empty disk in (example: good disk `/dev/sda`,
new disk `/dev/sdc`, it replaces disk number 1):

```
sudo sgdisk --replicate=/dev/sdc /dev/sda      # same partitions as a good disk
sudo sgdisk --randomize-guids /dev/sdc
sudo sgdisk -c1:p8s-bios1 -c2:p8s-esp1 -c3:p8s-boot1 -c4:p8s-pool1 /dev/sdc
sudo zpool replace bpool <old /boot partition from zpool status> /dev/sdc3
sudo zpool replace tank  <old pool partition from zpool status> /dev/sdc4
sudo mkdosfs -F 32 -s 1 -n EFI /dev/sdc2 && sudo p8s-esp-sync     # EFI boot files
sudo grub-install --target=i386-pc /dev/sdc                        # BIOS machines only
zpool status        # wait until "resilvered"
```

If it was the first disk (`p8s-esp0`, mounted at `/boot/efi`) that died, `/boot/efi` isn't mounted
until the new disk has the `p8s-esp0` label: label the new partition `p8s-esp0`, then
`sudo mount /boot/efi` before `p8s-esp-sync` (which then copies nothing — instead run
`sudo grub-install --target=x86_64-efi --efi-directory=/boot/efi --bootloader-id=p8s` once).

## Not yet

- Text menus only (keyboard). A graphical installer with the mouse comes later.
- p8sd is still built on the first start (needs internet, ~10 minutes); pre-built on the ISO later.
- No ZFS encryption of the system itself (only of the VM disks), no swap on ZFS layouts.
- Secure Boot must be off.
