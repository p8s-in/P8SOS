# P8S OS

A virtualization platform built on Debian 13, KVM/QEMU, libvirt and ZFS, with
its own management layer: **p8sd**.

Status: **Phase 1** — single-node daemon that creates VMs from templates over a REST API.

## Layout

```
cmd/p8sd/           daemon entrypoint (serve, token, version)
internal/api/       REST API v1, auth, errors
internal/vm/        VM lifecycle, validation, rollback
internal/virt/      libvirt domain XML + Hypervisor (virsh backend)
internal/storage/   ZFS disks (clone from template snapshot)
internal/cloudinit/ NoCloud seed ISOs
internal/config/    JSON config
internal/execx/     command runner (faked in tests)
deploy/             systemd unit, example config
install.sh          one-command host install
update.sh           update an installed host from a newer copy of this folder
iso/                installer ISO (ZFS system disks, RAID choice: cmd/p8s-install), classic ISO, first-boot setup
scripts/            p8 CLI, smoke test, host bootstrap, template import
docs/               api.md, architecture.md, host-setup.md, roadmap.md
```

## Quick start

```bash
make test                 # no root/KVM needed
make build                # bin/p8sd, static binary
```

**Try it: [USER.md](USER.md)** — Debian 13 VM in VMware/VirtualBox → `sudo bash install.sh /dev/sdb` → `p8 test`.
Step-by-step manual version: [docs/testing-in-depth.md](docs/testing-in-depth.md).
**Install on real hardware: [docs/iso.md](docs/iso.md)** — build the P8S OS alpha ISO, write it to USB, install.
VMs on your home network: [docs/networking.md](docs/networking.md).
API reference: [docs/api.md](docs/api.md).
Design decisions: [docs/architecture.md](docs/architecture.md).
