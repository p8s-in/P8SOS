# P8S OS: test checklist (new laptop / real server)

Everything below is built and passes the automated tests, but has **not yet been tried on real
hardware**. Work through it on the new machine; tick each line and note problems (screenshot +
`journalctl -u p8sd -n 100`). Go top to bottom; each section says what it needs.

## 0. Install

- [ ] Install from the ISO (or Debian 13 + `sudo bash install.sh`); dashboard at `https://<ip>:8006`
- [ ] `sudo bash update.sh` works from a git checkout
- [ ] Server page → Memory shows a sensible split; **ZFS cache limit** Set / Auto works
- [ ] Server page → **Check guest internet**: all ✔ (else **Repair**, then check again)
- [ ] Reboot the server: autostart VMs/containers come back; guest internet still works (p8s-net.service)

## 1. VMs (one server)

- [ ] Create a Linux VM from a template: gets an IP, SSH port works, **Login details** box shows the root password, `ssh -p <port> root@<server>` works with it
- [ ] Manage → Login → **New password** (VM running) works
- [ ] RAM column shows "used of size" after a minute; the VM's Memory chart shows "Used inside the VM"
- [ ] Shutdown → Start an old VM: 1 socket / N cores and the memory balloon are applied (`virsh dumpxml p8s-<name> | grep -E "topology|memballoon"`)
- [ ] Snapshot, roll back, delete snapshot
- [ ] Backup now, restore over, restore as new; the daily schedule runs
- [ ] Firewall: enable with SSH only; other ports are blocked
- [ ] Resize CPU / RAM / disk
- [ ] Console: keyboard, **Keys…** menu (Shift+F10, Esc, F12), Ctrl+Alt+Del, Type text, Set password

## 2. Windows

- [ ] ISO library: a Windows ISO shows "boots on BIOS and UEFI"; virtio-win shows "not bootable" and is greyed out as install CD
- [ ] Download virtio-win from the ISO page (DNS fallback works)
- [ ] Windows 11 VM: the installer starts by itself (Enter pressed during "Press any key"), installs, reaches the desktop
- [ ] Windows has internet; no "DNS address could not be found" after a while (guest DNS fix)
- [ ] Task Manager in Windows shows all vCPUs
- [ ] virtio-win-guest-tools installs; Remote Desktop to `<server>:3391` works (Windows Pro)

## 3. Containers

- [ ] Create from a template: **Login details** box; SSH as root with the password; console terminal
- [ ] RAM column shows real use (tens of MB for an idle container)
- [ ] Firewall, backup + restore, daily schedule, snapshot, resize
- [ ] Public IP from a pool (if you have one)

## 4. Import VMs (needs a Proxmox host; the `pve1` VM is fine)

- [ ] Proxmox: back up a small Linux VM (`vzdump <id> --compress zstd --mode stop`), scp the `.vma.zst` to `/var/lib/p8s/import/`
- [ ] Storage → Import VMs → Check: settings match the Proxmox VM; warnings make sense
- [ ] Import: the VM boots and has network (fix the NIC name inside if it changed)
- [ ] Same with a UEFI VM (bios: ovmf, 4M efidisk): boots without the EFI shell
- [ ] A Windows VM from Proxmox (virtio-scsi + virtio drivers): boots
- [ ] A `.vmdk` (VMware / VirtualBox) and a `.vhdx` (Hyper-V): import, set firmware/bus, boots
- [ ] Upload in the browser (small file) works; big files via scp

## 5. Backups off the server (needs a USB disk / NAS / S3 bucket / second Linux box)

- [ ] Off-site target: folder (USB/NAS), S3 (real bucket), SSH; with and without encryption
- [ ] Every new backup is copied; "keep N" deletes old copies; **Bring back** + restore works

## 6. Two machines (cluster; needs the second laptop)

- [ ] Same network: Add server → the new laptop is listed → invite → accept (6-digit numbers match)
- [ ] Other network: join with the public address (router ports TCP 8006 + UDP 51820)
- [ ] Create VM picks a server automatically; the "Server" select places it by hand
- [ ] Worker VM: console, power, snapshots, backups work from the master
- [ ] Worker offline: its VMs show "unknown"; back online: normal
- [ ] Server → Cluster → **Shell** on the worker
- [ ] Move a shut-down VM master → worker (VM → Manage → Options → Move): job finishes, VM starts there, has internet, same root password works on the new SSH port
- [ ] Move it back worker → master; with 3 servers also worker → worker
- [ ] A running VM / one with a CD in the drive: Move is greyed out with the reason
- [ ] Pull the network cable halfway through a move: the job fails, the original is still there and starts
- [ ] Move a UEFI / Windows 11 VM: boots without the EFI shell, Windows doesn't ask for the BitLocker key
- [ ] Remove the worker; leave the cluster

## 6b. High availability (two machines on the same network)

- [ ] VM on the worker → Options → High availability → Protect (copy on the master): "last copy" shows within a minute; Server → High availability lists it
- [ ] Change a file in the VM, wait for the next copy (set 1 minute to test)
- [ ] Pull the worker's power plug: about 3 minutes later the VM runs on the master (alert arrives), the changed file is there
- [ ] Plug the worker back in: the old VM is gone from it, Server → High availability on the worker lists the copy, copies go master → worker
- [ ] Pull only the worker's network cable: after ~1 minute the worker switches the VM off (its log says so), the master starts the copy; plug in: no VM runs twice
- [ ] Stop p8sd on the worker (`sudo systemctl stop p8sd`): no failover (tunnel still up), warning alert instead
- [ ] Reboot the worker normally: the protected VM starts again by itself when it is back
- [ ] Protected VM on the master: switch the master off, worker dashboard → Server → High availability → Start here
- [ ] Turn off HA: the copy is deleted on the other server

## 7. Admin and safety

- [ ] Server → **>_ Shell** (root shell) opens and is logged in Activity
- [ ] A customer account sees only its own VMs/containers and their login details
- [ ] 2FA sign-in
- [ ] Debian updates from the Server page; guest network still fine afterwards

## 8. Browsers

- [ ] Dashboard on a MacBook (1280–1512 px wide), Safari and Chrome: nothing cut off, no sideways scrolling
- [ ] Phone: menu button, tables shown as cards

## 9. Health & alerts

- [ ] Server → Health & alerts lists the checks (pool, disks with SMART, space, memory, CPU temperature, power)
- [ ] Enter an email (email set up under Settings), a webhook (e.g. a Discord channel webhook) and an ntfy topic; **Send a test** arrives everywhere
- [ ] Fill a test pool / disk past the warning level (or lower the level): warning arrives, then RESOLVED after cleaning up
- [ ] Laptop: unplug the charger → "power cut" warning; plug in → resolved
- [ ] Laptop: set the battery level high (e.g. 95 %), unplug → VMs shut down cleanly and the laptop powers off (set it back to 10 % afterwards)
- [ ] UPS with NUT (if you have one): `upsc` works, on-battery warning, low-battery shutdown
- [ ] A crashed VM (e.g. `virsh destroy` doesn't count; kill its qemu process) shows a critical alert
- [ ] The red/orange number on the server button in the sidebar

## 10. Security

- [ ] Users: make an **operator** — can create/stop VMs and open consoles; no Settings button, no server shell, can't add users
- [ ] Make an **auditor** — sees all VMs and Activity, "(read-only)" next to the name; stopping a VM is refused
- [ ] Settings → Company login with a real Google Workspace or Keycloak: "Sign in with …" works, account created, role from the group
- [ ] Remove the person from the admin group in the provider: next sign-in has the new role
- [ ] Activity → Check integrity ✔; edit a line in /var/lib/p8s/audit.log by hand → Check integrity shows it (then restore)
- [ ] Activity → Download CSV for a date range opens in Excel
- [ ] `journalctl --disk-usage` grows over days; logs survive a reboot
- [ ] `timedatectl` shows NTP synced; Health & alerts has "Clock (NTP)"
- [ ] Company directory against a real AD / OpenLDAP / FreeIPA (a Samba AD VM is fine): Test ✔, a directory user signs in, role from the group, wrong password refused
- [ ] Security → allowed networks = your LAN only: the dashboard from a phone on mobile data gets "may not use this server"; empty the list again
- [ ] Password policy 12 + letters/digits: "password1234" and "abcdefghijkl" refused
- [ ] Two-factor for staff on: an operator without 2FA lands in My account and can't do anything else until it is set up
- [ ] Fresh install with "encrypted VM storage" (boot menu): `zfs get encryption tank/p8s` = aes-256-gcm; VMs work after a reboot (key loaded); Server → Storage → Show the key

## 11. Remote access (from outside the home network)

- [ ] Server → Remote access: public IP shown; the CGNAT line matches what the router's status page says
- [ ] Tailscale: Install and connect → sign-in link in the task log → phone with Tailscale on mobile data opens https://100.x.x.x:8006
- [ ] Tailscale with "Also reach my home network": approve the route, reach a LAN VM from the phone
- [ ] (only with a real public IP) Built-in VPN: on, Open the port on my router (or forward UDP 51830 by hand), add a phone, import the .conf, dashboard at https://10.251.0.1:8006 on mobile data; Remove cuts it off
- [ ] Reboot: the VPN comes back (p8s-vpn interface), devices reconnect

## 12. VLANs (needs a managed switch, or a router that does VLANs)

- [ ] `p8 test` section 8 passes the VLAN lines (VLAN 4000 wiring)
- [ ] Switch port = trunk with VLAN 20 tagged; VM on VLAN 20 gets an address from VLAN 20's DHCP and reaches its gateway
- [ ] Reboot the server: the VLAN VM comes back on VLAN 20 (p8s-vlans.service ran before libvirt)

## 13. Bonding and IPv6

- [ ] (needs a USB Ethernet adapter or a 2-port PC) Server → Home network: Add as second cable → bond0 active-backup; pull the active cable: ping to a LAN VM keeps going
- [ ] Server page shows the ports, cable state, speed and which one is active
- [ ] With IPv6 from the ISP: Turn on IPv6 for the private network → in a NAT VM `ip -6 addr` has fdXX:…, `ping -6 google.com` works
- [ ] `p8 ipv6 off` → back to IPv4 only, VMs stay connected

## 14. Extra disks

- [ ] `p8 test` section 6b2 passes, and 6c says "backup has both disks"
- [ ] Linux VM running: Manage → Resources → Add disk 20 GB → `lsblk` in the VM shows vdb at once; `mkfs.ext4 /dev/vdb`, mount, write a file
- [ ] Windows VM (SATA): Add disk while running → note says it appears after a shut down and start; it does, Disk Management sees it
- [ ] Snapshot, delete the file on vdb, roll back: the file is back on vdb too
- [ ] Backup with two disks, restore as a copy: the copy has both disks and the file
- [ ] Grow disk 1 (VM stopped) → `growpart`/`resize2fs` inside sees the new size; shrinking is refused
- [ ] Remove disk 1 (two clicks) → gone in the VM, `zfs list` has no …-disk1
- [ ] HA on a VM with an extra disk is refused with a clear message; Move too

## 15. Network storage (needs a NAS/SAN — TrueNAS in a VM on the second laptop works)

- [ ] `p8 test` section 9 passes; with `P8S_NFS=192.168.0.x:/mnt/tank/p8s p8 test` and `P8S_ISCSI='192.168.0.x iqn... 0' p8 test` the NAS/SAN lines pass too
- [ ] NFS share with "Copy every backup here": take a backup → after a minute it is in `p8s/backups` on the NAS (Backups → Off-site shows the copy)
- [ ] An ISO copied into `p8s/iso` on the NAS shows in the ISO library "on NAS" and a VM installs from it
- [ ] NAS switched off: Server → Network storage shows it offline within seconds (dashboard doesn't hang); on again → Connect
- [ ] iSCSI: Find disks lists the target; Connect without Format on a used disk asks to confirm; with Format → pool p8s-san online
- [ ] Create a VM on san → boots, snapshot, backup, extra disk all work; `zfs list p8s-san/vms` shows its disks
- [ ] Reboot the server: the SAN VM starts by itself (pool imported before libvirt)
- [ ] Remove is refused while the VM is on it; after deleting the VM it disconnects
- [ ] Reinstall test: remove, add again without Format → "taken over", the VM disk is still in `zfs list`

## 16. Logo and boot screens

- [ ] New ISO (`sudo bash iso/build-iso.sh`): BIOS boot menu shows the P8S logo with 3 entries; UEFI boot (laptop) shows the logo too, menu below it
- [ ] Installed server, after `sudo bash update.sh` and a reboot: GRUB menu on the blue P8S background, then the logo splash while it boots (Esc shows the messages)
- [ ] Console login screen and SSH login show the ASCII P8S OS logo and the dashboard address
- [ ] Dashboard: P8S tab icon, logo on the sign-in page (blue in light mode, white in dark), icon in the top bar; "Add to home screen" on a phone uses the P8S icon
- [ ] `sudo touch /etc/p8s/no-splash && sudo bash update.sh` + reboot → plain boot messages again

## 17. Live moves (needs two servers in a cluster)

- [ ] After update on both: `ss -ltn | grep 16509` shows libvirt on the 10.250.0.x address only; `nft list table inet p8s_migrate` exists
- [ ] Linux VM running a `ping 8.8.8.8` and an SSH session: Move while running → job shows % → done; ping lost at most 1-2 replies, SSH still open (home-network VM)
- [ ] Same with a VM with an extra disk; data on vdb is there after the move
- [ ] Different CPUs (laptop + another PC): move refused with "the CPUs differ"; `p8 cpu NAME compatible`, shut down + start, then the live move works
- [ ] Pull the network cable mid-move: job fails, VM still runs on the old server, `zfs list` on the new one has no leftover p8s-NAME-disk*
- [ ] Worker → worker live move (through the master)

## 18. Incremental backups and restore tests

- [ ] `p8 test` 6c: "second backup stores only the changes", "restore test passed", one kept backup snapshot
- [ ] Back up a VM twice: the second file is much smaller (`p8 backups`: KIND changes)
- [ ] Restore the newest ("changes") backup as a copy: it boots with the latest files
- [ ] Delete the middle backup of a chain: refused with "delete the newer backups first"
- [ ] Daily schedule, keep 3, for 10 days: never fewer than the chain needs; a full one every 7th
- [ ] Damage a backup file (`truncate -s 1M …zfs.zst`) → Test fails with a clear message, and the alert/job shows it
- [ ] Off-site: a "changes" backup brought back from the NAS brings its chain with it

## 19. No single master (needs 3 servers; VMs in VirtualBox on the laptops work)

- [ ] After update: `p8 cluster` on every server shows the same term and "a new master is chosen automatically"
- [ ] Switch the master off: within ~1-2 minutes server n2 says it is the master (`p8 cluster`), its dashboard shows all servers, sign in with an account made on the old master works
- [ ] A protected (HA) VM of the dead master starts on its copy a few minutes later
- [ ] Switch the old master on: it becomes a worker of n2 (`p8 cluster`), its old HA VM stays off
- [ ] Pull the network cable of the master only: it pauses HA ("can't reach most of the cluster"), its protected VMs go off; the other two elect a new master
- [ ] Two servers: master off → worker shows "hasn't answered" + Make this server the master; `p8 cluster promote` works

## 20. Installer: system on ZFS with a RAID choice (VirtualBox VM first, see docs/installer.md)

- [ ] `make-iso.sh` builds `p8sos-installer-*.iso` (first time 30-60 min); boot menu says "Install P8S OS" with the logo (UEFI and BIOS)
- [ ] The installer starts by itself on the first screen; *Show the disks* lists the VM's disks; the ISO/CD itself is not offered
- [ ] VM with Secure Boot on (VirtualBox 7: Settings → System → Secure Boot): the installer says to turn it off
- [ ] Two 20 GB disks, **mirror**, UEFI: summary lists both as ERASED; typing anything but ERASE changes nothing
- [ ] A disk with data on it (format one in another VM first): shows `HAS DATA ...` and asks a second time
- [ ] After the restart: first-start setup runs, login screen shows the address; `zpool status` → `tank` and `bpool` mirrors, ONLINE; `p8 test` passes
- [ ] `cat /etc/p8s/installed.json` shows the layout; `ls /boot/efi/EFI` has `p8s` and `BOOT`; `sudo p8s-esp-sync` says 1 spare updated
- [ ] Remove disk 1, start: boots from disk 2, `zpool status` DEGRADED, VMs start; put it back → resilvers
- [ ] Same with EFI off (BIOS): boots from either disk
- [ ] 4 disks **raidz2** and **raid10**: `zpool status` shows the right layout; 3 disks only offer mirror / RAID-Z1
- [ ] **ext4** on one disk: `/` is ext4 (`findmnt /`), VMs in `tank` on partition 4
- [ ] Static address + DNS chosen in the installer: works after the restart (`ip a`, `ping deb.debian.org`)
- [ ] Encrypt VM disks: `zfs get encryption tank/p8s` → aes-256-gcm after the first start
- [ ] `ANSWERS=... make-iso.sh` → the `-auto` ISO installs with no questions; `p8s-install --answers a.json --dry-run` lists every command
- [ ] `sudo apt upgrade` with a new kernel: still boots (ZFS rebuilt by DKMS, GRUB finds the ZFS system)
- [ ] Laptop: the 1 TB hard disk shows `HAS DATA` — **don't tick it**; install on the SSD only

## 21. Ceph (3 servers in a cluster, each with an empty disk; VirtualBox VMs with a second 20 GB disk work)

- [ ] Server → Ceph on the master: every server listed; Install Ceph on each (a few minutes; ⏳ then ✔)
- [ ] An install that failed before (dpkg error 1, from the old p8sd sandbox) works on the next **Install Ceph** after `update.sh`: it finishes the half-done packages first; a real failure names the package (`dpkg: error processing package …`)
- [ ] **Set up Ceph** on the master works first time (no "monmap: Permission denied"); after a failed try, pressing it again works (nothing left half set up)
- [ ] Set up Ceph with the LAN network (pre-filled): health shows, 1 monitor, manager up; the other servers show "installed" and get the configuration (`ls /etc/ceph` on them)
- [ ] Add monitor on two other servers → monitors 3/3; Add manager on one → `p8 ceph` shows it
- [ ] Each server's empty disk is offered ("Give to Ceph"); a disk with data or the system disk is not; after giving: osd.N up
- [ ] Create the VM pool (automatic = 3 copies); health OK after a minute
- [ ] New VM with Storage **ceph**: starts, `lsblk` on the server shows /dev/rbd0, `cat /etc/p8s/rbd-maps` lists it; snapshot + rollback work; add a data disk
- [ ] Move it shut down to another server: done in seconds ("no disk copied"); starts there
- [ ] Move it while running: only memory moves (seconds); keeps running; the old server's `rbd showmapped` no longer lists it
- [ ] Protect it (HA), switch its server off: it starts on another server from the same disk (no data lost); switch the old server on: its old definition is gone, the disk still there
- [ ] Reboot a server with Ceph VMs on autostart: they start (p8s-rbd opened their disks first)
- [ ] Delete the VM: `rbd ls p8s-vms` no longer lists its disks
- [ ] Backup of a Ceph VM: works (a disk-image backup, "from Ceph"); Verify passes; restore over it and as a new VM (lands on Ceph); off-site copy of it works
- [ ] Switch one Ceph server off: alert "Ceph needs attention", VMs on Ceph keep running; back on → healthy
- [ ] Remove a disk: first press = taken out (data moves), second press when healthy = removed and wiped
- [ ] `p8 ceph`, `p8 ceph osd n2 /dev/sdb`, `p8 create x --storage ceph` work from the command line

## 22. Clones and templates from VMs

- [ ] Manage → Options → Clone `web2` of a running `web1` with a data disk: job finishes, web2 has both disks and web1's files, its own IP; `ssh` host key differs from web1's
- [ ] Both run at the same time without an IP clash (`p8 list`); deleting web2 leaves web1 untouched
- [ ] Clone a VM installed from an ISO: exact copy (note in the job result)
- [ ] Make a template `web-base` from a running VM with the guest agent: VM is cleaned and shut down; it starts again fine
- [ ] New VM → "Made from your VMs" → web-base: made in seconds, has the packages of the original, own hostname and IP
- [ ] Two VMs from web-base at once: different IPs (DHCP by MAC)
- [ ] Without a guest agent: making a template from the running VM fails with a clear message; shut down: works ("copied as it is")
- [ ] Delete the template while a VM from it exists: refused; after deleting that VM: works
- [ ] Cluster: template made on n2 shows "on n2" in New VM; a VM from it is created on n2
- [ ] `p8 clone`, `p8 template-from`, `p8 templates`, `p8 templates rm` work

## 23. Administrator Guide (web) and installer error screen

- [ ] Dashboard header **Help** (and sidebar Help → Administrator Guide) opens `https://<server>:8006/docs/` in a new tab: contents page with 16 chapters
- [ ] Open a chapter: sidebar shows where you are, "On this page" links jump to sections, ← / → links at the bottom go to the next chapter
- [ ] 12. Cluster shows the HA failover picture; dark mode (system setting) is readable
- [ ] Search "ceph": headings first, then chapters mentioning it; Esc clears
- [ ] Phone width: ☰ opens the chapter list; tables scroll sideways
- [ ] `/docs/` works without signing in; "← Dashboard" goes back
- [ ] Static site: `go run ./cmd/p8s-docs -src docs/guide dist/docs-site` → open `dist/docs-site/index.html` from disk: same pages, links work
- [ ] Installer finishes all steps; `cat /etc/hostid` on the new system equals the installer's; first start imports tank and bpool without "previously in use from another system"
- [ ] Installer: make a step fail (e.g. Command line → `zpool create busy ...` on a chosen disk first): the error screen shows the failed step's command and its last lines before the menu

## 24. More OS and container templates

- [ ] Server → OS templates → **+ Add another OS**: name `opensuse-15.6`, a Leap 15.6 "Minimal-VM Cloud" `.qcow2` link, login `opensuse`, checksum `.sha256` link → task log shows "checksum OK", the dashboard restarts, Create VM lists it; a VM from it boots and takes the SSH key
- [ ] Same with an `.xz` image (Kali cloud image) — it is unpacked first
- [ ] Wrong checksum → task fails with CHECKSUM MISMATCH and nothing is added; no checksum → asks first, log says "not checked"
- [ ] Name of an existing template, `ftp://` address, bad login → clear error, nothing starts
- [ ] Container templates → **+ Add another**: the list shows every image (not the added ones); add `kali/current` → task log, then + Create CT offers "Kali current"; it is listed with the others afterwards
- [ ] `p8 templates add-url …`, `p8 ct templates available`, `p8 ct templates add kali/current` work
- [ ] With no VM running, wait 5 minutes: `systemctl is-active libvirtd p8sd` both active (libvirtd no longer quits when idle); `sudo systemctl restart libvirtd` → p8sd keeps running

## 25. Renaming and removing servers

- [ ] Server page → **Rename** → `p8s-vm1`: task log, dashboard reconnects, header and login prompt (after re-login) say p8s-vm1; `sudo` shows no "unable to resolve host"
- [ ] In a cluster: rename the master from its Server page → the Cluster list shows the new name within ~15 s
- [ ] On the master, **Rename** on a worker's row → the worker renames itself; the row shows the new name after a few seconds
- [ ] **Remove** asks first; afterwards the worker's own dashboard says "stands alone" (it was told); its VMs still run there
- [ ] Remove a worker that is switched off: removed on the master; when it is back its dashboard still says worker → **Leave the cluster** there works
- [ ] Add the removed server again with + Add a server: joins as before
- [ ] `p8 rename`, `p8 cluster rename n2 NAME`, `p8 cluster remove n2` work
- [ ] Two servers with the same name: the master's Find servers list says "same name… rename it first" (no Add); a join command is refused with the same advice
- [ ] Fresh installs get different default names (p8s-xxxx)
- [ ] Just after Accept the worker shows "connecting…", then online — never a red error
- [ ] Pull the worker's cable for 20 s: it stays online; for 60 s+: offline with a plain-words reason; plug in: online again
- [ ] Send an invitation, restart p8sd on the new server (`sudo systemctl restart p8sd`): the invitation is still there

## 26. Several network cards per VM

- [ ] Running Debian VM → Manage → Network & firewall → Add network card (private network): plugged in at once; `ip -br a` in the VM shows a new interface with a 192.168.122.x address (or `sudo dhclient <it>`)
- [ ] Add one on the home network and one on VLAN 20 (switch carrying it tagged): each gets an address from that network
- [ ] Firewall on, allow only SSH: port 80 is blocked on every card, SSH works on every card
- [ ] Remove a card while it runs: gone from the VM and the list; the first card can't be removed
- [ ] Clone the VM: the copy's cards all have new MACs (`p8 nics copy`)
- [ ] Move it (shut down) to another server: the cards come along; one whose network isn't there lands on the private network with a note
- [ ] Windows VM: second card shows up in Windows (e1000e), gets DHCP
- [ ] Customer account: no network-card section (staff only); API answers 403
- [ ] `p8 nics`, `p8 nic-add web1 lan --vlan 20`, `p8 nic-rm web1 MAC` work

## 27. Server page and the server tree

- [ ] Click the server in the sidebar: the Server page fills the content area (no narrow window) with tabs Summary · Storage · Templates · Network · Cluster · Tasks; sections are cards, two per row on a wide screen, one on a phone
- [ ] The tab you used last is opened again next time; "Datacenter" in the breadcrumb goes back
- [ ] Rename / Shell buttons and the "task running" banner still work; an empty section (e.g. Ceph not set up, HA off) doesn't leave an empty card
- [ ] Cluster of 2: the sidebar shows the master with "master" and its VMs, and **every** worker with "worker" and a dot — also a worker with no VMs (p8s-vm1)
- [ ] Stop p8sd on the worker: within about 30 s its dot turns red (offline); start it: green again (amber "connecting…" right after joining)
- [ ] Click a worker in the sidebar: the Server page opens on Cluster (Shell / Rename / Remove for it)
- [ ] Standalone server: no extra entries, sidebar as before

## 28. Memory snapshots (snapshot with RAM)

- [ ] Running Debian VM: open an SSH session and run `top`; Manage → Snapshots → tick **Include RAM** → Take snapshot: the VM pauses a few seconds, the list shows "with RAM" and the memory size
- [ ] Change something in the VM (create a file, close `top`); **Roll back** while it runs: the VM is back with `top` still running and the file gone, without a reboot (`uptime` continues from before)
- [ ] Windows VM: same with Notepad open
- [ ] Stopped VM: the RAM box is greyed out; a normal snapshot still rolls back only when stopped
- [ ] Delete the snapshot: its file in /var/lib/p8s/vmstate/p8s-NAME/ is gone; delete the VM: the folder is gone
- [ ] Not enough free space (fill the disk or use a VM with more RAM than free space): clear refusal, the VM keeps running
- [ ] `p8 snap web1 test --ram`, `p8 snaps web1` (RAM column), `p8 rollback web1 test`
- [ ] Worker server: the same from the master's dashboard

## 29. PCI / GPU / USB passthrough (laptop: NVIDIA GTX 1650 + a USB stick)

- [ ] Manage → Hardware → PCI and USB devices on a VM: IOMMU "off" note with **Turn on IOMMU**; press it, reboot the server, the note is gone (`p8 iommu status`; else VT-d in the BIOS)
- [ ] The list groups the devices (graphics, network, disk controllers, USB); the laptop's own Intel graphics and its network card show ⚠ and ask twice; bridges aren't listed
- [ ] Shut a Windows/Linux VM down; give it the NVIDIA card: its sound part (same IOMMU group) comes along; start: in the VM `lspci` shows it, the NVIDIA driver installs; stop: `lspci -k` on the server shows the host driver again
- [ ] While the VM runs: PCI picks are greyed out ("shut the VM down first"); **Take away** only when stopped
- [ ] Plug in a USB stick; give it to a running VM: it appears in the VM at once; take it away: gone at once
- [ ] A USB device that isn't plugged in: clear note; the VM won't start until it is plugged in or taken away
- [ ] The same device for a second VM: refused, names the VM that has it
- [ ] VM with a device: Move / Move while running refused with the reason; clone has no devices; Include RAM snapshot refused
- [ ] Operator: sees the list, can't give devices (admin only); customer: no section
- [ ] `p8 devices web1`, `p8 dev-add web1 usb 0781:5567`, `p8 dev-rm web1 usb 0781:5567`, `sudo p8 iommu on`
- [ ] VM on the worker (p8s-vm1): its own devices are listed (from that server)

## 30. Speed limits, hot-plug, Prometheus

- [ ] Manage → Hardware → Speed limits on a running VM: write 50 MB/s → inside `dd if=/dev/zero of=t bs=1M count=500 oflag=direct` shows ~50 MB/s at once; empty the field + Save: full speed again
- [ ] Network out 20 Mbit/s: `iperf3`/a download from the VM tops out near 20 Mbit/s; the limit stays after a restart
- [ ] Customer: limit fields greyed out, API `PUT /limits` → 403
- [ ] Hot-plug: shut the VM down, tick CPU + RAM hot-plug, Save (empty = twice now), start; Resize to 4 CPUs / more RAM while it runs: Apply works without a restart; inside `nproc` and `free -m` show the new values (Debian onlines them by itself)
- [ ] Lowering RAM or growing the disk while it runs: clear refusal; going above the ceiling: refusal naming the ceiling
- [ ] Windows Server VM: more CPUs while running appear in Task Manager
- [ ] `p8 hotplug web1 on cpus=8`, `p8 resize web1 cpus=6` (no shutdown asked), `p8 limits web1 write=100`, `p8 limits web1 off`
- [ ] Prometheus: `curl -k -H "Authorization: Bearer $(cat ~/.p8s-token)" https://127.0.0.1:8006/api/v1/prometheus` lists `p8s_up 1`, `p8s_host_*` and one `p8s_vm_up` per VM (master: also the worker's VMs); add it to Prometheus/Grafana and graph `p8s_vm_cpu_percent`

## 31. Scoped API tokens and sharing a VM

- [ ] Your name → API tokens: make one **Read-only**: `curl -k -H "Authorization: Bearer p8sk_…" https://SERVER:8006/api/v1/vms` works, `-X POST …/vms/web1/stop` → 403 "read-only"
- [ ] A token for **only web1**: `GET /vms` lists only web1; `GET /vms/db1` and `GET /users` → 403
- [ ] **Valid for 1 day**: works now; the list shows "until …"; after the date → 401
- [ ] Customer alice shares her VM with bob as **user** (Manage → Options → Shared with): bob sees it in his list with a note on its Summary, can start/stop/console/snapshot, gets 403 on Delete / Resize / adding disks, and can't share it further
- [ ] alice shares with carol as **viewer**: carol sees it, every button that changes something answers 403
- [ ] Stop sharing: bob doesn't see it any more; deleting the VM removes its shares
- [ ] `p8 share shop bob user`, `p8 shares shop`, `p8 share shop bob off`
- [ ] Cluster: share a VM on the worker from the master: works the same

## 32. Let's Encrypt certificate

- [ ] A domain (e.g. a free duckdns.org name) points to your public IP; forward TCP 80 to the laptop; Server page → Network → HTTPS certificate → Get certificate: the task log shows certbot, then "✔"; the card shows the domain and "valid 89 more days"
- [ ] `https://DOMAIN:8006` (port 8006 forwarded too, or from inside with the name in the hosts file) opens with no browser warning; `https://192.168.0.106:8006` still shows the old warning (own certificate)
- [ ] The cluster keeps working afterwards (worker online; Cluster page)
- [ ] Cloudflare way: domain at Cloudflare, token with DNS edit → works without any forwarded port
- [ ] `sudo certbot renew --dry-run` succeeds; `systemctl list-timers certbot.timer` is active
- [ ] **Remove** / `p8 cert off`: back to the own certificate within a minute; `p8 cert` shows the state
- [ ] Wrong domain (not pointing here): the task fails with certbot's explanation; nothing changes

## 33. Move disks between storages, Ceph backups

- [ ] VM on local ZFS (shut down), Hardware → Disk storage → ceph → Move disks: the job copies the disks (Tasks bar), the VM starts from Ceph (`p8 show NAME` storage ceph), `zfs list` has no old zvol any more
- [ ] With snapshots: refused until "delete its snapshots" is ticked
- [ ] Back to local; also a VM with an extra disk (both move); a NAS/SAN storage if you have one
- [ ] `p8 storage web1 ceph` (asks to shut it down, starts it again)
- [ ] Ceph VM → Back up now: "disk image(s) from Ceph"; Verify passes; Restore over it; Copy as new VM: it lands on Ceph
- [ ] A Ceph backup restored on a server without Ceph: it lands on local ZFS and boots

## 34. File-level restore

- [ ] Backups → **Files** on a Debian VM's backup: "Opening…", then tabs for its filesystems (root ext4, EFI vfat); browse /etc, Download /etc/hostname: the file is right
- [ ] An incremental backup (not the first): opens too (its chain is restored)
- [ ] Windows VM backup: the NTFS partition opens; download a file from C:\Users
- [ ] A Ceph VM's backup (disk image) opens too
- [ ] Symbolic links (e.g. /etc/os-release → ../usr/lib/os-release) don't lead out of the backup (no host files)
- [ ] Close: `zfs list | grep p8s-browse` and `losetup -a` show nothing left; without Close it closes after an hour; after a p8sd restart leftovers are removed
- [ ] Customer: only their own VMs' backups; another user's session id → 404
- [ ] `p8 files web1 ID /etc`, `p8 files web1 ID /etc/hostname` (saved in the current folder)

## 35. OVA import

- [ ] Export a VM from VirtualBox (File → Export Appliance, OVF 2.0, .ova); upload it under Storage → Import VMs: Check shows its name, CPUs, RAM, disks; Import: it boots
- [ ] An OVA from a VMware appliance (e.g. a vendor's virtual appliance): firmware EFI and vmxnet3 come along
- [ ] An OVA with two disks: the second is a data disk (Hardware → Extra disks)
- [ ] A Windows OVA: SATA disk + e1000e, boots; then install virtio drivers
- [ ] The unpacked .vmdk files are gone from /var/lib/p8s/import afterwards (only the .ova stays until you delete it)

## 36. Datacenter firewall and IP sets

- [ ] Settings → Datacenter firewall: an IP set `office: <your PC's IP>`; a VM's firewall on with rule tcp 22 from `+office`: SSH from your PC works, from another PC (or phone hotspot) not
- [ ] Allowed rule for every VM: tcp 9100 from `+office` → every VM with its firewall on accepts it (`nc -zv VM 9100` with a listener)
- [ ] Blocked: your phone's IP → the phone can't reach any VM (ping / web), and VMs can't reach the phone; remove it: works again
- [ ] Saved on the master: `sudo nft list table bridge p8s_fw` on the worker shows the same sets and blocks
- [ ] A VM rule using a set that was deleted: the rule is skipped (`# skipped` in nft), nothing breaks
- [ ] Operator/auditor: can see it, can't save (admin only)

## 37. Witness (two-server tie-breaker)

Needs a third always-on Linux box (a Raspberry Pi, another PC, a small VM on your Windows PC with bridged network — not p8s-node or p8s-vm1).

- [ ] Server → Cluster → Add a witness… → download the right p8s-witness; on the box `sudo ./p8s-witness-* install` prints address, key, fingerprint; `systemctl status p8s-witness` is running
- [ ] Wrong key or wrong fingerprint in the dashboard → refused with a clear message; right values → "Witness … online", `p8 cluster` says "2 servers + a witness"
- [ ] `p8 cluster witness` on the worker also shows it online (the setting travelled in the book)
- [ ] Unplug the worker's network from the master only (or `nft`-block 10.250.0.1 on the worker): after 2 minutes the worker is still a worker (the witness still hears the master)
- [ ] Switch the master (p8s-node) off: within about a minute p8s-vm1 says it is the master (term +1); an HA-protected VM starts on it
- [ ] Switch p8s-node back on: it becomes a worker of p8s-vm1 by itself
- [ ] Stop the witness (`systemctl stop p8s-witness`): the cluster keeps working; the dashboard shows "not answering"; start it again: online
- [ ] Witness down and the master switched off: the worker does NOT take over (only 1 of 3 votes) — `p8 cluster promote` still works by hand
- [ ] Remove witness → `p8 cluster` back to "2 servers", no automatic takeover

## 38. Virtual networks (SDN)

- [ ] Settings → Virtual networks → `office` with subnet `10.60.0.0/24` → Add: "on every server"; `p8 vnets` on p8s-vm1 lists it too
- [ ] On both servers: `ip -br link | grep vx` shows `vxb10000` and `vxl10000` UP; `bridge fdb show dev vxl10000 | grep 00:00:00` points at the other server's 10.250.0.x
- [ ] On the master: `systemctl status p8s-vnet-office` running; `ip -br addr show vxb10000` = 10.60.0.1/24
- [ ] Create VM `a` on p8s-node and VM `b` on p8s-vm1, both Network → Virtual network — office: each gets a 10.60.0.x address (`p8 show`), `ip link` inside shows mtu 1230
- [ ] In `a`: `ping` b's 10.60.0.x works; `ping 1.1.1.1` and `apt update` work (internet through the master)
- [ ] In `b`: `ping 192.168.0.1` (your router) fails — fenced like the private network; `ssh 10.60.0.1` (the master) fails, DNS (`dig debian.org @10.60.0.1`) works
- [ ] Live-move `b` to p8s-node: it keeps its address and the ping from `a` continues
- [ ] Add a second card on a new switch-only network `db` to both VMs; set 10.70.0.1/24 and .2/24 by hand: they ping each other across the servers
- [ ] A network without internet (untick "internet"): VMs get addresses but `ping 1.1.1.1` fails
- [ ] Remove `office` while VMs use it → refused with their names; move them off → removed on both servers (`ip -br link | grep vx` empty for it)
- [ ] Reboot p8s-vm1 with a VM on `office` set to start at boot: it starts (bridge made at boot by p8s-vlans.service) and gets its address again
- [ ] From another PC on the home network, a VXLAN packet to the server's LAN IP (UDP 4789) is dropped (`nft list table inet p8s_sdn` shows the rule)

## 39. Disks view

- [ ] Server page → Storage → Disks on p8s-node: the NVMe/system disk shows *system*, the 1 TB HDD `sda` shows *data* with its partitions; SMART shows temperature / age (on p8s-vm1, a VirtualBox disk: "SMART: n/a")
- [ ] **Protect `sda` first** → 🔒 protected; it has no Wipe button and `p8 disk-wipe sda` is refused
- [ ] The system disk and the disk of pool tank have no Wipe button; the API refuses them too (`p8 disk-wipe <system disk>` → "can't be wiped")
- [ ] On p8s-vm1: add two empty 20 GB disks in VirtualBox → both show *empty*; Make pool `fast` mirror of both → `zpool status p8s-fast` ONLINE; Create VM → Storage offers `fast`; the VM's disk is `p8s-fast/vms/NAME-disk0`
- [ ] Reboot p8s-vm1: the pool is back (`zpool list`), the VM on it starts
- [ ] Destroy `fast` while the VM is on it → refused; delete or move the VM, Destroy (type the name) → disks show *empty* again
- [ ] A disk with old data (e.g. one that was a Ceph OSD after removal, or formatted elsewhere): Wipe asks to type its name, then it shows *empty*
- [ ] On the master, the Server picker → p8s-vm1 shows p8s-vm1's disks; actions there work
- [ ] Operator account: sees the Disks list, no buttons; API changes → 403

## 40. Graphical installer and pre-built p8sd (new ISO)

Build a new ISO on the laptop: `sudo bash iso/build-installer.sh ~/P8SOS/dist` (now runs every unit test and builds p8sd first). Test in VirtualBox (EFI on, two disks, bridged), not on the laptop.

- [ ] Boot menu shows **Install P8S OS** and **Install P8S OS (text mode, safe graphics)**
- [ ] Install P8S OS → after ~30 s a full-screen page with the P8S header; the mouse works; steps Welcome → Disks → … → Check
- [ ] Disks: the ISO disk is greyed out ("not offered"); a disk with data shows red HAS DATA; Look again finds a disk added while running
- [ ] Wrong password twice / too short → message, stays on the page; Check shows the summary and the red warning for disks with data; typing `erase` (small letters) is refused
- [ ] Type ERASE → progress bar with step names → "P8S OS is installed" → Restart now reboots
- [ ] First start: `journalctl -u p8s-firstboot` says "pre-built on the installer ISO (no build needed)"; it is done within a few minutes; `p8sd version` and the dashboard header show the version
- [ ] From another PC during install: `https://<VM address>:8006` → PIN page; wrong PIN refused (5 wrong → wait a minute); right PIN → same wizard. Start an install there while the screen shows the wizard: the screen's Install says one is already running
- [ ] Text mode entry: the welcome menu shows the `https://…:8006 (PIN ……)` line; text install still works end to end
- [ ] Make the graphical screen fail (VirtualBox: Graphics Controller = VBoxVGA, or boot with `nomodeset` added to the main entry): the text installer appears on its own
- [ ] Failed install (e.g. remove a disk in VirtualBox mid-install): the page shows "The install stopped", the failed step, Show the whole log, Back to the start works
- [ ] Installed system has no Firefox or cage (`dpkg -l firefox-esr` → not installed)
- [ ] After `git pull && sudo bash update.sh` with a changed source, update.sh builds p8sd with Go as before

## 41. High availability and moves of VMs with data disks

- [ ] VM with a data disk (`p8 disk-add web1 20`, put a file on it): HA on → the copy on the other server has both disks (`zfs list -r tank/p8s/vms/replica` shows `p8s-web1-disk0` and `-disk1`)
- [ ] Add a second data disk while HA is on: the next copy includes it (full once), later copies are small
- [ ] Switch the VM's server off: the VM starts on the other one with both data disks and the file
- [ ] Shut it down, Move to the other server: both disks arrive (`p8 disks web1`), the file is there
- [ ] Remove the data disk again: the next copy drops `-disk1` on the other server

## 42. Live moves of UEFI / Windows 11 / SAN VMs

- [ ] A UEFI Linux VM (or the Windows 11 VM) running: Move → live → it keeps running on the other server; reboot it there: it boots (UEFI boot entries kept); Windows 11: BitLocker / TPM still fine (no recovery key prompt)
- [ ] A VM on the SAN disk (iSCSI) running: live move → it runs on the other server, its disk is now on local storage (`p8 show`)

## 43. Restore a backup onto another storage

- [ ] Backups → a backup → Copy: a storage picker appears (local / SAN pool / ceph); pick the SAN pool (or ceph) → the new VM starts and `p8 show NAME` says that storage
- [ ] `P8_STORAGE=fast p8 restore web1 ID web1-b` puts the copy on the pool `fast` (Disks view)
- [ ] Restoring over the VM itself with a storage is refused ("pick a storage only for a new VM")

## 44. Templates from Ceph VMs and with data disks; clone onto another storage

- [ ] A VM with a data disk (files on it): Make a template → the job says "with 1 data disk"; New VM from it → it has the data disk with the files
- [ ] A VM on Ceph: Make a template → works; new VMs from it (local or Ceph) boot
- [ ] Delete the template: refused while VMs from it exist; after deleting them: gone (`zfs list -r tank/p8s/templates`)
- [ ] Clone with the storage list → ceph (or the SAN pool): the copy's disks are on that storage (`p8 show`); `p8 clone web1 web2 --storage fast`

## 45. Containers across the cluster

- [ ] Master: New container → the Server list shows the workers; pick p8s-vm1 → the container runs there (`lxc-ls` on the worker), the list says "on p8s-vm1"
- [ ] Automatic with a template only the worker has → it lands on the worker; a template nobody has → clear error
- [ ] A container with a public IP → always made on the master
- [ ] From the master on a worker's container: start/stop, resize, snapshot + rollback, backup + restore, set password, Terminal (types and resizes)
- [ ] Manage → Move to server (stopped) → job finishes; it's gone from the old server, there on the new one with its files; "start after" starts it
- [ ] Move a running one or one with a public IP → refused with a clear message
- [ ] `p8 ct-move NAME SERVER --start` works the same
- [ ] Network `vnet:NAME`: two containers (or a container and a VM) on different servers ping each other; `ip link` in the container shows mtu 1230
- [ ] Auditor accounts can't use the move or cluster ct endpoints

## 46. Incremental backups of Ceph VMs

- [ ] A VM on Ceph: Back up → "N disk image(s) from Ceph"; `rbd snap ls p8s-vms/p8s-NAME-disk0` shows one `p8s-backup-…` snapshot
- [ ] Write a file in the VM, back up again → "changes since …", much smaller; the backup list says "changes since"; still only the newest snapshot on the image
- [ ] Restore the newest one as a new VM (on Ceph, and on local) → boots, the file is there; restore over the VM (stopped) → same
- [ ] Restore test (Verify) on the incremental one passes; Browse files of it works
- [ ] Deleting the full backup while newer ones build on it → refused
- [ ] Roll back to a snapshot, back up → a full one again
- [ ] Copy off-site and fetch back (delete local, fetch, restore) → works, the chain comes along

## 47. Proxmox imports: VMs with several disks, containers

- [ ] A Proxmox VM with 2 disks (scsi0 + scsi1 with files) → vzdump → Import: Check shows "1 data disk(s) come along: scsi1"; the imported VM has the data disk with the files (`p8 show`)
- [ ] A Proxmox VM with a CD-ROM (ide2) → not listed as a disk
- [ ] A Proxmox container (Debian, unprivileged, fixed IP) → `vzdump 105 --compress zstd --mode stop` → copy to /var/lib/p8s/import → Check: "Proxmox container backup", CPUs/RAM/disk from it, the fixed-address warning
- [ ] Import container → it appears on the Containers page, starts, gets a DHCP address, files are there; `ls -ln /var/lib/p8s/ct/NAME/root` shows ids 100000+
- [ ] A privileged Proxmox container → warning; it still starts (unprivileged)
- [ ] A container with a mount point (backup=1) → its files are in the container; the disk is made bigger
- [ ] Import with a name a VM or container already has → refused

## 48. One ISO library for the cluster

- [ ] Master: New VM → from ISO (debian netinst), Server → p8s-vm1 → it is created there and boots the installer (console works)
- [ ] On p8s-vm1 `ls /var/lib/p8s/isos` shows the copy; a second ISO VM on p8s-vm1 starts at once (no copy)
- [ ] Windows ISO + virtio-win drivers ISO on the worker → both copied, installer sees the drivers CD
- [ ] Automatic placement with an ISO → may pick the worker (no longer refused)
- [ ] Delete the ISO on the master (no VM using it) → gone on the worker too; with a worker VM still having it inserted → the worker keeps its copy

## 49. Public IPs: extra addresses, IPv6, changing the address

- [ ] Pool with IPv6 (`p8 ippool add dc6 … ipv6=2001:db8:5::/64 gw6=fe80::1`, or IPv6… on an existing pool) → a new public VM gets an IPv6 address (`ip -6 a` inside, ping6 the gateway if the network has IPv6)
- [ ] Manage → + Extra IPv4 on a running VM → the address appears inside at once (`ip a`), still there after a reboot; ping it from outside
- [ ] ✕ removes it (gone inside too); + IPv6 on an older VM → set inside
- [ ] Change address (empty) → the VM gets the next free one, the old one is gone inside, SSH on the new one works; the pool list shows it
- [ ] Change to a pool on another bridge while running → refused; shut down → works, the VM's card is on the new bridge, it boots with the new address
- [ ] Containers: the same on a public container (applied at once, after restart too)
- [ ] Anti-spoofing: inside the VM `ip addr add <a free pool address>` and ping out → dropped; IPv6 from an address not its own → dropped
- [ ] `p8 ip web1`, `p8 ip add web1`, `p8 ip change web1 --pool dc2` print the addresses

## 50. VM disks on an NFS share

- [ ] Server → Network storage → Add NAS share, tick "VM disks here too", 100 GB → online, holds "VM disks, backup copies, ISOs"; on the NAS a file `p8s/vmdisks-nas.zpool` (small: sparse); `zpool list p8s-nas`
- [ ] New VM with Storage: nas → boots; snapshot, rollback, backup + restore, data disk, clone all work
- [ ] Move an existing VM to nas (Manage → Storage) and back
- [ ] Reboot the server → share mounted, pool imported before the VMs start (autostart VM on nas comes up); `journalctl -u p8s-storages`
- [ ] Shutdown doesn't hang (pool closed: "nas: pool closed" in the log)
- [ ] Pull the NAS cable for 30 s while a VM writes → the VM pauses and carries on, no disk errors inside
- [ ] Remove the storage with a VM on it → refused; without VMs → pool released, unmounted, the file stays on the NAS
- [ ] Add it again with the same name + VM disks → taken over (its VM disks are there: `zfs list -r p8s-nas`)
- [ ] `p8 storages add-nfs nas2 NAS /export --vm-disks 50`

## 51. Linked clones

- [ ] Manage → Options → Clone with "Linked clone" → done in seconds; `zfs list -o name,origin,used -r tank/p8s/vms` shows the clone with origin `…@p8s-link-…` and almost no space used
- [ ] The clone boots, has its own name/MAC/IP; writing files in it doesn't change the original
- [ ] Manage of the original lists "Linked clones of this VM"; Delete it → refused naming the clone; Move to another server / storage → refused
- [ ] Back up the linked clone and restore it as a new VM → works
- [ ] Make independent (clone shut down) → job copies the disks; `origin` is `-` now; the original can be deleted
- [ ] Delete a linked clone → its `p8s-link-…` snapshot on the original is gone (`zfs list -t snapshot`)
- [ ] On Ceph: Linked clone → made in seconds; `rbd children p8s-vms/p8s-<vm>-disk0@p8s-link-…` lists it; Make independent → `rbd info` shows no parent; deleting the clone removes the snapshot on the original
- [ ] `p8 clone web1 web3 --linked`, `p8 unlink web3`

## 52. Encrypted system, remote unlock, Secure Boot

- [ ] VirtualBox (EFI), installer → mirror → "Encrypt EVERYTHING" + passphrase → summary says so; after the install it asks for the passphrase at boot; wrong one → asks again; right one → boots, first-start setup runs
- [ ] `zfs get encryption,keylocation tank tank/ROOT/debian tank/p8s` → aes-256-gcm / prompt (inherited); `bpool` not encrypted
- [ ] Server → Storage says "Everything is encrypted"; "Show the key" isn't offered
- [ ] `sudo p8 disk-passphrase` → reboot asks for the new one
- [ ] `p8 boot-unlock add "<your key>"` → reboot → from the PC: `ssh -p 2222 root@<ip>`, `zfsunlock`, passphrase → the server boots (static address server too)
- [ ] Answers file with `"encrypt_system": true` and an ext4 layout → refused
- [ ] Secure Boot (VirtualBox 7, EFI, Secure Boot off): `sudo p8 secureboot setup` → one-time password; reboot → MokManager → enroll → boots; Secure Boot on → boots; `p8 secureboot status` all ✔, `mokutil --sb-state` = enabled; VMs start (ZFS module loads)
- [ ] After a kernel update (apt upgrade) with Secure Boot on: still boots (DKMS signed the new module)
- [ ] Secure Boot on but the old entry chosen in the boot menu → refused by the firmware; switching Secure Boot off → old entry boots
- [ ] Dashboard: Server → Storage → Boot security shows the state; "Get ready for Secure Boot" runs as a task (log has the one-time password); on an encrypted system: Add key / Remove, Change passphrase (wrong own password → refused); an operator sees it but has no buttons

## 53. Containers in high availability

- [ ] Cluster of 2 (p8s-node master, p8s-vm1 worker): a container on the worker, running → Manage → High availability shows "Copy to" the master; Protect → first copy in seconds; the box shows "last copy … ✔ Automatic failover"
- [ ] On the master: `zfs list -t all -r tank/p8s/ct-replica` shows the copy (not mounted, one `@p8s-rep-…` snapshot); on the worker `zfs list -t snapshot tank/p8s/ct/NAME` shows the copy points; the container's Snapshots list does NOT show them
- [ ] Write a file in the container; after the next copy (set 1 minute) the copy's used size grows a little (only the change is sent)
- [ ] Manage → Autostart is off while protected; turning it on → refused ("high availability starts this container")
- [ ] Server → High availability lists it as "container", the master's "Copies kept on this server" shows it
- [ ] Power the worker off (VirtualBox: Power off) → after ~3 min the container runs on the master (Containers list: on p8s-node), with its files up to the last copy; alert/log "container NAME started on …"
- [ ] Power the worker on → its old container is switched off and disappears from its list; copies now go master → worker (Server → HA on the worker shows the copy)
- [ ] Unplug the worker's network cable only (master still up) → after 60 s the worker switches the container off (log "cut off … switched off NAME"); plug back → it starts again (no failover happened)
- [ ] Snapshot rollback of a protected container works (the next copy is a full one)
- [ ] Turn off → the copy is deleted (`zfs list tank/p8s/ct-replica` empty), autostart is back as before
- [ ] A container with a public IP: the box says "Not possible: it has a public IP …"

## 54. Moving a running container

- [ ] A running container on p8s-vm1 (write a file in it, e.g. `date > /root/before`) → Manage → Move to server → p8s-node: the job shows "copying the disk while it runs" (container still answers ping), then "shutting it down…", "copying the last changes", "started"
- [ ] Ping it during the move: only a few seconds (plus its shutdown) without answers; it is on p8s-node now with the file, same MAC (same DHCP address on the same network)
- [ ] `zfs list -t snapshot -r tank/p8s/ct` on both servers: no `p8s-move-pre` / `p8s-move` snapshots left
- [ ] `p8 ct-move NAME p8s-vm1` (running) moves it back
- [ ] Cut the move short (unplug the target's cable during the first copy) → job fails, container keeps running on the old server; no half disk left on the target after it is back (or the next move cleans it up)
- [ ] A stopped container still moves as before (start only if ticked); a container with a public IP → refused

## 55. Removing an offline server

- [ ] Worker switched off → master's Server → Cluster shows it offline with a **Remove (offline)** button; one confirmation explains its VMs stay there → removed within a second or two (no long wait)
- [ ] Online worker → **Remove** → it is told to stand alone (its own dashboard shows no cluster); with VMs on it a second confirmation asks "remove it anyway?"
- [ ] On the removed server once it runs again: Server → Cluster → **Leave the cluster** (or `p8 cluster leave`) → stands alone; can be added again with + Add a server
- [ ] `sudo p8 cluster …` (and any `sudo p8 …`) works: it uses the token of the user who ran sudo
- [ ] `p8 cluster remove n2` on an offline server with VMs → refused naming a VM; `p8 cluster remove n2 --force` → removed at once

## 56. Ceph doesn't answer: rescue or remove

- [ ] Ceph on 2 servers with a monitor each, one server off for good → alert "Ceph doesn't answer"; Server → Ceph opens quickly (no long wait) and shows the yellow box with both buttons
- [ ] **Keep Ceph with this server's monitor only** → task done "monitors taken out: …"; `sudo ceph -s` answers, 1 monitor; the alert clears within a few minutes; a VM on Ceph with copies here starts
- [ ] **Remove Ceph from this server** with a VM on Ceph → refused naming the VM
- [ ] Remove (no VMs on Ceph) → `systemctl list-units 'ceph*'` shows nothing running; `/etc/ceph/ceph.conf` gone, files in `/var/lib/ceph/p8s-removed-…`; Server → Ceph offers "Set up Ceph" again; the alert clears
- [ ] Remove with "also wipe" → the disk Ceph had shows as empty again (offered under "Give to Ceph" after setting Ceph up again); **never touches /dev/sda** or any disk Ceph didn't have
- [ ] Reboot after Remove → no Ceph service starts, no errors about ceph-volume
- [ ] `p8 ceph alone`, `p8 ceph remove [--wipe]` ask first and work the same

## 57. No sideways scrolling

- [ ] Server page → every tab (Storage with Ceph and Disks, Templates …) at full screen and with the browser window half-wide: no left-right scroll bar inside any card; long names and messages wrap
- [ ] ISO images, All backups, Settings, Users, Activity, Account dialogs: wider on a big screen, no left-right scroll bar; buttons wrap onto a second line on a narrow window
- [ ] VM / container page → every tab: no left-right scroll bar; CPU type list and its Set button stay on one line
- [ ] Narrow window (800 px): the top bar fits (version text and "Create" words are hidden)
- [ ] Server → Disks and "Give to Ceph" no longer list the VMs' own ZFS volumes (zd16, zd112 …) as disks
- [ ] Ceph that doesn't answer: the card shows the error, then right away what to do (Keep this monitor only / Remove Ceph); no "Next: Give to Ceph" hints meanwhile

## 58. Wide pages, quick dashboard

- [ ] Settings opens in the content area (Datacenter › Settings), its sections as cards: 3 side by side on a 2000 px screen, 2 on a laptop, 1 on a phone; Close / Datacenter goes back
- [ ] Users, Activity, Your account, Backups open as pages too (no small box in the middle); everything works as before (add user, export activity, change password, restore a backup)
- [ ] Ceph that doesn't answer (monitor server off): the VM list, Server page and every other page react at once (before: about 20 s on every refresh); Server → Storage → Ceph card shows the error within ~6 s
- [ ] With Ceph healthy: VMs on Ceph still start, move and show their storage as before

## 59. P8S Mirror (live mirror)

- [ ] Needs 2 servers on one LAN. VM → Options → High availability → How: **Live mirror** → Protect → the box shows "first sync N%", then "in sync: every write is on both servers" (a 20 GB disk: a few minutes)
- [ ] On the other server: `zfs list -t all -r tank/p8s/vms/replica` shows the copy (with `@p8s-mirror-good` only while a sync runs); `systemctl status p8s-mirror-p8s-NAME` runs qemu-storage-daemon on 10.250.0.x:108xx
- [ ] In the VM: `dd if=/dev/urandom of=/root/big bs=1M count=500 conv=fsync` works (a bit slower than without the mirror)
- [ ] Write a file in the VM, `sync`, then pull the power of the VM's server → after ~3 min the VM runs on the other server **with that file** (nothing lost); log "started on … automatically"
- [ ] Power the old server on → its old VM is switched off; the mirror goes the other way (first sync again)
- [ ] Unplug the other server's cable while the VM runs → the VM keeps running (writes wait ~3 s once); state "first sync…" again when it is back
- [ ] Shut the VM down / start it → the mirror waits, then syncs again on start
- [ ] Live move of a mirrored VM → works; the mirror starts again from the new server
- [ ] Turn off → the mirror stops, the copy is deleted, `p8s-mirror-…` service gone
- [ ] Server on another network (tunnel over the internet) → Live mirror refused with a clear message

## 60. P8S Mirror: quick restarts

- [ ] A mirrored VM in sync → log/console: a pause of well under a second once; `zfs list -t snapshot | grep p8s-rep` shows the same snapshot on both servers
- [ ] Reboot the VM from inside (or Shutdown + Start) → the box shows "catching up: only the changes…", then "in sync" within seconds (no "first sync N%"); network ping to the VM shows at most a short hiccup
- [ ] Write 1 GB in the VM, shut it down, start it → catch-up sends about 1 GB, not the whole disk
- [ ] Roll the VM back to an older snapshot (while off), start → a full first sync (the common snapshot is gone)
- [ ] Kill p8sd on the master during a catch-up (`sudo systemctl kill p8sd` at the right moment) → the VM resumes by itself within 60 s

## 61. P8S Mirror in the dashboard

- [ ] On the master of 2 servers: + Create VM → Storage shows **P8S Mirror**; creating a VM with it → toast "… mirrored live to <server>"; the VM's High availability box says Live mirror
- [ ] Standalone server: no P8S Mirror in the Storage list
- [ ] Server → Storage → **Mirror storage** lists mirrored VMs with a green dot when in sync, orange while syncing, red with the error
- [ ] Ceph not set up: its card is folded ("Not set up… show Ceph anyway"); set up: shown as before

## 62. Copying templates to other servers

- [ ] Cluster of 2: make a template from a VM on the worker (VM → Options → Make a template); Server → OS templates → Made from VMs shows it "on <worker>" with a **Copy** button and the master in the list
- [ ] Copy → job "template … copied to <master>"; the list now says "on <worker>, <master>"; `zfs list -t snapshot | grep templates/<name>` on the master shows it
- [ ] + Create VM from it on the master: works; with only the worker having it, a new VM always goes to the worker (the master shows "no copy of template …")
- [ ] A template with a data disk: the copy has `<name>-disk1` too; a VM made from the copy gets the data disk
- [ ] `p8 templates copy NAME` with every server having it → "every online server has … already"; `p8 templates rm NAME --on <server>` deletes only that copy; Delete (every copy) removes them all
- [ ] Copying a built-in template (debian-13) is refused with "add it on each server"

## 63. Server page: Show log from any tab

- [ ] Start a server task (e.g. Storage → Get ready for Secure Boot); on the Storage tab click **Show log** in the orange "… is running" bar → the page switches to the Tasks tab and shows the log (it keeps updating)

## 64. Import VMs button after signing in

- [ ] Open the dashboard in a new tab, sign in with the form, click **Import VMs** in the sidebar → the Import dialog opens (it used to do nothing until the page was reloaded)
- [ ] Upload a small .qcow2 → Check → Import → the job finishes and the VM is listed

## 65. Public IPs on workers

- [ ] Cluster of 2, pool dc1 on bridge br0 (on the master). Worker without br0: + Create VM → Public IP — dc1 → it goes to the master; the placement list says "no bridge br0 for public IP pool dc1" for the worker
- [ ] `sudo p8 lan-setup` on the worker; create another VM (or container) with Public IP — dc1, Server = the worker → it gets the next free address, works from the internet; Settings → Public IP pools on the master shows it as used
- [ ] On the worker's own dashboard: the pool is listed; adding a pool there is refused ("manage them on the master's dashboard")
- [ ] Master → Manage → Public IP addresses → + Extra IPv4 for the worker's VM: added (on the master's pool), set inside the guest
- [ ] Delete that VM: its addresses are free again on the master
- [ ] Stop p8sd on the master: the worker's public VMs keep working; creating a new public VM on the worker fails with "the master … didn't answer"
- [ ] Remove the worker from the cluster: its public VMs keep their addresses (`p8 ippools` on it), the master still shows them used

## 66. Clone onto another server

- [ ] Cluster of 2: VM on the worker → Manage → Options → Clone: the list next to the name offers "on <master>"; pick it, name db2, Start the copy → one job: copying on the worker, copying the disk, removing the original (of the copy), starting; db2 runs on the master, the VM is untouched
- [ ] Picking another server greys out Linked clone and the storage list
- [ ] `p8 clone db1 db3 --on <worker>` (the VM's own server) → an ordinary clone there
- [ ] Unplug the target server during the copy: the job ends with "… moving it to … failed … (it stays on …)" and the copy is on the VM's server

## 67. Grow network storage

- [ ] NAS share with VM disks (e.g. 50 GB): Server → Storage → Network storage → **Grow** → 100 → toast "nas grown: 100 GB for VM disks"; `zpool list p8s-nas` shows ~100G; a running VM on it carries on
- [ ] Grow to more than the share has free → refused with "the share has … GB free"
- [ ] SAN disk: grow the LUN on the SAN (e.g. TrueNAS zvol +20 GB), then **Grow** → the pool shows the new size
- [ ] `p8 storages grow nas 150` works the same

## 68. Separate plans for containers

- [ ] Settings → Plans: add c1 "Container S" 1 CPU 512 MB 10 GB ₹149, For = Containers only → listed with a "containers only" pill; Edit shows the choice again
- [ ] As a customer: + Create CT shows c1 (and plans for both); + Create VM doesn't show c1
- [ ] Create a container on c1 → billed at ₹149/month (Billing shows it)
- [ ] API: POST /vms with plan c1 → 400 "plan Container S is for containers only"; a "VMs only" plan for a container → 400 likewise
- [ ] A container plan with 128 MB RAM is accepted; a VM plan with 128 MB is refused
- [ ] `p8 plan c2 "Container M" 2 1024 20 249 --containers`; `p8 plans` shows "containers only"

## 69. Datacenter → Network

- [ ] Datacenter page (admin): **Network** panel → Check all servers → within a minute one row per server: server's internet ●, guests' internet ●, From outside (the address the internet sees, CGNAT or not, Tailscale / tunnel / VPN set up)
- [ ] Break guest NAT (`sudo nft flush ruleset` on a test server) → Check → "1 problem(s)" with the reason; **Repair** → check again after a minute → ok
- [ ] Cluster: a worker's row shows its own results; an offline worker shows "offline"
- [ ] Home line with Jio/Airtel CGNAT: the From outside column says so; with Tailscale on: "reachable from anywhere through: Tailscale (100.x.y.z)"
- [ ] Operator account: no Network panel

## 70. Moving guests with a public IP

- [ ] Cluster of 2, both with br0: a VM with Public IP — dc1 on the worker, shut down → Manage → Move to server → master → job done, "its public IP … came along"; start it: same address, reachable from outside, `nft list table bridge p8s_fw` on the master pins it
- [ ] The pool still lists the address as used by that VM (not released by the move)
- [ ] Live move of a running VM with a public IP (tick "while it runs") → it keeps running and keeps its address; a ping from outside drops at most one reply
- [ ] A container with a public IP, running → Move to server → moves live, keeps its address
- [ ] Target without br0 → refused with "no bridge br0 for public IP pool dc1"
- [ ] Delete the moved VM → its address is free again

## Add new features here as they are built

(Every new feature adds its own lines above before it counts as done.)
