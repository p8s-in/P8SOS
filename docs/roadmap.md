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

- [x] User accounts (v0.3): admin / customer roles, sign-in with password, TOTP 2FA, lockout after failed sign-ins,
      per-customer plans (VMs, CPUs, RAM, disk, LAN), customers see only their VMs/backups/jobs, VM owner hand-over,
      activity (audit) log; dashboard Users / Activity / account dialogs; `p8 users|user|give|audit`
- [x] v0.3 hardening: customers see error references not host internals, no host SSH key in customer VMs,
      per-user SSH keys, sign-ins survive restarts
- [x] Per-VM firewall (v0.4 part 1): allow-list of incoming tcp/udp/ping with optional source, nftables bridge table by MAC,
      dashboard Manage → Firewall, `p8 fw`
- [x] Public access (v0.4 part 2): Cloudflare Tunnel set up by `p8 tunnel setup` (API: tunnel, routes, DNS), dashboard on
      panel.<domain>, VM websites `p8 publish` / Manage → Websites, `sites` in customer plans, real visitor IP in the audit log
- [x] Public IP pools (v0.4): address blocks per bridge (bridged or routed), static config via cloud-init network-config,
      anti-spoofing (MAC + IPv4 pinned to the VM's port), `public_ips` in plans, address kept for recreated VMs;
      Settings → Public IP pools, New VM → Public IP, `p8 ippool` / `p8 create --ip`
- [x] P8S Cloud (v0.5): self sign-up with email confirmation, password reset, profile; plans with monthly prices billed
      hourly from a prepaid wallet (monthly cap); Razorpay top-ups (Checkout + signature + capture + webhook), GST tax
      invoices, manual payments; low/negative balance emails and automatic stop after a grace period; SMTP emails;
      support tickets; personal API tokens; Settings → Selling / Plans / Payments / Email, Billing and Support pages
- [x] Thin VM disks, `p8 storage grow`, file pool sized from free space
- [x] P8S OS subscriptions (v0.6): Home (free) / Business / Hosting / Partner / Internal, Ed25519-signed keys tied to
      server IDs, offline check + optional daily online check, 14-day grace, dashboard Subscription page and notice,
      `p8 license`; `p8s-licensed` licence server with a Razorpay shop, GST invoices, renew/rebind/revoke ([subscriptions.md](subscriptions.md))
- [x] Windows 11 / Server 2022+ VMs: UEFI + Secure Boot + TPM 2.0, full Hyper-V enlightenments, virtio-win drivers CD ([windows.md](windows.md))
- [x] P8S OS branding on the server (boot menu, login screen, SSH welcome)
- [x] Containers (LXC, unprivileged, ZFS clones, cgroup limits, cloud-init, browser terminal with xterm.js), Create CT, Manage, container templates on the Server page, `p8 ct` ([containers.md](containers.md))
- [x] Container backups (freeze + ZFS snapshot + zstd, restore as new, daily schedule), per-container firewall,
  public IPs from pools, P8S Cloud plans and billing for containers
- [x] Server shell in the browser (root, like Proxmox's node Shell): Server → >_ Shell, and per cluster server; admins only,
  logged in Activity, `host_shell: false` turns it off
- [x] Clusters v1: master + workers over WireGuard (works across NAT/CGNAT), one-time join codes with certificate pinning,
  one dashboard for all servers' VMs, automatic placement by free RAM (or pick a server), console/backups/firewall
  through the tunnel, `p8 cluster` / `p8 join` ([cluster.md](cluster.md)). Next: move VMs between servers, HA
- [x] VM and container pages like Proxmox: click one in the sidebar → toolbar (Start, Shutdown, Reboot, Force off,
  Delete) and tabs Summary, Console (inside the page), Hardware, Network & firewall, Snapshots, Backups, Options;
  task bar at the bottom (backups, restores, downloads, off-site copies, server tasks)
- [x] Off-site backups: folder / S3-compatible / SSH targets, optional encryption, retention per place, bring back ([offsite-backups.md](offsite-backups.md))
- [x] Several network cards per VM (nat / home network / VLAN, hot-plug, firewall on every card, kept on moves and failover, new MACs on clones)
- [x] Disk and network speed limits per VM (MB/s, IOPS, Mbit/s; live), CPU and RAM hot-plug (ceilings, live resize)
- [x] Graphical installer (mouse, full screen on the ISO; or from another computer's browser with a PIN) and p8sd pre-built on the ISO (first start without a Go build)
- [x] Disks view: every physical disk with use and SMART health, protect, wipe (typed confirmation for data), ZFS pools from empty disks as extra VM storages (single / mirror / RAID-Z1 / RAID-Z2)
- [x] Virtual networks across the servers (SDN, VXLAN in the cluster tunnel): one switch for VMs wherever they run; optional subnet with gateway, DHCP, DNS and NAT on the master ([sdn.md](sdn.md))
- [x] Witness for two-server clusters (like Proxmox's QDevice): `p8s-witness` on a Pi / NAS / VPS is the third vote, so the surviving server takes over by itself (`p8 cluster witness`)
- [x] Datacenter firewall: IP sets, blocked addresses and rules for every VM on every server
- [x] OVA import (VMware / VirtualBox appliances: settings from the OVF, all disks)
- [x] File-level restore: browse a backup (ext4/XFS/Btrfs/FAT/NTFS) and download single files (`p8 files`)
- [x] Move a VM's disks between storages (local ZFS ↔ NAS/SAN pool ↔ Ceph) and backups of Ceph VMs (disk images; restore onto Ceph or local)
- [x] Let's Encrypt certificate for the dashboard (port 80 or Cloudflare DNS; auto-renew; the cluster keeps its pinned certificate)
- [x] Scoped API tokens (read-only, only some VMs, expiry) and per-VM permissions (share a VM as user or viewer)
- [x] Prometheus export (`/api/v1/prometheus`) for Grafana
- [x] PCI / GPU / USB passthrough (VFIO, IOMMU groups, warnings for the server's own screen/network/disks, `p8 iommu on`, `p8 dev-add`)
- [x] Memory snapshots ("include RAM"): the running VM's memory saved with the disk snapshot; rollback resumes it where it was (`p8 snap NAME --ram`)
- [x] Speed: VM disks with an I/O thread, native AIO and multi-queue; vhost-net multi-queue NICs; host tuning
  (ZFS ARC capped at 10% of RAM, KSM, vhost_net, swappiness 10); faster, flicker-free dashboard polling
- [x] Editions: Home / Standard / Enterprise / Datacenter (one ISO, the licence key decides)
- [x] Proxmox-style dashboard: top bar, sidebar tree (server → VMs and containers), grouped menus, ⋯ action menus
- [x] Server page in the dashboard: storage + grow, add OS templates, home-network setup, Debian updates, reboot, task log with live output
- [x] Clone VMs (full copy of all disks, also while running) and make templates from VMs (guest cleaned via the agent) ([clones-templates.md](clones-templates.md))
- [x] Ceph on the servers themselves: install, monitors, managers, disks (OSDs) and the VM pool from the dashboard; VMs on Ceph move without copying disks and fail over from the same disk ([ceph.md](ceph.md))
- [x] Installer with the system on ZFS: pick the disks, then one disk / mirror / RAID 10 / RAID-Z1-2-3 (or ext4); boots from every disk; offline install from the ISO, unattended with an answers file ([installer.md](installer.md))
- [x] No single master: cluster book on every server, elections by majority (3+ servers), old master steps down, fencing without quorum, `p8 cluster promote` for two servers; direct server-to-server tunnels
- [x] Incremental backups (only changes since the last, full every 7th, chain-aware keep/delete/off-site) and restore tests (weekly automatic + on demand)
- [x] Live moves: running VMs move between cluster servers without stopping (memory + disks over the tunnel); CPU type "compatible" = the cluster's common CPU ([cluster.md](cluster.md))
- [x] P8S logo everywhere: installer boot menu, server boot menu (GRUB theme) and boot splash (Plymouth), console/SSH login, dashboard icons ([assets/brand](../assets/brand/README.md))
- [x] Network storage: NAS shares (NFS) for backup copies and ISOs; SAN disks (iSCSI) with a ZFS pool for VM disks, connected at boot ([network-storage.md](network-storage.md))
- [x] Extra disks per VM (up to 8): add (live on virtio/SCSI), grow, remove; snapshots, backups, off-site copies and restores cover all disks (`p8 disk-add`)
- [x] Update server: signed release bundles, testing / stable channels (stable for subscribers), one-click install from the dashboard, `p8 update`

Still to do: projects, scoped API tokens per user; per-tenant VLAN/bridge;
nftables firewall per VM; IP pools (static assignment via cloud-init network
config); container backups / firewall / public IPs. Replaces Proxmox on the home lab.

## Phase 3 — Web UI and console (months 4–5)

- [x] Web dashboard v1 (pulled forward): HTTPS with self-signed cert, token sign-in, host cards,
      VM list with live refresh, create / start / stop / reboot / force-off / delete, phone layout
- [x] Browser console (noVNC over a ticketed WebSocket proxy in p8sd), set guest password, type text
- [x] More OS templates: Debian 12/13, Ubuntu 22.04/24.04, Rocky 9, AlmaLinux 9 (`p8 templates add`, checksum-verified)
- [x] Backups + restore: compressed zfs-send files, restore in place / as new VM / recreate deleted VM, daily schedule with retention, background jobs
- [x] VM guard (nftables): VMs can't reach the host or (private VMs) the home LAN; `vm_allow` exceptions
- [x] Screen image straight from QEMU (`GET /vms/{name}/screenshot`, console "Screen image")
- [x] Install from any ISO: ISO library (upload / download with checksum), empty-disk VMs booting the CD, Windows device profile, eject
- [x] Usage graphs: CPU, memory, disk and network per VM (last hour, 10 s), host CPU/memory sparklines

Admin + customer panels, noVNC console proxied through p8sd auth, metrics,
scheduled backups (ZFS send to a second pool or remote).

## Phase 4 — Installer ISO (months 5–6)

- [x] Debian live-build: branded installer → partitions, ZFS root with a RAID choice, installs p8sd,
  boots to the web UI ([installer.md](installer.md)). This is the moment it becomes **P8S OS**.
- [ ] Graphical installer (mouse, like Proxmox's); p8sd pre-built on the ISO (first start without internet)

## Phase 5 — Clustering (month 7+)

Multi-node, shared state (etcd), live migration, Ceph RBD storage backend, HA.
