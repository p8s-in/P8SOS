# 9. Networking
Summary: Private, home, public-IP, VLAN and virtual networks, bonding, IPv6, firewall and ports.

## The VM networks

|  | **Private (NAT)** — default | **Home network (LAN)** | **Public IP** | **VLAN** |
| --- | --- | --- | --- | --- |
| VM address | 192.168.122.x from the host | From your router | From a public IP pool (static) | On a tagged VLAN |
| Reachable from | The host only | Every device on the LAN | The internet | That VLAN |
| Internet | Yes | Yes | Yes | Depends on the VLAN |
| Setup | None | `sudo p8 lan-setup` once (Ethernet) | Add a pool | Switch port carries the VLAN tagged |
| Good for | Tests, build boxes, back-end services | Home and office servers | Hosting VPSes | Separating departments or customers |

**Home network setup:** plug the server into the router with a cable and run `sudo p8 lan-setup`. It turns the port into a bridge `br0`; if the network isn't back within 60 seconds the old settings return by themselves, so it's safe over SSH. Wi-Fi can't be bridged — Wi-Fi servers only have the private network.

**VMs are fenced in.** No VM can reach the P8S server itself (only DHCP, DNS, ping), and private-network VMs can't reach your home devices. Exceptions (e.g. a NAS) go in `/etc/p8s/p8sd.json` as `"vm_allow": ["192.168.0.50"]`.

## More options

| Feature | How |
| --- | --- |
| Two cables (failover or both at once) | Server → Home network → **Add as second cable**, or `sudo p8 bond-setup enp4s0 [--mode 802.3ad]` |
| IPv6 | Home-network VMs get it from your router. Private network: `sudo p8 ipv6 on` |
| Public IP pools | Settings → Public IP pools, or `p8 ippool add dc1 203.0.113.8/29 203.0.113.9`; then create with `--ip dc1`. Anti-spoofing stops a VM from using another's IP |
| Per-VM firewall | Manage → Firewall, or `p8 fw web1 on`, `p8 fw web1 allow tcp 443` |
| Datacenter firewall (every VM, every server) | Settings → **Datacenter firewall**: **IP sets** (named address lists, used as a rule's source `+office` here and in any VM's firewall), **Blocked** addresses (no VM talks to or hears from them), and **Allowed** rules every VM with its firewall on gets first. Saved on the master, applied on all servers |
| Virtual networks (one switch across every server) | Settings → **Virtual networks** → name + optional subnet (`p8 vnet-add office 10.60.0.0/24`). VMs on it talk to each other wherever they run and keep their addresses when they move; with a subnet the master gives addresses (DHCP), DNS and internet. VXLAN inside the cluster tunnel; cards get MTU 1230. VMs only (not containers) |
| A VM on several networks | Manage → Network & firewall → **Add network card** (private network, home network or a VLAN; up to 8 cards, plugged in while it runs). For router/firewall VMs (pfSense, OPNsense) or a database on a private VLAN. The VM's firewall covers every card; a public IP stays on the first card |
| Websites without opening ports (Cloudflare Tunnel) | Settings → Public access (domain on Cloudflare + API token), then `p8 publish web1 shop 80` → `https://shop.yourdomain`. Works behind CGNAT |
| Fixed LAN address for a VM | Reserve its MAC (from `p8 show NAME`) in your router's DHCP settings |

## Ports the server uses

| Port | For |
| --- | --- |
| TCP 8006 | Dashboard and API (HTTPS); cluster joining |
| UDP 51820 | Cluster tunnel (WireGuard) |
| TCP 22 | SSH to the server |
| Ceph: TCP 3300, 6789, 6800–7300 | Only between cluster servers when Ceph is on |
