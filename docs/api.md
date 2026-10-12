# p8sd REST API — v1

Base URL: `http://<host>:8006/api/v1` (HTTPS when `tls_cert`/`tls_key` are set).
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
| 500  | `internal`    | host-side failure; details only in logs    |

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

## Endpoints

### `GET /health` (no auth)
`200 {"status":"ok","version":"0.1.0-dev"}`

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
`204`. **Irreversible.**

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

- Resize CPU/RAM; change autostart on an existing VM.
- Snapshots: `POST /vms/{name}/snapshots`, rollback.
- Async jobs: `POST` returns `202` + `/jobs/{id}` for long operations.
- Tenants, projects, quotas, scoped tokens (Phase 2).
- Console websocket for noVNC (Phase 3).
