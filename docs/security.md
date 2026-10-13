# Security: roles, company login, activity log

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
| GET | `/api/v1/sso/start` · `/api/v1/sso/callback` | the browser flow (no login) |
