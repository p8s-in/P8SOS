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

### Snapshots
A snapshot is a ZFS snapshot of the VM's disk (instant, copy-on-write). Only the disk
is saved — CPU/RAM settings are not. At most 30 per VM.

`GET /vms/{name}/snapshots` → `200 {"snapshots":[{"name":"before-upgrade","created_at":"2026-10-12T10:00:00Z","used_bytes":1048576}]}`
(oldest first; `used_bytes` = space only that snapshot holds).

`POST /vms/{name}/snapshots` `{"name":"before-upgrade"}` → `201` snapshot. Name optional
(default `snap-YYYYMMDD-HHMMSS`), `^[a-z0-9][a-z0-9._-]{0,39}$`. Works on running VMs:
the guest's filesystems are frozen for the instant of the snapshot when the guest agent
answers (clean snapshot), otherwise it is crash-consistent.

`POST /vms/{name}/snapshots/{snap}/rollback` `{"destroy_newer":true}` → `200` VM. The VM
must be stopped. ZFS can only roll back to the newest snapshot, so rolling back further
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
