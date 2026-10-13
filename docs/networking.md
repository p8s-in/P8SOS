# VM networks

Every VM is on one of two networks. You choose when you create it.

| | **nat** (default) | **lan** (home network) |
|---|---|---|
| VM's IP | 192.168.122.x, from the host | 192.168.0.x, from your router |
| Who can reach the VM | only the host (`p8 ssh`) | every PC and phone at home |
| VM can reach the internet | yes | yes |
| Setup | none | once: `sudo p8 lan-setup` (Ethernet cable) |
| Good for | experiments, build boxes | web servers, game servers, anything you open from another device |

## VMs can't touch the server or your home devices

P8S OS keeps every VM away from the P8S server itself (no dashboard, no SSH, nothing but DHCP/DNS/ping)
and keeps **private-network** VMs away from your home network (192.168.x.x, 10.x, 172.16-31.x).
They still reach the internet. The server can still reach every VM (`p8 ssh`, console).
Home-network VMs are on your LAN by choice, but they can't reach the server either.

Exceptions, e.g. a NAS a VM should use, go in `/etc/p8s/p8sd.json`: `"vm_allow": ["192.168.0.50"]`.
To switch the protection off: `"vm_guard": false`. Then `sudo systemctl restart p8sd`.
Check it's on: `sudo nft list table inet p8s_guard`.

## Turn on the home network (once)

1. Plug the host into your router with an **Ethernet cable**.
   Wi-Fi can't be bridged: a Wi-Fi card only sends traffic for its own address.
2. On the host:
   ```
   sudo p8 lan-setup
   ```
   It shows what it will change and asks. It turns the Ethernet port into a bridge
   `br0`. The network drops for a few seconds; the host normally keeps the same IP
   (br0 copies the port's MAC). **If the network isn't back within 60 s, the old
   settings come back by themselves.** Safe to run over SSH.
3. Done. It survives reboots.

## Make a VM on the home network

```
p8 create web2 2 2048 20 --lan
p8 list                         # NET column says lan; IP is 192.168.0.x
```

Dashboard: **+ New VM** → Network → **Home network**.

Then from your Windows PC: `ssh debian@<that IP>` (same SSH key needed — or use
`p8 ssh web2` on the host). A web server in the VM is at `http://<that IP>`.

Tip: give the VM a fixed address in your router's DHCP settings ("address
reservation") using the MAC from `p8 show web2`.

## If something goes wrong

| What you see | What to do |
|---|---|
| `The host is online through Wi-Fi` | Use an Ethernet cable; the private (nat) network keeps working meanwhile |
| `old settings were restored` | `sudo cat /var/lib/p8s/bridge/apply.log` → send it |
| LAN VM shows no IP | Router's DHCP on? Check `p8 bootlog web2`. The IP shows once the guest agent runs (~1 min) |
| Want to undo | `sudo cp /var/lib/p8s/bridge/interfaces.before-bridge /etc/network/interfaces && sudo reboot`, then remove `"lan_bridge"` from `/etc/p8s/p8sd.json` |

## SSH from other computers (SSH ports)

VMs and containers on the **Private (NAT)** network have addresses like `192.168.122.x` / `10.0.3.x` that exist
only inside the server, so `ssh root@192.168.122.50` works on the server but not from your PC.
P8S OS therefore gives each of them an **SSH port on the server** (like NAT VPS providers):

```
ssh -p 2205 debian@192.168.0.106        # server address + the guest's port
```

- The dashboard shows it under the IP address ("SSH port 2205"); click it to copy the command.
- Ports start at 2201, stay with a guest until it is deleted (`/var/lib/p8s/ssh-ports.json`), and are only
  shown to the guest's owner. p8sd relays the connection to the guest's port 22.
- VMs and containers on the home network or with a public IP don't need this (they are reachable directly).
- Turn off with `"ssh_ports": false` in `/etc/p8s/p8sd.json`, or move with `"ssh_port_base": 30001`.
- Login: cloud VMs use their image user (`debian`, `ubuntu`, …) with your SSH key, or `root` with the password
  you set; containers use `root`.

## VLANs (tagged networks)

For offices and datacenters where the switch carries several VLANs on the
server's cable. Needs the home network (`sudo p8 lan-setup`) first.

- Create VM → Network **Home network** → **VLAN** 1-4094 (or `p8 create web1 --vlan 100`).
  The VM sees only that VLAN, untagged, and gets its address from that VLAN's DHCP/router.
- The switch port the server is plugged into must carry the VLAN **tagged**
  (trunk). The untagged network stays the normal home network.
- How it works: for VLAN 100 P8S makes `p8sv100` (a VLAN device on the
  network port behind br0) and a bridge `br0v100`; the server itself gets no
  address there. The list is in `/var/lib/p8s/vlans.list` and
  `p8s-vlans.service` makes them again at boot, before the VMs start.
- Moves, backups and HA keep the VLAN; a server without the VLAN puts the
  VM on the private network and says so.
- Per-VM firewall rules work the same on VLANs.

## Two cables (bonding, failover)

Server page → Home network shows the network ports. With a second Ethernet
port: pick it and **Add as second cable** (or `sudo p8 bond-setup enp4s0`).
The home-network bridge then runs on `bond0` made of both cables:

- default mode **active-backup**: one cable carries the traffic, the other
  takes over within a second if the cable, port or switch dies. Works with
  any switch.
- `sudo p8 bond-setup enp4s0 --mode 802.3ad`: both cables at once (more
  speed) — the switch ports must be set up as an LACP group.

The network drops for a few seconds while switching; if it isn't back within
60 s the old settings come back by themselves (log:
`/var/lib/p8s/bond/apply.log`). VMs stay plugged in; VLAN bridges move onto
the bond too. Check: `cat /proc/net/bonding/bond0`.

## IPv6

- **Home network VMs** get IPv6 from your router, if it offers it — nothing to do.
- **Private (NAT) network:** Server page → Home network → **Turn on IPv6 for the
  private network** (or `sudo p8 ipv6 on`). The NAT network gets a private
  IPv6 range (fdXX:…/64, the same each time) and is NATed to the server's
  own IPv6, like IPv4. Needs IPv6 internet on the server. VMs get an
  address via DHCPv6 / router advertisements after their network restarts.
  The guard rules still keep VMs off the server and private networks.
