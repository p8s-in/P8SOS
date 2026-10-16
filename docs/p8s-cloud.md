# P8S Cloud — selling servers like a cloud provider

Turns a P8S OS host into a small DigitalOcean / Hetzner Cloud: customers sign
up, add credit, pick a size and get a server in a minute; you get paid, they
get invoices and emails, and they can ask for help in the panel.

Everything is in the dashboard (**Settings**, **Billing**, **Support**); the
`p8` command has the same for scripts.

## 1. Settings → Selling: P8S Cloud

- **Brand** (shown on the sign-in page, in emails and invoices), **panel
  address** (used for links in emails, e.g. `https://panel.example.com` from
  Settings → Public access), **support email** (gets new support requests).
- **Anyone can create an account** — self sign-up on the sign-in page.
  New customers get the "sign-up quota" as a safety cap; billing limits real use.
- **Currency, tax** (e.g. INR, GST 18%), **seller name, address and GSTIN**
  (printed on invoices), **invoice prefix** (numbers like `P8S-2026-000001`).
- **Smallest top-up**, and after how many hours of negative balance servers
  are stopped (default 72).

## 2. Plans

Sizes with a monthly price before tax, e.g.

| id | name | CPUs | RAM | disk | price |
|---|---|---|---|---|---|
| s1 | Starter | 1 | 1 GB | 25 GB | ₹399/mo |
| s2 | Standard | 2 | 2 GB | 50 GB | ₹799/mo |
| s4 | Pro (public IP) | 4 | 8 GB | 160 GB | ₹2,499/mo |

- Billed by the hour (monthly ÷ 730, rounded up to the paisa), charged from
  the wallet at the start of each hour, **never more than the monthly price**
  in a calendar month; stopped servers still cost (their resources are kept),
  deleted servers stop costing.
- "Includes a public IP": the server gets an address from the pool chosen in
  settings ([public-ips.md](public-ips.md)).
- Once plans exist, customers must pick one; admins can still create any size
  (not billed). Changing a price affects new servers only.
- **Plans for containers:** a plan is for VMs and containers (the default), for **VMs
  only** or for **containers only** (Settings → Plans → For). Containers share the
  host's kernel and need less RAM, so container plans are usually cheaper (and may
  start at 128 MB). + Create VM shows the plans for VMs, + Create CT those for containers.
- CLI: `p8 plans`, `p8 plan s1 Starter 1 1024 25 399`,
  `p8 plan c1 "Container S" 1 512 10 149 --containers` (or `--vms`).

## 3. Wallet, payments, invoices

- Customers add credit under **Billing**: Razorpay Checkout (cards, UPI,
  netbanking). Tax is added on top; the wallet gets the pre-tax amount and the
  customer gets a **tax invoice** (Billing → View → Print / Save as PDF).
- Razorpay: put the **key id / key secret** in Settings → Payments, enable
  automatic capture, and add a webhook (`payment.captured`) to
  `https://<panel>/api/v1/billing/razorpay/webhook` with the webhook secret.
  Use `rzp_test_…` keys to try it without real money.
- Payments by bank transfer / UPI: Billing (admin) → customer → **Credit**,
  tick "payment (invoice)"; or `p8 credit ravi 1000 "UPI 4411" --invoice`.
  Goodwill credit or corrections: same without the tick (negative to debit).
- A server needs one day of credit to be created.

### When money runs out

1. Credit for less than ~3 days left → "balance running low" email (once a day).
2. Balance below zero → "action needed" email.
3. Still negative after the grace period → servers are **stopped** (disks and
   data kept), email; the customer can't start or create servers.
4. Customer tops up → account active again (email); they start their servers.

## 4. Email (Settings → Email)

SMTP server, port (587 STARTTLS or 465 TLS), user, password and sender, then
**Send test email**. Messages: confirm your email, welcome, server ready (IP
and how to log in), password reset, payment received, low / negative balance,
servers stopped, account active again, support replies. "Sent emails" shows
what went out or why not. Without email, sign-ups are confirmed immediately
and password reset isn't offered.

## 5. Support

Customers open requests under **Support** (optionally about a server) and
reply in the thread; admins see all requests, answer, and mark them solved.
New requests and replies go to the support email; answers are emailed to the
customer.

## 6. Customer API

Customers (and admins) create **API tokens** under their name (top right):
`Authorization: Bearer p8sk_…` acts as that user on the same API the panel
uses ([api.md](api.md)) — create/start/stop servers, snapshots, backups,
firewall, billing. Tokens can't change the password, 2FA or tokens.
