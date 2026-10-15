# P8S OS alpha ISO

A bootable installer for P8S OS. **The current installer is described in [installer.md](installer.md)**:
the system on ZFS with a RAID choice (one disk, mirror, RAID 10, RAID-Z), booting from every disk,
installed from the ISO itself. This page covers what is the same for both ISOs and what comes after.

## 1. Build the ISO (in the p8s-test VM)

1. VirtualBox → p8s-test → **Settings → Shared Folders** → double-click **P8SOS** → **untick Read-only** → OK.
   (Do this with the VM powered off.)
2. Start the VM, log in, open Terminal:
   ```
   su -c 'mount -t vboxsf P8SOS /mnt || mount -t vboxsf p8sos /mnt; SUDO_USER=vboxuser bash /mnt/make-iso.sh'
   ```
3. First time 30–60 minutes, 15 GB free needed (later builds are faster). Result on Windows:
   `L:\P8SOS\dist\p8sos-installer-YYYYMMDD-amd64.iso` (+ `.sha256`).

The **classic** ISO (Debian's own installer, ext4, P8S set up on the first start; the boot menu has
"encrypted VM storage" as a separate entry) still builds with `bash /mnt/make-iso.sh classic` →
`p8sos-alpha-YYYYMMDD-amd64.iso`.

## 2. Write it to a USB stick (Windows)

1. Download **Rufus** from rufus.ie.
2. Device: your USB stick (**everything on it is erased**). Boot selection: the P8S OS ISO. Start.
3. When Rufus asks *ISO or DD image mode* → choose **DD Image**.

## 3. Install on the laptop (FX505GT)

> ⚠️ This **erases the disks you choose**. The laptop's 1 TB hard disk holds your files: don't pick it
> (the installer marks it `HAS DATA` and asks twice before erasing anything that isn't empty).

1. **BIOS first:** power on, press **F2** →
   - Security → **Secure Boot: Disabled** while installing (the installer's ZFS module is not signed; afterwards `p8 secureboot setup` makes the server ready for it)
   - Advanced → **Intel Virtualization Technology: Enabled**
   - Save & Exit (F10).
2. Plug in the USB stick and Ethernet if you have it (Wi-Fi also works).
3. Power on, press **Esc** repeatedly (ASUS boot menu) → pick the USB stick (UEFI entry).
4. **Install P8S OS** starts by itself. Steps and screens: [installer.md](installer.md#3-install).
5. Remove the USB stick when it says so; it restarts.

## 4. First boot (automatic, 10–20 min)

The screen shows `[P8S OS] ...` progress lines: it finishes KVM, libvirt and ZFS setup, downloads
the Debian template and installs p8sd (pre-built on the ISO). When done, the login screen says:

```
P8S OS (alpha) is ready.   Host IP: 192.168.x.x
```

Log in with your user, then:

```
p8 status          # p8sd running, token works
p8 test            # full self-test (no SLOW=1 needed on real hardware)
p8 create web1
p8 ssh web1
```

**VM storage:** with the P8S installer, VMs live in the ZFS pool the installer made (`tank`).
With the classic ISO: a second, completely **empty** disk if there is one, else a pool file on the
system disk. A second disk that has any partitions or data on it is **never touched**.

## 5. Web dashboard

On the laptop run `p8 dashboard`. It prints the address, e.g. **https://192.168.1.50:8006**, and your token.
Open that address in a browser on any PC or phone on the same network:

1. The browser warns about the certificate (the host made its own) → **Advanced → Proceed**. Once per browser.
2. Paste the token → **Sign in**.
3. You see host CPU / memory / storage and all VMs. **+ New VM**, Start, Stop, Reboot, Force off and
   Delete (type the name to confirm) are all buttons. VMs created here trust the host user's SSH key,
   so `p8 ssh <name>` works on the laptop.

## 6. VMs on your home network (optional)

By default VMs are only reachable from the laptop. To let other PCs/phones reach them:
Ethernet cable in, then `sudo p8 lan-setup` once, then `p8 create web2 --lan` (or choose
**Home network** in the dashboard). Details: [networking.md](networking.md).

## 7. Snapshots and resizing

```
p8 snap web1 before-upgrade        # instant copy of the disk
p8 rollback web1 before-upgrade    # undo everything since (asks first)
p8 resize web1 cpus=4 ram=4096 disk=40
p8 disk-add web1 100               # a second (empty) disk for data: vdb in the VM
p8 disk-grow web1 1 200            # grow it later
```
In the dashboard: **Manage** on a VM. Resizing and rolling back need the VM stopped;
`p8` stops and restarts it for you after asking.

## More operating systems

Debian 13 comes installed. Add others once (downloads the official cloud image, checks its checksum):
```
p8 templates available            # debian-12, ubuntu-24.04, ubuntu-22.04, rocky-9, alma-9
p8 templates add ubuntu-24.04
p8 create app1 2 2048 20 --os ubuntu-24.04
```
They then also appear under **Operating system** in the dashboard's New VM dialog. Each OS has its
own login user (`ubuntu`, `rocky`, `almalinux`, …) — `p8 ssh` and the dashboard's copy-SSH know it.
Test one with: `TEMPLATE=ubuntu-24.04 p8 test`.

## Root password (like a VPS)

When creating a VM you can set a **root password** (dashboard: *Root password* fields; command line:
`p8 create web3 2 2048 20 --root-password`). By default SSH login as root with that password is
allowed (`ssh root@<VM IP>`); untick the box for console-only. Change any password later:
dashboard **Manage → Change password**, or `p8 passwd web3` (root) / `p8 passwd web3 debian`.

## Installing any system from an ISO (Windows, other Linux, BSD…)

1. Dashboard → **ISOs** → upload the `.iso` from your PC, or paste a download link (the server fetches it).
   Command line: `p8 iso-upload ~/Downloads/x.iso` or `p8 iso-download https://…/x.iso`.
2. **+ New VM** → Operating system → pick the ISO under *Install from ISO*; for Windows choose
   *Kind of system: Windows*. The console opens: install as on a real PC.
3. When installed: stop the VM, **Manage → Eject** the CD (`p8 eject NAME`), start it.

Windows 10 / Server 2016–2025 install as-is (SATA disk, Intel network card). Windows 11 needs UEFI and
TPM, which come later.

## Backups

A backup is a copy of a VM's disks in files — unlike a snapshot it survives deleting the VM.
The first backup is full; the next ones store **only what changed** since the previous one
(ZFS keeps the newest backup's snapshot on the disks for that), and every 7th is full again.
Restoring a "changes" backup restores the chain it builds on automatically; old backups the
kept ones need are never deleted by "keep the last N". Scheduled backups run a **restore test**
once a week (the backup is restored into a scratch disk and every block is checked; the VM is
not touched) — press **Test** on any backup, or `p8 verify web1 ID`, to run one now.
```
p8 backup web1                       # now (the VM can keep running)
p8 autobackup web1 on 7              # every day at 02:30, keep the last 7
p8 backups                           # list (also of deleted VMs)
p8 restore web1 20261012-023000      # put it back (asks, stops and restarts web1)
p8 restore web1 20261012-023000 web1-copy   # or: a new VM from it
```
Dashboard: **Manage → Backups** on a VM, or the **Backups** button at the top for all of them.
Backups live in `/var/lib/p8s/backups`. For real safety point `backup_dir` in `/etc/p8s/p8sd.json`
at a second disk (e.g. `/mnt/backup`) and `sudo systemctl restart p8sd`.

## 8. Console (the VM's screen in your browser)

Dashboard → **Console** on a running VM opens its screen in a new tab: you see it boot and can
type into it, even when its network is broken. VMs have no password by default (SSH keys
only): press **Set password** in the console once, then log in as `debian`.
**Type text** types long strings for you (US keyboard layout); **Ctrl+Alt+Del** reboots like a
real keyboard would.

## Updating an installed host

Copy the newest `P8SOS` folder to the host (USB stick, or `git clone`), then: `sudo bash update.sh`.
VMs, config and token are kept.

## If something goes wrong

| What you see | What to do |
|---|---|
| Laptop doesn't boot from USB | BIOS (F2): disable **Secure Boot**, enable USB boot |
| Login screen says *first-boot setup FAILED* | Log in, `sudo tail -50 /var/log/p8s-firstboot.log`, send it to me. Reboot retries. |
| `No hardware virtualization` in the log | BIOS (F2) → Advanced → enable **Intel Virtualization Technology** |
| ZFS module errors | Secure Boot must be **off** (ZFS's kernel module is not signed) |
| Anything else | `p8 report` → send `p8s-report.txt` |
