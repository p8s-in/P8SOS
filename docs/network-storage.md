# Network storage (NAS and SAN)

Server → **Network storage** (or `p8 storages`) connects storage from other machines.

| | NAS share (NFS) | SAN disk (iSCSI) |
|---|---|---|
| Holds | a copy of every backup, your ISO collection, and (optional) VM disks | VM disks |
| On this server | mounted at `/mnt/p8s/<name>`, kept in `/etc/fstab` | logged in with open-iscsi; P8S makes a ZFS pool `p8s-<name>` on it |
| VM disks: snapshots, backups, extra disks | same as local VMs (a ZFS pool in one file on the share) | same as local VMs |
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

### VM disks on the NAS share

Tick **VM disks here too** and give the space (GB), or
`p8 storages add-nfs nas 192.168.0.20 /mnt/tank/p8s --vm-disks 500`.

- P8S makes a ZFS pool `p8s-<name>` in one file on the share
  (`p8s/vmdisks-<name>.zpool`, sparse: it only takes what VMs write). VM disks
  there work like local ones: snapshots, backups (incremental), data disks,
  clones, templates. Create → **Storage: nas** or `p8 create db1 --storage nas`;
  existing VMs move there with Manage → Storage.
- The share is then mounted **hard**: if the NAS goes away for a moment, VMs
  on it pause and carry on when it is back, instead of getting disk errors.
  (A NAS that stays away also makes `ls /mnt/p8s/nas` hang until it is back.)
- At boot the pool is opened after the share is mounted; at shutdown it is
  closed after the VMs stopped and before the network goes down.
- After reinstalling the server, add the share again with the same name and
  VM disks ticked: the pool file is found and taken over with its VM disks.
- One server at a time, like a SAN disk (a ZFS pool must not be open on two
  servers). Two servers sharing one NAS: give each its own storage name.
- Speed: every write goes over the network twice as far as a SAN block disk
  (NFS + ZFS); 10 GbE and a NAS with an SSD/NVMe write cache (SLOG) help.

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

- VM disks on NFS live in one ZFS pool file per server (not one file per VM
  disk), so they can't be opened by two servers at once and moving a VM to
  another server copies its disks. For disks that several servers use at
  once (instant moves, HA without copies) use Ceph.
- The pool file can't be made bigger from the dashboard yet
  (by hand: `truncate -s +100G <file>; zpool online -e p8s-nas <file>`).
- High availability is for VMs on local storage; Move works (the VM lands on
  the other server's local storage).
- A SAN disk must not be used by two servers at once (ZFS isn't a cluster
  file system); the SAN's initiator list is what prevents that.
- Restoring a deleted VM puts it on local storage; a backup restored as a new VM (copy) can go
  onto a SAN pool or Ceph (Backups → Copy → pick the storage).
