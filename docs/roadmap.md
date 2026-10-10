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
- [ ] **First run on real hardware** (FX505GT, plain Debian 13) — see [host-setup.md](host-setup.md)
- [ ] VM IP lookup via guest agent
- [ ] Reboot, resize CPU/RAM, ZFS snapshots + rollback
- [x] End-to-end smoke test script (`scripts/smoke-test.sh`) + [USER.md](../USER.md) test guide
- [ ] Smoke test passing on real hardware
- [ ] `p8sctl` CLI client

## Phase 2 — Tenants and networking (months 2–4)

Users, projects, quotas (SQLite); scoped tokens; per-tenant VLAN/bridge;
nftables firewall per VM; IP pools (static assignment via cloud-init network
config); containers via Incus. Replaces Proxmox on the home lab.

## Phase 3 — Web UI and console (months 4–5)

Admin + customer panels, noVNC console proxied through p8sd auth, metrics,
scheduled backups (ZFS send to a second pool or remote).

## Phase 4 — Installer ISO (months 5–6)

Debian live-build: branded installer → partitions, ZFS root, installs p8sd,
boots to the web UI. This is the moment it becomes **P8S OS**.

## Phase 5 — Clustering (month 7+)

Multi-node, shared state (etcd), live migration, Ceph RBD storage backend, HA.
