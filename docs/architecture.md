# P8S OS architecture (Phase 1)

```
            HTTP/JSON (bearer token)
                     │
              ┌──────▼──────┐
              │  internal/api│  routing, auth, JSON errors
              └──────┬──────┘
              ┌──────▼──────┐
              │  internal/vm │  lifecycle + validation + rollback, per-VM locks
              └─┬────┬────┬─┘
     ┌──────────┘    │    └───────────┐
┌────▼─────┐  ┌──────▼─────┐  ┌───────▼──────┐
│ storage  │  │ cloudinit  │  │    virt      │
│ zfs clone│  │ seed ISO   │  │ domain XML + │
│ /destroy │  │ (NoCloud)  │  │ Hypervisor   │
└────┬─────┘  └──────┬─────┘  └───────┬──────┘
     │ zfs           │ cloud-localds  │ virsh → libvirtd → QEMU/KVM
```

Every layer that touches the host goes through `execx.Runner`, so the whole
daemon is unit-tested with fakes and never needs root or KVM in CI.

## Decisions

| # | Decision | Why | Revisit when |
|---|----------|-----|--------------|
| 1 | **Go**, stdlib only | Single static binary, fast, same language as Incus/Harvester. No deps = nothing to vendor or audit yet. | Need a native libvirt client or Postgres driver. |
| 2 | **ZFS** zvols, template = snapshot, VM disk = clone | Instant copy-on-write provisioning, snapshots and send/receive for backups later. | Clustering → add Ceph RBD as a second backend. |
| 3 | **Debian 13** host | Stable, same base as Proxmox, ZFS in contrib. | — |
| 4 | **libvirt is the source of truth** in Phase 1 | P8S metadata lives in each domain's `<metadata>` (namespace `https://p8s.dev/xmlns/vm/1`). No DB to drift out of sync. | Phase 2 adds tenants/quotas → SQLite (single node) then etcd/Postgres. |
| 5 | **virsh backend** behind `virt.Hypervisor` | Zero dependencies; every call is visible in logs and reproducible by hand. | Performance or events needed → swap in `github.com/digitalocean/go-libvirt` (pure-Go RPC) without touching api/vm. |
| 6 | Domains named `p8s-<name>` | p8sd never touches VMs it didn't create. | — |
| 7 | Synchronous create | Simple; create is a few seconds on ZFS. | Image downloads or migrations → job queue. |
| 8 | VNC bound to 127.0.0.1 | Console must go through p8sd auth (Phase 3), never exposed directly. | — |

## Host layout

| Path | Purpose |
|------|---------|
| `/usr/local/bin/p8sd` | daemon |
| `/etc/p8s/p8sd.json` | config (0600, contains token hashes) |
| `/var/lib/p8s/seeds/` | cloud-init seed ISOs, one per VM |
| `tank/p8s/templates/<name>@base` | golden images |
| `tank/p8s/vms/p8s-<name>-disk0` | VM root disks (clones) |

## Known Phase 1 limits

- One NIC per VM on one bridge; no IP management (DHCP from your LAN/libvirt).
- No quotas or tenants; any token can do anything.
- Per-VM locks are in-process only (fine for one daemon per host).
- If p8sd crashes mid-create, rollback doesn't run; leftovers are named
  `p8s-<name>` and can be removed with `DELETE` or `zfs destroy`.
