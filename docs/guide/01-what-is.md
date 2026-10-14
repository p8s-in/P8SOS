# 1. What P8S OS is
Summary: The parts P8S OS is built from, how they fit, and the words this guide uses.

P8S OS turns one or more ordinary computers into a private cloud. You install it from a USB stick or ISO, open a web page, and make virtual machines (VMs) and containers. Several servers join into one cluster that you run from a single dashboard. It does the same job as Proxmox VE, built on the same proven parts.

| Part | What it is | What it does for you |
| --- | --- | --- |
| Debian 13 | The base operating system | Stable, long support, every driver |
| KVM + libvirt + QEMU | The hypervisor | Runs Linux and Windows VMs at near-native speed |
| LXC | Containers | Light Linux systems that share the host kernel |
| ZFS | The file system and volume manager | Mirrors/RAID, instant snapshots, compression, copy-on-write clones, replication |
| Ceph (optional) | Storage spread across servers | VM disks every server can reach: live moves and HA without copying |
| p8sd | The P8S OS daemon (Go) | The API, jobs, cluster, HA, backups, monitoring |
| Dashboard | Web interface on port **8006** | Everything you do day to day |
| `p8` | Command line tool | The same things from a shell or script |

**How the parts fit.** Every server runs p8sd. You talk to it through the dashboard or `p8`; it drives libvirt for VMs, LXC for containers, and ZFS or Ceph for disks. Servers in a cluster talk to each other through an encrypted WireGuard tunnel (UDP 51820).

**Words used in this guide**

- **Server / node**: one physical machine running P8S OS.
- **Master**: the server that holds users, billing and placement for a cluster. If it dies, the others elect a new one (no single master).
- **Worker**: any other server in the cluster.
- **Template**: a ready OS image (Debian, Ubuntu, Rocky…) or one you made from your own VM; new VMs are instant copies of it.
- **Pool `tank`**: the main ZFS pool, where the system and VM disks live.
