# Ceph: storage made of the servers' own disks

Like Proxmox, P8S OS can run **Ceph** on the servers of a cluster. Every server can give empty disks,
and all of them see the same pool `p8s-vms`. VMs with their disks there:

- **move between servers without copying disks** — a live move only sends the memory (seconds, not minutes);
- **restart on another server when theirs dies** (high availability) from the very same disk — nothing is
  lost, no copies every few minutes;
- survive a dead disk or server: every block is stored **3 times on 3 different servers**.

Ceph traffic between the servers is encrypted (msgr2 secure mode) and authenticated with keys.

## What you need

- **3 servers** in a P8S cluster (Server → Cluster), each with at least one **empty** disk for Ceph.
  One or two servers work for trying it out (1 or 2 copies, no real safety).
- A network all servers are on. Ceph uses the servers' real addresses on it (not the cluster tunnel):
  a separate 10 Gbit network is best; the normal LAN works for small setups. Ports 3300, 6789 and
  6800-7300 must be reachable between the servers.
- Internet on every server for installing Ceph (about 300 MB of packages).

## Set it up (dashboard: Server → Ceph, on the master)

1. **Install Ceph** on each server (button on each server's line). A few minutes each.
2. On the master: **Set up Ceph** with the shared network (e.g. `192.168.1.0/24`). The master becomes the
   first monitor and manager. The other servers get the configuration and keys automatically.
3. **Add monitor** on two more servers (3 monitors: Ceph keeps working when one is off) and
   **Add manager** on one more (a spare).
4. **Give to Ceph** an empty disk on each server. Only disks with nothing on them are offered; the
   installer USB stick and disks in use never are. The disk is erased.
5. **Create the VM pool** (automatic: 3 copies, or one per server with disks when fewer).
6. New VM → **Storage: ceph**.

Command line (on the master):

```
p8 ceph                          # health, monitors, disks, what to do next
p8 ceph install n2               # on server n2 (name or id; nothing = this one)
p8 ceph init 192.168.1.0/24
p8 ceph mon n2 ; p8 ceph mon n3 ; p8 ceph mgr n2
p8 ceph osd n1 /dev/sdb ; p8 ceph osd n2 /dev/sdb ; p8 ceph osd n3 /dev/sdb
p8 ceph pool                     # then:
p8 create web1 --storage ceph
```

## Using it

| | VM on Ceph |
|---|---|
| Create, start, stop, console, resize, data disks | as usual |
| Snapshots, rollback | yes (Ceph snapshots of every disk) |
| Move to another server (shut down) | instant: no disk is copied |
| Move while running | only the memory moves |
| High availability | starts from the same disk on another server; extra disks included |
| Backups | yes: a whole disk image (`rbd export`) first, then only the changes since the previous backup (`rbd export-diff`; its snapshot stays on the images; a full one every 8th time or after a rollback); restore over the VM, or as a new VM (on Ceph, or local when this server has no Ceph) |
| Move its disks to local ZFS (or back) | Hardware → Disk storage (VM shut down) |

How it works: each VM disk is an RBD image `<vm>-diskN` in the pool, opened as a block device
(`/dev/rbd/p8s-vms/...`) on the server where the VM is defined. `/etc/p8s/rbd-maps` lists them and
`p8s-rbd.service` opens them at boot, before the VMs start. A disk is **never deleted while another
server has it open** — so removing the "old" copy after a move or failover can't destroy the VM.
Templates are copied into the pool once (`p8s-tpl-...`); new VMs are instant clones of them.

## Health

The dashboard (and the alert e-mails) show Ceph's own state: monitors, managers, disks up / in,
space used. A warning means Ceph is still working but something needs attention (a disk down, data
being moved); an error means VMs on Ceph may stop. Ceph stops writing at 95 % full — add disks before.

Taking a disk out: **Remove** on its line (or `p8 ceph osd-rm ID`). Ceph first moves its data to the
other disks; press Remove again when Ceph is healthy, and it is removed and wiped.

## Limits (v1)

- Backups of VMs on Ceph are full every time (no "only the changes" backups yet).
- Snapshots of a VM with several disks are taken one disk after the other (not at the same instant).
- Ceph is managed from the master; after a master failover the new master manages it if Ceph is
  installed there (every Ceph server has the configuration). Ceph itself has no master and keeps running.
- No CephFS, no erasure-coded pools, no separate cluster network setting yet.
- VM disk traffic goes through the kernel RBD client (krbd), like Proxmox's "KRBD" option.
