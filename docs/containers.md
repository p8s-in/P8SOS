# Containers (LXC)

P8S OS runs **containers** next to VMs, like Proxmox's "CT". A container shares the server's Linux
kernel, so it starts in about a second and needs far less memory than a VM: the right choice for small,
cheap Linux servers (websites, bots, databases, VPNs). Windows or a custom kernel needs a VM.

## Using them

1. Add a template once: dashboard → **Server** → **Container templates** → **Add** (Debian 13/12,
   Ubuntu 24.04/22.04, Alpine, Rocky, Alma, Fedora, Arch). 50-200 MB download from
   images.linuxcontainers.org, checked against its SHA256SUMS.
   CLI: `p8 ct templates add debian-13`.
2. **+ Create CT** in the top bar: name, template, CPUs, RAM, disk, network, root password and/or SSH keys.
   CLI: `p8 ct create web2 1 512 8 --os debian-13 --root-password`.
3. It shows in the sidebar (box icon) and in the **Containers** table: Console (a terminal in the browser),
   Shutdown, Manage (resources, snapshots, backups, firewall, SSH, password, owner), Reboot / Force off / Delete under **⋯**.
4. SSH in with `ssh root@<ip>` (cloud-init installs and starts the SSH server on first boot).

## How it works

| | |
|---|---|
| Isolation | **Unprivileged**: root inside is uid 100000 on the host (nobody), AppArmor profile, own namespaces |
| Disk | Own ZFS dataset `tank/p8s/ct/NAME`, an instant clone of `tank/p8s/ct-templates/T@base`; size limit = ZFS `refquota` |
| CPU / RAM | cgroup v2 (`cpu.max`, `memory.max`, swap up to 512 MB, 8192 processes); changed live |
| Network | veth on the same bridges as VMs (`virbr0` NAT, or `br0` home network); the VM guard rules apply too |
| First boot | cloud-init NoCloud seed in the container: hostname, root password (hashed), SSH keys, openssh-server |
| Running | each container is a systemd unit `p8s-ct-NAME` (`lxc-start -F`), outside p8sd, so p8sd restarts don't touch it |
| Autostart | containers marked "start with the server" are started by p8sd after a reboot |
| Owners / quotas | same as VMs: customers see only theirs; a container counts as one server in the quota |

| Backups | Manage → **Back up now**: processes frozen for an instant (`lxc-freeze`), ZFS snapshot, `zfs send` + zstd into `<backup dir>/_ct/NAME/`. **Restore as new…** brings it back as a new container (new MAC, hostname set); it can take the old name once the original is deleted |
| Daily backups | Manage → Backups → "Every day at …, keep N" (same time as VM backups; schedule saved as `ct:NAME`) |
| Plans (P8S Cloud) | customers pick a size from the plans for containers: plans for both, or **containers only** ones (often cheaper; Settings → Plans → For) |
| Public IP | Create CT → Network → **Public IP — pool**: address from the same pools as VMs, set in the LXC config, the cloud-init seed and the distro's own files (netplan, systemd-networkd, ifupdown, NetworkManager, ifcfg). Pinned to the container (anti-spoofing) on its fixed host port `vc…` |
| P8S Cloud | Customers pick a plan like for VMs; billed per hour, stopped when suspended, no free resize |
| Firewall | Manage → **Firewall**: the same rules as for VMs (SSH / Web / Ping / custom, optional source), in the same nftables table, matched by the container's MAC; saved as `ct:NAME` in `firewall.json` |

Files: config `/var/lib/lxc/NAME/config` (written by p8sd), log `/var/lib/lxc/NAME/lxc.log`,
list `/var/lib/p8s/containers.json`. Shell on the server: `p8 ct enter NAME`.

## In a cluster

- **Where it runs:** on the master the New container dialog has a **Server** list
  (Automatic picks the server with the most free memory that has the template).
  Containers with a public IP go on a server that has the pool's bridge (the master hands out the address).
- **One list:** the master lists the containers of every server ("on NODE");
  start/stop, resize, snapshots, backups, passwords and the terminal work the
  same wherever the container runs.
- **Moving:** Manage → **Move to server**. A container with a public IP keeps it, if the other server has the pool's bridge.
  The disk is copied with `zfs send`, the container is removed from the old server
  and optionally started on the new one. `p8 ct-move NAME SERVER [--start]`.
  A **running** container moves with a short pause: its disk is copied while it
  keeps running, then it is shut down, only what changed since is copied, and it
  starts on the new server (off for its shutdown plus a few seconds; open
  connections are dropped, like a reboot). If something fails before the end it
  stays on the old server and runs again there.
  If its network doesn't exist on the new server it's put on the NAT network (the job says so).
- **Virtual networks:** pick a `vnet:NAME` network in the dialog; the container gets
  an MTU of 1230 like VMs on virtual networks.

- **High availability:** Manage → High availability keeps a copy of the
  container on another server of the cluster and starts it there if its
  server dies. See [ha.md](ha.md#containers).

## Not yet (next steps)

- Moving a running container without any pause (memory copy, CRIU): it moves with a short pause instead
