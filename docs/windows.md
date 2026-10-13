# Windows VMs

| Kind of system (Create VM → ISO) | For | Firmware | TPM | Disk / network |
|---|---|---|---|---|
| **Windows 11 / Server 2022, 2025** | Windows 11, Windows Server 2022/2025 | UEFI + Secure Boot (Microsoft keys) | TPM 2.0 (software, swtpm) | SATA disk, Intel e1000e NIC |
| **Windows 10 / Server 2019 or older** | Windows 10, 8, 7, Server 2019/2016 | BIOS | – | SATA disk, Intel e1000e NIC |

Both get the Hyper-V enlightenments Proxmox uses (relaxed, vapic, spinlocks, vpindex, runtime, synic,
stimer, reset, frequencies, tlbflush, ipi, hypervclock), so Windows runs fast on KVM.

## Install Windows 11

1. **ISO images**: upload the Windows ISO (from Microsoft) and click **Download virtio-win** (drivers and
   guest tools, ~700 MB; version 0.1.271, the one Proxmox recommends, with "stable" as fallback). It is
   **optional**: Windows installs and runs without it.
   - If the download fails with "can only be reached over IPv6": the Fedora site answered this network only
     over IPv6 and the network has no IPv6 internet. Download
     [virtio-win.iso](https://fedorapeople.org/groups/virt/virtio-win/direct-downloads/archive-virtio/virtio-win-0.1.271-1/virtio-win.iso)
     on your PC and add it with **ISO images → Upload**, or turn on IPv6 on your router. (The server also asks
     public DNS servers for an IPv4 address when the router's DNS gives none.)
2. **+ Create VM** → Operating system: the Windows ISO → Kind of system: **Windows 11 / Server 2022, 2025**
   (picked automatically when the ISO name has "win") → Drivers CD: **virtio-win.iso**.
   At least 2 CPUs, 4 GB RAM and 64 GB disk (the Windows 11 installer refuses less).
3. **Console** → install as usual. While the disk is still empty, P8S presses Enter for the "Press any key to boot from CD" prompt during the first 15 seconds after each start, so the installer always starts (once Windows is on the disk nothing is pressed). If you still land on a boot error or the UEFI boot page, click **Ctrl+Alt+Del** in the console and press a key when the prompt shows.
   The disk is SATA, so the installer sees it without extra drivers.
4. After installing: open the drivers CD in Windows and run `virtio-win-guest-tools.exe`
   (guest agent, memory balloon, better display).
5. Stop the VM and **Eject CD** in Manage (removes both CDs).

No Microsoft product key is included: activate Windows with your own licence (selling Windows VMs needs
Microsoft SPLA licensing).

## Good to know

- Server needs `swtpm`, `swtpm-tools` and `ovmf` (installed by `update.sh`).
- UEFI settings (NVRAM) live in `/var/lib/libvirt/qemu/nvram/`, TPM state in `/var/lib/libvirt/swtpm/`.
  Disk snapshots don't include them. A VM restored from a backup as a new VM gets a fresh TPM: if
  BitLocker is on, have the recovery key ready.
- Resizing, ejecting and upgrades keep UEFI and the TPM (the definition is re-rendered from its type).

## Setup says "Let's connect you to a network" / "Something went wrong"

Windows 11 setup wants internet. If the network shows **Connected** but setup still fails, the VM
has a link but no working internet or DNS: run **Server → Check guest internet** (and **Repair**).
To finish setup without internet: console → **Keys… → Shift+F10**, then type
`start ms-cxh:localonly` (newer Windows 11) or `OOBE\BYPASSNRO` (older builds; the VM restarts and
offers "I don't have internet"). Microsoft removes these shortcuts from time to time, so they may
not work on the newest builds.

## Making Windows smooth

- **CPUs**: P8S gives every VM its vCPUs as cores of **one socket** (since definition version 4).
  Windows 10/11 Home only use one socket and Pro two, so older VMs with 4 vCPUs ran on 1-2 of
  them. Existing VMs pick this up after **Shutdown → Start** (not a restart inside Windows).
- **Size**: Windows 11 wants at least 4 vCPUs and 6-8 GB RAM to feel quick; 2 vCPUs / 4 GB works
  but is slow, especially while Windows Update and Defender run after setup.
- **Drivers**: open the virtio-win CD in Explorer and run `virtio-win-guest-tools.exe` (balloon,
  guest agent, display and network drivers).
- **Use Remote Desktop for daily work**: the browser console is for installing and rescue. Windows
  VMs on the private network get an RDP port on the server (shown under the VM: `RDP host:3391`).
  Turn on Remote Desktop in Windows (Settings → System → Remote Desktop; Pro or higher) and connect
  with "Remote Desktop Connection" to that address. VMs on the home network: connect to their own IP.
