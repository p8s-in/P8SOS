# Public IP pools

How hosting providers give VPSes their own internet address. You get a block
of public IPv4 addresses from your datacenter or server provider, enter it in
P8S once, and every VM created with **Public IP** gets the next free address.

- Dashboard: **Settings → Public IP pools → Add a pool**; then **+ New VM →
  Network → Public IP — <pool>**.
- CLI: `p8 ippool add dc1 203.0.113.8/29 203.0.113.9` then
  `p8 create web1 2 2048 20 --ip dc1`; `p8 ippools` shows who has which address.

Containers get addresses from the same pools (**+ Create CT → Network → Public IP**).

The address is set inside the VM by cloud-init (static, no DHCP): address,
gateway and DNS. For VMs installed from an ISO, Manage shows the values to type
into the installer. Deleting the VM returns its address to the pool; a deleted
VM recreated from its backup gets the same address back if it is still free.

**Anti-spoofing:** a VM can only send with its own MAC and its own public
address (nftables, `chain spoof` in `table bridge p8s_fw`), so one customer
can't take over another's IP. The per-VM firewall (Manage → Firewall) works
on public VMs as on any other.

**Plans:** customers can use pools marked "Customers may use this pool", up to
`public_ips` in their plan (`p8 user quota alice ips=1`).

## Which setup does my provider use?

### Bridged (the block is on the same cable as the server)

The provider's router answers for the block directly, e.g. a colocation rack
or many Indian datacenters: "your /29 is 203.0.113.8/29, gateway .9".

1. Make the bridge on the uplink once: `sudo p8 lan-setup` (creates `br0`).
2. Pool: network `203.0.113.8/29`, gateway `203.0.113.9`, bridge `br0`.
   P8S skips the network, broadcast and gateway addresses; use **Don't use**
   for addresses taken by anything else (e.g. the server itself).

Some providers (OVH, Hetzner "additional IPs") only accept traffic for an extra
IP from a **virtual MAC** they assign. P8S gives VMs their own MACs, so with
those providers use the routed setup below instead.

### Routed (the block is routed to your server's main IP)

E.g. a Hetzner subnet: "203.0.113.8/29 is routed via 198.51.100.20" (your
server). The server becomes the VMs' gateway:

```
# /etc/network/interfaces.d/vmbr1   (then: sudo ifup vmbr1)
auto vmbr1
iface vmbr1 inet static
    address 203.0.113.9/29
    bridge-ports none
    bridge-stp off
    bridge-fd 0
    up sysctl -w net.ipv4.ip_forward=1
```

Pool: network `203.0.113.8/29`, gateway `203.0.113.9`, bridge `vmbr1`.
(For a single routed address /32 the gateway may be outside the network; P8S
then adds an on-link route inside the VM.)

Note: the VM guard (VMs can't reach the server itself) covers the NAT and home
bridges; on a routed bridge, protect the host with its own firewall or with
`vm_allow`/guard settings.

## Several addresses, IPv6, changing the address

Manage → **Public IP addresses** (admins), or `p8 ip`:

| | Dashboard | CLI |
|---|---|---|
| See them | the list in Manage | `p8 ip web1` |
| One more IPv4 (same pool, up to 8 extra) | **+ Extra IPv4** | `p8 ip add web1 [203.0.113.12]` |
| Give an extra one back | ✕ next to it | `p8 ip rm web1 203.0.113.12` |
| An IPv6 address | **+ IPv6** (pool with IPv6) | `p8 ip ipv6 web1` |
| A new main address | **Change address** (empty = next free; or pick another pool) | `p8 ip change web1 [IP] [--pool dc2]` |

- **Inside the guest:** for a running VM with the QEMU guest agent (every
  P8S template has it) the new addresses are set at once and kept after
  reboots (netplan, systemd-networkd or ifupdown files; cloud-init's network
  setup is switched off so it doesn't bring the old address back). Otherwise
  the result says what to set by hand. Containers: their network files are
  rewritten and the change is made inside at once when it runs.
- **Another pool on another bridge** needs the VM/container shut down (its
  network card moves to that bridge). Extra addresses stay with the old pool's
  main address only when the pool stays; IPv6 is renewed in the new pool.
- Promote an extra address to the main one: Change address → type it.
- The anti-spoofing rules follow every change (all its IPv4s, its IPv6 and
  link-local IPv6; other IPv6 sources are dropped).

**IPv6 for a pool:** Settings → Public IP pools → **IPv6…** (or when adding:
IPv6 network + gateway), `p8 ippool ipv6 dc1 2001:db8:5::/64 fe80::1`. New VMs
then get one IPv6 address each (from `::100` on), set by cloud-init like the
IPv4 one; existing VMs get one with **+ IPv6**. The gateway is the provider's
router: an address in the network or a link-local one (often `fe80::1`).

## Not yet

IPv6 prefixes routed to a VM (a whole /64 per VM), provider virtual MACs,
extra addresses from another pool, extra addresses in backups (a restored VM
gets its main address back; add the others again), extra IPs in customer plans
(admins add them).
