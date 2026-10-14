# 6. First start, dashboard and users
Summary: The first start, signing in to the dashboard, roles and the first security steps.

## First start (10–20 minutes, needs internet)

The screen shows `[P8S OS] ...` lines while it finishes KVM, libvirt and ZFS setup, downloads the Debian VM template and builds p8sd. When it's done the login screen shows:

```
P8S OS (alpha) is ready.   Host IP: 192.168.x.x
```

Log in with your admin user and check:

```
zpool status     # tank and bpool ONLINE, every disk listed
p8 status        # p8sd running
p8 dashboard     # the dashboard address and how to sign in
p8 test          # full self-test (optional, takes a few minutes)
```

If the login screen says *first-boot setup FAILED*: log in, run `sudo tail -50 /var/log/p8s-firstboot.log`. A reboot retries.

## The dashboard

Open **https://\<server-ip>:8006** in a browser. The certificate is self-signed at first, so the browser warns once: choose *Advanced → Continue*.

The first time, the dashboard asks you to **create your dashboard admin account** with a one-time **setup code**:

1. On the server's screen (or over SSH), log in with the admin user from the install and run `cat ~/.p8s-token`. It prints a code that starts with `p8s_`.
2. In the browser: paste it into **Setup code**, choose a user name and a password (10+ characters), then **Create account**.
3. From now on you sign in with that user name and password. Keep the code secret: it is the server's master API key.

The dashboard account is separate from the Linux login, but you can use the same name.

| Area | What's there |
| --- | --- |
| VMs / Containers | Your machines; **Create**; each one's Manage page (console, options, disks, snapshots, backups, firewall) |
| Server (the server's name in the sidebar) | A full page with tabs: **Summary** (health, memory, updates, reboot), **Storage** (ZFS, network storage, Ceph), **Templates**, **Network**, **Cluster**, **Tasks**; Rename and Shell at the top |
| Cluster | Server page → Cluster: servers, joining, moves. In a cluster the sidebar lists every server (master / worker) with its VMs |
| Users | Accounts, roles, plans and quotas |
| Health & alerts | Checks, alert channels |
| Activity | Who did what (tamper-evident log) |
| Settings | Security, company login, remote access, backups |

## Users and roles

| Role | Can |
| --- | --- |
| **admin** | Everything: accounts, settings, the server shell, updates, the cluster |
| **operator** | Every VM and container: create, start/stop, console, snapshots, backups, firewall, move, HA. Not accounts, billing, settings, updates or the shell |
| **auditor** | Sees everything an admin sees and the Activity log; changes nothing |
| **customer** | Only their own VMs and containers, within their plan (quota) |

Add people under **Users** or `p8 user add NAME [--admin] [vms=2 cpus=4 ram=4096 disk=60]`. Good first steps:

1. **My account → Two-factor**: turn it on for your admin user.
2. **Settings → Security**: limit who may open the dashboard (e.g. `192.168.0.0/24`), and require two-factor for staff.
3. Company sign-in if you have one: **Company login** (Google, Microsoft Entra, Keycloak, any OpenID Connect) or **Company directory** (LDAP / Active Directory). Keep one local admin.

Locked yourself out by the address list? On the server: `sudo rm /var/lib/p8s/security.json && sudo systemctl restart p8sd`.
