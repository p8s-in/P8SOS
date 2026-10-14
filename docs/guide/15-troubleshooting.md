# 15. Troubleshooting
Summary: What to do when installing, VMs, storage or the cluster go wrong.

**First step for anything:** `p8 status`, then `p8 report` (saves `p8s-report.txt` to send along) and `p8 logs` to follow p8sd.

## Installing and booting

| What you see | What to do |
| --- | --- |
| Machine doesn't boot from the USB stick | Firmware: Secure Boot off, USB boot on; Rufus in **DD Image** mode |
| ZFS module errors | Secure Boot must be **off** |
| Installer: *the answers must say "erase": true* | Fixed in newer ISOs: build the ISO from the latest code |
| Installer stops at *Closing the pools: exit status 1* (last step) | Fixed in newer ISOs: rebuild and reinstall. If it still appears, the screen now lists what keeps the pool busy — photograph it |
| VirtualBox: *The virtual machine failed to boot* | The install didn't finish, so the disks have no boot loader. Put the ISO back in the drive and install again |
| *first-boot setup FAILED* on the login screen | `sudo tail -50 /var/log/p8s-firstboot.log`; reboot retries; needs internet |
| `No hardware virtualization` | Firmware: enable VT-x / AMD-V. VirtualBox: Nested VT-x on, Hyper-V off in Windows |
| VirtualBox: Nested VT-x greyed out | Windows: Memory integrity off, Hyper-V off, `bcdedit /set hypervisorlaunchtype off`, restart |

## VMs

| What you see | What to do |
| --- | --- |
| VM shows no IP | Wait \~1 min for the guest agent; `p8 bootlog NAME`; on home network check the router's DHCP |
| Can't reach a home-network VM from the PC | `p8 show NAME` shows `lan`? Firewall on the VM? Try `p8 ssh NAME` from the host |
| Windows: *Press any key* missed / boot error | Console → **Ctrl+Alt+Del**, press a key at the prompt |
| Windows setup wants internet | Server → Check guest internet; or Shift+F10 → `start ms-cxh:localonly` |
| VM won't start: not enough memory | Lower its RAM or stop another VM; Server page shows RAM in use |
| Make template fails: guest agent | Install `qemu-guest-agent` in the VM, or shut the VM down and try again |
| Live move refused | Different CPUs: `p8 cpu NAME compatible`, restart the VM once; UEFI or CD inserted: move shut down |

## Storage and cluster

| What you see | What to do |
| --- | --- |
| Pool DEGRADED | A disk failed: replace it (below). The server keeps running |
| Pool almost full | Delete old snapshots and backups, `p8 storage`, add disks |
| Joining refused: "both called …" | Every server needs its own name: on the new one, Server page → **Rename** (or `p8 rename p8s-2`), then **Add** it again |
| Invitation not visible on the new server | Open its Server page → Cluster ("Invitation waiting"), or run `p8 cluster accept` there. Invitations last 15 minutes |
| Worker shows **connecting…** | Normal for up to 2 minutes after joining while its tunnel starts |
| Worker shows offline | Check its power and network; `wg show p8s-wg` on both; VMs keep running there |
| Two servers both think they are master | Only after a manual promote while the old master was cut off: it steps down once it hears the newer term |
| Ceph HEALTH\_WARN | `p8 ceph` says what's wrong; a server off is normal until it returns |

## Replacing a failed disk in a mirror or RAID

Example: good disk `/dev/sda`, new empty disk `/dev/sdc`, replacing disk number 1:

```
sudo sgdisk --replicate=/dev/sdc /dev/sda
sudo sgdisk --randomize-guids /dev/sdc
sudo sgdisk -c1:p8s-bios1 -c2:p8s-esp1 -c3:p8s-boot1 -c4:p8s-pool1 /dev/sdc
sudo zpool replace bpool <old boot partition from zpool status> /dev/sdc3
sudo zpool replace tank <old pool partition from zpool status> /dev/sdc4
sudo mkdosfs -F 32 -s 1 -n EFI /dev/sdc2 && sudo p8s-esp-sync
sudo grub-install --target=i386-pc /dev/sdc     # BIOS machines only
zpool status                                    # wait for "resilvered"
```

If the dead disk was the first (`p8s-esp0`), label the new partition `p8s-esp0`, `sudo mount /boot/efi`, and run `sudo grub-install --target=x86_64-efi --efi-directory=/boot/efi --bootloader-id=p8s` once.

## Locked out

| Problem | Fix (on the server's console or SSH) |
| --- | --- |
| Dashboard refuses your address | `sudo rm /var/lib/p8s/security.json && sudo systemctl restart p8sd` |
| Lost the admin's two-factor | `p8 user 2fa-off NAME` |
| Forgot the dashboard password | `p8 user passwd NAME` |
