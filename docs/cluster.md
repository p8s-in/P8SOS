# Clusters: more servers, one dashboard

Add more machines (another laptop, a PC, a rented server) to your P8S OS server
and use them as one bigger server. The first server is the **master**: it has
the dashboard, the user accounts and billing, and decides where new VMs go.
Every added server is a **worker** ("extender") that runs VMs for it.

```
 you / customers ──► master (dashboard, users, billing, placement)
                        │  WireGuard tunnel (encrypted)
             ┌──────────┴──────────┐
          worker n2             worker n3      … up to 250 servers
```

## Adding a server

1. Install P8S OS on the new machine as usual.
2. On the master: **Server → Cluster → + Add a server → Make join command**
   (or `p8 cluster add`). It shows one line, valid for an hour, usable once:
   `p8 join https://192.168.0.106:8006 abcd…`
3. Run that line on the new machine. It joins within seconds and shows up under
   Server → Cluster and in the sidebar.

Same Wi-Fi/LAN: nothing else to do. **Over the internet** (another house, a
VPS): enter the master's public IP or name in "Address the new server uses",
and forward **TCP 8006** (joining) and **UDP 51820** (the tunnel) to the master
on its router. The worker itself can be behind any NAT or CGNAT (Jio, Airtel …):
it dials out to the master and keeps the tunnel open.

## How VMs are placed

- **Automatic** (default, and always for customers): the server with the most
  free memory gets the VM (RAM given to VMs and containers + 1.5 GB for the host
  are counted as used). A server that is offline, has no home network (for
  "Home network" VMs) or too little RAM is skipped; if none fits, creating fails
  with the reason per server.
- **Admins can pick a server** in Create VM → Server (shows free RAM per server).
- VMs installed **from an ISO** and VMs with a **public IP** go on the master for
  now (its ISO library and IP pools).

Everything else works the same for VMs on workers: console (through the tunnel),
start/stop, resize, snapshots, backups and restore, firewall, password, owner,
usage charts. Customers' quotas, plans and billing are handled on the master.

If a worker is offline its VMs still show (state "unknown") and come back when
it does. Server → Cluster shows each server's status, RAM in use and VM count.

## Security

- Join codes are random, single use, expire after an hour, and carry the
  master's certificate fingerprint: `p8 join` refuses a server that isn't the
  real master (no man-in-the-middle on first contact).
- After joining, master and worker talk only through WireGuard
  (10.250.0.0/24, interface `p8s-wg`) and pin each other's TLS certificates.
- The master calls the worker's API with a token the worker made for it; it acts
  as admin there. Accounts stay on the master; a worker trusts the master's
  checks of owners and quotas.
- Only the master may open a worker VM's screen stream.

## Commands

| | |
|---|---|
| `p8 cluster` | servers, roles, tunnel addresses, RAM in use, online/offline |
| `p8 cluster add [PUBLIC_ADDR]` | on the master: make a join command |
| `p8 join URL CODE` | on the new server: join |
| `p8 cluster remove n2 [--force]` | on the master: drop a server (refused while it has VMs unless `--force`; its VMs keep running there) |
| `p8 cluster leave` | on a worker: stand alone again |

Files: `/var/lib/p8s/cluster/state.json` (roles, keys, tokens; root only),
`wg.conf` (tunnel). `wg show p8s-wg` shows the tunnel and last handshakes.

## Not yet

- Moving a VM to another server (stop, copy the disk, start there)
- High availability (restart VMs of a dead server elsewhere; needs shared or replicated storage)
- Containers on workers (containers run on the master), ISO installs and public IPs on workers
- One shared ISO library and template list
