# 13. Monitoring, security, remote access and updates
Summary: Health checks and alerts, a security checklist, access from outside, updates.

## Health & alerts

Server → **Health & alerts** checks every minute:

| Watched | Warning | Critical |
| --- | --- | --- |
| ZFS pool health | — | DEGRADED, FAULTED (a disk failed) |
| ZFS pool space | ≥ 80% | ≥ 90% |
| System / backup disk space | ≥ 85% | ≥ 95% |
| RAM in use | ≥ 92% | ≥ 98% |
| CPU temperature | ≥ 85 °C | ≥ 95 °C |
| Disks (SMART every 30 min) | Bad sectors, wear ≥ 90%, ≥ 55 °C | Disk reports FAILING |
| VMs | — | Crashed |
| Backups | Failed in the last 24 h | — |
| Power (UPS or laptop battery) | On battery | Battery almost empty |
| Ceph (when set up) | HEALTH\_WARN | HEALTH\_ERR, monitors lost |

Alerts go out once when they start, again if worse, and once when fixed. Channels: **email**, **webhook** (Slack, Teams, Discord, Mattermost), and **phone push** with the ntfy app (`https://ntfy.sh/<secret-topic>`). Press **Send a test**.

**Power cuts:** with *On a power cut, shut down cleanly* on (default), P8S shuts every VM down and powers off when the battery is nearly empty. Servers need a UPS over USB and `sudo apt install nut`.

## A real HTTPS certificate (Let's Encrypt)

With a domain that points to the server (an A record, e.g. `p8s.example.com`), the dashboard can get a free Let's Encrypt certificate: Server page → Network → **HTTPS certificate** (or `p8 cert p8s.example.com`). Two ways to prove the domain is yours:

| Way | Needs | Behind CGNAT |
| --- | --- | --- |
| **Port 80** | TCP 80 forwarded from the router to the server | No |
| **Cloudflare DNS** | The domain's DNS at Cloudflare and an API token with *Zone → DNS → Edit* (`p8 cert DOMAIN --cloudflare TOKEN`) | Yes |

It renews by itself (certbot's timer). Browsers stop warning at `https://DOMAIN:8006`. Opening the server by its IP still shows its own certificate — that is the one the cluster servers check, so a new certificate never breaks the cluster. `p8 cert off` goes back.

## Prometheus and Grafana

Every server gives its numbers in Prometheus format at `https://SERVER:8006/api/v1/prometheus`: the server's CPU, RAM, pool space and uptime, and every VM's state, size, CPU, RAM, disk and network speed (on the master: the VMs of all servers). Make an API token (Settings → API tokens) and add to `prometheus.yml`:

```
scrape_configs:
  - job_name: p8s
    scheme: https
    tls_config: {insecure_skip_verify: true}
    authorization: {credentials: "p8s_YOUR_TOKEN"}
    metrics_path: /api/v1/prometheus
    static_configs: [{targets: ["192.168.0.106:8006"]}]
```

Metric names start with `p8s_` (`p8s_vm_cpu_percent{vm="web1",server="p8s-node"}`, `p8s_host_pool_free_bytes`, …). In Grafana, add Prometheus as a data source and graph them.

## Security checklist

1. Two-factor on every admin; Settings → Security → *Two-factor for staff*.
2. Limit who may open the dashboard to your networks.
3. Give people the smallest role: operator for helpers, auditor for reviewers, customer for clients.
4. Use SSH keys for VMs; turn on each internet-facing VM's firewall (`p8 fw web1 on`, then allow only needed ports).
5. Back up the encryption key (if used) and the off-site passphrase somewhere safe.
6. The **Activity** log records every change and sign-in for 180 days and is tamper-evident (**Check integrity**, **Download CSV**).

## Using it from outside

| Way | Works behind CGNAT (Jio, Airtel) | Router setup | Good for |
| --- | --- | --- | --- |
| **Tailscale** | Yes | None | You and your team from phone or laptop |
| **Built-in VPN (WireGuard)** | No | Forward UDP 51830 | A server with a real public IP |
| **Cloudflare Tunnel** | Yes | None; needs a domain | VM websites and the dashboard on a web address |

Server → **Remote access** checks for CGNAT and sets up each one. Never forward port 8006 straight to the internet.

## Updates

Server → **Updates** shows the running version and any new release; **Install** updates P8S OS with VMs kept running. Command line: `p8 update check`, `p8 update install`. Channels: `testing` (newest) or `stable` (with a subscription). Debian security updates come through apt as usual (`sudo apt update && sudo apt full-upgrade`); the EFI copies on every disk are kept equal automatically.
