# High availability (HA)

If a server of the cluster dies, its protected VMs start again on another
server, by themselves, within about 3 minutes. Needs a cluster
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

## When it is automatic, and when by hand

| VM runs on | Automatic failover |
|---|---|
| a worker on the master's local network | yes |
| a worker on another network (internet) | **no** — P8S OS can't tell "server off" from "internet path broken". You get an alert; when you are sure the server is off, VM → Options → High availability → **Start the copy** |
| the master | **no** — if the master dies, open the dashboard of the server with the copy (Server → High availability → **Start here**) |

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

- VMs only (containers later). One disk per VM.
- Not for VMs with a public IP from a pool (the IP belongs to one server).
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

Server to server (cluster): `GET /cluster/ha-sync`, `GET|DELETE /cluster/replica/{name}`,
`GET /cluster/replica-send/{name}`, `POST /cluster/replica-recv`,
`POST /cluster/replica/{name}/promote|retire`.
