# Public access (Cloudflare Tunnel)

Put the dashboard and VMs' websites on the internet with real HTTPS, without
opening anything on your router. It also works when your ISP gives you no
public IP (CGNAT), which is common on home connections.

How it works: `cloudflared` on the P8S server keeps an outgoing connection to
Cloudflare. A visitor opens `https://shop.example.com`; Cloudflare (which holds
the certificate) sends the request down that connection; `cloudflared` passes
it to the VM's IP and port. p8sd sets the tunnel's routes and DNS records
through the Cloudflare API, and follows VM address changes every minute.

## One-time setup

You need a domain on Cloudflare (free plan is fine) and an API token:

1. dash.cloudflare.com → **Add a domain**, and switch its nameservers at your
   registrar to the two Cloudflare shows.
2. **My Profile → API Tokens → Create Token → Custom token** with:
   - Account · Cloudflare Tunnel · Edit
   - Zone · DNS · Edit (Zone Resources: include your domain)
3. On the P8S server:

```
p8 tunnel setup
```

It asks for the domain and the token, creates the tunnel (`p8s-<host>`),
installs `cloudflared` from Cloudflare's package repository and starts it, and
puts the dashboard on `https://panel.<domain>` (or a name you pick, or none).

The API token is stored only in `/var/lib/p8s/tunnel.json` (mode 0600).

## Websites

```
p8 publish web1 shop 80          # https://shop.example.com -> web1, port 80
p8 publish web1 app 8443 --https # the VM serves HTTPS itself (its certificate isn't checked)
p8 sites                         # list
p8 unpublish shop.example.com
```

Or in the dashboard: **Manage → Websites on the internet**. Customers can
publish their own VMs, up to the `sites` number in their plan
(`p8 user quota alice sites=5`). Names are one level (`shop`, not `a.shop`) so
Cloudflare's free certificate covers them; `www`, `mail`, `api`, `admin`,
`panel`, `dashboard`, `p8s`, `ns1`, `ns2` are kept for the admin.

Notes:
- Traffic reaches the VM from the P8S server, so if the VM's firewall is on,
  allow the port (`p8 fw web1 allow tcp 80`).
- Only web traffic (HTTP/HTTPS, WebSockets). SSH to VMs from outside is not
  part of this (use the dashboard console, or Cloudflare Access later).
- An existing DNS record with the same name is never overwritten; pick
  another name or delete it in Cloudflare first.
- Deleting a VM removes its websites and their DNS records.

## Dashboard on the internet

Anyone can reach the sign-in page, so:
- give every admin a strong password and turn on **two-factor sign-in**
  (top right → your name);
- failed sign-ins lock a user name for 15 minutes; the activity log shows the
  visitor's real IP (Cloudflare's `CF-Connecting-IP`, trusted only from
  `cloudflared` on the server itself).

Take it off again: `p8 tunnel dashboard off`. Status: `p8 tunnel`.
