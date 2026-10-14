# 3. Preparing the machine
Summary: Firmware settings for a real server, and how to set up a VirtualBox VM on Windows for testing.

## A real server or PC

In the firmware setup (F2, Del or Esc at power-on, depending on the maker):

| Setting | Value | Why |
| --- | --- | --- |
| Intel VT-x / AMD SVM (Virtualization) | **Enabled** | VMs can't run without it |
| VT-d / AMD-Vi (IOMMU) | Enabled | Needed later for PCI passthrough |
| Secure Boot | **Disabled** | The ZFS module isn't signed; the installer warns if it's on |
| Boot mode | UEFI (legacy also works) | Every disk gets both boot loaders |
| SATA mode | AHCI (not RAID/RST) | ZFS must see the plain disks |
| Restore on AC power loss | Power On | The server comes back by itself after a power cut |

## Testing in VirtualBox on Windows

**Windows first** (once, then restart). VirtualBox can't offer nested virtualization while Hyper-V owns the CPU:

1. Windows Security → Device security → Core isolation → **Memory integrity: Off**.
2. Windows features: untick Hyper-V, Virtual Machine Platform, Windows Hypervisor Platform.
3. Administrator PowerShell: `bcdedit /set hypervisorlaunchtype off`, then restart.
4. Check: Task Manager → Performance → CPU shows **Virtualization: Enabled**.

**The VM**

| Setting | Value |
| --- | --- |
| Type / version | Linux / Debian (64-bit) |
| RAM | 8 GB (4 GB minimum) |
| CPUs | 4 (2 minimum) |
| System → Enable EFI | On |
| Processor → Enable Nested VT-x/AMD-V | **On** (so P8S OS can run VMs inside) |
| Disks | Two new VDI disks of 40 GB each on the SATA controller (to test a mirror) |
| Optical drive | The installer ISO |
| Network | Bridged Adapter (your Wi-Fi/Ethernet) so the dashboard is reachable from Windows; Promiscuous Mode: Allow All |
| Display | 16 MB+ video memory; VMSVGA |

If *Nested VT-x* is greyed out, Hyper-V is still on: repeat the Windows steps. From the command line: `VBoxManage modifyvm p8s-inst --nested-hw-virt on`.
