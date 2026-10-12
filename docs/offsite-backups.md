# Off-site backups

Backups on the same server don't survive a dead disk, a fire or a stolen
machine. P8S OS copies every finished backup (VMs and containers, made by hand
or by the nightly schedule) to one or more other places.

Dashboard: **Backups → Off-site copies → + Add a place**. Admins only; needs a
subscription with off-site backups (everything is unlocked in development mode).

| Place | What to enter |
|---|---|
| **A folder** | A USB disk or NAS share mounted on the server, e.g. `/mnt/usb-backup` (not under `/home` or `/root`). If the disk isn't mounted, copies wait and the place shows a problem. |
| **Cloud storage** | Any S3-compatible storage: endpoint, region, bucket, access key, secret key. Examples below. |
| **Another server** | Host, user, port and a folder there. Put the key shown in the form into that user's `~/.ssh/authorized_keys` on the other server. Another P8S server, a NAS or a cheap VPS all work. |

| Provider | Endpoint | Region |
|---|---|---|
| AWS S3 | `https://s3.ap-south-1.amazonaws.com` | `ap-south-1` (tick "bucket in the host name" for new buckets) |
| Backblaze B2 | `https://s3.us-west-004.backblazeb2.com` (from the bucket page) | `us-west-004` |
| Cloudflare R2 | `https://<account id>.r2.cloudflarestorage.com` | `auto` |
| Wasabi | `https://s3.ap-southeast-1.wasabisys.com` | `ap-southeast-1` |
| MinIO | `https://minio.example.com:9000` | `us-east-1` |

**Save and test** writes, reads back and deletes a small file, so a wrong key
or folder shows at once.

## How it works

- After each backup p8sd copies it to every enabled place (one at a time per
  place); every 6 hours it also copies anything missing, so a place that was
  offline catches up. **Copy now** does that at once.
- Each place keeps the newest *N* backups per VM / container ("Keep"); older
  copies there are deleted. Local backups follow their own schedule.
- Layout: `[folder/]vm/<name>/<name>-<id>.zfs.zst` (or `ct/…`) plus a `.json`
  description, written last, so an interrupted copy never looks complete.
- Uploads to cloud storage are multipart (64 MB parts, retried), so backups of
  any size stream without temporary files.

**Encryption.** With a passphrase (12+ characters) the backup data is encrypted
before it leaves the server (AES-256-GCM, key from PBKDF2-SHA256; file ends with
`.p8senc`). The provider sees only noise. The small `.json` description (name,
date, size, owner) stays readable so the list works without the passphrase.
**Keep the passphrase somewhere safe: without it the copies can't be restored.**

## Restoring from off-site

Backups → Off-site copies → **Stored** lists what a place holds. **Bring back**
downloads (and decrypts) one onto this server; it then shows with the other
backups, and **Recreate / Copy / Restore…** work as usual. On a new server:
install P8S OS, add the same place (same passphrase), Stored → Bring back.

API: `GET /api/v1/offsite`, `PUT|DELETE /api/v1/offsite/targets/{name}`,
`POST /api/v1/offsite/targets/{name}/test|sync|fetch`,
`GET /api/v1/offsite/targets/{name}/backups` (see [api.md](api.md)).
