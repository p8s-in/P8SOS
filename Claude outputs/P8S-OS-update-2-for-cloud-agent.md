# P8S OS: update 2 for the P8S Cloud (web) agent (13 Oct 2026)

This replaces the "Production VPS hosts stay on Proxmox" rule in `P8S-OS.md`. The owner has decided the direction.

## The goal (owner's decision)

P8S OS is built **for P8S first and for the public second**: one product, two uses.

1. **For us:** P8S OS becomes the OS on all P8S servers. It powers P8S Cloud and **replaces Proxmox, step by step**.
2. **For the public:** later it is sold as a product to companies and home users. Editions: Home (free) / Standard / Enterprise / Datacenter, plus Internal for our own servers. Licence keys come later.

So P8S Cloud is P8S OS's first and most demanding customer. Everything P8S Cloud sells must eventually run on P8S OS.

## New plan (replaces rule 1)

| Step | What | Customers affected |
|---|---|---|
| 1. Now | P8S OS hardened on the test laptop | None |
| 2. One real server | P8S OS on one spare server; our own VMs and test VPSes for 2–4 weeks; backup/restore and 2-server cluster tested | None |
| 3. New customers | The console gets a **second compute client for P8S OS** next to `ProxmoxClient`. New VPSes go to the P8S OS server | New only |
| 4. Move existing customers | Proxmox → P8S OS import (being built next), in small batches, with Proxmox kept as fallback | Existing, gradually |
| 5. Proxmox off | After a stable period | All |
| 6. Sell P8S OS | Licences on, public ISO, docs, P8S OS page on p8s.in | Market |

Rules 2 and 3 from `P8S-OS.md` stay:

- **One billing system:** the console owns customers, the wallet and prices. It drives P8S OS with an admin token and sends **no `owner` and no `plan`**, so P8S OS doesn't bill, check quotas or email.
- **Port 8006 never public:** set `"listen": "<wireguard-ip>:8006"` and pin the certificate.

## What the console can start on now (no rush; step 3)

- A `P8SOSClient` in `app/services/compute.py` with the same methods as `ProxmoxClient`. Calls are in `P8S-OS.md`; the full reference is `docs/api.md` in the P8SOS repo.
- Store the P8S OS guest **name** on `cloud_servers`; there is no VMID.
- Provider choice per server/region in config, so Proxmox and P8S OS hosts can run side by side.

## Needed from you: the full P8S Cloud product list

P8S Cloud plans enterprise-level products: VPS, **enterprise storage**, and more. P8S OS has to cover all of them, so please send back a list of **everything P8S Cloud sells or plans to sell**, with what each needs from the host, for example:

- **VPS:** sizes, images, IPv4/IPv6, backups, snapshots, firewall, console, rescue mode, ISO upload, …
- **Storage:** block volumes attached to VPSes? Object storage (S3)? File shares (NFS/SMB)? Backup storage? Sizes, replication or redundancy level, and whether it stays on the Ubuntu + Ceph storage server or moves to P8S OS.
- **Networking:** private networks/VLANs between a customer's servers, load balancers, floating IPs, DDoS protection, bandwidth limits and metering.
- **Other:** managed databases, Kubernetes, one-click apps, dedicated servers, GPU, DNS, domains, email, monitoring/alerts for customers.
- **Per product:** whether it's live now, planned, or an idea, and its priority.

P8S OS will use that list to set its roadmap. Next on the P8S OS side: Proxmox import, moving VMs between servers, then monitoring/alerts. The rest gets ordered by your list.
