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

Install P8S OS on the new machine as usual, then on the master open
**Server → Cluster → + Add a server**. There are two ways:

### On this network (same Wi-Fi / LAN)

The master searches the network and lists every P8S OS server it finds, with
its CPUs, RAM, version and whether it can join (not if it is already in a
cluster or runs another P8S OS version). Click **Add**:

1. The new server gets an **invitation** — a box in its dashboard, a line in its
   log, and `p8 cluster accept` on its command line.
2. Both screens show the same **6-digit number**. Accept only if they match.
3. It joins; the master shows "✔ joined" and puts VMs there from now on.

Nothing joins just by being found: someone must accept on that server.
Command line: `p8 cluster find`, `p8 cluster invite NAME` (master),
`p8 cluster accept` (new server). Servers answer the search on UDP port 51821.
Not found (e.g. another subnet)? "Add it with a command instead" gives a
one-time `p8 join …` line for this network.

### Another network (other house, office, VPS)

1. Enter the master's **public address** (filled in with your current public IP;
   better a dynamic-DNS name like `myhome.duckdns.org` if your IP changes).
2. On the master's router forward **TCP 8006** (joining) and **UDP 51820** (the
   tunnel) to the master. **Check** tests the address (a LAN or CGNAT address is
   refused at once; some routers can't test themselves).
3. **Make join command** gives a one-time line with the public address:
   `p8 join https://myhome.duckdns.org:8006 abcd…` — run it on the new server.
   Command line: `p8 cluster add myhome.duckdns.org`.
4. In Server → Cluster the new server shows **tunnel up** once WireGuard has
   connected; "tunnel not up yet" means UDP 51820 isn't getting through.

The new server can be behind any NAT or CGNAT: it dials out to the master. The
**master** must be reachable; if your home line is CGNAT (Jio/Airtel often are),
make a machine with a public IP (e.g. a VPS) the master instead.

## How VMs are placed

- **Automatic** (default, and always for customers): the server with the most
  free memory gets the VM (RAM given to VMs and containers + 1.5 GB for the host
  are counted as used). A server that is offline, has no home network (for
  "Home network" VMs) or too little RAM is skipped; if none fits, creating fails
  with the reason per server.
- **Admins can pick a server** in Create VM → Server (shows free RAM per server).
- **One ISO library**: the master's. A VM installed from an ISO can go on any
  server; the ISO (and a drivers ISO) is copied there first, once, and kept for
  the next install (a 5 GB Windows ISO takes about a minute at 1 Gbit/s).
  Deleting an ISO on the master deletes the copies too (not while a VM there
  still has it inserted).
- **One template list**: templates made from a VM are listed with the servers
  that have them; new VMs from such a template go on one of those. Copy one to
  other servers with Server → OS templates → Made from VMs → Copy
  ([clones-templates.md](clones-templates.md)).
- VMs with a **public IP** go on the master for now (its IP pools).

Each worker has a **>_ Shell** button in Server → Cluster: a root shell of that
server in the browser, through the tunnel (logged on both servers).

Everything else works the same for VMs on workers: console (through the tunnel),
start/stop, resize, snapshots, backups and restore, firewall, password, owner,
usage charts. Customers' quotas, plans and billing are handled on the master.

If a worker is offline its VMs still show (state "unknown") and come back when
it does. Server → Cluster shows each server's status, RAM in use and VM count.

## Moving a VM to another server

VM → Manage → Options → **Move to another server** (admins). Pick the server,
leave "Start it there" ticked, press Move (twice, to confirm). The move runs as
a job (shown like a backup), from any server to any other, also worker → worker.

- The VM must be **shut down**. Not yet movable: VMs with a public IP from a pool
  (release it first) and VMs with an installer CD in the drive (eject it first).
- What happens: a ZFS snapshot of the disk is sent over the tunnel (compressed,
  only the used blocks), the VM is defined on the new server with the same name,
  UUID, CPU/RAM, firmware, disk/NIC type, MAC address, owner, network, firewall
  rules and backup schedule, then the original is deleted. If anything fails
  before that, the original stays untouched and the half copy is removed.
- What changes: the IP address (each server has its own private network) and the
  SSH / RDP port (shown in the VM's Login box). A "Home network" VM moved to a
  server without one lands on the private network.
- What stays behind: snapshots and old backups (they're on the old server).
  Take a new backup after the move.
- Time: about as long as copying the used part of the disk over your network
  (1 Gbit/s ≈ 100 MB/s).

API: `GET /api/v1/vms/{name}/move` (where it can go, or why not),
`POST /api/v1/vms/{name}/move {"to":"n2","start":true}` → job.

UEFI variables and the TPM state move with the VM (Windows 11 keeps its boot
entries and BitLocker keys).

## Moving a running VM (live move)

A running VM can move without being shut down: Manage → Options → Move to
another server → **Move while running**, or `p8 move web1 server2 --live`.
QEMU copies its memory and disks to the other server while it keeps working,
then switches over; the pause at the end is well under a second, so SSH
sessions and connections normally survive. Its private IP can still change
(each server has its own NAT network) — VMs on the home network keep their IP.

- **Time:** the whole size of every disk crosses the tunnel (empty space too)
  plus the memory; 20 GB of disks over 1 Gbit/s ≈ 3-4 minutes.
- **Same CPU, or "compatible":** a VM normally gets the server's own CPU
  (fastest). It can move live only to a server with exactly the same CPU. For
  servers with different CPUs, set **CPU type → Compatible with every server**
  (`p8 cpu web1 compatible`), shut it down and start it once: it then runs on
  the newest CPU model all servers share and can move live anywhere. A server
  added later with an older CPU: set it again.
- **Live moves** work for UEFI VMs and Windows 11 too (their UEFI variables and TPM state come
  along) and for VMs on a SAN disk (they land on the other server's local storage, like a normal
  move). Not live: VMs with a public IP, a CD in the drive or PCI/USB devices — move those shut down.
- **VMs on Ceph** ([ceph.md](ceph.md)) move without copying any disk: shut down it is instant,
  running only the memory goes over.
- **How:** libvirt on each server listens on its tunnel address only
  (`scripts/libvirt-migrate.sh`, port 16509 and 49152-49215, firewalled to the
  tunnel); `virsh migrate --live --copy-storage-all` runs on the old server.
  Worker → worker moves pass through the master. If anything fails, the VM keeps
  running where it was and the half copy is removed.

## No single master (3 or more servers)

Every server keeps the **cluster book**: all servers, the keys to reach them,
and the master's settings (accounts, high availability, IP pools, security,
company sign-in). The master hands it out every 15 seconds.

- **The master dies:** after about 45 seconds without the book, the servers
  hold an election. The lowest-numbered server that the others still reach
  asks for votes; with a **majority of all servers** (2 of 3, 3 of 5 …) it
  becomes the master of the next *term*, takes the settings from the book and
  restarts p8sd (VMs keep running). Open its dashboard to manage the cluster.
  High availability carries on there: protected VMs of the dead master start
  on their copies.
- **The old master comes back:** it learns about the newer term and becomes a
  worker of the new master.
- **No two masters:** a master that can't reach a majority for 30 seconds
  stops acting as master — no failovers, no leases — and switches its
  protected VMs off (they start on the majority side). A worker cut off from
  the majority switches its protected VMs off too. A server alone can never
  become master.
- **Two servers:** nobody can be outvoted, so there is no automatic takeover.
  When the master is really gone: `p8 cluster promote` on the worker (or the
  button in Server → Cluster). Don't do this while the old master is only cut
  off — it would come back as a second master until it hears of the new term.
- **Two servers and a witness:** `p8s-witness` (cmd/p8s-witness) on a third
  box is the third vote, so the majority rules above apply. The master sends
  it a heartbeat with every book; it counts toward the master's majority. A
  worker that hears no master asks the witness first: if the witness heard the
  master in the last 22 s, only the worker is cut off and nothing happens.
  Otherwise the witness gives its vote (once per term, for a newer term only)
  and the worker becomes master with 2 of 3 votes. An old master whose
  heartbeat the witness refuses (older term) steps down. The witness's
  certificate is pinned (sha256 printed by `p8s-witness init`), calls carry
  its key, and its settings travel in the cluster book. If the witness is
  down, the two servers carry on as before (2 of 3 votes).
- **Tunnels:** servers now connect to each other directly (each listens on
  UDP 51820) where they can; pairs that can't (both behind NAT on other
  networks) still go through the master.
- **What stays on the old master:** P8S Cloud billing data and the support
  inbox, containers with public IPs, the update/licence server data.

## High availability

Protected VMs and containers are copied to a second server every few minutes
and started there if their server dies. See [ha.md](ha.md).

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
| `p8 cluster find` / `p8 cluster invite NAME` | on the master: servers on this network / invite one |
| `p8 cluster accept` / `p8 cluster decline ID` | on the new server: answer an invitation |
| `p8 cluster add` / `p8 cluster add PUBLIC_ADDR` | on the master: join command for this network / another network |
| `p8 join URL CODE` | on the new server: join with a command |
| `p8 cluster remove n2 [--force]` | on the master: drop a server (refused while it has VMs unless `--force`; its VMs keep running there) |
| `p8 cluster leave` | on a worker: stand alone again |
| `p8 cluster witness URL KEY FINGERPRINT` / `off` | on the master of 2 servers: add / remove the tie-breaker; without arguments: is it answering |

Files: `/var/lib/p8s/cluster/state.json` (roles, keys, tokens; root only),
`wg.conf` (tunnel). `wg show p8s-wg` shows the tunnel and last handshakes.

## Not yet

- Moving a running container without any pause (it moves with a short pause: [containers.md](containers.md))
- Public IPs for containers on workers (they stay on the master)
