# Clones and templates made from VMs

## Clone a VM

Dashboard: **Manage → Options → Clone** (admins and operators). Command line: `p8 clone web1 web2 [--start]`.

- A new VM with a **full copy of every disk** (root and data disks), taken at one instant — also while the
  VM runs (its file systems are frozen for that instant through the guest agent).
- Same CPUs, memory, disk sizes, network, VLAN, CPU type and firewall rules. On the same storage
  (local ZFS, SAN pool or Ceph) and the same server.
- Its own name, MAC address and IP. VMs made from a template get a new cloud-init disk: the new name,
  new SSH host keys, DHCP by MAC (so the copy never asks for the original's address).
- VMs installed from an ISO are copied **exactly** (same computer name and IDs inside). Windows: run
  `sysprep /generalize /oobe /shutdown` in the VM first.
- Not copied: snapshots, backups, the public IP, high availability, autostart (off on the copy).
- The copy is independent: deleting either one doesn't touch the other.

## Make a template from a VM

Set up one VM the way you want (packages, settings), then **Manage → Options → Make a template**
(admins). Command line: `p8 template-from web-base-vm web-base "Web server base"`.

- If the VM runs, P8S first cleans it through its **guest agent** — cloud-init state, machine id, SSH
  host keys, DHCP leases, root's shell history — and shuts it down. The VM stays and keeps working; it
  gets new SSH host keys when started.
- If it is already shut down it is copied as it is (start it and make the template while it runs to
  get it cleaned).
- The VM's root disk is copied into `tank/p8s/templates/<name>` (data disks are not part of a template).
- **New VM → Operating system → "Made from your VMs"**, or `p8 create web7 --os web-base`. New VMs
  are instant copies, like with the built-in templates, and their size starts at the template's.
- In a cluster the template lives on the server where its VM was; VMs made from it are created there.
- Delete: Server → OS templates → Made from VMs → Delete, or `p8 templates rm web-base` — refused while
  VMs made from it still exist (they are ZFS clones of it).

## API

`POST /vms/{name}/clone {"name","start"}` and `POST /vms/{name}/template {"name","label"}` start jobs
(`GET /jobs/{id}`); `GET /templates` lists all (`custom`, `from`, `node` for those made from VMs);
`DELETE /templates/{name}`.

## Not yet

- Templates from VMs on Ceph; templates with data disks; linked clones (a clone is always a full copy).
- Cloning to another server or another storage in one step (clone, then move it).
