# 2. Hardware planning
Summary: CPU, RAM, disks and network for one server or a cluster, and which disk layout to choose.

**What any server needs**

| Item | Minimum | Recommended |
| --- | --- | --- |
| CPU | 64-bit with Intel VT-x or AMD-V | 6+ cores; same CPU family on every cluster server makes live moves simplest |
| RAM | 4 GB (testing) | 32 GB+; the host keeps about 2 GB plus the ZFS cache (capped at 10% of RAM, 1–16 GB) |
| System/VM disks | 1 × 32 GB | 2+ SSDs or NVMe in a mirror; enterprise SSDs if VMs write a lot |
| Network | 1 Ethernet port | 2 ports (bond for failover) or 10 GbE for Ceph and fast moves |
| Firmware | UEFI or legacy BIOS | UEFI, Secure Boot **off** (the ZFS module isn't signed) |

**Choosing a disk layout**

| Layout | Disks | Survives | Usable of 4 × 1 TB | Use it for |
| --- | --- | --- | --- | --- |
| ZFS one disk | 1 | nothing | 1 TB (with 1 disk) | Tests, labs |
| ZFS mirror (RAID 1) | 2+ | all but one disk | 1 TB | Most servers |
| ZFS RAID 10 | 4, 6, 8… | 1 disk per pair | 2 TB | Many busy VMs: fastest |
| ZFS RAID-Z1 | 3+ | 1 disk | 3 TB | Space, file servers |
| ZFS RAID-Z2 | 4+ | 2 disks | 2 TB | Large disks, backups |
| ZFS RAID-Z3 | 5+ | 3 disks | 2 TB (with 5) | Very large arrays |
| ext4 + ZFS for VMs | 1 | nothing | system 32 GB, rest for VMs | Old machines, small RAM |

Every disk counts as the smallest one, and every disk in a mirror or RAID can boot the server on its own.

**Sizing VMs on a server.** Add up the RAM of all VMs that run at once and keep about 2 GB plus the ZFS cache free for the host. CPU can be overcommitted (4–6 virtual cores per real core for light VMs). Disk space: VMs and clones only use what they write, and ZFS compression usually saves 20–50%.

**Planning a cluster**

- **2 servers**: moves, replication and HA copies work. If the master dies you start the copies by hand (one click).
- **3+ servers**: fully automatic HA, including when the master dies. Needed for Ceph (3 monitors).
- Put servers on the same LAN for automatic failover. Servers over the internet can join, but failover there is manual.
- Ceph: at least 3 servers, each with 1+ empty disk for Ceph, and ideally a separate 10 GbE network.

> **A disk with files you want to keep** (for example a laptop's second hard disk) shows as `HAS DATA` in the installer. Never tick it: every ticked disk is erased.
