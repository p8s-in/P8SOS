# Virtual networks (SDN)

A **virtual network** is a switch that spans every server of the cluster (like
Proxmox SDN's VXLAN zone). VMs on the same virtual network talk to each other
directly, wherever they run, and keep their addresses when they move or fail
over to another server.

## Using it

1. Settings → **Virtual networks** → name (`office`), optionally a subnet
   (`10.60.0.0/24`) → **Add**. Or `p8 vnet-add office 10.60.0.0/24`.
2. New VM: Network → *Virtual network — office* (`p8 create web3 --net office`).
   Existing VM: Manage → Network & firewall → **Add network card** →
   *Virtual network — office* (`p8 nic-add web1 vnet:office`).

| | With a subnet | Without a subnet |
| --- | --- | --- |
| Addresses | DHCP from the master (from the 10th address on) | Set inside the VMs |
| Gateway / DNS | The master: the first address (10.60.0.1) | None |
| Internet | Yes (NAT through the master), unless "internet" is off | No |
| Reach from your home network | No (like the private network) | No |

A server on its own can have virtual networks too (they are then local
switches with the server as their gateway); when it joins a cluster, they span
every server.

## How it works

- Each virtual network is a VXLAN (VNI 10000, 10001 …) carried **inside the
  cluster's WireGuard tunnel**: on every server a device `vxl<VNI>` in a bridge
  `vxb<VNI>`. Broadcasts go to every other server's tunnel address (no
  multicast needed); the rest is learned. Servers that can't reach each other
  directly go through the master, like the rest of the cluster traffic.
- VXLAN packets (UDP 4789) are accepted only from the tunnel: no one on the
  home network can inject frames.
- Frames are at most 1230 bytes (the tunnel's 1280 minus VXLAN's 50), so VM
  cards on a virtual network get **MTU 1230** (set by the host and by DHCP).
- The gateway runs on the **master**: an address on the bridge, `dnsmasq`
  (service `p8s-vnet-NAME`) for DHCP and DNS, and NAT (nftables table
  `inet p8s_sdn`). VMs reach the master only for DHCP, DNS and ping, and not
  the private ranges (10/8, 172.16/12, 192.168/16) beyond their own subnet —
  the same fence as the private network. When another server becomes master,
  it takes the gateway over (same address; leases start again).
- The settings live in `/var/lib/p8s/sdn.json` on the master, are sent to
  every worker when saved, and travel in the cluster book (a worker that was
  off picks them up within a minute). p8sd checks the devices every minute;
  `sdn.list` lets `p8s-vlans.service` make the bridges at boot, before
  libvirt starts the VMs.
- The firewall of each VM works on virtual networks as everywhere else.

## Limits (v1)

- VMs only (containers: private or home network).
- IPv4 gateway/DHCP; IPv6 inside a virtual network works between VMs only if
  they use MTU 1280 or less (1230 is below IPv6's minimum, so in practice IPv4).
- A virtual network that VMs still use can't be removed or get another VNI.
- One gateway (the master): if it is down, VMs keep their addresses until
  their lease runs out (12 hours) and talk to each other, but have no internet
  until a new master takes over.

## Commands

| | |
| --- | --- |
| `p8 vnets` | list them |
| `p8 vnet-add NAME [SUBNET] [--no-internet] [--comment TEXT]` | add one |
| `p8 vnet-rm NAME` | remove one |
| `p8 create NAME --net NAME` / `p8 nic-add VM vnet:NAME` | put a VM on one |

On a server: `ip -d link show vxl10000`, `bridge fdb show dev vxl10000`,
`systemctl status p8s-vnet-office`, `nft list table inet p8s_sdn`.
