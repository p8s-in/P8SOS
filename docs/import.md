# Importing VMs and containers (Proxmox, VMware, Hyper-V, VirtualBox)

Dashboard: **Import VMs** on the VMs page (admins; containers too). API: `/api/v1/imports` (below).

## What can be imported

| From | File | Notes |
|---|---|---|
| Proxmox VE VM | vzdump backup: `vzdump-qemu-<id>-….vma`, `.vma.zst`, `.vma.lzo`, `.vma.gz` | Settings (CPUs, RAM, OS type, BIOS/UEFI, disk bus, network card, MAC) are read from the backup. UEFI boot entries (4M efidisk) come along. **Every disk** comes along: the boot disk, the others as data disks (up to 8). |
| Proxmox VE container | vzdump backup: `vzdump-lxc-<id>-….tar.zst`, `.tar.gz`, `.tar.lzo`, `.tgz`, `.tar` | Becomes a P8S container: CPUs, RAM, disk size, MAC from `pct.conf`; files unpacked as an unprivileged container. A fixed address is switched to DHCP on the P8S network; mount points that were in the backup land in the container's disk. |
| OVA appliance | `.ova` (VMware / VirtualBox export) | CPUs, RAM, firmware, network card and every disk |
| VMware ESXi / Workstation | `.vmdk` (export the VM or copy the disk; flat/monolithic VMDKs) | Settings are entered by hand |
| Hyper-V | `.vhdx`, `.vhd` | |
| VirtualBox | `.vdi` | |
| Anything else | `.qcow2`, `.img`, `.raw` | |

Not yet: TPM state, VLAN tags, more than 8 data disks, a container's second network card, device
passthrough and `lxc.*` extras of Proxmox containers, public IPs for imported containers (import on
the private network), linked/differencing images (merge first with `qemu-img convert`).

## Steps

1. **Make the file.** Proxmox: *VM (or CT) → Backup → Backup now* (mode *Stop* gives the cleanest copy),
   or `vzdump 100 --compress zstd --mode stop`. Files land in `/var/lib/vz/dump/`.
2. **Copy it to the P8S server**: upload it in the dashboard, or (big files) from the Proxmox host:
   `scp /var/lib/vz/dump/vzdump-qemu-100-….vma.zst <user>@<p8s-server>:/var/lib/p8s/import/`
   (the folder belongs to the admin user made at install). Check the free space first.
3. **Check**: P8S reads the file and shows the VM it would create, with warnings.
4. **Import**: a new ZFS disk is made and filled (zeros are skipped), the VM is defined and
   started. Open its console to see it boot. A container backup becomes a container (on the
   Containers page); its files are unpacked into a new ZFS dataset.

Keep the disk connection (virtio / virtio-scsi / SATA) and firmware (BIOS / UEFI) the VM was
installed with. Windows only boots from a disk type it has a driver for: move it to virtio after
installing the virtio-win drivers inside Windows. **Keep the MAC address** only when the original
VM is switched off for good (two machines with one MAC break the network). A Linux VM may name its
network card differently on new hardware; fix it in the console if it has no network.

The source file is never changed; delete it from the import folder when you're done.

## API (admins)

| | |
|---|---|
| `GET /imports` | `{"files":[{"name","bytes","kind","modified"}],"folder":"/var/lib/p8s/import"}` |
| `PUT /imports/{file}` | upload (raw body) → `201` |
| `GET /imports/{file}/plan` | what would be created: `kind, name, vcpus, memory_mib, disk_gb, os_type, firmware, disk_bus, nic_model, mac, efi_vars, devices, data_disks, config, ct_os, static_ip, warnings` |
| `POST /imports/{file}/start` | `{"name","vcpus","memory_mib","disk_gb","os_type","firmware":"bios|uefi","disk_bus":"virtio|scsi|sata","nic_model":"virtio|e1000e|e1000|vmxnet3|rtl8139","network":"nat|lan","keep_mac":false,"start":true}` (all optional; the plan fills the rest; a container backup takes `name, vcpus, memory_mib, disk_gb, network, keep_mac, start`) → `202` job; follow it at `GET /jobs/{id}` |
| `DELETE /imports/{file}` | remove the file |

## Legal

Importing reads the publicly documented Proxmox backup (VMA) format, plain tar archives and standard disk image
formats; P8S OS contains no Proxmox code (the VMA reader is our own implementation). "Proxmox" is a
trademark of Proxmox Server Solutions GmbH, "VMware" of Broadcom, "Hyper-V" of Microsoft and
"VirtualBox" of Oracle; P8S OS is not affiliated with or endorsed by them, and the names are used
only to say which files can be imported.
