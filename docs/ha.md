# High availability (HA)

If a server of the cluster dies, its protected VMs and containers start again
on another server, by themselves, within about 3 minutes. Needs a cluster
([cluster.md](cluster.md)) of at least 2 servers.

## How it works

- **Copies.** For each protected VM, the master copies the VM's disk to a second
  server every few minutes (default 15; 1 to 1440). The first copy is the whole
  disk, after that only what changed (ZFS replication, compressed, over the
  cluster tunnel). The VM keeps running while it is copied; the copy is like
  the disk after a power cut (journaling file systems and databases handle that).
- **Failover.** When the VM's server stops answering for 3 minutes (and its
  tunnel is silent too, so the machine is really down, not only P8S OS on it),
  the master starts the copy on the second server: same name, MAC address,
  settings, firewall, UEFI variables and TPM. **What changed after the last copy
  is lost** — pick a shorter interval for VMs where that matters.
- **Coming back.** When the dead server is back, its old VM is switched off and
  becomes the copy; copies then go the other way. Nothing to do by hand.
- **Never twice.** A worker that hears nothing from the master for 60 seconds
  *and* can't reach its own router switches its protected VMs off (it is the
  one cut off, so the master may start the copies). It starts them again when
  the master says so. The master never fails over while it can't reach its own
  router itself.
- Protected VMs don't use the host's autostart; HA starts them after a reboot
  or an outage if they were running. A crashed VM is started again.

## Live mirror (P8S Mirror) — nothing lost

Instead of a copy every few minutes, a VM can be protected with a **live
mirror**: VM → Options → High availability → How: **Live mirror**. Every write
of the VM goes to both servers before the VM is told it is done, so the second
server always has an exact copy. If the VM's server dies, the VM starts there
with nothing lost.

- Built from parts that have run for years: ZFS volumes on both servers,
  QEMU's own block mirror (write-blocking mode) and NBD over the cluster
  tunnel (WireGuard); P8S OS drives it (no Ceph, no monitors, almost no RAM).
- Both servers must be on the **same local network**: each write waits for the
  other server (a few tenths of a millisecond on a LAN).
- The first sync copies the whole disk in the background while the VM runs,
  and **again every time the VM starts** (only the changes: a later version).
  While a sync runs, the copy keeps a snapshot of its last complete state:
  if the VM's server dies then, the copy goes back to that state (a crash
  copy of then) instead of a half-copied disk.
- If the other server or the network goes away, the VM keeps running on its
  own disk; the mirror starts again (a full sync) when the other server is back.
- A live move (Move → live) stops the mirror; it starts again from the new server.
- Disks grow only while the VM is off (as always); the next start syncs the new size.
- VMs only (containers: copies). Ports 10810-10899 on the cluster tunnel.

## Containers

Containers ([containers.md](containers.md)) are protected the same way:
container → Manage → **High availability**. Their disk (a ZFS dataset) is
copied with `zfs send` every few minutes, also while the container runs; the
other server keeps it as `<pool>/p8s/ct-replica/NAME` (not mounted). On
failover the copy becomes the container there (same name, MAC, sizes, owner,
network; a network that server doesn't have becomes the private one) and is
started if it was running. Fencing, coming back, autostart and the limits
below are as for VMs. The Server → High availability list marks them
"container".

## When it is automatic, and when by hand

| VM runs on | Automatic failover |
|---|---|
| a worker on the master's local network | yes |
| a worker on another network (internet) | **no** — P8S OS can't tell "server off" from "internet path broken". You get an alert; when you are sure the server is off, VM → Options → High availability → **Start the copy** |
| the master | **yes with 3+ servers** — the others elect a new master, which starts the copies; with 2 servers: open the dashboard of the server with the copy (Server → High availability → **Start here**), or `p8 cluster promote` there |

Only start a copy by hand when the VM's own server is really off (unplug it if
you aren't sure): otherwise the VM runs twice with the same disk and IP.

## Using it

- VM → Manage → Options → **High availability**: choose the server for the
  copy and how often, press **Protect**. The box shows the last copy and
  whether failover is automatic. **Turn off** deletes the copy.
- Server → **High availability**: all protected VMs, the copies this server
  keeps (Start here / Delete), timings (fence 60 s, failover 180 s) and a log.
- Alerts (Server → Health & alerts) are sent for failovers, restarts and
  failed copies.

## Limits

- VMs and containers on local ZFS. VMs with data disks are copied with all their disks, snapshotted at
  the same instant (a disk added later is copied in full once, then only its changes). VMs on
  **Ceph** ([ceph.md](ceph.md)) need no copies at all: they start from the same disks and lose nothing.
- Not for VMs or containers with a public IP from a pool (the IP belongs to one server).
- The copy needs room on the second server's pool (about the used size of the disk).
- With 2 servers on the same network, a broken switch port between them while
  both still reach the router could make both run the VM. Use a good switch;
  3+ servers and shared storage come later.
- Your own snapshots aren't copied; backups stay on each server.

## API

| Method | Path | |
|---|---|---|
| GET | `/api/v1/vms/{name}/ha` | `{"protected","config","why","targets"}` |
| PUT | `/api/v1/vms/{name}/ha` | `{"enabled":true,"target":"n2","every_min":15}` / `{"enabled":false}` |
| POST | `/api/v1/vms/{name}/ha/failover` | start the copy now (the VM's server must be offline) |
| GET | `/api/v1/ha` | protected VMs, copies kept here, timings, log |
| PUT | `/api/v1/ha/settings` | `{"fence_after_s":60,"failover_after_s":180}` |
| POST | `/api/v1/ha/replicas/{name}/start` · DELETE `/api/v1/ha/replicas/{name}` | a copy kept on this server: start it here / delete it |
| PUT | `/api/v1/vms/{name}/autostart` | `{"autostart":bool}` (refused for protected VMs) |
| GET · PUT | `/api/v1/containers/{name}/ha` | the same for a container |
| PUT | `/api/v1/vms/{name}/ha` | `{"enabled":true,"target":"n2","mode":"mirror"}`: a live mirror instead of copies |
| POST | `/api/v1/containers/{name}/ha/failover` | start a container's copy now |
| POST · DELETE | `/api/v1/ha/replicas/{name}…?kind=ct` | a container's copy kept here (the list shows `"kind":"ct"`) |

`PATCH /api/v1/containers/{name}` with `"autostart":true` is refused while the container is protected.

Server to server (cluster): `GET /cluster/ha-sync`, `GET|DELETE /cluster/replica/{name}`,
`GET /cluster/replica-send/{name}`, `POST /cluster/replica-recv`,
`POST /cluster/replica/{name}/promote|retire` (with `?kind=ct` for containers;
`GET /containers/{name}/ha-check`).
