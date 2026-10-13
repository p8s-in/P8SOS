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

## Add new features here as they are built

(Every new feature adds its own lines above before it counts as done.)
