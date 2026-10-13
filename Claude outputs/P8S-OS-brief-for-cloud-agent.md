# P8S OS: brief for the P8S Cloud (web) agent

From: the P8S OS build sessions (Oct 10–13, 2026). Please add this to your notes / TODO.

## 1. What it is built on

- **Debian 13 (trixie)**, standard Debian kernel and packages from the Debian repos. No own kernel, no fork. `/etc/os-release` still says Debian.
- On top: **KVM/QEMU + libvirt** (VMs), **LXC** (containers, unprivileged), **ZFS** (VM/CT disks, snapshots, backups), WireGuard (clusters).
- P8S adds its own layer: **`p8sd`** (management daemon, Go, static binary, REST API on port 8006), the **`p8` CLI**, a **web dashboard** (Proxmox-style), branding (GRUB, login screen), and an install ISO (`p8sos-alpha-…-amd64.iso`).

## 2. What it is for

It is a **host / hypervisor OS** (like Proxmox VE), **not** a guest image customers pick when creating a server. Three uses were agreed:

1. Our own servers / datacenter (replacing Proxmox over time).
2. **P8S Cloud**: selling VPSes and containers. P8S OS has its own built-in cloud module: customer accounts, quotas, plans, hourly billing, Razorpay top-ups, tickets, public IP pools.
3. Selling the OS to others, cheaper than Proxmox. The planned editions are Home (free), Standard, Enterprise, Datacenter and Internal. Licence keys are not active yet (development mode).

So the guest images for customers stay Ubuntu / Debian / Rocky etc. Nothing changes in your image list because of P8S OS.

## 3. Where it lives

- Code: **https://github.com/p8s-in/P8SOS** (branch `main`), local copy `L:\P8SOS` on the Windows PC.
- Test machine: ASUS FX505GT laptop, host `p8s-node`, `192.168.0.106` (home network), dashboard `https://192.168.0.106:8006`.
- Docs in the repo: `docs/api.md` (full REST API), `docs/p8s-cloud.md`, `docs/cluster.md`, `docs/containers.md`, `docs/windows.md`, `docs/host-setup.md`.

## 4. Who maintains it / updates

- Maintained by Jagadeeswar (built together with Claude).
- Debian security updates: normal `apt`, from the dashboard (Server → Debian updates → Check / Install).
- P8S updates: `cd ~/P8SOS && git pull && sudo bash update.sh` (an update-server path also exists).
- **Status: alpha.** It runs on one test laptop and isn't in production yet. Several features are built but not yet tried on real hardware: clusters, off-site backups to S3, Windows 11 VMs.

## What it can do today (summary)

- VMs from cloud templates (cloud-init: SSH keys, root password) or from ISO (any OS, incl. Windows 10/11 with UEFI + TPM).
- Containers (LXC) with the same features: plans, billing, public IPs, firewall, backups.
- Snapshots, backups (daily schedule, restore as new), off-site backups (USB/NAS, S3-compatible, SSH; optional encryption).
- Per-guest firewall, public IP pools, SSH and RDP port relay for NAT guests, publishing websites via Cloudflare Tunnel.
- **Root passwords:** every new VM or container gets a random root password if none is given. It is shown to the owner in the dashboard and via the API (details below).
- Browser console (VNC) and browser terminal, root shell for admins (like Proxmox).
- Clusters: master + workers over WireGuard (LAN or internet), automatic VM placement.
- Dashboard: Proxmox-style pages with tabs and a task bar; memory breakdown; network check and repair.

## Your concerns

- **Ceph / Proxmox support:** P8S OS doesn't use Ceph. Storage is local ZFS per server, and clusters move work between servers (moving VMs between servers and HA are not built yet). The point stands for paying customers: until P8S OS is proven, **production VPS hosts stay on Proxmox**, and P8S OS runs on test hardware next to it.
- **"Extra OS choice when creating a server":** not what P8S OS is (see section 2). No image-list change is needed.

## If the web panel talks to a P8S OS server (later)

The web can use P8S OS as a backend the same way it uses the Proxmox API today.

- Base URL `https://HOST:8006/api/v1`. Self-signed TLS, so pin its certificate.
- Auth: header `Authorization: Bearer <API token>`. Create the token in the dashboard (your account → API tokens) or with `POST /me/tokens`. The host's own config token is an admin token.
- Creating with an admin token: `"owner": "<customer>"` assigns the guest to a P8S OS customer account. With that customer's own token, quotas and billing apply.

| Need | Call |
|---|---|
| List images | `GET /templates` (VMs), `GET /ct-templates` (containers) |
| Plans | `GET /plans` |
| Create VPS | `POST /vms` `{"name":"web1","template":"ubuntu-24.04","plan":"<id>"}`, or `vcpus`/`memory_mib`/`disk_gb`, optional `root_password`, `ssh_keys`, `network` (`nat`/`lan`), `ip_pool` (public IP) |
| Create container | `POST /containers` (same fields) |
| Login details | Create answer includes `"login":{"user":"root","password":"…","ssh_password":true}`; later `GET /vms/{name}/login`, `GET /containers/{name}/login` |
| New root password | `POST /vms/{name}/password` `{"user":"root","generate":true}` → `{"user","password"}` (same under `/containers/`) |
| Status / list | `GET /vms`, `GET /vms/{name}`, `GET /containers` |
| Power | `POST /vms/{name}/start` \| `stop` (`{"force":true}`) \| `reboot`; containers: `POST /containers/{name}/start` \| `stop` \| `reboot` |
| Resize | `PATCH /vms/{name}`, `PATCH /containers/{name}` |
| Delete | `DELETE /vms/{name}`, `DELETE /containers/{name}` |
| Console | `POST /vms/{name}/console` → one-time ticket for the noVNC WebSocket; containers: `POST /containers/{name}/console` (terminal) |
| SSH / RDP ports (NAT guests) | `GET /ssh-ports` → `{"ports":{name:port},"rdp":{name:port}}` |
| Usage charts | `GET /vms/{name}/metrics`, `GET /host/metrics` |
| Snapshots / backups | `…/snapshots`, `…/backups`, `…/backup-schedule`, `POST /backups/{name}/{id}/restore` |
| Firewall | `GET`/`PUT /vms/{name}/firewall`, `/containers/{name}/firewall` |
| Public IPs | `GET /ippools` |
| Capacity (clusters) | `GET /cluster/capacity` |

Full details, error codes and examples: `docs/api.md` in the repo.

## Open decisions (need Jagadeeswar)

1. **Who owns customers, wallet and billing:** the web panel (your wallet, deployed 5 Oct) or P8S OS's built-in billing? Using both would double-charge. Suggestion: the web keeps customers and the wallet, and calls P8S OS with an admin token.
2. When to move a first real VPS host from Proxmox to P8S OS: after P8S OS has run stable on test hardware.
3. A public page for P8S OS on p8s.in, and "build in public" videos (your suggestion; it fits the plan to sell the OS).
