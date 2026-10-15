# 16. Command reference (`p8`)
Summary: Every p8 command on one page.

Run on the server (over SSH or the dashboard's shell). `p8 help` lists everything.

| Area | Commands |
| --- | --- |
| VMs | `p8 list` · `p8 show NAME` · `p8 create NAME [CPUS] [RAM_MB] [DISK_GB] [--os T] [--lan] [--vlan ID] [--storage S] [--ip POOL] [--for USER]` · `p8 create NAME CPUS RAM DISK --iso FILE.iso [--windows]` |
| Power | `p8 start\|stop\|reboot\|kill NAME` · `p8 delete NAME [-y]` |
| Access | `p8 ip NAME` · `p8 ssh NAME` · `p8 console NAME` · `p8 bootlog NAME` · `p8 passwd NAME [USER]` |
| Size and disks | `p8 resize NAME cpus=N ram=MB disk=GB` · `p8 disks NAME` · `p8 disk-add NAME GB` · `p8 disk-grow NAME N GB` · `p8 disk-rm NAME N` |
| Snapshots | `p8 snap NAME [SNAP]` · `p8 snaps NAME` · `p8 rollback NAME SNAP` · `p8 unsnap NAME SNAP` |
| Backups | `p8 backup NAME` · `p8 backups [NAME]` · `p8 restore NAME ID [NEWNAME]` · `p8 verify NAME ID` · `p8 rmbackup NAME ID` · `p8 autobackup NAME on [KEEP]\|off` · `p8 jobs` |
| Clones and templates | `p8 clone VM NEW [--start]` · `p8 template-from VM NAME ["label"]` · `p8 templates` · `p8 templates available` · `p8 templates add NAME` · `p8 templates rm NAME` |
| ISOs | `p8 isos` · `p8 iso-upload PATH.iso` · `p8 iso-download URL [SHA256]` · `p8 rmiso NAME.iso` · `p8 eject NAME` |
| Containers | `p8 ct` · `p8 ct create NAME [CPUS] [RAM] [DISK] [--os T]` · `p8 ct start\|stop\|reboot\|kill\|delete NAME` · `p8 ct enter NAME` · `p8 ct templates [add NAME]` |
| Network | `p8 lan-setup` · `p8 bond-setup PORT [--mode M]` · `p8 ipv6 on\|off\|status` · `p8 ippools` · `p8 ippool add NAME NET GW` · `p8 fw NAME [on\|off]` · `p8 fw NAME allow tcp 22 [from CIDR]` |
| Websites | `p8 tunnel setup` · `p8 tunnel [dashboard NAME\|off]` · `p8 publish VM NAME PORT [--https]` · `p8 sites` · `p8 unpublish HOST` |
| Storage | `p8 storage` · `p8 storage grow` · `p8 storages` · `p8 storages add-nfs NAME SERVER EXPORT [--backups]` · `p8 storages find SERVER` · `p8 storages add-iscsi NAME SERVER TARGET [LUN] [--format]` · `p8 storages connect\|rm NAME` |
| Ceph | `p8 ceph` · `p8 ceph install [SERVER]` · `p8 ceph init NETWORK` · `p8 ceph mon\|mgr SERVER` · `p8 ceph osd SERVER DISK` · `p8 ceph osd-rm ID` · `p8 ceph pool [COPIES]` |
| Cluster | `p8 cluster` · `p8 cluster find` · `p8 cluster invite NAME` · `p8 cluster accept\|decline` · `p8 cluster add [PUBLIC_ADDR]` · `p8 join URL CODE` · `p8 cluster remove ID [--force]` · `p8 cluster leave` · `p8 cluster promote` · `p8 cluster witness URL KEY FINGERPRINT\|off` |
| Disks | `p8 hostdisks` · `p8 disk-protect DISK [off]` · `p8 disk-wipe DISK` · `p8 pool-make NAME single\|mirror\|raidz1\|raidz2 DISK…` · `p8 pool-rm NAME` |
| Virtual networks | `p8 vnets` · `p8 vnet-add NAME [SUBNET] [--no-internet]` · `p8 vnet-rm NAME` · `p8 create NAME --net NAME` · `p8 nic-add VM vnet:NAME` |
| Moves | `p8 move NAME SERVER [--live]` · `p8 cpu NAME host\|compatible` |
| Users | `p8 users` · `p8 user add NAME [--admin] [vms=N cpus=N ram=MB disk=GB]` · `p8 user quota NAME ...` · `p8 user passwd\|disable\|enable\|2fa-off\|rm NAME` · `p8 give VM USER` · `p8 audit [N]` |
| Selling | `p8 plans` · `p8 plan ID NAME CPUS RAM DISK PRICE [--public-ip]` · `p8 balance [USER]` · `p8 credit USER AMOUNT [NOTE] [--invoice]` · `p8 license` |
| Server | `p8 status` · `p8 logs` · `p8 dashboard` · `p8 update [check\|install]` · `p8 test` · `p8 report` |
| Installer (on the ISO) | `p8s-install --list` · `p8s-install --answers a.json [--dry-run]` |

**Useful system commands:** `zpool status`, `zfs list -o name,used,avail`, `virsh list --all`, `wg show p8s-wg`, `journalctl -u p8sd -f`.

Everything the dashboard and `p8` do is also in the REST API at `https://<server>:8006/api/v1/...` with `Authorization: Bearer <token>` (tokens under your name, top right).
