# 12. Cluster, moves and high availability
Summary: Join servers, move VMs (also live), no single master, and high availability.

## Joining servers

Install P8S OS on each machine as usual. The first server is the **master**.

- **Same network:** on the master, Server → Cluster → **Find servers**, pick one, **Invite**; accept on the new server (`p8 cluster find`, `p8 cluster invite NAME`, then `p8 cluster accept` there).
- **Another network (internet):** on the master `p8 cluster add PUBLIC_ADDR`, forward **TCP 8006** and **UDP 51820** to the master, then run the printed `p8 join URL CODE` on the new server. The new server can be behind NAT or CGNAT; the master must be reachable (on CGNAT, use a VPS with a public IP as the master).
- Join codes are single use, expire after an hour and pin the master's certificate. After joining, servers talk only through the WireGuard tunnel (10.250.0.0/24).

New VMs go to the server with the most free RAM, or the one an admin picks. Each worker has a **>\_ Shell** button in Server → Cluster.

The sidebar on the master lists every server of the cluster, marked **master** or **worker**, with its VMs under it and a dot: green online, amber connecting (just joined), red offline. A worker with no VMs is listed too. Click a worker to open Server → Cluster with its Shell / Rename / Remove buttons.

## Renaming and removing servers

| Task | Dashboard | Command |
|---|---|---|
| Rename this server | Server page → **Rename** (top right) | `p8 rename NEWNAME` |
| Rename a worker | On the master: Server → Cluster → **Rename** on its row | `p8 cluster rename n2 NEWNAME` |
| Remove a worker | On the master: Server → Cluster → **Remove** on its row | `p8 cluster remove n2` |
| Leave the cluster | On the worker: Server → Cluster → **Leave the cluster** | `p8 cluster leave` |

Give every server its own name before joining (two servers both called `p8s-node` are hard to tell apart). A rename shows on every server within a few seconds; VMs keep running. **Remove** tells the worker to stand alone again; its VMs keep running there but disappear from the master's dashboard (move them first if they should stay in the cluster). An offline worker can't be told: press **Leave the cluster** on it when it is back. A removed server can be added again any time with **+ Add a server**.

## Moving VMs

| Move | How | Time | Notes |
| --- | --- | --- | --- |
| Shut down (local ZFS) | Manage → Options → Move to another server, or `p8 move web1 n2` | Copy of the used disk (1 Gbit/s ≈ 100 MB/s) | UEFI and TPM come along; private IP changes |
| Live (local ZFS) | **Move while running**, or `p8 move web1 n2 --live` | Whole disk + memory (20 GB ≈ 3–4 min at 1 Gbit/s) | Pause under a second. Same CPU, or set `p8 cpu web1 compatible` |
| Ceph, shut down | Same | Instant | No disk copied |
| Ceph, live | Same | Seconds | Only memory moves |

UEFI and Windows 11 VMs move live too (UEFI variables and TPM state come along); a VM on a SAN disk lands on the other server's local storage. Not live: VMs with a public IP, a CD inserted or PCI/USB devices — move those shut down. VMs with data disks move (live or shut down) and are protected by HA with all their disks. Snapshots and old backups stay behind; take a new backup after a move.

Containers run on any server too (pick the server when creating). Move a stopped one with Manage → **Move to server** or `p8 ct-move web2 n2`; containers with a public IP stay on the master, and HA doesn't cover containers yet.

## No single master (3+ servers)

Every server keeps the *cluster book* (servers, keys, accounts, settings). If the master dies, after about 45 seconds the others elect a new master by **majority** (2 of 3, 3 of 5) and carry on; VMs keep running. The old master rejoins as a worker when it returns. A server cut off from the majority switches its protected VMs off, so a VM never runs twice.

With **two servers** nobody can be outvoted: if the master is really gone, run `p8 cluster promote` on the worker (or the button in Server → Cluster). Only do this when the old master is off for good.

### A witness for two servers

Or add a **witness** (like Proxmox's QDevice): a tiny program on any always-on Linux box that is *not* one of the two servers — a Raspberry Pi, a NAS, a small VPS. It is the third vote, so two servers then behave like three: if the master dies, the other server takes over by itself after about a minute; a server cut off from both never does.

1. Server → Cluster → **Add a witness…** → download `p8s-witness` (amd64 for a PC/VPS, arm64 for a Raspberry Pi). The servers also keep both in `/usr/local/share/p8s/witness/`.
2. Copy it to the box and run `chmod +x p8s-witness-* && sudo ./p8s-witness-* install`. It starts at boot and prints an address, a key and a fingerprint.
3. Paste the three into the dashboard (or `p8 cluster witness https://ADDRESS:8007 KEY FINGERPRINT` on the master). Both servers must reach the box on TCP 8007.

`p8 cluster witness` shows whether it answers; `p8 cluster witness off` removes it.

## High availability (HA)

Turn it on per VM: Manage → Options → **High availability**, pick the second server and the copy interval (default 15 minutes, 1–1440).

![High availability: a dead server's protected VMs restart on their copy in about 3 minutes](ha-failover.svg)

Nothing has to be done by hand when the dead server returns: it hands its role over and becomes the copy.

- **Local ZFS VMs:** the disk is copied to the second server every interval (only changes after the first copy). On failover, changes after the last copy are lost — pick a short interval for databases.
- **Ceph VMs:** no copies needed; the VM starts from the same disk on another server and nothing is lost.

| VM runs on | Automatic failover |
| --- | --- |
| A worker on the master's LAN | Yes, within about 3 minutes |
| A worker over the internet | No: you get an alert; when sure it's off, Options → High availability → **Start the copy** |
| The master | Yes with 3+ servers (new master starts the copies); with 2: **Start here** on the other server |

Only start a copy by hand when the VM's server is really off (unplug it if unsure), otherwise the VM runs twice with the same disk and IP.
