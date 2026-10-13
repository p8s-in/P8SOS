# Remote access: using P8S OS from outside your network

At home or in the office, the dashboard is at `https://<server-ip>:8006` and
works only on that network. To use it from anywhere, pick one of three ways
(Server page → **Remote access**). The page also checks your connection:

- **CGNAT check:** it asks the router (UPnP) for its internet address. If
  that is 100.64–100.127.x.x or 10.x, the provider (Jio, Airtel and others)
  shares one public address among many homes: **nothing on the internet can
  connect in to your server**. Port forwarding then can't work; use way 1 or 3.

| | Works with CGNAT | Router setup | Good for |
|---|---|---|---|
| **1. Tailscale** | yes | none | you and your team, from phone / laptop |
| **2. Built-in VPN (WireGuard)** | no | forward UDP 51830 | a server with a real public IP (datacenter, business line) |
| **3. Cloudflare Tunnel** | yes | none, needs a domain | websites of your VMs; the dashboard on a web address |

## 1. Tailscale

Press **Install and connect**. The task log shows a sign-in link — open it
and sign in (Google / Microsoft / GitHub account; free for personal use).
Install the Tailscale app on your phone / laptop with the same account, then
open `https://<the server's 100.x address>:8006`. Tick "Also reach my home
network" to reach the VMs on your LAN too (approve the route once in the
Tailscale admin page). If you use Settings → Security → allowed networks,
add `100.64.0.0/10`.

## 2. Built-in VPN

1. Turn it on, give the address your devices dial (the public IP, or a free
   dynamic-DNS name like `myhome.duckdns.org` if the IP changes).
2. Forward **UDP 51830** on the router to this server — **Open the port on my
   router** does it through UPnP when the router allows it.
3. **Add device** for each phone / laptop: the WireGuard config is shown
   **once** (download the .conf and import it in the WireGuard app).
4. Connected devices reach the dashboard at `https://10.251.0.1:8006` and the
   VMs' SSH / RDP ports on 10.251.0.1. Give the home network (e.g.
   `192.168.0.0/24`) to reach it as well.

Every device has its own key; **Remove** cuts it off at once. Devices can't
talk to each other. The VPN addresses are always allowed by Settings →
Security → allowed networks.

## 3. Cloudflare Tunnel

Settings → **Public access**: see [public-access.md](public-access.md). Best for
websites; if you also put the dashboard on the internet, use strong passwords,
two-factor, and preferably an allowed-networks list.

## API

| Method | Path | |
|---|---|---|
| GET | `/api/v1/remote` | VPN, devices, public IP, router (UPnP, CGNAT), Tailscale, tunnel (`?refresh=1` checks again) |
| PUT | `/api/v1/remote/vpn` | `{"enabled","endpoint","port","lan","lan_net","mtu"}` |
| POST | `/api/v1/remote/vpn/devices` | `{"name"}` → `{"device","config"}` (config only here) |
| DELETE | `/api/v1/remote/vpn/devices/{id}` | |
| POST | `/api/v1/remote/forward` | UPnP: forward the VPN port to this server |
| POST | `/api/v1/node/tasks` | `{"kind":"tailscale","arg":""|"lan"}` · `{"kind":"tailscale-off"}` |
