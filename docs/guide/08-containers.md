# 8. Containers (LXC)
Summary: Light Linux containers (LXC): when to use them instead of VMs, and how.

A container shares the server's Linux kernel, so it starts in about a second and uses far less RAM than a VM. Use containers for small Linux services; use a VM for Windows, a custom kernel, Docker-heavy work or anything that needs full isolation.

|  | Container | VM |
| --- | --- | --- |
| Start time | \~1 s | 5–30 s |
| RAM overhead | A few MB | 100+ MB |
| Operating systems | Linux only | Any |
| Isolation | Unprivileged (root inside = nobody on the host), AppArmor | Full hardware virtualization |
| Live move between servers | No (stop, move, start) | Yes |

**Steps**

1. Add a template once: **Server → Container templates → Add** (Debian 13/12, Ubuntu 24.04/22.04, Alpine, Rocky, Alma, Fedora, Arch), or `p8 ct templates add debian-13`. Want another distribution or release (Kali, openSUSE, Gentoo, Void, Devuan, Ubuntu 25.x, Debian testing…)? **+ Add another** lists every image on images.linuxcontainers.org; or `p8 ct templates available` and `p8 ct templates add kali/current`.
2. **+ Create CT**: name, template, CPUs, RAM, disk, network, root password or SSH key. Or `p8 ct create web2 1 512 8 --os debian-13 --root-password`.
3. Use it: Console (terminal in the browser), `p8 ct enter web2` (root shell), or `ssh root@<ip>`.
4. Manage: resources (changed live), snapshots, backups, firewall, owner — the same as VMs.

Each container is its own ZFS dataset (`tank/p8s/ct/NAME`), runs as systemd unit `p8s-ct-NAME`, and keeps running if p8sd restarts. Mark it *start with the server* to have it come back after a reboot.
