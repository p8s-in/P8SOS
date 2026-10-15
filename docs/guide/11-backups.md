# 11. Backups
Summary: Snapshots, backups, restores and off-site copies (the 3-2-1 rule).

Snapshots are not backups: they live on the same disks. A backup is a full, compressed file of the VM's disks that you can keep elsewhere and restore anywhere.

|  | Snapshot | Backup | Off-site copy |
| --- | --- | --- | --- |
| Speed | Instant | Minutes (VM keeps running) | After each backup |
| Survives a dead pool / stolen server | No | No (same server) | **Yes** |
| Use for | "Undo" before changes | Daily restore points | Disasters |

## Making backups

| Task | Dashboard | Command |
| --- | --- | --- |
| Back up now | Manage → Backups → Back up now | `p8 backup web1` |
| Daily automatic backup | Manage → Backups → Daily, keep N | `p8 autobackup web1 on 7` |
| List (also of deleted VMs) | Backups | `p8 backups [NAME]` |
| Restore over the VM | Backups → Restore | `p8 restore web1 ID` |
| Restore as a new VM | Backups → Copy | `p8 restore web1 ID web1-copy` |
| Test a backup without touching the VM | Backups → Verify | `p8 verify web1 ID` |
| Delete one | Backups → Delete | `p8 rmbackup web1 ID` |
| Take single files out | Backups → **Files**: browse the backup, Download | `p8 files web1 ID /etc`, `p8 files web1 ID /etc/nginx/nginx.conf` |

VMs on Ceph are backed up too: a Ceph snapshot of every disk, saved as a whole disk image the first time and then only the changes since the last backup (like ZFS backups; every 8th one is full again). Such a backup restores over the VM, or as a new VM — on Ceph when the server has it, otherwise on local ZFS.

**Single files.** **Files** opens a backup read-only (restored into a scratch disk, closed after an hour) and lists its filesystems — ext4, XFS, Btrfs, FAT/EFI, NTFS (Windows). Click through the folders and **Download** what you need, then copy it back into the VM. LVM and encrypted disks inside the VM can't be opened this way: restore the backup as a new VM to get at them.

## Off-site copies (the 3-2-1 rule)

Keep 3 copies of your data, on 2 kinds of media, 1 of them in another place. **Backups → Off-site copies → + Add a place**:

| Place | What to enter |
| --- | --- |
| A folder | A USB disk or NAS share mounted on the server, e.g. `/mnt/usb-backup` |
| Cloud storage (S3) | Endpoint, region, bucket, access key, secret (AWS S3, Backblaze B2, Cloudflare R2, Wasabi, MinIO) |
| Another server | Host, user, port, folder; add the shown key to that user's `authorized_keys` |
| NAS share | Added under Network storage with *Copy every backup here* |

- **Save and test** writes, reads back and deletes a small file.
- Each place keeps the newest *N* backups per VM. Every 6 hours missing copies are caught up.
- Set a **passphrase** (12+ characters) to encrypt copies before they leave the server. Keep it safe: without it the copies can't be restored.

**Restoring after losing a server:** install P8S OS on a new machine, add the same place with the same passphrase, Off-site copies → Stored → **Bring back**, then Restore.

**Good default:** daily backups keeping 7, plus an off-site place keeping 14. Run a Verify on one important VM each month.
