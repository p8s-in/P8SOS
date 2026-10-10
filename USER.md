# P8S OS — Try it in VMware or VirtualBox

You'll make one Debian 13 VM on your Windows PC, run **one install command** inside it,
then **one test command**. About 30–45 minutes, mostly waiting.

```
 Windows PC
 └── VMware / VirtualBox
     └── Debian 13 VM  ← p8sd runs here  (install.sh)
         └── test VMs  ← made by p8sd    (p8 create …)
```

---

## Step 1 — Get Windows ready (5 min, once)

P8S OS runs VMs **inside** your VM. Windows must not be using its own hypervisor for that to work.

1. Open **PowerShell** and run:
   ```powershell
   systeminfo | Select-String "Hyper-V"
   ```
2. Read the answer:
   - Four lines ending in **Yes** → you're ready. **Go to step 2.**
   - **"A hypervisor has been detected"** → do 3 and 4.
3. **Windows Security → Device security → Core isolation → Memory integrity: Off**
4. **PowerShell as Administrator**, then **restart** the PC:
   ```powershell
   bcdedit /set hypervisorlaunchtype off
   ```
   Check again with step 1.

> This turns off WSL2 and Docker Desktop until you undo it with
> `bcdedit /set hypervisorlaunchtype auto` and a restart.

---

## Step 2 — Download Debian 13

From **https://cdimage.debian.org/debian-cd/current/amd64/iso-cd/** download the file
ending in **`-amd64-netinst.iso`** (~750 MB).

---

## Step 3 — Make the VM

Same settings in both apps:

| Setting | Value |
|---|---|
| OS type | Debian 12 / Debian 64-bit |
| CPUs | **4** |
| RAM | **8 GB** |
| Disk 1 | **32 GB** — for Debian |
| Disk 2 | **64 GB** — empty, for VM storage |
| Network | **NAT** |
| Firmware | BIOS (no EFI / no Secure Boot) |

### In VMware Workstation

1. **File → New Virtual Machine → Custom** → *"I will install the operating system later"* → Linux, **Debian 12.x 64-bit**.
2. Set CPUs, RAM, a 32 GB **SCSI** disk, network **NAT**. Finish.
3. **VM → Settings:**
   - **Processors** → tick **☑ Virtualize Intel VT-x/EPT or AMD-V/RVI**  ← the important one
   - **Add… → Hard Disk → SCSI → 64 GB**
   - **CD/DVD** → *Use ISO image* → the Debian ISO
4. **Power on.**

### In VirtualBox

1. **Machine → New** → choose the Debian ISO, Type Linux, Debian (64-bit) → tick **☑ Skip Unattended Installation**.
2. 8192 MB, 4 CPUs, **EFI unticked**, 32 GB disk. Finish.
3. **Settings:**
   - **System → Processor** → tick **☑ Enable Nested VT-x/AMD-V**  ← the important one
     *(greyed out? close VirtualBox and run in PowerShell:*
     `& "C:\Program Files\Oracle\VirtualBox\VBoxManage.exe" modifyvm "p8s-test" --nested-hw-virt on` *— use your VM's name)*
   - **Storage → SATA → Add hard disk → Create → 64 GB**
   - **Network → Advanced → Port Forwarding → ➕**: Host Port **2222**, Guest Port **22**
4. **Start.**

> **Green turtle 🐢** in VirtualBox's bottom bar? Windows' hypervisor is still on → step 1.

---

## Step 4 — Install Debian

Go through the installer with defaults, except:

- **Partition:** *Guided – use entire disk* → pick the **first** disk (32 GB). Don't touch the 64 GB one.
- **Software selection:** untick everything except **☑ SSH server** and **☑ standard system utilities**.
- Remember the **user name and password** you create.

After reboot, log in and run:

```bash
su -c "apt install -y sudo git && usermod -aG sudo $USER"
```

Then **log out and back in** (type `exit`, log in again).

> Left the root password empty during install? Then `sudo` already works — skip this.

👉 **Take a snapshot now** (VMware: VM → Snapshot; VirtualBox: Machine → Take Snapshot). Name it `clean-debian`.
If anything goes wrong later, roll back to it and start again from step 5.

---

## Step 5 — Copy P8S OS in and install

**1. Find the VM's IP** (VMware only — VirtualBox uses the port forward):

```bash
ip -br addr      # look for 192.168.x.x on ens33 or similar
```

**2. On Windows**, in PowerShell, copy the folder (replace `you` and the IP):

```powershell
# VMware:
scp -r L:\P8SOS you@192.168.x.x:~/p8sos
# VirtualBox:
scp -P 2222 -r L:\P8SOS you@127.0.0.1:~/p8sos
```

**3. In the VM**, find the empty 64 GB disk:

```bash
lsblk
```

```
NAME   SIZE TYPE MOUNTPOINTS
sda     32G disk               ← Debian is here. NOT this one.
├─sda1  31G part /
sdb     64G disk               ← empty. THIS one.
```

(VMware with NVMe disks shows `nvme0n1` and `nvme0n2` instead — use `/dev/nvme0n2`.)

**4. Install** (use the disk name you saw — usually `/dev/sdb`):

```bash
cd ~/p8sos
sudo bash install.sh /dev/sdb
```

Type **`yes`** when it asks to use the disk. It runs 7 steps and shows ✔ for each:

```
[1/7] Checking this machine
[2/7] Installing KVM, libvirt and ZFS      ← slowest, 3–10 min
[3/7] Setting up the network for VMs
[4/7] Importing the Debian 13 VM template
[5/7] Building p8sd
[6/7] Writing config and API token
[7/7] Starting p8sd

✔ P8S OS is installed.
```

If a step fails, it tells you why and what to do. Fix it and run the same command again —
it skips what's already done.

---

## Step 6 — Test it

```bash
p8 test
```

It creates a VM, logs into it, stops, starts and deletes it, checking everything on the
way. Takes 2–5 minutes. In **VirtualBox** run `SLOW=1 p8 test` instead (VMs inside it boot slower).

You want to see:

```
== result: 39 passed, 0 failed
```

**Send me the full output** — whether it passes or not.

---

## Step 7 — Play with it

```bash
p8 create web1             # make a VM (1 CPU, 1 GB RAM)
p8 create db1 2 2048 20    # 2 CPUs, 2048 MB RAM, 20 GB disk
p8 list                    # see all VMs
p8 ssh web1                # log in (waits for it to boot)
p8 stop web1               # shut down
p8 start web1              # power on
p8 delete web1             # remove it
p8 help                    # everything else
```

Inside a VM you made: `hostname`, `df -h /` (disk size you asked for), `free -m` (RAM).
Type `exit` to come back.

---

## If something goes wrong

| What you see | What to do |
|---|---|
| `No hardware virtualization` at step 1/7 | The VT-x tick box in step 3 is off, or Windows' hypervisor is on (step 1) |
| VMware: *"VT-x/EPT is not supported on this platform"* | Step 1 — Windows' hypervisor is on |
| VirtualBox: green turtle 🐢, or nested option greyed out | Step 1, then the `VBoxManage` command in step 3 |
| `is in use (mounted). Pick the EMPTY disk` | You picked the Debian disk. Run `lsblk` again |
| `scp: Connection refused` | VirtualBox: port forward missing (step 3). VMware: wrong IP |
| `p8 test` fails at "no IP" | Slow nested VM: `SLOW=1 p8 test`. Give the VM more CPUs |
| `sudo: command not found` | Redo the `su -c ...` line in step 4 |
| Anything else | Run **`p8 report`** and send me **p8s-report.txt** |

---

**Want the details?** [docs/testing-in-depth.md](docs/testing-in-depth.md) explains every
step by hand, plus extra checks (failure rollback, parallel creates, restarts).
API reference: [docs/api.md](docs/api.md).
