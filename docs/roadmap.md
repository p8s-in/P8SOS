# Roadmap

Each phase ships something usable on its own.

## Phase 1 — Core daemon (months 1–2) ← **we are here**

- [x] Repo, Go module, stdlib-only build
- [x] Config with hashed API tokens, templates, limits
- [x] ZFS clone/resize/destroy for VM disks
- [x] cloud-init NoCloud seed (hostname, SSH keys, growpart, guest agent)
- [x] Domain XML (q35, virtio, host-passthrough, localhost VNC) with P8S metadata
- [x] VM create / get / list / start / stop / delete with rollback
- [x] REST API v1 + auth + error model ([api.md](api.md))
- [x] Unit tests with fakes for every layer
- [x] systemd unit, bootstrap + template scripts
- [x] **First real run** — Debian 13 in VirtualBox (nested): install + 39/39 smoke test + `p8 ssh web1` (2026-10-11)
- [x] VM IP in API and `p8 list` (DHCP lease → guest agent → ARP)
- [x] Autostart with the host (default on), reboot
- [x] Serial console log per VM (`p8 bootlog`), recovery from interrupted creates
- [x] Resize CPU/RAM/disk, ZFS snapshots + rollback (API, `p8 snap/rollback/resize`, dashboard Manage dialog)
- [x] End-to-end smoke test script (`scripts/smoke-test.sh`) + [USER.md](../USER.md) test guide
- [x] One-command `install.sh` and `p8` CLI helper
- [x] **Bare metal** — alpha ISO installed on the ASUS FX505GT; `p8 create web1 2 2048 20` + `p8 ssh web1` working (2026-10-12)
- [ ] Full `p8 test` run on bare metal
- [x] VMs on the home network: `sudo p8 lan-setup` (br0 with auto-rollback), `--lan` / dashboard choice ([networking.md](networking.md))
- [ ] `p8sctl` CLI client in Go (replaces the `p8` shell helper)

## Phase 2 — Tenants and networking (months 2–4)

Users, projects, quotas (SQLite); scoped tokens; per-tenant VLAN/bridge;
nftables firewall per VM; IP pools (static assignment via cloud-init network
config); containers via Incus. Replaces Proxmox on the home lab.

## Phase 3 — Web UI and console (months 4–5)

- [x] Web dashboard v1 (pulled forward): HTTPS with self-signed cert, token sign-in, host cards,
      VM list with live refresh, create / start / stop / reboot / force-off / delete, phone layout
- [x] Browser console (noVNC over a ticketed WebSocket proxy in p8sd), set guest password, type text
- [x] More OS templates: Debian 12/13, Ubuntu 22.04/24.04, Rocky 9, AlmaLinux 9 (`p8 templates add`, checksum-verified)
- [x] Backups + restore: compressed zfs-send files, restore in place / as new VM / recreate deleted VM, daily schedule with retention, background jobs
- [x] Install from any ISO: ISO library (upload / download with checksum), empty-disk VMs booting the CD, Windows device profile, eject
- [x] Usage graphs: CPU, memory, disk and network per VM (last hour, 10 s), host CPU/memory sparklines

Admin + customer panels, noVNC console proxied through p8sd auth, metrics,
scheduled backups (ZFS send to a second pool or remote).

## Phase 4 — Installer ISO (months 5–6)

Debian live-build: branded installer → partitions, ZFS root, installs p8sd,
boots to the web UI. This is the moment it becomes **P8S OS**.

## Phase 5 — Clustering (month 7+)

Multi-node, shared state (etcd), live migration, Ceph RBD storage backend, HA.
