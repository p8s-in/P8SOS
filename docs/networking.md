# VM networks

Every VM is on one of two networks. You choose when you create it.

| | **nat** (default) | **lan** (home network) |
|---|---|---|
| VM's IP | 192.168.122.x, from the host | 192.168.0.x, from your router |
| Who can reach the VM | only the host (`p8 ssh`) | every PC and phone at home |
| VM can reach the internet | yes | yes |
| Setup | none | once: `sudo p8 lan-setup` (Ethernet cable) |
| Good for | experiments, build boxes | web servers, game servers, anything you open from another device |

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
