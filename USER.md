# P8S OS — Test & Deploy Guide (Phase 1)

This guide takes you from "code in a folder" to "a VM created by p8sd that you can SSH into",
and then through every check worth doing. Work top to bottom; each part ends with a
**✅ Check** so you know it worked before moving on.

> **What Phase 1 is:** one daemon (`p8sd`) on one Debian 13 host. It creates, starts, stops
> and deletes KVM VMs from a ZFS template through a REST API. No web UI, no tenants, no
> clustering yet — see [Known limits](#11-known-limits-dont-report-these-as-bugs).

---

## Contents

1. [Pick your test setup](#1-pick-your-test-setup)
2. [Level 0 — unit tests (any computer, 2 minutes)](#2-level-0--unit-tests-any-computer)
3. [Create the test VM (nested virtualization)](#3-create-the-test-vm)
4. [Install Debian 13 in the test VM](#4-install-debian-13-in-the-test-vm)
5. [Copy the code and install Go](#5-copy-the-code-and-install-go)
6. [Prepare the host: KVM, libvirt, ZFS](#6-prepare-the-host-kvm-libvirt-zfs)
7. [Import the Debian 13 template](#7-import-the-debian-13-template)
8. [Install, configure and start p8sd](#8-install-configure-and-start-p8sd)
9. [Test it — automatic smoke test](#9-test-it--automatic-smoke-test)
10. [Test it — by hand, step by step](#10-test-it--by-hand-step-by-step)
11. [Known limits](#11-known-limits-dont-report-these-as-bugs)
12. [Troubleshooting](#12-troubleshooting)
13. [Updating, resetting and uninstalling](#13-updating-resetting-and-uninstalling)
14. [Command cheat sheet](#14-command-cheat-sheet)
15. [Reporting a problem](#15-reporting-a-problem)

---

## 1. Pick your test setup

p8sd runs VMs, so the machine it runs on needs hardware virtualization (`/dev/kvm`).
If that machine is itself a VM, you need **nested virtualization**.

| Option | Where p8sd runs | Good for | Notes |
|---|---|---|---|
| **A. VM on Proxmox** (recommended) | Debian 13 VM on the FX505GT's Proxmox | Safe testing, snapshots, easy reset | Needs nested virt — section 3A |
| B. VM on Windows | Debian 13 VM in VMware Workstation or Hyper-V | Testing without the laptop | Section 3B; VirtualBox nested is slow/unreliable |
| C. Bare metal | Debian 13 directly on a machine | Final real-world test | Wipes the machine; skip section 3 |

**Minimum for the test VM:** 4 vCPU, 8 GB RAM, 32 GB OS disk, plus a **second empty disk
(64 GB+)** for the ZFS pool. Each test guest uses 1 GB RAM by default.

---

## 2. Level 0 — unit tests (any computer)

These run without root, KVM or ZFS — every host command is faked. Do this first; if it
fails, nothing later will work.

```bash
cd p8sos                # or L:\P8SOS on Windows (needs Go 1.24+ from go.dev/dl)
go vet ./...
go test ./...
```

✅ **Check:** every package prints `ok`. On Linux you can also run `make test` (adds the race detector).

---

## 3. Create the test VM

### 3A. On Proxmox (FX505GT)

**Step 1 — enable nested virtualization on the Proxmox host** (shell on the Proxmox node):

```bash
cat /sys/module/kvm_intel/parameters/nested      # Y or 1 = already on
```

If it says `N`:

```bash
echo "options kvm-intel nested=Y" > /etc/modprobe.d/kvm-intel.conf
# stop all running VMs first, then:
modprobe -r kvm_intel && modprobe kvm_intel
cat /sys/module/kvm_intel/parameters/nested      # should now be Y
```

**Step 2 — create the VM** (web UI → Create VM):

| Tab | Setting |
|---|---|
| General | Name: `p8s-test` |
| OS | Debian 13 netinst ISO (download from debian.org, upload to `local` storage) |
| System | Machine `q35`, BIOS `SeaBIOS` (default) |
| Disks | `scsi0` 32 GB (OS). Then **Hardware → Add → Hard Disk** `scsi1` 64 GB (for ZFS) |
| CPU | 4 cores, **Type: `host`** ← required for nested KVM |
| Memory | 8192 MB, untick Ballooning |
| Network | `vmbr0`, VirtIO |

Before first boot, take a Proxmox **snapshot** later (after section 4) so you can roll back
to a clean Debian install any time.

### 3B. On Windows

- **VMware Workstation:** VM Settings → Processors → tick **Virtualize Intel VT-x/EPT**. Add a second disk.
- **Hyper-V** (PowerShell as admin, VM powered off):
  ```powershell
  Set-VMProcessor -VMName p8s-test -ExposeVirtualizationExtensions $true
  Get-VMNetworkAdapter -VMName p8s-test | Set-VMNetworkAdapter -MacAddressSpoofing On
  ```
  Use a Generation 2 VM with **Secure Boot off** (ZFS's kernel module is not signed).

---

## 4. Install Debian 13 in the test VM

1. Boot the netinst ISO → **Install** (text installer is fine).
2. Hostname `p8s-test`, create your normal user, set a root password.
3. Partitioning: **Guided – use entire disk** on the **first** disk only (32 GB). Leave the second disk untouched.
4. Software selection: tick only **SSH server** and **standard system utilities**.
5. Reboot, log in, then:

```bash
sudo apt update && sudo apt install -y sudo git make curl jq   # if sudo is missing, use su - first
sudo usermod -aG sudo $USER                                    # log out/in after this
ip -br addr                                                     # note the VM's IP
lsblk                                                           # note the empty disk, e.g. sdb
```

✅ **Check — virtualization works inside the VM:**

```bash
grep -cE 'vmx|svm' /proc/cpuinfo     # must be > 0
ls -l /dev/kvm                        # appears after libvirt is installed in section 6
```

If the first command prints `0`, nested virt is off — go back to section 3.

👉 **Take a Proxmox snapshot now** (`clean-debian`).

---

## 5. Copy the code and install Go

**From Windows** (PowerShell, OpenSSH is built in). Replace the IP and user:

```powershell
scp -r L:\P8SOS you@192.168.1.50:~/p8sos
```

Or push it to a private Git repo and `git clone` inside the VM.

**In the VM:**

```bash
sudo apt install -y golang-go
go version            # needs go1.24 or newer
cd ~/p8sos
go test ./...         # same as section 2, now on the target
```

✅ **Check:** all `ok`.

> If Debian's Go is older than 1.24, install from go.dev:
> `curl -LO https://go.dev/dl/go1.24.7.linux-amd64.tar.gz && sudo tar -C /usr/local -xzf go1.24.7.linux-amd64.tar.gz && export PATH=/usr/local/go/bin:$PATH`

---

## 6. Prepare the host: KVM, libvirt, ZFS

```bash
cd ~/p8sos
lsblk                                         # confirm the empty disk name again!
sudo scripts/bootstrap-debian13.sh /dev/sdb   # ← WIPES /dev/sdb. Type "yes" when asked.
```

What it does: enables Debian `contrib`, installs QEMU/KVM, libvirt, ZFS (builds a kernel
module — **takes 3–10 minutes**), cloud-init tools; creates pool `tank` and datasets
`tank/p8s/vms` and `tank/p8s/templates`.

> No spare disk? Run it with no argument: it makes a 64 GB sparse file pool. Fine for
> testing, slow, never for real use.

### Networking for the test guests

For the first test, use libvirt's built-in NAT network. It needs no bridge setup and can't
break your SSH connection:

```bash
sudo virsh net-start default
sudo virsh net-autostart default
ip -br addr show virbr0          # should show 192.168.122.1/24
```

You'll set `"bridge": "virbr0"` in the config (section 8). Guests get `192.168.122.x`
addresses, reachable **from the test VM** (not from your Windows PC directly).

> Later, to put guests on your real LAN, create `vmbr0` inside the test VM as in
> [docs/host-setup.md](docs/host-setup.md) step 2, and use `"bridge": "vmbr0"`. In Proxmox
> nested setups, also disable the **MAC filter** on the test VM's network device
> (VM → Firewall → Options) or guests won't get DHCP.

✅ **Check:**

```bash
lsmod | grep -w zfs                   # zfs module loaded
sudo zpool status tank                # state: ONLINE
sudo zfs list -r tank/p8s             # tank/p8s, tank/p8s/templates, tank/p8s/vms
systemctl is-active libvirtd          # active
sudo virsh -c qemu:///system version  # shows QEMU/libvirt versions
ls -l /dev/kvm                        # exists
which cloud-localds                   # /usr/bin/cloud-localds
```

---

## 7. Import the Debian 13 template

```bash
sudo scripts/make-template.sh
```

It downloads the official Debian 13 cloud image, **verifies its SHA-512 checksum**, writes
it into a ZFS volume and snapshots it as `tank/p8s/templates/debian-13@base`. At the end it
prints a JSON line — keep it for section 8.

✅ **Check:**

```bash
sudo zfs list -t snapshot -r tank/p8s/templates
# NAME                                  USED  AVAIL  REFER  MOUNTPOINT
# tank/p8s/templates/debian-13@base      0B      -   ...    -
```

---

## 8. Install, configure and start p8sd

### 8.1 Build and install

```bash
cd ~/p8sos
make test
sudo make install
```

This installs `/usr/local/bin/p8sd`, the systemd unit, and copies the example config to
`/etc/p8s/p8sd.json` (only if it doesn't exist yet).

### 8.2 Create an API token

```bash
sudo p8sd token -name admin
```

Output has two parts:

- `p8s_...` — **the token**. You send this in API calls. It is shown once — save it in a password manager.
- `{"name":"admin","sha256":"..."}` — **the hash**. This goes in the config file.

> Common mistake: putting the token in the config, or using the hash in `curl`. Token → curl. Hash → config.

### 8.3 Edit the config

```bash
sudoedit /etc/p8s/p8sd.json
```

Set at least these (for the NAT test setup):

```json
{
  "listen": "127.0.0.1:8006",
  "data_dir": "/var/lib/p8s",
  "libvirt_uri": "qemu:///system",
  "vm_dataset": "tank/p8s/vms",
  "bridge": "virbr0",
  "max_vcpus": 4,
  "max_memory_mib": 4096,
  "max_disk_gb": 50,
  "tokens": [
    { "name": "admin", "sha256": "PASTE_THE_HASH_HERE" }
  ],
  "templates": [
    { "name": "debian-13", "snapshot": "tank/p8s/templates/debian-13@base", "min_disk_gb": 3, "os_variant": "debian13" }
  ]
}
```

Use the `min_disk_gb` number that `make-template.sh` printed.

### 8.4 Start it

```bash
sudo systemctl enable --now p8sd
systemctl status p8sd --no-pager
journalctl -u p8sd -n 20 --no-pager
```

✅ **Check:**

```bash
curl -s http://127.0.0.1:8006/api/v1/health
# {"status":"ok","version":"..."}
```

- Logs show `p8sd listening` and **no** `template snapshot missing` warning.
- If the service fails to start, `journalctl -u p8sd` prints the exact config error (bad hash, missing field, etc.).

---

## 9. Test it — automatic smoke test

One script runs the whole lifecycle and checks the API **and** the host (libvirt, ZFS, seed
files, and SSH into the guest):

```bash
ssh-keygen -t ed25519           # only if ~/.ssh/id_ed25519.pub doesn't exist yet
export P8S_TOKEN=p8s_...        # your token from 8.2
cd ~/p8sos
scripts/smoke-test.sh
```

It takes 2–4 minutes (mostly waiting for the guest to boot and cloud-init to finish).
Expected end:

```
== result: 39 passed, 0 failed
```

What it covers:

| Stage | What is checked |
|---|---|
| 1. Health & auth | `/health` open; missing/wrong token → 401; template listed |
| 2. Validation | bad name, unknown template, vcpus over limit, unknown field, bad JSON → 400; missing VM → 404 |
| 3. Create | 201, state `running`, duplicate name → 409, shows in list |
| 4. Host side | libvirt domain running; ZFS disk exists, is a **clone** of the template, has the requested size; seed ISO exists |
| 5. Guest | gets an IP; SSH as `debian` works; hostname = VM name; root disk grew to ~10 GB; guest agent running |
| 6. Power | ACPI shutdown finishes; stop/start are idempotent; force stop is immediate |
| 7. Delete | 204; then 404; domain, disk and seed ISO all gone; template untouched |

Options:

```bash
KEEP=1 scripts/smoke-test.sh          # leave the VM running afterwards to poke at it
TEMPLATE=debian-13 PUBKEY=~/.ssh/other.pub scripts/smoke-test.sh
```

If you press Ctrl+C, the script still deletes its test VM (unless `KEEP=1`).

---

## 10. Test it — by hand, step by step

Do this at least once so you see what each call does.

### 10.1 Setup

```bash
export P8S=http://127.0.0.1:8006/api/v1
export TOK=p8s_...
alias p8="curl -s -H \"Authorization: Bearer \$TOK\" -H 'Content-Type: application/json'"
```

### 10.2 Create a VM

```bash
p8 -X POST $P8/vms -d '{
  "name": "web1",
  "template": "debian-13",
  "vcpus": 2,
  "memory_mib": 2048,
  "disk_gb": 20,
  "ssh_keys": ["'"$(cat ~/.ssh/id_ed25519.pub)"'"]
}' | jq
```

✅ **Check:** returns the VM with `"state": "running"`. Then look at the host:

```bash
sudo virsh list --all                                   # p8s-web1  running
sudo zfs list -o name,origin,volsize,used -r tank/p8s/vms
#   tank/p8s/vms/p8s-web1-disk0  tank/p8s/templates/debian-13@base  20G  <small>
ls -l /var/lib/p8s/seeds/                               # p8s-web1-seed.iso
sudo virsh dumpxml p8s-web1 | less                      # see the generated domain + <metadata>
```

Notice `used` is tiny — the disk is a copy-on-write clone, not a full copy.

### 10.3 Log into the guest

```bash
sudo virsh domifaddr p8s-web1           # wait ~30s after create; shows 192.168.122.x
ssh debian@192.168.122.x
```

Inside the guest:

```bash
hostname                                 # web1
df -h /                                  # ~20G — growpart worked
nproc; free -m                           # 2 CPUs, ~2 GB
cloud-init status --long                 # status: done
systemctl is-active qemu-guest-agent     # active (may take a minute — it installs on first boot)
exit
```

No IP? Watch it boot: `sudo virsh console p8s-web1` (exit with `Ctrl+]`).

### 10.4 List and get

```bash
p8 $P8/vms | jq
p8 $P8/vms/web1 | jq
p8 $P8/templates | jq
```

### 10.5 Power

```bash
p8 -X POST $P8/vms/web1/stop | jq .state           # "stopping" (polite ACPI shutdown)
sleep 15; p8 $P8/vms/web1 | jq .state               # "stopped"
p8 -X POST $P8/vms/web1/start | jq .state           # "running"
p8 -X POST $P8/vms/web1/stop -d '{"force":true}' | jq .state   # "stopped" at once (power cut)
```

### 10.6 Errors you should see

```bash
p8 -X POST $P8/vms -d '{"name":"web1","template":"debian-13"}' | jq   # 409 exists
p8 -X POST $P8/vms -d '{"name":"Web_1","template":"debian-13"}' | jq  # 400 invalid name
p8 -X POST $P8/vms -d '{"name":"big","template":"debian-13","vcpus":64}' | jq   # 400 limit
p8 -X POST $P8/vms -d '{"name":"x","tempalte":"debian-13"}' | jq      # 400 unknown field (typo)
p8 $P8/vms/nope | jq                                                   # 404
curl -s $P8/vms | jq                                                   # 401 no token
```

### 10.7 Delete

```bash
p8 -X DELETE $P8/vms/web1 -w '%{http_code}\n'      # 204
sudo virsh list --all                              # p8s-web1 gone
sudo zfs list -r tank/p8s/vms                      # disk gone
sudo zfs list -t snapshot -r tank/p8s/templates    # template still there
```

### 10.8 Extra checks worth doing

| Test | How | Expected |
|---|---|---|
| **Rollback on failure** | Set `"bridge": "nobr0"` in config, `sudo systemctl restart p8sd`, create a VM | API returns 500; `virsh list --all`, `zfs list -r tank/p8s/vms` and `/var/lib/p8s/seeds` show **nothing left behind**. Logs explain why. Set the bridge back afterwards. |
| **Restart safety** | Create a VM, `sudo systemctl restart p8sd`, `GET /vms` | VM still listed and running (libvirt is the source of truth) |
| **Hand-made VMs ignored** | Create a VM named without `p8s-` via `virt-install` or `virsh`, then `GET /vms` | Not listed; p8sd never touches it |
| **Parallel creates** | `for i in 1 2 3; do p8 -X POST $P8/vms -d '{"name":"par'$i'","template":"debian-13"}' & done; wait` | All three created, distinct MACs/IPs |
| **Same name in parallel** | Run the same create twice at once with `&` | One 201, one 409 |
| **Custom cloud-init** | Create with `"user_data": "#cloud-config\npackages: [nginx]\n"` | nginx installed in the guest (your SSH keys are **not** added — custom user_data replaces the default) |
| **Template protection** | Try `sudo zfs destroy tank/p8s/templates/debian-13@base` while VMs exist | ZFS refuses — clones depend on it |
| **Disk space** | `sudo zfs list -o name,used,refer -r tank` before/after 5 VMs | Usage grows only by what guests write |
| **API from Windows** | On Windows: `ssh -L 8006:127.0.0.1:8006 you@<test-vm-ip>`, then in Git Bash/WSL use `curl http://127.0.0.1:8006/api/v1/health` | Works through the tunnel; the API itself stays on localhost |
| **Host reboot** | `sudo reboot` the test VM | p8sd comes back by itself; guests come back **stopped** (no autostart yet) — start them via API |

---

## 11. Known limits (don't report these as bugs)

- No web UI or console in the API (Phase 3). Use `virsh console` for now.
- The API doesn't show the guest's IP yet — use `virsh domifaddr`.
- Guests don't auto-start after a host reboot.
- One network card per VM, on the single bridge from the config.
- Any token can do everything (tenants and scoped tokens come in Phase 2).
- Create waits until the VM is started (a few seconds); no background job queue yet.
- If p8sd is killed **during** a create, leftovers can stay — remove them with `DELETE` or by hand (section 13).
- Changing CPU/RAM/disk of an existing VM, snapshots, backups — not yet.

---

## 12. Troubleshooting

First look at the logs — they always contain the failing command and its error:

```bash
journalctl -u p8sd -n 50 --no-pager
```

| Symptom | Likely cause | Fix |
|---|---|---|
| `grep -c vmx /proc/cpuinfo` = 0 | Nested virt off | Section 3; Proxmox CPU type must be `host` |
| `/dev/kvm` missing | Same as above, or kvm module not loaded | `sudo modprobe kvm_intel` |
| `modprobe: FATAL: Module zfs not found` | ZFS module build failed, or kernel upgraded without matching headers | `sudo apt install linux-headers-$(uname -r) && sudo dpkg-reconfigure zfs-dkms`, then reboot |
| ZFS module won't load, "Key was rejected" | Secure Boot on | Disable Secure Boot in the VM/BIOS |
| p8sd won't start: `sha256 must be 64 lowercase hex chars` | Token pasted instead of hash, or placeholder left | Put the **hash** from `p8sd token` in the config |
| p8sd won't start: `address already in use` | Something else on 8006 (e.g. Proxmox UI when testing on the Proxmox host) | Change `listen` to `127.0.0.1:8106` |
| `401 unauthorized` | Wrong token, extra spaces, or using the hash | `echo -n "$TOK" \| sha256sum` must equal the hash in config |
| `template "debian-13" snapshot ... is missing` | Template not imported or name mismatch | Section 7; compare with `sudo zfs list -t snapshot` |
| Create → 500, log says `Cannot get interface MTU on 'virbr0'` | NAT network not started | `sudo virsh net-start default` |
| …log says `Cannot get interface MTU on 'vmbr0'` | Bridge doesn't exist | Use `virbr0` or create the bridge |
| Create → 500, `cloud-localds: not found` | Package missing | `sudo apt install cloud-image-utils` |
| Create → 500, `Permission denied` on `/dev/zvol/...` | AppArmor | `sudo journalctl -k \| grep -i apparmor` and send me the lines |
| VM running but no IP | Still booting, or network down | Wait 60s; `sudo virsh console p8s-<name>`; check `sudo virsh net-dhcp-leases default` |
| SSH `Permission denied (publickey)` | Key not passed, or custom `user_data` used | Check `ssh_keys` in the create call; user is `debian` |
| Stop leaves VM in `stopping` forever | Guest ignores ACPI (still booting / no OS) | Use `{"force": true}` |
| `df -h /` in guest shows ~3 GB | growpart didn't run | In guest: `sudo cloud-init status --long`, `sudo journalctl -u cloud-init` |

---

## 13. Updating, resetting and uninstalling

**Update p8sd after code changes:**

```bash
cd ~/p8sos && git pull     # or scp the folder again
make test && sudo make install && sudo systemctl restart p8sd
```

`make install` never overwrites your existing `/etc/p8s/p8sd.json`.

**Delete every p8sd VM** (API first, then by hand for leftovers):

```bash
for n in $(p8 $P8/vms | jq -r '.vms[].name'); do p8 -X DELETE $P8/vms/$n; done
# leftovers after a crash:
for d in $(sudo virsh list --all --name | grep '^p8s-'); do sudo virsh destroy $d; sudo virsh undefine $d --nvram; done
sudo zfs list -H -o name -r tank/p8s/vms | grep -- '-disk0$' | xargs -r -n1 sudo zfs destroy -r
sudo rm -f /var/lib/p8s/seeds/*.iso
```

**Start completely fresh:** roll back the Proxmox snapshot `clean-debian`.

**Uninstall:**

```bash
sudo systemctl disable --now p8sd
sudo rm /usr/local/bin/p8sd /etc/systemd/system/p8sd.service && sudo systemctl daemon-reload
sudo rm -rf /etc/p8s /var/lib/p8s
sudo zpool destroy tank          # ← destroys all VM disks and templates
```

---

## 14. Command cheat sheet

| Want to… | Command |
|---|---|
| Service status / logs | `systemctl status p8sd` · `journalctl -u p8sd -f` |
| All libvirt VMs | `sudo virsh list --all` |
| A VM's IP | `sudo virsh domifaddr p8s-<name>` |
| Serial console | `sudo virsh console p8s-<name>` (exit `Ctrl+]`) |
| Domain definition | `sudo virsh dumpxml p8s-<name>` |
| VM disks | `sudo zfs list -o name,origin,volsize,used -r tank/p8s/vms` |
| Templates | `sudo zfs list -t snapshot -r tank/p8s/templates` |
| Pool health | `sudo zpool status tank` · `sudo zpool list` |
| DHCP leases (NAT) | `sudo virsh net-dhcp-leases default` |
| New token | `sudo p8sd token -name <label>` → add hash → `sudo systemctl restart p8sd` |
| Daemon version | `p8sd version` · `curl -s $P8/health` |

Full API reference: [docs/api.md](docs/api.md) · Design: [docs/architecture.md](docs/architecture.md) · Plan: [docs/roadmap.md](docs/roadmap.md)

---

## 15. Reporting a problem

When something fails, collect this and paste it to me (remove tokens first):

```bash
{
  echo "== versions";  p8sd version; uname -r; virsh --version; zfs version
  echo "== config";    sudo sed -E 's/"sha256": *"[0-9a-f]+"/"sha256":"<hidden>"/' /etc/p8s/p8sd.json
  echo "== logs";      journalctl -u p8sd -n 80 --no-pager
  echo "== libvirt";   sudo virsh list --all
  echo "== zfs";       sudo zfs list -t all -r tank/p8s
  echo "== seeds";     ls -l /var/lib/p8s/seeds
} > p8s-report.txt 2>&1
```

Plus: the exact command you ran, what you expected, and what you got. If the smoke test
failed, include its full output.
