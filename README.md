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
scripts/            host bootstrap, template import
docs/               api.md, architecture.md, host-setup.md, roadmap.md
```

## Quick start

```bash
make test                 # no root/KVM needed
make build                # bin/p8sd, static binary
```

**Testing and deploying: start with [USER.md](USER.md)** — step-by-step from a fresh
Debian 13 VM to a passing end-to-end smoke test (`scripts/smoke-test.sh`).
API reference: [docs/api.md](docs/api.md).
Design decisions: [docs/architecture.md](docs/architecture.md).
