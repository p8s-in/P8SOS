# p8sd REST API — v1

Base URL: `https://<host>:8006/api/v1` (self-signed certificate unless `tls_cert`/`tls_key` are set).
All bodies are JSON. Unknown fields in request bodies are rejected.

## Authentication

Every endpoint except `/health` requires a bearer token:

```
Authorization: Bearer p8s_<64 hex chars>
```

Generate one with `p8sd token -name <label>`. The daemon stores only the
SHA-256 of the token in `/etc/p8s/p8sd.json`. Phase 1 tokens are all-powerful;
scoped tokens arrive with tenants in Phase 2.

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
- Tenants, projects, quotas, scoped tokens (Phase 2).
- Console websocket for noVNC (Phase 3).
