# Clones and templates made from VMs

## Clone a VM

Dashboard: **Manage → Options → Clone** (admins and operators). Command line: `p8 clone web1 web2 [--start]`.

- A new VM with a **full copy of every disk** (root and data disks), taken at one instant — also while the
  VM runs (its file systems are frozen for that instant through the guest agent).
- Same CPUs, memory, disk sizes, network, VLAN, CPU type and firewall rules. On the same server and,
  unless you pick another (the storage list next to the name, or `p8 clone web1 web2 --storage ceph`),
  the same storage (local ZFS, SAN pool or Ceph).
- Its own name, MAC address and IP. VMs made from a template get a new cloud-init disk: the new name,
  new SSH host keys, DHCP by MAC (so the copy never asks for the original's address).
- VMs installed from an ISO are copied **exactly** (same computer name and IDs inside). Windows: run
  `sysprep /generalize /oobe /shutdown` in the VM first.
- Not copied: snapshots, backups, the public IP, high availability, autostart (off on the copy).
- The copy is independent: deleting either one doesn't touch the other.
- **On another server** (cluster): pick the server in the list next to the name ("on p8s-worker"), or
  `p8 clone web1 web2 --on p8s-worker`. One job makes the copy next to the VM (shut down), then moves it
  over the tunnel and starts it if asked. A full copy on that server's local disks; not for linked clones.
  If the move can't be done (e.g. that server went offline), the copy stays on the VM's server and the job
  says why.

### Linked clones

Tick **Linked clone** (or `p8 clone web1 web2 --linked`): the copy is made in a second and takes no
disk space at first — its disks are ZFS clones of a snapshot of the original's disks, and only what
changes afterwards uses space. Good for many test VMs from one prepared VM.

- The original keeps that snapshot (hidden, `p8s-link-…`) while the clone exists, so the original
  **can't be deleted or moved** (to another server or storage) while it has linked clones; Manage
  lists them. Rolling the original back past that snapshot is refused too.
- **Make independent** (Manage → Options, VM shut down; `p8 unlink web2`) copies the clone's disks
  in full; after that the two are separate VMs. Deleting a linked clone removes its snapshot on the
  original.
- Same storage as the original (local ZFS, SAN pool or NAS pool); not on Ceph (make a full copy there).
- Backups, snapshots, resize, moving the clone to another server: all work (a backup or move copies
  the clone's full data). Moving a linked clone to another storage on the same server: make it
  independent first.

## Make a template from a VM

Set up one VM the way you want (packages, settings), then **Manage → Options → Make a template**
(admins). Command line: `p8 template-from web-base-vm web-base "Web server base"`.

- If the VM runs, P8S first cleans it through its **guest agent** — cloud-init state, machine id, SSH
  host keys, DHCP leases, root's shell history — and shuts it down. The VM stays and keeps working; it
  gets new SSH host keys when started.
- If it is already shut down it is copied as it is (start it and make the template while it runs to
  get it cleaned).
- The VM's root disk is copied into `tank/p8s/templates/<name>`, its data disks into
  `<name>-disk1` … — VMs made from the template get copies of the data disks too.
- VMs on Ceph work as well: their disks are copied out of Ceph into the local template.
- **New VM → Operating system → "Made from your VMs"**, or `p8 create web7 --os web-base`. New VMs
  are instant copies, like with the built-in templates, and their size starts at the template's.
- In a cluster the template starts on the server where its VM was. **Copy** it to other servers:
  Server → OS templates → Made from VMs → pick a server (or "every server without it") → Copy, or
  `p8 templates copy web-base [SERVER]`. The disks go over the cluster tunnel (ZFS send, compressed,
  only the used blocks). The list shows every server that has a copy.
- New VMs from it go on a server that has a copy (the one with the most room); the others show
  "no copy of template …" as the reason they can't take it.
- Delete: Server → OS templates → Made from VMs → Delete (every copy), or `p8 templates rm web-base`;
  one server's copy: `p8 templates rm web-base --on SERVER`. Refused while VMs made from it still
  exist on that server (they are ZFS clones of it).

## API

`POST /vms/{name}/clone {"name","start","linked","storage"}`, `POST /vms/{name}/unlink` (make a linked clone
independent) and `POST /vms/{name}/template {"name","label"}` start jobs
(`GET /jobs/{id}`); `GET /templates` lists all (`custom`, `from`, `node`, `nodes` for those made from VMs);
`DELETE /templates/{name}[?node=ID]`; `POST /templates/{name}/copy {"to":["n2"]}` copies it (no `to`: every
online server without it; a job). Between servers: `GET /cluster/tpl-export/{name}`, `POST /cluster/tpl-import`.

## Not yet

- Linked clones on Ceph (rbd clone).
