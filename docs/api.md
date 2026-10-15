# p8sd REST API — v1

Base URL: `https://<host>:8006/api/v1` (self-signed certificate unless `tls_cert`/`tls_key` are set).
All bodies are JSON. Unknown fields in request bodies are rejected.

## Authentication

Every endpoint except `/health`, `/auth` and `/login` needs a bearer token:

```
Authorization: Bearer p8s_<64 hex chars>
```

Two kinds of token work:

- **API tokens** (`p8sd token -name <label>`; the `p8` command uses one from
  `~/.p8s-token`). Only their SHA-256 is stored in `/etc/p8s/p8sd.json`.
  They act as an admin.
- **Sign-in tokens** from `POST /login` with a user name and password (plus a
  6-digit code when that user has 2FA on). They expire after 12 h unused
  (7 days at most) and on `POST /logout`, a password change, or when the
  account is disabled. Sign-ins survive p8sd restarts (`/var/lib/p8s/sessions.json`, token hashes only).

### Users, roles and plans

| role | can |
|---|---|
| `admin` | everything: all VMs, host details, ISOs, users, the whole activity log |
| `customer` | only VMs they own, within their plan (quota); ISOs can be used, not added |

A customer's plan: `{"vms":2,"vcpus":4,"memory_mib":4096,"disk_gb":60,"lan":false}`
(totals across all their VMs; `lan` = may use the home/LAN network). Creating,
resizing or restoring past the plan returns `403` with code `quota`. Another
user's VM answers `404`, exactly like a VM that doesn't exist.

| endpoint | who | what |
|---|---|---|
| `GET /auth` | anyone | `{"accounts": true}` once user accounts exist |
| `POST /login` | anyone | `{"user","password","otp"}` → `{"token","idle_timeout","user"}`; `401 otp_required` asks for the code; 5 failures per user (20 per IP) in 15 min → `429` |
| `POST /logout` | signed in | ends this sign-in |
| `GET /me` | signed in | name, role, plan, usage, 2FA state |
| `POST /me/password` | user | `{"old_password","new_password"}` (10+ chars); signs out other devices |
| `POST /me/2fa/setup` | user | `{"secret","uri"}` for an authenticator app (TOTP, 30 s, 6 digits) |
| `POST /me/2fa/enable` | user | `{"code"}` turns 2FA on |
| `POST /me/2fa/disable` | user | `{"code"}` turns it off |
| `GET /users` | admin | users with their usage |
| `POST /users` | admin | `{"name","role","password","quota"}` |
| `PATCH /users/{user}` | admin | any of `role`, `quota`, `disabled`, `password`, `reset_2fa` |
| `DELETE /users/{user}` | admin | only when they own no VMs; the last admin can't be removed |
| `PUT /vms/{name}/owner` | admin | `{"owner":"alice"}` (`""` = admins only); works while the VM runs |
| `POST /vms` with `"owner"` | admin | make a VM for a customer |
| `GET /audit?limit=200` | signed in | who changed what, newest first (customers: their own) |
| `PUT /me/ssh-keys` | user | `{"keys":[...]}` put into the user's new VMs when they give none |

Customer VMs never get the host's default SSH keys. Server errors show
customers only a reference (`ref 1a2b3c4d`); the details are in p8sd's log.

### Public websites (Cloudflare Tunnel) — see [public-access.md](public-access.md)

| endpoint | who | what |
|---|---|---|
| `GET /tunnel` | signed in | `{"set_up","domain","dashboard"}` (+ tunnel id/name for admins) |
| `POST /tunnel/setup` | admin | `{"api_token","domain","name"}` → status; p8sd then runs cloudflared itself (`connector.state`: starting / connected / error / not_installed) |
| `DELETE /tunnel` | admin | public access off: routes, DNS records and cloudflared removed |
| `PUT /tunnel/dashboard` | admin | `{"name":"panel"}` publishes the dashboard as panel.<domain>; `""` removes it |
| `GET /sites`, `GET /vms/{name}/sites` | signed in | published websites (customers: their own) |
| `POST /vms/{name}/sites` | owner/admin | `{"name":"shop","port":80,"https":false}` → `201` site; counts against `quota.sites` |
| `DELETE /sites/{host}` | owner/admin | takes it off (route and DNS record) |

### P8S Cloud — see [p8s-cloud.md](p8s-cloud.md)

| endpoint | who | what |
|---|---|---|
| `POST /signup` | anyone (when open) | `{"user","email","password","full_name"}` → `201 {"token"}` or `202 {"verify":true}` (email link) |
| `POST /verify`, `/verify/resend` | anyone | confirm email with the link's code (→ token) / send it again |
| `POST /password/forgot`, `/password/reset` | anyone | email a reset link / `{"code","password"}` |
| `PUT /me/profile` | user | email, full_name, phone, company, gstin, address |
| `GET/POST /me/tokens`, `DELETE /me/tokens/{id}` | user (signed in) | personal API tokens `p8sk_…`. `POST {"name","read_only":true,"vms":["web1"],"days":30}` (or `"expires_at"`): a scoped token — read-only answers `403` to changes; a VM-scoped token sees only `GET /vms` (filtered), `/vms/{name}/…` of its VMs and `GET /me`; expired tokens get `401` |
| `GET/PUT /vms/{name}/shares` | owner or staff | per-VM permissions `{"user":"bob","level":"user"\|"viewer"\|""}`; shared users see the VM in `GET /vms` with `"access"`. **user**: power, console, password, snapshots, backups, eject, screenshots; reads everything else. **viewer**: GET only. Never: delete, resize, hardware, owner, sharing further |
| `GET /plans`; `PUT/DELETE /plans/{id}` | all; admin | sizes and monthly prices (`hourly` = price/730 rounded up) |
| `POST /vms` with `"plan"` | customer, admin | size from the plan; billing starts; needs a day of credit |
| `GET /billing` (`?user=` admin) | signed in | balance, hourly burn, billed servers, suspended, payment info |
| `GET /billing/entries`, `/billing/invoices`, `/billing/invoices/{number}` | owner, admin | transactions and tax invoices |
| `POST /billing/topup` → `POST /billing/topup/verify` | customer | Razorpay order → Checkout → signature check, capture, credit, invoice |
| `POST /billing/razorpay/webhook` | Razorpay | `payment.captured` (signature `X-Razorpay-Signature`) |
| `POST /billing/adjust` | admin | `{"user","amount","note","invoice"}` credit/debit; `invoice` = payment received |
| `GET /billing/customers` | admin | balances, servers, monthly spend |
| `GET/PUT /cloud/settings`, `POST /cloud/test-email`, `GET /cloud/outbox` | admin | brand, sign-up, tax, seller, Razorpay, SMTP (secrets shown as `********`) |
| `GET/POST /tickets`, `GET /tickets/{id}`, `POST /tickets/{id}/reply`, `/close` | owner, admin | support requests |

### Public IP pools — see [public-ips.md](public-ips.md)

| endpoint | who | what |
|---|---|---|
| `GET /ippools` | signed in | admins: pools with `used` (address → VM), size, free; customers: name/free/size of pools open to them |
| `POST /ippools` | admin | `{"name","bridge","cidr","gateway","dns":[],"exclude":[],"customers":false}` |
| `PATCH /ippools/{pool}` | admin | `{"dns","exclude","customers"}` |
| `DELETE /ippools/{pool}` | admin | only when no VM uses it |
| `POST /vms` with `"ip_pool":"dc1"` | admin, customer (open pool, `quota.public_ips`) | VM on the pool's bridge with the next free address (cloud-init static); the VM object gets `"network":"public"` and `"public":{"pool","ip","prefix","gateway","dns","bridge"}` |

### Per-VM firewall

`GET /vms/{name}/firewall`, `PUT /vms/{name}/firewall` (owner or admin):

```json
{"enabled": true, "rules": [
  {"proto": "tcp", "ports": "22", "comment": "SSH"},
  {"proto": "tcp", "ports": "80,443"},
  {"proto": "udp", "ports": "51820", "source": "203.0.113.0/24"},
  {"proto": "icmp"}
]}
```

Off (default): everything may connect. On: only the listed incoming traffic
reaches the VM; replies to the VM's own connections, ARP, IPv6 neighbor
discovery and DHCP always pass. `proto` tcp/udp/icmp (ping); `ports` like
`22`, `80,443`, `8000-8100`; `source` an IPv4/IPv6 address or network (empty =
anywhere); up to 32 rules. Enforced by nftables (`table bridge p8s_fw`,
matched by the VM's MAC) for private and home-network VMs alike; needs the
`nf_conntrack_bridge` kernel module. If the rules can't be loaded the change
is refused and nothing changes. CLI: `p8 fw NAME [on|off|allow tcp 22 [from CIDR]|allow ping|remove N]`.

Users live in `/var/lib/p8s/users.json` (passwords as PBKDF2-SHA256), the
activity log in `/var/lib/p8s/audit.log`.

## Errors

```json
{ "error": { "code": "not_found", "message": "not found: vm \"web9\"" } }
```

| HTTP | code          | when                                       |
|------|---------------|--------------------------------------------|
| 400  | `bad_request` | body is not valid JSON / unknown field     |
| 400  | `invalid`     | validation failed (message lists all)      |
| 401  | `unauthorized`| missing or wrong token                     |
| 404  | `not_found`   | VM does not exist                          |
| 405  | —             | wrong method for the path                  |
| 409  | `exists`      | VM name already taken                      |
| 500  | `internal`    | host-side failure; message has the cause   |

## VM object

```json
{
  "name": "web1",
  "state": "running",
  "template": "debian-13",
  "vcpus": 2,
  "memory_mib": 2048,
  "disk_gb": 20,
  "mac": "52:54:00:3a:91:c4",
  "network": "nat",
  "ip": "192.168.122.57",
  "autostart": true,
  "uuid": "6f1e4c1a-6c0b-4f1e-9a51-0f6f3a7f9b20",
  "created_at": "2026-10-10T12:00:00Z"
}
```

`state` is one of `running`, `stopped`, `stopping`, `paused`, `crashed`, `unknown`.
`ip` is the guest's first IPv4 address (from libvirt DHCP leases, the guest agent,
or the ARP table); it is empty while the guest boots and when it is stopped.
`autostart` means the VM starts automatically when the host boots.
`network` is `nat` (private network behind the host, 192.168.122.x) or `lan`
(bridged onto the home network; the router hands out the IP).

## Endpoints

### `GET /health` (no auth)
`200 {"status":"ok","version":"0.1.0-dev"}`

### `GET /host`
`200 {"hostname":"p8s-node","cpus":8,"load1":0.2,"mem_total_mib":15872,"mem_free_mib":12000,
"pool_size_gb":200,"pool_free_gb":180,"uptime_sec":3600,"networks":["nat","lan"]}` —
host capacity for the dashboard. `networks` lists what new VMs can join.

### `GET /templates`
`200 {"templates":[{"name":"debian-13","min_disk_gb":3,"os_variant":"debian13"}]}`

### `GET /vms`
`200 {"vms":[VM, ...]}` — sorted by name. Only VMs created by p8sd
(libvirt domains named `p8s-*`) are listed.

### `POST /vms` — create
```json
{
  "name": "web1",
  "template": "debian-13",
  "vcpus": 2,
  "memory_mib": 2048,
  "disk_gb": 20,
  "ssh_keys": ["ssh-ed25519 AAAA... you@laptop"],
  "user_data": "#cloud-config\n...",
  "network": "nat",
  "start": true,
  "autostart": true
}
```

| field       | required | default              | rules                                         |
|-------------|----------|----------------------|-----------------------------------------------|
| name        | yes      | —                    | `^[a-z][a-z0-9-]{0,47}[a-z0-9]$`              |
| template    | yes      | —                    | one of `GET /templates`                       |
| vcpus       | no       | 1                    | 1 … `max_vcpus`                               |
| memory_mib  | no       | 1024                 | 256 … `max_memory_mib`                        |
| disk_gb     | no       | template min         | template `min_disk_gb` … `max_disk_gb`        |
| ssh_keys    | no       | []                   | OpenSSH public keys                           |
| user_data   | no       | generated            | must start with `#cloud-config`; replaces the generated config entirely |
| network     | no       | nat                  | `nat` or `lan`; `lan` needs `sudo p8 lan-setup` once (see `GET /host` → `networks`) |
| root_password | no     | none (root locked)   | 8–128 chars. Root gets this password (stored only as a SHA-512 crypt hash in the seed; never returned). Can't be combined with `user_data` |
| ssh_password_auth | no | true if root_password | also allow `ssh root@vm` with that password (`PermitRootLogin yes`) |
| start       | no       | true                 | define only when false                        |
| autostart   | no       | true                 | start the VM when the host boots              |

What happens: ZFS clone of the template snapshot → resize → cloud-init seed
ISO → libvirt define → start. Any failure rolls back everything created.

`201` + `Location: /api/v1/vms/web1` + VM object.

The generated cloud-config sets the hostname, installs your SSH keys for the
image's default user (`debian` on Debian cloud images), grows the root
filesystem, and installs `qemu-guest-agent`.

### `GET /vms/{name}`
`200` VM object.

### `POST /vms/{name}/start`
Idempotent. `200` VM object.

### `POST /vms/{name}/stop`
Body optional: `{"force": true}` pulls the power; default is ACPI shutdown.
`202` VM object — with ACPI the state will be `stopping` until the guest powers off.
Idempotent on a stopped VM.

### `POST /vms/{name}/reboot`
ACPI reboot of a running guest. `202` VM object. `409 conflict` if the VM is not running.

### `DELETE /vms/{name}`
Force-stops if running, undefines the domain, destroys the disk and seed ISO.
`204`. **Irreversible.** Also deletes the VM's snapshots.

### `PATCH /vms/{name}` — resize
`{"vcpus":4, "memory_mib":4096, "disk_gb":40}` — any subset. The VM must be **stopped**
(`409` otherwise). Disks only grow (`400` on shrink); the guest's root filesystem grows
to the new size on its next boot (cloud-init growpart). `200` + VM object.

### Extra disks
A VM has its system disk (`disk_gb`) and up to 8 data disks, listed in the VM object as
`"disks":[{"index":1,"size_gb":50,"device":"vdb"}]`. `index` stays the same while the disk
exists, so it keeps its device name inside the guest (`vdb`…`vdi` on virtio, `sdf`…`sdm` on
SATA/SCSI) when another disk is removed. They count toward a customer's disk quota.

- `POST /vms/{name}/disks` `{"size_gb":50}` → `201` VM object plus `"note"` (what to do next).
  A running virtio/SCSI VM gets the disk at once; SATA (Windows default) after a shut down and start.
- `PATCH /vms/{name}/disks/{index}` `{"size_gb":80}` → grow it; VM stopped (`409`), only grows (`400`).
- `DELETE /vms/{name}/disks/{index}` → remove it and its data; VM stopped.

Snapshots, rollbacks, backups and restores cover every disk. Moves and high availability
refuse VMs with extra disks for now (`409` / a reason in the HA list).

### Network storage (admins)
See [network-storage.md](network-storage.md). `GET /storages` → `{"storages":[{name,type,online,path,size_bytes,free_bytes,uses,vms,error,...}],"initiator":"iqn..."}`;
`POST /storages` `{"name":"nas","type":"nfs","server":"10.0.0.5","export":"/mnt/tank/p8s","backups":true}` or
`{"name":"san","type":"iscsi","portal":"10.0.0.5","target":"iqn...","lun":0,"format":true}` → `201 {"storage":…,"note":…}`
(`409 confirm` when the disk would be erased without `format`); `POST /storages/discover` `{"portal"}` → `{"targets":[…]}`;
`POST /storages/{name}/connect`; `DELETE /storages/{name}` (`409` while VMs are on it).

### Ceph (admins; on the master or a server alone)

See [ceph.md](ceph.md). `GET /ceph` → `{"ceph":{installed,configured,network,status:{health,problems,mons,quorum,mgr_up,osd_count,osd_up,used_bytes,size_bytes,osds,pools},pool_ready,nodes:[{id,name,self,online,info:{installed,configured,mon,mgr,osds,disks,addrs,task}}]},"advice":[…]}`.
Steps start in the background (`202`, follow `info.task`): `POST /ceph/install {"node"}`, `POST /ceph/init {"network":"192.168.1.0/24"}`,
`POST /ceph/mons {"node"}`, `POST /ceph/mgrs {"node"}`, `POST /ceph/osds {"node","disk":"/dev/sdb"}`, `DELETE /ceph/osds/{id}`.
`POST /ceph/pool {"size":0}` → `{"pool":"p8s-vms","size":3}`. Every server also answers `GET /ceph/node`,
`POST /ceph/node/config` and `POST /ceph/node/{action}` (called by the master). VMs: `"storage":"ceph"` on create.
Create a VM there with `"storage":"san"` in `POST /vms`; the VM object then has `"storage":"san"`.

### Snapshots
A snapshot is a ZFS snapshot of the VM's disks (instant, copy-on-write; all disks at the
same moment). Only the disks are saved — CPU/RAM settings are not. At most 30 per VM.

`GET /vms/{name}/snapshots` → `200 {"snapshots":[{"name":"before-upgrade","created_at":"2026-10-12T10:00:00Z","used_bytes":1048576}]}`
(oldest first; `used_bytes` = space only that snapshot holds; `"ram":true, "ram_bytes":N` when the
VM's memory was saved with it).

`POST /vms/{name}/snapshots` `{"name":"before-upgrade"}` → `201` snapshot. Name optional
(default `snap-YYYYMMDD-HHMMSS`), `^[a-z0-9][a-z0-9._-]{0,39}$`. Works on running VMs:
the guest's filesystems are frozen for the instant of the snapshot when the guest agent
answers (clean snapshot), otherwise it is crash-consistent. `{"ram":true}` also saves the running
VM's memory (`virsh save`, then the ZFS snapshot, then `virsh restore`): the VM pauses about 1 s per GB
of RAM; `409` if it isn't running or there isn't RAM + 512 MB free in `data_dir/vmstate`.

`POST /vms/{name}/snapshots/{snap}/rollback` `{"destroy_newer":true}` → `200` VM. The VM
must be stopped — except for a snapshot with RAM: then a running VM is powered off, the disks go
back and the VM resumes from the saved memory (state `running`). ZFS can only roll back to the newest snapshot, so rolling back further
deletes the newer ones — without `destroy_newer` that is a `409` listing them.

`DELETE /vms/{name}/snapshots/{snap}` → `204`.

### ISO library and installing from an ISO
| call | does |
|---|---|
| `GET /isos` | `{"isos":[{"name","bytes","added_at"}]}` |
| `PUT /isos/{name}` (raw body) | upload; name ends in `.iso`; must be an ISO 9660/UDF image; up to 16 GB → `201` |
| `POST /isos/download` `{"url","name"?,"sha256"?}` | fetch on the server → `202` job; checked against `sha256` if given |
| `DELETE /isos/{name}` | `409` while a VM still has it in its CD drive |
| `POST /vms` `{"name","iso":"x.iso","os_type":"linux"\|"windows","vcpus","memory_mib","disk_gb","network"}` | empty disk + CD; boots the CD until the disk has a system. `windows` = SATA disk, e1000e NIC, Hyper-V enlightenments, local-time clock |
| `POST /vms/{name}/eject` | remove the CD (VM stopped) |

ISO VMs have `template: "iso:<name>"`, `user: ""`, and `iso` while the CD is inserted. No cloud-init:
users and passwords are set in the installer. Windows 11 (needs UEFI + TPM) is not supported yet.

### Screen image
`GET /vms/{name}/screenshot` → `image/png` of the VM's screen taken directly from QEMU (not via VNC).
`409` if the VM isn't running.

### Usage metrics
`GET /vms/{name}/metrics` → `{"interval":10,"points":[{"t":…,"cpu":25.5,"mem_mib":736,"disk_read_bps":…,
"disk_write_bps":…,"net_rx_bps":…,"net_tx_bps":…}]}` — one point per 10 s for the last hour, oldest first.
`cpu` is % of the VM's own vCPUs; `mem_mib` is host RAM the VM uses. `GET /host/metrics` → `cpu` (% of all
host CPUs), `mem_used_mib`, `mem_total_mib`. Kept in memory only (empty again after a p8sd restart).

### Backups (full copies in files) and jobs
Backups are `zfs send` streams of the VM disk, compressed (zstd) into files under `backup_dir`
(default `/var/lib/p8s/backups/<vm>/`), with a `.json` describing the VM. They outlive the VM.
Backing up and restoring take minutes, so they run as **jobs**: the call returns `202` + a job;
follow it with `GET /jobs/{id}` (`state`: `running` → `done` | `failed`, `message`, `error`).
One job per VM at a time (`409` otherwise).

| call | does |
|---|---|
| `POST /vms/{name}/backups` | back up now (running VMs are frozen for an instant: `consistent:true`) → `202` job |
| `GET /vms/{name}/backups`, `GET /backups` | list (newest first; `/backups` includes deleted VMs) |
| `DELETE /backups/{vm}/{id}` | delete one → `204` |
| `POST /backups/{vm}/{id}/restore` `{}` | put the disk back; the VM must be stopped (`409`). If the VM was deleted, it is recreated |
| `POST /backups/{vm}/{id}/restore` `{"name":"web1-copy"}` | new VM from the backup (new MAC, hostname, SSH host keys) → `202` job |
| `GET/PUT /vms/{name}/backup-schedule` `{"daily":true,"keep":7}` | daily backup at `backup_time` (default 02:30), keeping the newest `keep` |
| `GET /jobs`, `GET /jobs/{id}` | recent jobs / one job |

Config: `backup_dir`, `backup_time` (`HH:MM`), `backup_keep` (default 7). Put `backup_dir` on a
different disk than the VM pool (under `/mnt`, `/srv` or `/var/lib/p8s`) so a disk failure can't take both.

### Console (VM screen in the browser)
`POST /vms/{name}/console` → `201 {"ticket":"…","path":"/api/v1/console?ticket=…","expires_in":30}`
(`409` if the VM isn't running). Then open a WebSocket (subprotocol `binary`) to `path`:
it carries the VM's VNC (RFB) stream, as used by noVNC. The ticket works **once**, for 30 s,
and only from a page served by this host (Origin check). No bearer token on the socket itself.

`POST /vms/{name}/password` `{"user":"debian","password":"…"}` → `204`. Sets a guest account's
password through the guest agent so you can log in on the console (cloud images ship with
passwords locked). 8–128 chars. `409` if the VM isn't running or the agent isn't up yet.
`{"user":"root","generate":true}` makes a random 16-character password instead and answers
`200 {"user","password"}`. Containers: same at `POST /containers/{name}/password`.

**Login details.** A template VM or container created without `root_password` gets a random one
(16 characters, no look-alike letters; config `"auto_root_passwords": false` turns this off). The
create answer has `"login": {"known":true,"user":"root","password":"…","ssh_password":true,"set_at":"…"}`.
Read it again later (owner or admin): `GET /vms/{name}/login`, `GET /containers/{name}/login`
(`known:false` when P8S didn't make or set one, e.g. ISO installs). Setting root's password through
P8S updates it; deleting the guest forgets it. Stored in `<data_dir>/credentials.json` (0600).

### Health & alerts (admins)

`GET /alerts` (active alerts, checks, history, settings), `PUT /alerts/settings`, `POST /alerts/test`.
Details: [alerts.md](alerts.md).

### Importing VMs (admins)

`GET /imports`, `PUT /imports/{file}` (upload), `GET /imports/{file}/plan`, `POST /imports/{file}/start`
(→ job), `DELETE /imports/{file}`. Proxmox vzdump backups (`.vma[.zst|.lzo|.gz]`), OVA appliances (`.ova`: settings from the OVF, every disk; the first is the system disk) and disk images
(qcow2, vmdk, vhd/vhdx, vdi, raw). Details: [import.md](import.md).

### Subscription (admins)

| Method | Path | |
|---|---|---|
| GET | `/api/v1/license` | server ID, tier, state (`none`, `active`, `expiring`, `grace`, `expired`, `revoked`), features, notice, user limit, tiers |
| PUT | `/api/v1/license` | `{"key":"P8S1-…"}` install a key (400 if forged, expired or for another server) |
| DELETE | `/api/v1/license` | remove the key (back to Home) |
| POST | `/api/v1/license/check` | ask the licence server now |

| GET | `/api/v1/updates` | running version, channel, available release, last install (state + log) |
| POST | `/api/v1/updates/check` | ask the update server now |
| POST | `/api/v1/updates/install` | download, verify and install the available release (202) |
| PUT | `/api/v1/updates/channel` | `{"channel":"stable"|"testing"}` (stable needs a subscription) |

### SSH ports

`GET /api/v1/ssh-ports` → `{"enabled": true, "ports": {"web1": 2201, …}}`: the port on this server that reaches each
private-network VM/container's SSH (customers see only theirs). See [networking.md](networking.md).

### Windows VMs

`POST /api/v1/vms` with `"iso"` takes `"os_type": "linux" | "windows" (BIOS) | "windows11"` (UEFI, Secure Boot,
TPM 2.0; needs ≥ 2 CPUs, 4096 MiB, 64 GB) and `"drivers_iso"` (a second CD from the library, e.g. `virtio-win.iso`).
VMs show `"os_type"` and `"drivers_iso"`. See [windows.md](windows.md).

### Containers (LXC)

| Method | Path | |
|---|---|---|
| GET | `/api/v1/containers` | containers the caller may see, `enabled` false when LXC isn't installed |
| POST | `/api/v1/containers` | `{"name","template","vcpus","memory_mib","disk_gb","network","ip_pool","plan","root_password","ssh_keys","owner","start","autostart","node"}` — `node` picks the server in a cluster (master only); `network` may be `vnet:NAME`; items in the list carry `node` |
| GET / DELETE / PATCH | `/api/v1/containers/{name}` | details / delete with its disk / resize `{"vcpus","memory_mib","disk_gb","autostart"}` (live) |
| POST | `/api/v1/containers/{name}/start` · `/stop` (`{"force":true}`) · `/reboot` | power |
| GET / POST | `/api/v1/containers/{name}/snapshots` | list / take `{"name"}` |
| POST / DELETE | `/api/v1/containers/{name}/snapshots/{snap}/rollback` · `/snapshots/{snap}` | roll back (stopped; `{"destroy_newer":true}`) / delete |
| POST | `/api/v1/containers/{name}/password` | `{"user","password"}` (running) |
| PUT | `/api/v1/containers/{name}/owner` | `{"owner"}` (admins) |
| POST | `/api/v1/containers/{name}/move` | `{"to","start"}` stopped container to another server (staff; 202 + job) |
| POST | `/api/v1/containers/{name}/console` | one-time ticket for `GET /api/v1/terminal?ticket=` (WebSocket: binary = terminal bytes, text = `{"cols","rows"}`) |
| GET / PUT | `/api/v1/containers/{name}/firewall` | same body as the VM firewall `{"enabled","rules":[{"proto","ports","source","comment"}]}` |
| GET / POST | `/api/v1/containers/{name}/backups` | list / start a backup (202 + job, follow `/api/v1/jobs/{id}`) |
| GET / PUT | `/api/v1/containers/{name}/backup-schedule` | `{"daily","keep"}` like VMs |
| GET | `/api/v1/ct-backups` | every container backup the caller may see (also of deleted containers) |
| DELETE | `/api/v1/ct-backups/{name}/{id}` | delete a backup |
| POST | `/api/v1/ct-backups/{name}/{id}/restore` | `{"name"}` restore as a new container (202 + job); counts against the quota |
| GET | `/api/v1/ct-templates` | installed container templates |

### Server shell (admins)

| Method | Path | |
|---|---|---|
| POST | `/api/v1/node/shell` | one-time ticket for `GET /api/v1/terminal?ticket=` — a root shell on this server (like Proxmox's node Shell). `?node=n2`: a cluster worker's shell, through the tunnel. Opening and closing are written to the activity log. Off with `"host_shell": false` in `/etc/p8s/p8sd.json` |

### Cluster (admins)

| Method | Path | |
|---|---|---|
| GET | `/api/v1/cluster` | role (`""`, `master`, `worker`), tunnel endpoint, servers with status and capacity |
| POST | `/api/v1/cluster/join-code` | `{"master_url","endpoint"}` → `{"code","command"}` (makes this server the master) |
| POST | `/api/v1/cluster/register` | (no login; the join code is the pass) a joining server registers |
| POST | `/api/v1/cluster/join` | `{"url","code"}` this server joins a master |
| POST | `/api/v1/cluster/leave` | worker: stand alone again |
| DELETE | `/api/v1/cluster/nodes/{id}` | master: drop a server (`?force=1` while it has VMs) |
| GET | `/api/v1/cluster/discover` | P8S OS servers on this network (`available`, or `why` not) |
| POST | `/api/v1/cluster/invite-send` | (master) a found server → `{"confirm"}` the 6-digit number |
| POST | `/api/v1/cluster/invite` | (no login) an invitation arrives; waits for this server's admin |
| GET | `/api/v1/cluster/invites` | open invitations |
| POST | `/api/v1/cluster/invites/{id}/accept` · `/decline` | answer one (accept = join) |
| GET | `/api/v1/cluster/public-ip` · POST `/api/v1/cluster/check` `{"host","port"}` | help for joining over the internet |
| GET | `/api/v1/cluster/candidates` | `?memory_mib&disk_gb&network&iso&ip_pool` where a VM could go, best first |

| GET | `/api/v1/vms/{name}/move` | `{"from","why","targets":[{"id","name","online"}]}` — `why` is set when it can't move now |
| POST | `/api/v1/vms/{name}/move` | `{"to":"n2","start":true}` → 202 job (`kind` "move"); the VM must be shut down |
| GET | `/api/v1/vms/{name}/move-check` | (server to server) `{"why"}` for a VM on this server |
| GET | `/api/v1/cluster/vm-export/{name}` · POST `/api/v1/cluster/vm-import` | (server to server) the VM as a stream: a JSON line with its settings, then `zfs send` of the disk |

High availability: see [ha.md](ha.md#api). Roles, company login (SSO) and the activity log export: see [security.md](security.md).

`POST /api/v1/vms` takes `"node"` (server id or name; admins) and VMs carry `node` / `node_id`.

### Off-site backups (admins)

| Method | Path | |
|---|---|---|
| GET | `/api/v1/offsite` | places with status (secrets hidden: `has_secret`, `has_passphrase`), this server's `ssh_public_key` |
| PUT | `/api/v1/offsite/targets/{name}` | `{"type":"dir|s3|ssh","enabled","keep","path","endpoint","region","bucket","prefix","access_key","secret_key","virtual_host","host","user","port","remote_dir","passphrase"}`; empty secret/passphrase keep the saved ones |
| DELETE | `/api/v1/offsite/targets/{name}` | forget the place (what is stored there stays) |
| POST | `/api/v1/offsite/targets/{name}/test` | write, read back and delete a small file |
| POST | `/api/v1/offsite/targets/{name}/sync` | copy every missing backup now (202 + job) |
| GET | `/api/v1/offsite/targets/{name}/backups` | what is stored there (`local`: also on this server) |
| POST | `/api/v1/offsite/targets/{name}/fetch` | `{"kind":"vm|ct","guest","id"}` download it back (202 + job), then restore as usual |

Names are shared with VMs (a container and a VM can't have the same name). See [containers.md](containers.md).

### Server page (admins)

| Method | Path | |
|---|---|---|
| GET | `/api/v1/node` | storage pool (size, used, free, health, how much it can grow), OS template catalog, home network, Debian updates, running task, recent tasks |
| POST | `/api/v1/node/tasks` | `{"kind":"template","arg":"ubuntu-24.04"}` · `ct-template` (arg e.g. debian-13) · `storage-grow` · `lan-setup` · `os-check` · `os-upgrade` · `reboot` · `zfs-cache` (arg MiB or `auto`) · `net-check` · `net-repair` (202; 409 while another runs) |
| GET | `/api/v1/node/tasks` / `/api/v1/node/tasks/{id}` | task list / one task with its log |
| GET | `/api/v1/node/ct-images` | every container image on the image server: `{"images":[{"id":"kali/current","label":"Kali current","installed":false}]}` (cached 1 h); add one with task `ct-image` (arg `DIST/RELEASE`) |
| POST | `/api/v1/vms/{name}/nics` | staff: another network card `{"network":"nat"\|"lan","vlan":N}` → VM + `note` (201; plugged in live) |
| DELETE | `/api/v1/vms/{name}/nics/{mac}` | staff: remove a card (not the first; unplugged live) |
| POST | `/api/v1/vms/{name}/storage` | staff: move the (shut down) VM's disks `{"storage":"ceph"\|"local"\|NAS,"drop_snapshots":true}` → job (202). `409` running / has snapshots without `drop_snapshots` |
| POST | `/api/v1/backups/{name}/{id}/browse` | backup owner or staff: open it read-only for single files → session `{"id","state":"preparing"\|"ready"\|"failed","parts":[{"id":"d0p1","fs","label","size","error"}],"expires_at"}` (202; max 4 open, 1 hour) |
| GET | `/api/v1/browse/{sid}`, `/browse/{sid}/ls?part=&path=`, `/browse/{sid}/file?part=&path=` | the session; a folder `{"entries":[{name,dir,link,size,mod_time}]}`; a file (download). Symbolic links never lead out of the backup's filesystem |
| DELETE | `/api/v1/browse/{sid}` | close it (unmount, delete the scratch copy) |
| PUT | `/api/v1/vms/{name}/limits` | staff: speed limits `{"read_mbs","write_mbs","iops","net_down_mbit","net_up_mbit"}` (0 = none; each disk / each card) → VM with `io_limits`; a running VM gets them at once |
| POST | `/api/v1/vms/{name}/hotplug` | staff: `{"on":true,"max_vcpus":8,"max_mib":16384}` (0 = twice now; VM shut down) → VM with `hot_cpus`, `hot_mem_mib`. Then `PATCH /vms/{name}` raises vCPUs / RAM of the running VM up to them (RAM in 128 MiB steps, not lower while it runs) |
| GET/POST/DELETE | `/api/v1/node/acme` | admin: Let's Encrypt for the dashboard. GET → `{"cert":{names,not_after,issuer}\|null,"request":…}`; POST `{"domain","email","method":"http"\|"cloudflare","cf_token"}` → task (202); DELETE → task (back to the own certificate). Served by SNI: the domain gets it, connections by IP keep the pinned own certificate |
| GET/PUT | `/api/v1/firewall` | datacenter firewall (GET staff/auditors, PUT admins): `{"sets":{"office":["203.0.113.0/24"]},"block":["192.0.2.66","+bad"],"allow":[{proto,ports,source,comment}]}`; a VM rule's `source` may be `+setname`. The master sends it to every worker → `{"firewall":…,"note"}` |
| GET | `/api/v1/node/disks[?node=ID][&smart=0]` | staff: physical disks `{"disks":[{name,path,by_id,size,model,serial,kind,parts,contents,mounted,usage,pool,in_use,protected,empty,smart:{available,passed,temp_c,power_on_hours,wear_pct,reallocated,pending,media_errors,note}}],"pools":[{name,pool,layout,disks,health,size,free,vm_disks}]}`; `node` (on the master) asks that cluster server |
| POST | `/api/v1/node/disks/{disk}/wipe`, `/node/disks/{disk}/protect` | admin: `{"confirm":"sdb"}` (needed when the disk has data; `409 confirm` otherwise) / `{"on":true}`. `409` for the system disk, pool / LVM / RAID members, mounted or protected disks |
| POST/DELETE | `/api/v1/node/pools`, `/node/pools/{name}?confirm=NAME` | admin: `{"name":"fast","layout":"single\|mirror\|raidz1\|raidz2","disks":["sdb","sdc"]}` (empty disks only) → ZFS pool `p8s-fast`, a VM storage `fast`; DELETE destroys it (no VM disks on it) |
| GET/PUT | `/api/v1/sdn` | virtual networks (GET any signed-in user, PUT admins on the master): `{"vnets":[{"name":"office","vni":10000,"subnet":"10.60.0.0/24","nat":true,"comment":""}]}` (vni 0 = picked; subnet optional) → `{"vnets":…,"note"}`; GET adds `gateway_here`, `mtu`. `409` when VMs use a network being removed. A VM's `network` (or a card's) is then `"vnet:office"` |
| GET/POST/DELETE | `/api/v1/cluster/witness` | admin, master of 2 servers: the tie-breaker. POST `{"url":"https://192.168.0.50:8007","token","pin"}` (it must answer) → `{"witness":{url,online,error,term,master_seen_s},"voters":3,"note"}`; GET the same; DELETE removes it. `GET /cluster` also shows `witness` and `voters` |
| GET | `/api/v1/cluster/witness/download/{amd64\|arm64}` | admin: the `p8s-witness` program |
| GET | `/api/v1/prometheus` | staff/auditors: Prometheus text format (`p8s_up`, `p8s_host_*`, `p8s_vm_*{vm,server,owner}`) — scrape with an API token as bearer |
| GET | `/api/v1/vms/{name}/devices` | staff: passthrough — `{"pci":[…],"usb":[…],"available":{"iommu":bool,"cpu":"intel","pci":[{"addr","class","name","kind","driver","group","others","warn","bridge"}],"usb":[{"id","name","port"}]},"used_by":{dev: vm}}` (devices of the VM's server) |
| POST | `/api/v1/vms/{name}/devices` | admin: `{"kind":"pci","id":"0000:01:00.0"}` (VM shut down; its IOMMU group comes along) or `{"kind":"usb","id":"046d:c52b"}` (live) → VM + `note` (201). `409` IOMMU off / VM running / device in another VM |
| DELETE | `/api/v1/vms/{name}/devices/{kind}/{id}` | admin: take it away (PCI: VM shut down; USB: unplugged live) |
| POST | `/api/v1/node/rename` | `{"name":"p8s-vm1"}` → task `rename` (202): /etc/hosts, hostname, p8sd restart; VMs keep running |
| POST | `/api/v1/cluster/nodes/{id}/rename` | master: `{"name"}` → asks that worker to rename itself (202) |
| POST | `/api/v1/node/templates/custom` | VM template from any cloud image: `{"name","label","url","login","variant","checksum"}` (checksum = SUMS file URL, `sha256:HEX`, `sha512:HEX` or empty) → task `template-url` (202) |
| GET | `/api/v1/node/memory` | where the RAM goes: `mem` (used/guests/system/zfs_cache/cache/free/ksm_saved MiB), `top` programs, `zfs_cache_max_mib` |

Tasks run as their own systemd units (`p8s-task-*`), so they finish even when p8sd restarts.

Features a tier doesn't unlock answer **402** `subscription` (e.g. a 4th account on Home, priced plans without Hosting).
See [subscriptions.md](subscriptions.md).

## Example session

```bash
export P8S=http://127.0.0.1:8006/api/v1 TOK=p8s_...
curl -s $P8S/health
curl -s -H "Authorization: Bearer $TOK" $P8S/templates
curl -s -H "Authorization: Bearer $TOK" -X POST $P8S/vms -d '{
  "name":"web1","template":"debian-13","vcpus":2,"memory_mib":2048,"disk_gb":20,
  "ssh_keys":["'"$(cat ~/.ssh/id_ed25519.pub)"'"]}'
curl -s -H "Authorization: Bearer $TOK" $P8S/vms/web1
curl -s -H "Authorization: Bearer $TOK" -X POST $P8S/vms/web1/stop
curl -s -H "Authorization: Bearer $TOK" -X DELETE $P8S/vms/web1 -w '%{http_code}\n'
```

## Planned (not in Phase 1)

- Change autostart on an existing VM; CPU/RAM changes on a running VM.
- Async jobs: `POST` returns `202` + `/jobs/{id}` for long operations.
- Scoped API tokens per user; billing (v0.5).
