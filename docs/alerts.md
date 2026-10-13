# Health & alerts

Server page → **Health & alerts** (admins). P8S checks the server every minute and tells you
**before** a problem becomes downtime or lost data.

| Watched | Warning | Critical |
|---|---|---|
| ZFS pool health | — | DEGRADED, FAULTED, UNAVAIL… (a disk failed or is missing) |
| ZFS pool space | ≥ 80 % full | ≥ 90 % |
| System disk / backup disk space | ≥ 85 % | ≥ 95 % |
| RAM really in use (caches not counted) | ≥ 92 % | ≥ 98 % |
| CPU temperature | ≥ 85 °C | ≥ 95 °C |
| Disks (SMART, every 30 min; `smartmontools`) | reallocated / pending / unreadable sectors, NVMe media errors or ≥ 90 % wear, drive ≥ 55 °C | the disk reports FAILING, NVMe critical warning |
| VMs | — | crashed |
| Backups / restores | failed in the last 24 h | — |
| Power: UPS (NUT `upsc`) or laptop battery | on battery, UPS battery needs replacing | battery almost empty (UPS "LB", laptop ≤ 10 %) |

All levels can be changed. Each alert is sent once when it starts, again if it gets worse, and
once when it is fixed ("RESOLVED"):

- **Email** to the listed addresses (uses the email server set under Settings).
- **Webhook**: a JSON POST with `text` (Slack, Mattermost, Teams), `content` (Discord) and
  `title, severity, event, check, host, time, detail` for anything else.
- **Phone push with ntfy**: install the ntfy app, subscribe to a hard-to-guess topic, enter
  `https://ntfy.sh/<topic>` (or your own ntfy server). Critical alerts arrive as urgent.

**Send a test** checks all three.

## Power cuts and overheating

With *"On a power cut, shut down cleanly"* on (the default), when the battery is nearly empty
P8S shuts every VM and container down (up to 4 minutes, then forced) and powers the server off,
so no disk is left half-written. Laptops use their own battery (shutdown at 10 % by default);
servers use a UPS through NUT:

```bash
sudo apt install nut
# /etc/nut/ups.conf:  [ups]  driver = usbhid-ups  port = auto
# /etc/nut/nut.conf:  MODE=standalone   (then: sudo systemctl restart nut-server)
upsc ups@localhost        # should print battery.charge, ups.status ...
```

Then enter `ups@localhost` as the UPS name. Turn the server's BIOS option "power on after power
loss" on so it starts again when the power is back; autostart VMs come back by themselves.

**Overheat shutdown** (off by default): with a limit set (70–110 °C), two readings in a row at
or above it shut the guests down and power the server off.

## API (admins)

`GET /alerts` → `{active, checks, history, settings}` · `PUT /alerts/settings` (same fields as
`settings`) · `POST /alerts/test`. Settings and history are in `<data_dir>/alerts.json`.
