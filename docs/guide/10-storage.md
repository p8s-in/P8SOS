# 10. Storage
Summary: Local ZFS, NAS and SAN, Ceph across servers, and encryption at rest.

## Which storage for what

| Storage | Lives on | Best for | Moves between servers | HA |
| --- | --- | --- | --- | --- |
| **Local ZFS** (`tank`) | The server's own disks | Most VMs: fast, snapshots, compression | Disk is copied (incremental) | Copies every few minutes; recent changes can be lost |
| **Ceph** (`p8s-vms`) | Disks of 3+ servers, 3 copies | VMs that must never stop or lose data | Instant (only memory moves) | Restarts from the same disk; nothing lost |
| **SAN disk (iSCSI)** | A storage box | Using an existing SAN | No (one server at a time) | No |
| **NAS share (NFS)** | A NAS (TrueNAS, Synology…) | Backup copies, ISO collection | — | — |

See what's used: **Server → Storage**, `p8 storage`, `zpool status`, `zfs list`.

## Local ZFS

- VM disks are `tank/p8s/vms/<vm>-diskN`, templates `tank/p8s/templates/...`, containers `tank/p8s/ct/...`.
- Snapshots and clones are instant. Compression is on.
- A disk in a mirror or RAID died? `zpool status` names it. Put in a new disk and follow *Replacing a failed disk* in Troubleshooting (section 15). The server keeps running and every remaining disk can boot it.
- Check pool health monthly with a scrub: `sudo zpool scrub tank` (Health & alerts warns on errors).

## NAS and SAN

- **NAS share:** on the NAS, share a folder over NFS to this server, read-write, root allowed. Then Server → Network storage → Add, or `p8 storages add-nfs nas 192.168.0.20 /mnt/tank/p8s --backups`. Every backup gets a copy there; ISOs you put in `p8s/iso` appear in the ISO library.
- **SAN disk:** `p8 storages find 192.168.0.30`, then `p8 storages add-iscsi san1 192.168.0.30 <target> --format`. P8S makes a ZFS pool on it; create VMs with `--storage san1`.
- A NAS or SAN that's down at boot is skipped; press **Connect** when it's back.

## Ceph (3+ servers)

Dashboard: **Server → Ceph** on the master.

1. **Install Ceph** on each server (about 300 MB download).
2. **Set up Ceph** with the shared network, e.g. `192.168.1.0/24`.
3. **Add monitor** on two more servers (3 total) and **Add manager** on one more.
4. **Give to Ceph** one empty disk on each server (it is erased).
5. **Create the VM pool** (3 copies).
6. New VM → **Storage: ceph**.

```
p8 ceph install n2
p8 ceph init 192.168.1.0/24
p8 ceph mon n2
p8 ceph mon n3
p8 ceph mgr n2
p8 ceph osd n1 /dev/sdb
p8 ceph osd n2 /dev/sdb
p8 ceph osd n3 /dev/sdb
p8 ceph pool
p8 create web1 --storage ceph
```

Ceph VMs support snapshots, resize, extra disks, clones, instant moves and HA from the same disk. Backups work too (whole disk images, always full). Not yet: making templates from them. A disk is never deleted while another server has it open. Use a separate 10 GbE network if you can.

## Moving a VM's disks to another storage

Manage → Hardware → **Disk storage** (VM shut down), or `p8 storage web1 ceph`: every disk of the VM is copied to the other storage — local ZFS, a NAS/SAN pool or Ceph — then the old ones are deleted. Only the used part is copied. Its snapshots can't come along (tick "delete its snapshots"; take a backup first if you need them). Typical: a VM grows important → move it to Ceph, so it can move between servers in seconds and restart elsewhere if its server dies.

## Encryption at rest

Chosen at install (*Encrypt the VM disks? Yes*). Everything under `tank/p8s` is ZFS AES-256-GCM; the key is a file on the system disk, unlocked at boot. It protects disks that leave the server (stolen, sent back under warranty). **Back up the key**: Server → Storage → *Show the key…*. Without it the VM disks can't be read. It can't be switched on later in place: reinstall or move VMs to an encrypted server.
