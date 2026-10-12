# P8S OS subscriptions

P8S runs three businesses on one OS:

1. **Our own servers and datacenter** run P8S OS with an **Internal** licence (free, everything on).
2. **P8S Cloud** sells VPSes from those servers ([p8s-cloud.md](p8s-cloud.md)).
3. **P8S OS is sold to others**, like Proxmox but cheaper, per server instead of per CPU socket.

## Tiers

| Tier | For | Price (suggested, + 18% GST) | Unlocks |
|---|---|---|---|
| Home | home, lab, learning | free, no key | every VM feature, up to 3 user accounts, testing updates, a "no subscription" notice |
| Business | companies running their own VMs | ₹4,999 / server / year | unlimited users, stable updates, email support, no notice |
| Hosting | public use: selling servers to customers | ₹9,999 / server / year | + P8S Cloud selling (public sign-up, priced plans), own brand name |
| Partner | outsourcing / MSPs | ₹24,999 / year for up to 10 servers | + priority support, resell licences |
| Internal | P8S's own servers | not sold | everything |

What a missing or expired subscription does **not** do: stop VMs, delete anything, or cut off customers.
Existing accounts, plans and wallets keep working. Without Hosting, new public sign-ups and plan
changes are refused, and the sign-in page shows the default "P8S Cloud" name.

Prices live in one place for the code (`internal/license/tiers.go`, shown in the dashboard) and are
overridden by `prices` in the licence server's config (what the shop really charges).

## How keys work

- Every server has an ID, `P8S-XXXX-XXXX-XXXX-XXXX`, made from `/etc/machine-id`.
  See it in the dashboard (**Subscription**), with `p8 license id` or `sudo p8sd server-id`.
- A key (`P8S1-…`) is signed by P8S with Ed25519 and lists the tier, the server IDs and the last valid day.
  p8sd checks it **offline** against the public keys built into `internal/license/pubkeys.txt`.
  Only the holder of the private signing key can make keys.
- After expiry there are **14 days of grace**, then the server falls back to Home.
- When `license_server` is set in `/etc/p8s/p8sd.json` (e.g. `"https://licence.example.com"`),
  p8sd asks it once a day: renewed keys are picked up automatically and cancelled keys stop working.
  Without it, keys are only checked offline.
- A reinstall changes the server ID: move the licence with `p8s-licensed rebind`.

Install a key: dashboard → **Subscription** → paste → **Save key**, or `p8 license set KEY`.

## The signing key (keep it secret)

P8S makes the key pair **on its own server**, so the private key never leaves it:

```bash
sudo p8s-licensed keygen      # writes /var/lib/p8s-licensed/signing.key, prints the public key
```

Put the printed public key on a new line in `internal/license/pubkeys.txt`, push, and update every
server (`git pull && sudo bash update.sh`). Until then no key is accepted and servers run as Home.

**Back the private key up offline** (USB stick, password manager). Anyone with it can make licences;
losing it means a new key pair and a P8S OS update before new keys work. If it leaks: make a new pair,
add its public key, release an update, re-issue keys, then remove the old line in a later release.

## Licence server (`p8s-licensed`)

Installed with P8S OS (`/usr/local/bin/p8s-licensed`), off by default. Run it on one of our servers.

```bash
sudo p8s-licensed pubkey      # must be a line in pubkeys.txt

# issue keys by hand (our own servers, bank-transfer customers, partners)
sudo p8s-licensed issue -tier internal -server "$(sudo p8sd server-id)" -customer P8S -months 120
sudo p8s-licensed issue -tier hosting -server P8S-AAAA-BBBB-CCCC-DDDD -customer "Acme" -email ops@acme.in
sudo p8s-licensed list | show ID | extend ID -months 12 | rebind ID -server NEWID | revoke ID -note "refunded" | invoices
```

Selling online: create `/etc/p8s-licensed.json`, then `sudo systemctl enable --now p8s-licensed`
and publish `127.0.0.1:8090` (e.g. as `licence.yourdomain` with Cloudflare Tunnel, see [public-access.md](public-access.md)).

```json
{
  "public_url": "https://licence.example.com",
  "brand": "P8S OS",
  "support_email": "support@example.com",
  "seller": {"name": "P8S (Problems Infinity Solutions)", "address": "…", "tax_id": "GSTIN…", "email": "billing@example.com"},
  "tax_percent": 18,
  "invoice_prefix": "P8SL",
  "prices": {"business": 499900, "hosting": 999900, "partner": 2499900},
  "razorpay_key_id": "rzp_live_…", "razorpay_key_secret": "…", "razorpay_webhook_secret": "…",
  "smtp_host": "smtp.example.com", "smtp_port": 587, "smtp_user": "…", "smtp_pass": "…",
  "mail_from": "P8S OS <licences@example.com>"
}
```

Razorpay webhook: `https://licence.example.com/api/razorpay/webhook`, events `payment.captured` and `order.paid`.

The shop (`/`) shows the tiers; a buyer enters server IDs (or a licence ID to renew, same tier or higher),
pays with Razorpay, and gets the key on screen and by email with a GST tax invoice (`P8SL-YYYY-NNNNNN`).
The dashboard's **Buy** buttons open the shop with the server ID filled in.

API used by p8sd: `POST /api/v1/check {"id","server"}` → `{"status":"active|revoked|unknown","key","expires"}`.

## Updates: stable and testing channels

The licence server is also the **update server**. Every release is a signed bundle of the source
tree; servers install it with `update.sh`, exactly like a manual update.

| Channel | Who | What |
|---|---|---|
| testing | everyone (Home included) | every release, as soon as it is published |
| stable | Business, Hosting, Partner, Internal | releases P8S has tested and promoted |

Publishing (on the licence server, from a checkout of the repo):

```bash
cd ~/P8SOS && git pull
sudo p8s-licensed release add -dir ~/P8SOS -version 0.6.0 -notes "Subscriptions, one-click updates"   # testing
# ...try it on a testing server for a few days, then:
sudo p8s-licensed release promote -version 0.6.0                                                      # stable
sudo p8s-licensed release list
```

On a server with `"license_server"` set in `/etc/p8s/p8sd.json`:
- p8sd checks once a day; the dashboard shows a badge on **Subscription** when an update is ready.
- **Subscription → Updates → Install update** (or `p8 update install`) downloads the bundle, checks its
  SHA-256 and P8S's Ed25519 signature (a release not signed by a key in `pubkeys.txt` is never installed),
  unpacks it and runs `update.sh` in its own systemd unit (`p8s-update-*`), so p8sd can restart halfway.
  VMs keep running. The log shows in the dashboard; `journalctl -u 'p8s-update-*'` on the server.
- Version names come from the bundle's `VERSION` file (install.sh passes it to the build).
- Servers without `license_server` keep updating from GitHub with `git pull && sudo bash update.sh`.

Server API: `POST /api/v1/updates {"channel","license","server","current"}` → newest release + signature;
`GET /api/v1/updates/download?version&channel&license&server` (stable checks the licence again).

## Before selling

- **Code licence.** If P8S OS is fully open source, anyone can remove the check. Decide between
  source-available (commercial use needs a subscription) and open source with paid updates/support,
  and add the matching `LICENSE` file and terms of sale. Get a lawyer to look at it.
- **GitHub stays open.** While the repo is public, anyone can still update from GitHub; stable only
  becomes a paid benefit once the repo is private or releases go only through the update server.
