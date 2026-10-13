# Security: roles, sign-in, encryption, activity log

## Roles

| Role | Can |
|---|---|
| **admin** | everything: accounts, settings, the server (shell, updates), the cluster |
| **operator** | every VM and container: create, start/stop, console, snapshots, backups, firewall, move, high availability. **Not**: accounts, plans/billing, settings, updates, cluster membership, the server shell |
| **auditor** | sees everything an admin sees, including the Activity log and its CSV export. Changes nothing; no consoles, screenshots, shells or VM passwords |
| **customer** | only their own VMs and containers, within their plan |

Set the role under **Users**. The server checks every API call; the
dashboard just hides what a role can't use. API tokens a person makes act
with that person's role. The host's own API token is an admin.

## Company login (single sign-on)

Settings → **Company login**. Works with any OpenID Connect provider. The
button "Sign in with …" appears on the sign-in page; people who sign in
for the first time get an account, and their groups decide the role every
time they sign in (the last admin is never demoted that way).

In the provider, register a **web application** with the redirect address
shown in the settings (`https://<dashboard>/api/v1/sso/callback`), then copy
the client ID and secret into P8S OS.

| Provider | Issuer | Groups |
|---|---|---|
| Google Workspace | `https://accounts.google.com` | no groups claim: use **Allowed email domains** + "people in none of the groups" = customers, or add admins by hand first (an existing account with the same verified email is linked) |
| Microsoft Entra ID | `https://login.microsoftonline.com/<tenant-id>/v2.0` | Token configuration → add groups claim (group IDs) |
| Keycloak | `https://<host>/realms/<realm>` | add a "Group Membership" mapper named `groups` (full path off) |
| Authentik / Zitadel / Okta | the provider's issuer URL | `groups` claim |

Safety: authorization code flow with PKCE, state and nonce; the ID token's
signature (provider keys), issuer, audience and expiry are checked; a
sign-in must finish within 10 minutes. Sign-ins and refusals are in the
Activity log. Two-factor is then the provider's job.

## Company directory (LDAP / Active Directory)

Settings → **Company directory**. People sign in on the normal sign-in page
with their directory user name (Windows name, uid, user@domain or email) and
password. P8S OS binds with a service account, finds the person, checks the
password by binding as them, and maps their groups (memberOf, or a group
filter for OpenLDAP without memberOf) to a role. Groups can be written as the
full DN or just the CN ("P8S Admins").

- Only `ldaps://` or `ldap://` + StartTLS: passwords never cross the network in the clear.
- An empty password is always refused (it would be an anonymous bind).
- Local accounts (made in Users) keep signing in locally; the directory is
  asked only for names without a local account or accounts the directory made.
  So keep local names different from directory names.
- Two-factor for directory accounts is set up in P8S (My account).
- **Test** checks the address, certificate, service account and base DN.
- If the directory is down, directory people get "can't be reached" and local
  admins can still sign in — keep one local admin.

## Who may connect, passwords, two-factor

Settings → **Security**:

- **Who may open this dashboard:** addresses or networks (CIDR). Everything else
  gets "403" before sign-in. The server itself (SSH tunnel), the cluster
  tunnel and the remote-access VPN (10.251.0.0/24) are always allowed; so are joining servers, the payment webhook and
  `/api/v1/health`. A list without your own address is refused. Locked out:
  `sudo rm /var/lib/p8s/security.json && sudo systemctl restart p8sd`.
- **Passwords:** shortest length (10-64), letters + digits, and the most
  common passwords are refused. Applies to new passwords.
- **Two-factor for staff:** admins, operators and auditors without 2FA can only
  set it up (and change their password) until they do. API tokens and
  company-login accounts are not affected.

## Encryption at rest

Chosen at install: boot menu **"Install P8S OS, encrypted VM storage"**, or
`sudo bash install.sh --encrypt /dev/sdX`. Everything under `tank/p8s` (VM
disks, containers, templates) is ZFS AES-256-GCM. The key is a file on the
system disk (`/etc/p8s/keys/p8s.key`), unlocked at boot by
`p8s-zfs-keys.service`.

- Protects VM data on **disks that leave the server** (stolen, returned for
  warranty, sold). With the file pool on the system disk it protects little.
- **Back up the key**: Server → Storage → **Show the key…** (asks for your
  password; only admins; logged). Without the key, the VM disks can't be read
  — for example if the system disk dies.
- Backups written to the backup folder are not encrypted (ZFS decrypts when it
  copies); turn on encryption for off-site copies.
- An existing server can't be switched on in place: reinstall, or move its VMs
  to a new encrypted server (cluster → Move).

## Activity log (audit)

- Every change and sign-in, with user and IP. Kept **180 days** by default
  (`audit_retain_days` in `/etc/p8s/p8sd.json`), a new file every day.
- **Tamper-evident:** each entry holds the hash of the one before. Activity →
  **Check integrity** finds a changed or removed entry.
- **Download CSV** for a date range (for auditors, or a CERT-In request).
- System logs (journald) are kept on disk (`/etc/systemd/journald.conf.d/p8s.conf`, up to 4 GB).
- The clock must be right: install turns NTP on, and Health & alerts warns
  when the clock isn't synced for 10 minutes.

## API

| Method | Path | |
|---|---|---|
| GET | `/api/v1/audit/export?from=YYYY-MM-DD&to=YYYY-MM-DD` | CSV (staff) |
| GET | `/api/v1/audit/verify` | `{"ok","entries","oldest","problem"}` |
| GET / PUT | `/api/v1/sso` | company login settings (secret never shown; empty keeps it) |
| GET / PUT | `/api/v1/ldap` · POST `/api/v1/ldap/test` | company directory settings / try them |
| GET / PUT | `/api/v1/security` | `{"allow_nets":[], "staff_2fa":bool, "password":{"min_length":10,"mix":bool}}` + `your_ip` |
| GET | `/api/v1/node/encryption` | `{"on","algorithm","root","key_status","key_file"}` |
| POST | `/api/v1/node/encryption-key` | `{"password"}` (or `{"otp"}`) → `{"key_hex","restore"}` (admins) |
| GET | `/api/v1/sso/start` · `/api/v1/sso/callback` | the browser flow (no login) |
