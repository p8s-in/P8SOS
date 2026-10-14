# 5. Installing
Summary: Every installer screen, what it creates on the disks, and installs without questions.

Boot the stick (the boot menu key is usually F12, F11, F8 or Esc; pick the UEFI entry). **Install P8S OS** starts by itself after a few seconds. Use the arrow keys, Space to tick, Tab to move to OK, Enter to confirm.

| Screen | What to choose |
| --- | --- |
| Disks | Tick the disks for P8S OS. Each line shows size, model, serial and `empty` or `HAS DATA`. The USB stick isn't offered. |
| Layout | Only layouts that fit the number of disks appear. Two disks → **mirror**. |
| Server name | e.g. `p8s1` or `p8s1.example.com` |
| Network port | The port with the cable (Wi-Fi works too: name and password) |
| Address | **DHCP** for tests; a **fixed address** for real servers (address/24, gateway, DNS) |
| Time zone, keyboard | e.g. Asia/Kolkata, us |
| Admin user + password | Your login; it gets sudo. Root login stays off. |
| Encrypt the VM disks? | **No** for first installs. Yes encrypts VM storage; the key sits on the system disk and must be backed up. |
| Summary | Lists what will be ERASED. Type **ERASE** to start. Nothing is written before this. |

The install takes 3–10 minutes (66 steps). When it says done, remove the stick (or the ISO from the VM) and press Enter to restart.

**What it creates** (ZFS layouts)

| Where | What |
| --- | --- |
| `tank/ROOT/debian` | The system (`/`) |
| `bpool/BOOT/debian` | `/boot`, a small pool GRUB can read, mirrored on every disk |
| `tank/p8s/vms`, `tank/p8s/templates` | VM disks and templates (made on the first start) |
| Partition 2 of each disk (`p8s-esp0`, `p8s-esp1`…) | EFI boot partitions, kept equal by `p8s-esp-sync` after updates |
| `/etc/p8s/installed.json` | The layout and disks chosen |

**If the install stops** it shows the failing step. Press Enter to see the log, take a photo of the last lines, and fix before retrying (rebuild the ISO if the fix was in the installer).

## Unattended installs

In the installer menu choose *Command line*, then:

```
p8s-install --list                        # disks and network ports
p8s-install --answers a.json --dry-run    # print every command, change nothing
p8s-install --answers a.json              # install with no questions
```

```json
{
  "disks": ["sdb", "sdc"],
  "layout": "mirror",
  "hostname": "p8s1.example.com",
  "timezone": "Asia/Kolkata",
  "keyboard": "us",
  "user": "admin",
  "password": "change-me-please",
  "network": {"interface": "auto", "mode": "static", "address": "192.168.1.21/24",
              "gateway": "192.168.1.1", "dns": ["1.1.1.1"]},
  "encrypt_vm_storage": false,
  "erase": true
}
```

- `disks`: `sdb`, `/dev/nvme0n1` or (safest) `/dev/disk/by-id/...`. `"erase": true` is required.
- `layout`: `single`, `mirror`, `raid10`, `raidz1`, `raidz2`, `raidz3`, `ext4` (`root_gb` sets the ext4 system size, default 32).
- `network.interface`: `auto` = first port with a cable; `mode`: `dhcp` or `static`.
