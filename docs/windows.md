# Windows VMs

| Kind of system (Create VM → ISO) | For | Firmware | TPM | Disk / network |
|---|---|---|---|---|
| **Windows 11 / Server 2022, 2025** | Windows 11, Windows Server 2022/2025 | UEFI + Secure Boot (Microsoft keys) | TPM 2.0 (software, swtpm) | SATA disk, Intel e1000e NIC |
| **Windows 10 / Server 2019 or older** | Windows 10, 8, 7, Server 2019/2016 | BIOS | – | SATA disk, Intel e1000e NIC |

Both get the Hyper-V enlightenments Proxmox uses (relaxed, vapic, spinlocks, vpindex, runtime, synic,
stimer, reset, frequencies, tlbflush, ipi, hypervclock), so Windows runs fast on KVM.

## Install Windows 11

1. **ISO images**: upload the Windows ISO (from Microsoft) and click **Download virtio-win** (drivers and
   guest tools, ~700 MB).
2. **+ Create VM** → Operating system: the Windows ISO → Kind of system: **Windows 11 / Server 2022, 2025**
   (picked automatically when the ISO name has "win") → Drivers CD: **virtio-win.iso**.
   At least 2 CPUs, 4 GB RAM and 64 GB disk (the Windows 11 installer refuses less).
3. **Console** → press a key when it says "Press any key to boot from CD" → install as usual.
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
