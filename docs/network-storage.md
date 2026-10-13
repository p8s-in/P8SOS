# Network storage (NAS and SAN)

Server → **Network storage** (or `p8 storages`) connects storage from other machines.

| | NAS share (NFS) | SAN disk (iSCSI) |
|---|---|---|
| Holds | a copy of every backup, your ISO collection | VM disks |
| On this server | mounted at `/mnt/p8s/<name>`, kept in `/etc/fstab` | logged in with open-iscsi; P8S makes a ZFS pool `p8s-<name>` on it |
| Snapshots, backups, extra disks | — | same as local VMs |
| Several servers at once | yes | no: one server uses a disk at a time |

At boot `p8s-storages.service` mounts the shares and imports the SAN pools
**before** libvirt starts the VMs. A NAS or SAN that doesn't answer is
skipped, so the server still boots; press **Connect** once it is back.

Health & alerts watches them: a NAS that goes away is a warning, a SAN with
VM disks a critical alert (e-mail if set up); under 10 % free is a warning.

## NAS share (NFS) — TrueNAS, Synology, QNAP, a Linux box

1. On the NAS: share a folder over NFS with this server's IP, read-write,
   **root allowed** (TrueNAS: Maproot User = root; Synology: Squash = "No mapping").
2. Server → Network storage → Add: type NAS share, name `nas`, NAS address,
   shared folder (e.g. `/mnt/tank/p8s`). Keep **Copy every backup here** on.

   `p8 storages add-nfs nas 192.168.0.20 /mnt/tank/p8s --backups`

P8S makes two folders on the share:

- `p8s/backups` — an off-site target named like the storage gets a copy of
  every backup (Backups → Off-site; keeps 7 per VM, change it there). Bring
  one back with **Bring back** after the server lost it.
- `p8s/iso` — ISO files you put here (from your PC, over the NAS's own
  shares) appear in the ISO library marked "on NAS". They are read-only for
  P8S: delete them on the NAS.

Options: `vers=3` for old NAS boxes that only speak NFS 3. Mounts are `soft`
(an unreachable NAS gives errors instead of hanging the server).

## SAN disk (iSCSI) — TrueNAS, a SAN, Windows Server iSCSI target

1. On the SAN: make a zvol/LUN (e.g. 500 GB), a target, and allow this
   server's iSCSI name (shown in Server → Network storage) as an initiator.
2. Add: type SAN disk, name `san`, SAN address → **Find disks** → pick the
   target, LUN (usually 0), CHAP user/password if the SAN asks for one,
   tick **Format the disk for VMs** (it erases the LUN).

   ```
   p8 storages find 192.168.0.20
   p8 storages add-iscsi san 192.168.0.20 iqn.2005-10.org.freenas.ctl:p8s 0 --format
   ```
3. Create → **Storage: san** (or `p8 create db1 --storage san`). The template
   is copied to the SAN (a full copy, so the first boot takes a little longer).

After reinstalling the server, add the same disk with the same name and
**without** Format: the pool and its VM disks are taken over (`zfs list
p8s-san/vms`). The VMs' settings (CPU, RAM, network) live on the server, not
on the SAN, so bring the VMs back from their backups.

Disconnecting (Remove) is refused while VMs are on the SAN disk; the data
stays on the SAN either way.

## Limits (v1)

- VM disks on NFS are not supported — use iSCSI for VM disks on a NAS.
- High availability is for VMs on local storage; Move works (the VM lands on
  the other server's local storage).
- A SAN disk must not be used by two servers at once (ZFS isn't a cluster
  file system); the SAN's initiator list is what prevents that.
- Restoring a deleted VM puts it on local storage.
