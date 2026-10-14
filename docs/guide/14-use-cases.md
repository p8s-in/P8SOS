# 14. Use cases, step by step
Summary: Step-by-step recipes: home lab, company servers, VPS hosting, two servers, Ceph cluster.

Each recipe uses only the features described above. Pick the one closest to you.

## A. Home lab on one PC or laptop

**Goal:** try Linux distributions, run a few services, learn. **Hardware:** one machine, 16 GB RAM, one SSD (mirror if you have two).

1. Install with layout *one disk* (or *mirror*), DHCP address.
2. `p8 templates add ubuntu-24.04` and create VMs on the private network.
3. Snapshot before every experiment: `p8 snap lab1 clean`, roll back when it breaks.
4. Home network once (`sudo p8 lan-setup`) for anything you open from your phone or TV (Jellyfin, Home Assistant).
5. Tailscale for access from outside; daily backups to a USB disk as an off-site folder.

## B. Your own company servers (one server)

**Goal:** replace a few physical servers (file server, ERP, database, Windows AD). **Hardware:** 64 GB RAM, 2–4 SSDs as mirror or RAID 10, UPS.

1. Install with *mirror* or *RAID 10*, fixed IP, encryption on if disks may leave the building.
2. `sudo p8 lan-setup`; VLANs per department if your switch supports it.
3. Windows Server VMs from ISO with virtio-win; Linux VMs from templates.
4. Build one tuned VM, **Make template**, and create the rest from it.
5. Daily backups keep 7, off-site to a NAS or S3 with a passphrase, monthly Verify.
6. Company login (Entra or Google) or LDAP for staff; operators for IT helpers.
7. Health & alerts to email and phone; UPS shutdown on.

## C. Selling VPS hosting (P8S Cloud)

**Goal:** customers sign up, pay, and get servers in a minute. **Hardware:** a datacenter server with a public IP block, ideally 2–3 servers.

1. Install, `sudo p8 lan-setup`, then **Public IP pools** with your block (e.g. `203.0.113.8/29`).
2. Settings → Selling: brand, panel address, GST, invoice prefix; Settings → Email (SMTP).
3. **Plans**, e.g. Starter 1 CPU / 1 GB / 25 GB → ₹399/mo; Pro with public IP.
4. Settings → Payments: Razorpay keys and webhook (test with `rzp_test_` keys first).
5. Cloudflare Tunnel for `https://panel.yourdomain`; turn on self sign-up.
6. Customers top up, create servers, open support requests, use API tokens. Servers stop (data kept) after the grace period when the balance stays negative.
7. Firewall defaults per VM; anti-spoofing keeps customers on their own IPs. Bring Windows licences under SPLA if you sell Windows.

## D. Small business with two servers

**Goal:** a spare server so a hardware fault doesn't stop work. **Hardware:** two similar servers on the same LAN.

1. Install both, join the second to the first.
2. Turn on **High availability** for important VMs with a 5–15 minute copy interval.
3. Move VMs off a server before maintenance (live where possible), then back.
4. If the master dies: `p8 cluster promote` on the other (or **Start here**). Add a third server later for automatic master takeover.

## E. Three-server cluster with Ceph (no data loss)

**Goal:** VMs that survive a dead server with nothing lost and move in seconds. **Hardware:** 3 servers, each with a system mirror plus 1–2 empty SSDs for Ceph, 10 GbE between them.

1. Install all three, join them (same LAN).
2. Server → Ceph: install on each, **Set up Ceph**, 3 monitors, 2 managers, one disk each, **Create the VM pool**.
3. Create important VMs with **Storage: ceph** and turn on HA.
4. Test: pull the power of one server; its VMs start on another within about 3 minutes, from the same disk. Ceph shows HEALTH\_WARN until the server returns.
5. Keep local ZFS for VMs that need the fastest disks and back them up as usual (Ceph VMs too: their backups are full disk images).

## Other ideas

| Use | How |
| --- | --- |
| Dev/test environments | Template from a prepared VM → instant clones per developer; delete when done |
| Training lab | One template per course; `p8 create` in a loop; customers role for students with quotas |
| Websites at home behind CGNAT | VMs on private network + `p8 publish web1 shop 80` through Cloudflare Tunnel |
| Moving off Proxmox/VMware | Storage → Import VMs from vzdump or VMDK files |
| Branch offices | One small server per branch joined over the internet to the head-office master; manual failover there |
