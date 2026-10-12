# P8S OS alpha ISO

A bootable installer: Debian 13 underneath, P8S OS set up automatically on first boot.

## 1. Build the ISO (in the p8s-test VM)

1. VirtualBox → p8s-test → **Settings → Shared Folders** → double-click **P8SOS** → **untick Read-only** → OK.
   (Do this with the VM powered off.)
2. Start the VM, log in, open Terminal:
   ```
   su -c 'mount -t vboxsf P8SOS /mnt || mount -t vboxsf p8sos /mnt; SUDO_USER=vboxuser bash /mnt/make-iso.sh'
   ```
3. ~5 minutes (downloads the 750 MB Debian ISO once). Result on Windows:
   `L:\P8SOS\dist\p8sos-alpha-YYYYMMDD-amd64.iso` (+ `.sha256`).

## 2. Write it to a USB stick (Windows)

1. Download **Rufus** from rufus.ie.
2. Device: your USB stick (**everything on it is erased**). Boot selection: the P8S OS ISO. Start.
3. When Rufus asks *ISO or DD image mode* → choose **DD Image**.

## 3. Install on the laptop (FX505GT)

> ⚠️ This **erases the disk you choose** (Proxmox and everything on it).

1. **BIOS first:** power on, press **F2** →
   - Security → **Secure Boot: Disabled** (ZFS's kernel module is not signed)
   - Advanced → **Intel Virtualization Technology: Enabled**
   - Save & Exit (F10).
2. Plug in the USB stick and Ethernet if you have it (Wi-Fi also works).
3. Power on, press **Esc** repeatedly (ASUS boot menu) → pick the USB stick (UEFI entry).
4. It boots **Install P8S OS (alpha)** automatically after 5 seconds.
5. The installer asks only:
   - network (Wi-Fi name + password, if no cable)
   - hostname (default `p8s-node`)
   - your name, user name and password
   - **which disk** to install on, then **"Write changes to disks?" → Yes**
6. ~10 minutes, then it reboots. Remove the USB stick.

## 4. First boot (automatic, 10–20 min)

The screen shows `[P8S OS] ...` progress lines: it installs KVM, libvirt, ZFS, the
Debian template and p8sd. When done, the login screen says:

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

**VM storage:** if the laptop has a second, completely **empty** disk, the ZFS pool goes
there. Otherwise P8S OS uses a pool file on the system disk (fine for the alpha).
A second disk that has any partitions or data on it is **never touched**.

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
```
In the dashboard: **Manage** on a VM. Resizing and rolling back need the VM stopped;
`p8` stops and restarts it for you after asking.

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
