// Package virt renders libvirt domain XML and drives the hypervisor.
package virt

import (
	"crypto/rand"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
)

// MetaNS is the XML namespace for P8S metadata stored inside each domain, so
// libvirt itself is the source of truth for VMs in Phase 1 (no database yet).
const MetaNS = "https://p8s.dev/xmlns/vm/1"

// Spec describes one VM. It round-trips through domain XML.
type Spec struct {
	Name      string
	UUID      string
	VCPUs     int
	MemoryMiB int
	DiskGB    int
	DiskPath  string // block device, e.g. /dev/zvol/tank/p8s/vms/web1-disk0
	SeedISO   string // cloud-init NoCloud ISO; empty to omit
	// InstallISO is an installer CD (VMs installed by hand from an ISO). The VM
	// boots from its disk first and falls back to the CD while the disk is empty.
	InstallISO string
	// OSType "windows" uses devices Windows has drivers for (SATA disk, e1000e
	// NIC) and Hyper-V enlightenments; anything else uses virtio.
	// "windows11" is the same on UEFI with Secure Boot and a TPM 2.0 chip
	// (what Windows 11 and Server 2022/2025 want).
	OSType string
	// DriversISO is a second CD, e.g. virtio-win (Windows drivers and guest tools).
	DriversISO string
	Bridge     string
	MAC        string
	Template   string
	CreatedAt  string // RFC 3339
	// Owner is the P8S user the VM belongs to; empty = the host's admins.
	Owner string
	// Video is the display adapter read back from a domain (DomainXML always
	// writes VideoModel).
	Video string
	// DefVersion is read back from a domain (DomainXML always writes the current one).
	DefVersion int
	// Overrides for VMs imported from other hypervisors, so they boot with
	// the hardware they were installed on ("" = what OSType implies):
	// DiskBus "virtio" | "scsi" (virtio-scsi) | "sata"; NICModel "virtio" |
	// "e1000e" | "e1000" | "rtl8139" | "vmxnet3"; Firmware "bios" | "uefi"
	// (UEFI without Secure Boot; "windows11" always has Secure Boot).
	DiskBus  string
	NICModel string
	Firmware string
	// Extra are data disks after the root disk (at most MaxExtraDisks).
	Extra []ExtraDisk
	// NICs are network cards after the first one (Bridge/MAC), at most MaxExtraNICs.
	NICs []ExtraNIC
	// CPUModel "" gives the VM this server's CPU as it is (fastest; live moves
	// only between servers with the same CPU). A named model (e.g. the cluster's
	// common CPU, see CPUBaseline) with CPUFeatures ("+name" required, "-name"
	// off) lets it move live between different CPUs.
	CPUModel    string
	CPUFeatures []string
	// PCI devices of the host given to the VM ("0000:01:00.0") and USB
	// devices (vendor:product); see hostdev.go.
	PCI []string
	USB []USBDev
	// Limits caps the VM's disks (each) and network cards (each); zero = no limit.
	Limits IOLimits
	// HotCPUs / HotMemMiB are the hot-plug ceilings: vCPUs and RAM can be
	// raised up to them while the VM runs (0 = hot-plug off).
	HotCPUs   int
	HotMemMiB int
}

// HotMemSlots is how many memory modules can be plugged into a running VM.
const HotMemSlots = 16

// ExtraDisk is a data disk. Index 1-8 stays the same for the disk's life, so
// it keeps its place (vdb, vdc, ...) inside the guest when others are removed.
type ExtraDisk struct {
	Index  int
	Path   string // block device
	SizeGB int
}

// ExtraNIC is a second, third … network card on its own bridge (another
// network or VLAN). The MAC identifies it for its whole life.
type ExtraNIC struct {
	MAC    string
	Bridge string
}

// MaxExtraNICs is how many network cards a VM can have next to its first one.
const MaxExtraNICs = 7

// MaxExtraDisks is how many data disks a VM can have next to its root disk.
const MaxExtraDisks = 8

var (
	DiskBuses = map[string]bool{"virtio": true, "scsi": true, "sata": true}
	NICModels = map[string]bool{"virtio": true, "e1000e": true, "e1000": true, "rtl8139": true, "vmxnet3": true}
)

// DefVersion is the version of the VM definitions DomainXML writes. Older
// definitions are re-rendered at start-up (vm.UpgradeDomains); they pick up
// the change at their next power-on.
//
//	1 VGA display; 2 I/O thread + multi-queue virtio disk and network;
//	3 memory balloon with free page reporting + guest memory statistics;
//	4 all vCPUs as cores of one socket (Windows 10/11 Home use 1 socket and Pro
//	  2: with libvirt's default of one socket per vCPU a 4-vCPU Windows VM
//	  only ran on 1-2 of them) and native AIO for the Windows SATA disk
const DefVersion = 4

// VideoModel is the display adapter every P8S VM gets.
const VideoModel = "vga"

type domainXML struct {
	XMLName  xml.Name `xml:"domain"`
	Type     string   `xml:"type,attr"`
	Name     string   `xml:"name"`
	UUID     string   `xml:"uuid,omitempty"`
	Metadata struct {
		VM metaXML `xml:"https://p8s.dev/xmlns/vm/1 vm"`
	} `xml:"metadata"`
	MaxMemory *struct {
		Slots int    `xml:"slots,attr"`
		Unit  string `xml:"unit,attr"`
		Value int    `xml:",chardata"`
	} `xml:"maxMemory"`
	Memory struct {
		Unit  string `xml:"unit,attr"`
		Value int    `xml:",chardata"`
	} `xml:"memory"`
	VCPU struct {
		Placement string `xml:"placement,attr,omitempty"`
		Current   int    `xml:"current,attr,omitempty"`
		Value     int    `xml:",chardata"`
	} `xml:"vcpu"`
	IOThreads int `xml:"iothreads,omitempty"`
	OS        struct {
		Firmware string `xml:"firmware,attr,omitempty"` // "efi" = UEFI (libvirt picks OVMF)
		Type     struct {
			Arch    string `xml:"arch,attr"`
			Machine string `xml:"machine,attr"`
			Value   string `xml:",chardata"`
		} `xml:"type"`
		FirmwareFeatures *struct {
			Features []struct {
				Enabled string `xml:"enabled,attr"`
				Name    string `xml:"name,attr"`
			} `xml:"feature"`
		} `xml:"firmware"`
		Loader *struct {
			Secure string `xml:"secure,attr,omitempty"`
		} `xml:"loader"`
		Boot []struct {
			Dev string `xml:"dev,attr"`
		} `xml:"boot"`
	} `xml:"os"`
	Features struct {
		ACPI   *struct{}  `xml:"acpi"`
		APIC   *struct{}  `xml:"apic"`
		HyperV *hypervXML `xml:"hyperv"`
		SMM    *struct {
			State string `xml:"state,attr"`
		} `xml:"smm"`
	} `xml:"features"`
	Clock *clockXML `xml:"clock"`
	CPU   struct {
		Mode     string    `xml:"mode,attr"`
		Match    string    `xml:"match,attr,omitempty"`
		Check    string    `xml:"check,attr,omitempty"`
		Model    *cpuModel `xml:"model"`
		Features []cpuFeat `xml:"feature"`
		Topology *struct {
			Sockets int `xml:"sockets,attr"`
			Dies    int `xml:"dies,attr"`
			Cores   int `xml:"cores,attr"`
			Threads int `xml:"threads,attr"`
		} `xml:"topology"`
		NUMA *struct {
			Cells []numaCell `xml:"cell"`
		} `xml:"numa"`
	} `xml:"cpu"`
	OnPoweroff string     `xml:"on_poweroff"`
	OnReboot   string     `xml:"on_reboot"`
	OnCrash    string     `xml:"on_crash"`
	Devices    devicesXML `xml:"devices"`
}

type cpuModel struct {
	Fallback string `xml:"fallback,attr,omitempty"`
	Name     string `xml:",chardata"`
}

type cpuFeat struct {
	Policy string `xml:"policy,attr"`
	Name   string `xml:"name,attr"`
}

type onOff struct {
	State string `xml:"state,attr"`
}

type hypervXML struct {
	Relaxed   onOff `xml:"relaxed"`
	VAPIC     onOff `xml:"vapic"`
	Spinlocks struct {
		State   string `xml:"state,attr"`
		Retries string `xml:"retries,attr"`
	} `xml:"spinlocks"`
	// The rest make Windows noticeably faster on KVM (what Proxmox enables).
	VPIndex     *onOff `xml:"vpindex"`
	Runtime     *onOff `xml:"runtime"`
	Synic       *onOff `xml:"synic"`
	STimer      *onOff `xml:"stimer"`
	Reset       *onOff `xml:"reset"`
	Frequencies *onOff `xml:"frequencies"`
	TLBFlush    *onOff `xml:"tlbflush"`
	IPI         *onOff `xml:"ipi"`
}

type clockXML struct {
	Offset string `xml:"offset,attr"`
	Timers []struct {
		Name       string `xml:"name,attr"`
		Present    string `xml:"present,attr,omitempty"`
		TickPolicy string `xml:"tickpolicy,attr,omitempty"`
	} `xml:"timer"`
}

type metaXML struct {
	DefVersion int        `xml:"def_version,omitempty"`
	OSType     string     `xml:"os_type,omitempty"`
	Template   string     `xml:"template"`
	DiskGB     int        `xml:"disk_gb"`
	CreatedAt  string     `xml:"created_at"`
	Owner      string     `xml:"owner,omitempty"`
	DiskBus    string     `xml:"disk_bus,omitempty"`
	NICModel   string     `xml:"nic_model,omitempty"`
	Firmware   string     `xml:"firmware,omitempty"`
	Disks      []metaDisk `xml:"disk,omitempty"`
	// hot-plug ceilings (0 = off)
	HotCPUs   int `xml:"hot_cpus,omitempty"`
	HotMemMiB int `xml:"hot_mem_mib,omitempty"`
}

type numaCell struct {
	ID     int    `xml:"id,attr"`
	CPUs   string `xml:"cpus,attr"`
	Memory int    `xml:"memory,attr"`
	Unit   string `xml:"unit,attr"`
}

type metaDisk struct {
	Index  int `xml:"index,attr"`
	SizeGB int `xml:"gb,attr"`
}

func metaDisks(ex []ExtraDisk) []metaDisk {
	var out []metaDisk
	for _, e := range ex {
		out = append(out, metaDisk{e.Index, e.SizeGB})
	}
	return out
}

type controllerXML struct {
	Type  string `xml:"type,attr"`
	Model string `xml:"model,attr,omitempty"`
}

type devicesXML struct {
	Disks       []diskXML       `xml:"disk"`
	Controllers []controllerXML `xml:"controller"`
	Interfaces  []ifaceXML      `xml:"interface"`
	Hostdevs    []hostdevXML    `xml:"hostdev"`
	Inputs      []struct {
		Type string `xml:"type,attr"`
		Bus  string `xml:"bus,attr"`
	} `xml:"input"`
	Serial struct {
		Type string `xml:"type,attr"`
		Log  *struct {
			File   string `xml:"file,attr"`
			Append string `xml:"append,attr"`
		} `xml:"log"`
	} `xml:"serial"`
	Console struct {
		Type string `xml:"type,attr"`
	} `xml:"console"`
	Channel struct {
		Type   string `xml:"type,attr"`
		Target struct {
			Type string `xml:"type,attr"`
			Name string `xml:"name,attr"`
		} `xml:"target"`
	} `xml:"channel"`
	Graphics struct {
		Type     string `xml:"type,attr"`
		Port     string `xml:"port,attr"`
		Autoport string `xml:"autoport,attr"`
		Listen   string `xml:"listen,attr"`
	} `xml:"graphics"`
	TPM *struct {
		Model   string `xml:"model,attr"`
		Backend struct {
			Type    string `xml:"type,attr"`
			Version string `xml:"version,attr"`
		} `xml:"backend"`
	} `xml:"tpm"`
	Video *struct {
		Model struct {
			Type    string `xml:"type,attr"`
			VRAM    int    `xml:"vram,attr,omitempty"`
			Heads   int    `xml:"heads,attr,omitempty"`
			Primary string `xml:"primary,attr,omitempty"`
		} `xml:"model"`
	} `xml:"video"`
	RNG struct {
		Model   string `xml:"model,attr"`
		Backend struct {
			Model string `xml:"model,attr"`
			Value string `xml:",chardata"`
		} `xml:"backend"`
	} `xml:"rng"`
	// Balloon: the guest hands RAM it frees back to the host (free page
	// reporting, like Proxmox's ballooning) and reports how much it really
	// uses (stats every 10 s, read by `virsh domstats`).
	Balloon *balloonXML `xml:"memballoon"`
}

type balloonXML struct {
	Model             string        `xml:"model,attr"`
	Autodeflate       string        `xml:"autodeflate,attr,omitempty"`
	FreePageReporting string        `xml:"freePageReporting,attr,omitempty"`
	Stats             *balloonStats `xml:"stats"`
}

type balloonStats struct {
	Period int `xml:"period,attr"`
}

type diskXML struct {
	Type   string `xml:"type,attr"`
	Device string `xml:"device,attr"`
	Driver struct {
		Name     string `xml:"name,attr"`
		Type     string `xml:"type,attr"`
		Cache    string `xml:"cache,attr,omitempty"`
		IO       string `xml:"io,attr,omitempty"`
		Discard  string `xml:"discard,attr,omitempty"`
		IOThread int    `xml:"iothread,attr,omitempty"`
		Queues   int    `xml:"queues,attr,omitempty"`
	} `xml:"driver"`
	Source struct {
		Dev  string `xml:"dev,attr,omitempty"`
		File string `xml:"file,attr,omitempty"`
	} `xml:"source"`
	Target struct {
		Dev string `xml:"dev,attr"`
		Bus string `xml:"bus,attr"`
	} `xml:"target"`
	ReadOnly *struct{}  `xml:"readonly"`
	IOTune   *ioTuneXML `xml:"iotune"`
}

type ifaceXML struct {
	Type string `xml:"type,attr"`
	MAC  struct {
		Address string `xml:"address,attr"`
	} `xml:"mac"`
	Source struct {
		Bridge string `xml:"bridge,attr"`
	} `xml:"source"`
	Model struct {
		Type string `xml:"type,attr"`
	} `xml:"model"`
	Driver *struct {
		Name   string `xml:"name,attr"`
		Queues int    `xml:"queues,attr,omitempty"`
	} `xml:"driver"`
	Bandwidth *bandwidthXML `xml:"bandwidth"`
}

// MetadataXML is the P8S metadata element of a spec, for updating a running
// domain in place (virsh metadata --live).
func MetadataXML(s Spec) (string, error) {
	m := metaXML{OSType: s.OSType, Template: s.Template, DiskGB: s.DiskGB, CreatedAt: s.CreatedAt, Owner: s.Owner,
		DiskBus: s.DiskBus, NICModel: s.NICModel, Firmware: s.Firmware, Disks: metaDisks(s.Extra), HotCPUs: s.HotCPUs, HotMemMiB: s.HotMemMiB}
	b, err := xml.Marshal(struct {
		XMLName xml.Name `xml:"vm"`
		metaXML
	}{metaXML: m})
	return string(b), err
}

// DomainXML renders a KVM domain: q35, host-passthrough CPU, virtio disk/NIC,
// serial console, guest-agent channel, VNC bound to localhost only.
func DomainXML(s Spec) ([]byte, error) {
	if s.Name == "" || s.DiskPath == "" || s.Bridge == "" || s.MAC == "" {
		return nil, fmt.Errorf("domain spec incomplete: %+v", s)
	}
	var d domainXML
	d.Type = "kvm"
	d.Name = s.Name
	d.UUID = s.UUID
	d.Metadata.VM = metaXML{DefVersion: DefVersion, OSType: s.OSType, Template: s.Template, DiskGB: s.DiskGB, CreatedAt: s.CreatedAt, Owner: s.Owner,
		DiskBus: s.DiskBus, NICModel: s.NICModel, Firmware: s.Firmware, Disks: metaDisks(s.Extra), HotCPUs: s.HotCPUs, HotMemMiB: s.HotMemMiB}
	win := s.OSType == "windows" || s.OSType == "windows11"
	secure := s.OSType == "windows11"
	uefi := secure || s.Firmware == "uefi"
	d.Memory.Unit, d.Memory.Value = "MiB", s.MemoryMiB
	d.VCPU.Value = s.VCPUs
	if s.HotCPUs > s.VCPUs { // CPU hot-plug: room for more vCPUs, s.VCPUs of them on
		d.VCPU.Placement, d.VCPU.Value, d.VCPU.Current = "static", s.HotCPUs, s.VCPUs
	}
	d.OS.Type.Arch, d.OS.Type.Machine, d.OS.Type.Value = "x86_64", "q35", "hvm"
	d.OS.Boot = append(d.OS.Boot, struct {
		Dev string `xml:"dev,attr"`
	}{"hd"})
	if s.InstallISO != "" {
		d.OS.Boot = append(d.OS.Boot, struct {
			Dev string `xml:"dev,attr"`
		}{"cdrom"})
	}
	d.Features.ACPI, d.Features.APIC = &struct{}{}, &struct{}{}
	if uefi && !secure { // plain UEFI (imported VMs installed that way)
		d.OS.Firmware = "efi"
	}
	if secure { // UEFI with Secure Boot (Microsoft keys enrolled); SMM is required for it
		d.OS.Firmware = "efi"
		d.OS.FirmwareFeatures = &struct {
			Features []struct {
				Enabled string `xml:"enabled,attr"`
				Name    string `xml:"name,attr"`
			} `xml:"feature"`
		}{}
		for _, f := range []string{"secure-boot", "enrolled-keys"} {
			d.OS.FirmwareFeatures.Features = append(d.OS.FirmwareFeatures.Features, struct {
				Enabled string `xml:"enabled,attr"`
				Name    string `xml:"name,attr"`
			}{"yes", f})
		}
		d.OS.Loader = &struct {
			Secure string `xml:"secure,attr,omitempty"`
		}{"yes"}
		d.Features.SMM = &struct {
			State string `xml:"state,attr"`
		}{"on"}
	}
	if win {
		h := &hypervXML{}
		h.Relaxed.State, h.VAPIC.State = "on", "on"
		h.Spinlocks.State, h.Spinlocks.Retries = "on", "8191"
		on := &onOff{"on"}
		h.VPIndex, h.Runtime, h.Synic, h.STimer, h.Reset, h.Frequencies, h.TLBFlush, h.IPI = on, on, on, on, on, on, on, on
		d.Features.HyperV = h
		d.Clock = &clockXML{Offset: "localtime"}
		for _, t := range [][3]string{{"rtc", "", "catchup"}, {"pit", "", "delay"}, {"hpet", "no", ""}, {"hypervclock", "yes", ""}} {
			d.Clock.Timers = append(d.Clock.Timers, struct {
				Name       string `xml:"name,attr"`
				Present    string `xml:"present,attr,omitempty"`
				TickPolicy string `xml:"tickpolicy,attr,omitempty"`
			}{t[0], t[1], t[2]})
		}
	}
	d.CPU.Mode = "host-passthrough"
	if s.CPUModel != "" {
		d.CPU.Mode, d.CPU.Match, d.CPU.Check = "custom", "exact", "partial"
		d.CPU.Model = &cpuModel{Fallback: "forbid", Name: s.CPUModel}
		for _, f := range s.CPUFeatures {
			pol := "require"
			if strings.HasPrefix(f, "-") {
				pol = "disable"
			}
			d.CPU.Features = append(d.CPU.Features, cpuFeat{Policy: pol, Name: strings.TrimLeft(f, "+-")})
		}
	}
	d.CPU.Topology = &struct {
		Sockets int `xml:"sockets,attr"`
		Dies    int `xml:"dies,attr"`
		Cores   int `xml:"cores,attr"`
		Threads int `xml:"threads,attr"`
	}{1, 1, max(d.VCPU.Value, 1), 1}
	if s.HotMemMiB > s.MemoryMiB { // RAM hot-plug: memory modules can be added up to the ceiling (needs a NUMA cell)
		d.MaxMemory = &struct {
			Slots int    `xml:"slots,attr"`
			Unit  string `xml:"unit,attr"`
			Value int    `xml:",chardata"`
		}{HotMemSlots, "MiB", s.HotMemMiB}
		d.CPU.NUMA = &struct {
			Cells []numaCell `xml:"cell"`
		}{[]numaCell{{ID: 0, CPUs: fmt.Sprintf("0-%d", max(d.VCPU.Value, 1)-1), Memory: s.MemoryMiB, Unit: "MiB"}}}
	}
	d.OnPoweroff, d.OnReboot, d.OnCrash = "destroy", "restart", "restart"

	root := diskXML{Type: "block", Device: "disk"}
	root.Driver.Name, root.Driver.Type, root.Driver.Cache, root.Driver.Discard = "qemu", "raw", "none", "unmap"
	root.Source.Dev = s.DiskPath
	root.Target.Dev, root.Target.Bus = "vda", "virtio"
	// queues: one per vCPU (up to 8) so disk I/O isn't funnelled through one CPU
	queues := min(max(s.VCPUs, 1), 8)
	bus := s.DiskBus
	if bus == "" && win { // the Windows installer has no virtio driver
		bus = "sata"
	}
	switch bus {
	case "sata":
		root.Target.Dev, root.Target.Bus = "sdc", "sata"
		root.Driver.IO = "native" // with cache=none: no extra copy through the host
	case "scsi": // virtio-scsi, the Proxmox default
		root.Target.Dev, root.Target.Bus = "sde", "scsi"
		root.Driver.IO = "native"
		d.Devices.Controllers = append(d.Devices.Controllers, controllerXML{Type: "scsi", Model: "virtio-scsi"})
	default:
		// its own I/O thread (disk work off the vCPU threads) and native AIO
		d.IOThreads = 1
		root.Driver.IO, root.Driver.IOThread, root.Driver.Queues = "native", 1, queues
	}
	root.IOTune = s.Limits.ioTune() // the data disks copy it: each gets the limit
	d.Devices.Disks = append(d.Devices.Disks, root)
	for _, e := range s.Extra {
		x, err := extraDisk(root, e)
		if err != nil {
			return nil, err
		}
		d.Devices.Disks = append(d.Devices.Disks, x)
	}

	if s.SeedISO != "" {
		seed := diskXML{Type: "file", Device: "cdrom", ReadOnly: &struct{}{}}
		seed.Driver.Name, seed.Driver.Type = "qemu", "raw"
		seed.Source.File = s.SeedISO
		seed.Target.Dev, seed.Target.Bus = "sda", "sata"
		d.Devices.Disks = append(d.Devices.Disks, seed)
	}
	if s.InstallISO != "" {
		cd := diskXML{Type: "file", Device: "cdrom", ReadOnly: &struct{}{}}
		cd.Driver.Name, cd.Driver.Type = "qemu", "raw"
		cd.Source.File = s.InstallISO
		cd.Target.Dev, cd.Target.Bus = "sdb", "sata"
		d.Devices.Disks = append(d.Devices.Disks, cd)
	}

	if s.DriversISO != "" {
		cd := diskXML{Type: "file", Device: "cdrom", ReadOnly: &struct{}{}}
		cd.Driver.Name, cd.Driver.Type = "qemu", "raw"
		cd.Source.File = s.DriversISO
		cd.Target.Dev, cd.Target.Bus = "sdd", "sata"
		d.Devices.Disks = append(d.Devices.Disks, cd)
	}

	var nic ifaceXML
	nic.Type = "bridge"
	nic.MAC.Address = s.MAC
	nic.Source.Bridge = s.Bridge
	nic.Model.Type = "virtio"
	if win {
		nic.Model.Type = "e1000e"
	}
	if s.NICModel != "" {
		nic.Model.Type = s.NICModel
	}
	if nic.Model.Type == "virtio" && queues > 1 { // vhost-net in the kernel, one queue per vCPU
		nic.Driver = &struct {
			Name   string `xml:"name,attr"`
			Queues int    `xml:"queues,attr,omitempty"`
		}{"vhost", queues}
	}
	nic.Bandwidth = s.Limits.bandwidth()
	d.Devices.Interfaces = []ifaceXML{nic}
	for _, e := range s.NICs {
		x := nic // same model and driver as the first card
		x.MAC.Address, x.Source.Bridge = e.MAC, e.Bridge
		d.Devices.Interfaces = append(d.Devices.Interfaces, x)
	}

	hd, err := s.hostdevs()
	if err != nil {
		return nil, err
	}
	d.Devices.Hostdevs = hd

	// A tablet makes the mouse follow the pointer exactly in the browser console.
	d.Devices.Inputs = append(d.Devices.Inputs, struct {
		Type string `xml:"type,attr"`
		Bus  string `xml:"bus,attr"`
	}{"tablet", "usb"})
	d.Devices.Serial.Type = "pty"
	// Everything the guest prints on its serial console (kernel, cloud-init) is
	// also kept in a file, so failed boots can be diagnosed without a console.
	d.Devices.Serial.Log = &struct {
		File   string `xml:"file,attr"`
		Append string `xml:"append,attr"`
	}{File: ConsoleLog(s.Name), Append: "off"}
	d.Devices.Console.Type = "pty"
	d.Devices.Channel.Type = "unix"
	d.Devices.Channel.Target.Type, d.Devices.Channel.Target.Name = "virtio", "org.qemu.guest_agent.0"
	// Standard VGA (Bochs/QEMU std). Without this libvirt may pick "cirrus",
	// whose screen showed garbled text in the browser console.
	d.Devices.Video = &struct {
		Model struct {
			Type    string `xml:"type,attr"`
			VRAM    int    `xml:"vram,attr,omitempty"`
			Heads   int    `xml:"heads,attr,omitempty"`
			Primary string `xml:"primary,attr,omitempty"`
		} `xml:"model"`
	}{}
	d.Devices.Video.Model.Type, d.Devices.Video.Model.VRAM, d.Devices.Video.Model.Heads, d.Devices.Video.Model.Primary = VideoModel, 16384, 1, "yes"
	if uefi { // TPM 2.0 chip (software, swtpm); its state is kept by libvirt
		d.Devices.TPM = &struct {
			Model   string `xml:"model,attr"`
			Backend struct {
				Type    string `xml:"type,attr"`
				Version string `xml:"version,attr"`
			} `xml:"backend"`
		}{Model: "tpm-crb"}
		d.Devices.TPM.Backend.Type, d.Devices.TPM.Backend.Version = "emulator", "2.0"
	}
	d.Devices.Graphics.Type, d.Devices.Graphics.Port = "vnc", "-1"
	d.Devices.Graphics.Autoport, d.Devices.Graphics.Listen = "yes", "127.0.0.1"
	d.Devices.RNG.Model = "virtio"
	d.Devices.RNG.Backend.Model, d.Devices.RNG.Backend.Value = "random", "/dev/urandom"
	d.Devices.Balloon = &balloonXML{Model: "virtio", Autodeflate: "on", FreePageReporting: "on", Stats: &balloonStats{Period: 10}}

	return xml.MarshalIndent(d, "", "  ")
}

// extraDisk is a data disk with the root disk's driver settings on the same bus.
// virtio: vdb..vdi; SATA / SCSI: sdf..sdm (sda-sde are the CDs and root disks).
func extraDisk(root diskXML, e ExtraDisk) (diskXML, error) {
	if e.Index < 1 || e.Index > MaxExtraDisks || e.Path == "" {
		return diskXML{}, fmt.Errorf("extra disk %d: bad index or path", e.Index)
	}
	x := root
	x.Source.Dev = e.Path
	if root.Target.Bus == "virtio" {
		x.Target.Dev = "vd" + string(rune('a'+e.Index))
	} else {
		x.Target.Dev = "sd" + string(rune('e'+e.Index))
	}
	return x, nil
}

// ExtraTarget is data disk i's device name inside the guest's view (vdb, sdf, ...).
func ExtraTarget(s Spec, i int) string {
	bus := s.DiskBus
	if bus == "" && (s.OSType == "windows" || s.OSType == "windows11") {
		bus = "sata"
	}
	if bus == "sata" || bus == "scsi" {
		return "sd" + string(rune('e'+i))
	}
	return "vd" + string(rune('a'+i))
}

// HotPlug says whether disks can be added to the running VM (SATA can't).
func HotPlug(s Spec) bool { return ExtraTarget(s, 1) == "vdb" || s.DiskBus == "scsi" }

// ExtraDiskXML is the <disk> element of a data disk, for attaching it to a
// running VM (virsh attach-device).
func ExtraDiskXML(s Spec, e ExtraDisk) ([]byte, error) {
	full, err := DomainXML(Spec{Name: s.Name, DiskPath: "/dev/null", Bridge: "x", MAC: "x", VCPUs: s.VCPUs,
		OSType: s.OSType, DiskBus: s.DiskBus})
	if err != nil {
		return nil, err
	}
	var d domainXML
	if err := xml.Unmarshal(full, &d); err != nil {
		return nil, err
	}
	x, err := extraDisk(d.Devices.Disks[0], e)
	if err != nil {
		return nil, err
	}
	return xml.MarshalIndent(struct {
		XMLName xml.Name `xml:"disk"`
		diskXML
	}{diskXML: x}, "", "  ")
}

// NICXML is one extra network card's <interface> element, for plugging it into
// a running VM (virsh attach-device / detach-device --live).
func NICXML(s Spec, e ExtraNIC) ([]byte, error) {
	t := s
	t.NICs = []ExtraNIC{e}
	if t.DiskPath == "" {
		t.DiskPath = "/dev/null"
	}
	full, err := DomainXML(t)
	if err != nil {
		return nil, err
	}
	var d domainXML
	if err := xml.Unmarshal(full, &d); err != nil {
		return nil, err
	}
	if len(d.Devices.Interfaces) < 2 {
		return nil, fmt.Errorf("no network card rendered")
	}
	return xml.MarshalIndent(struct {
		XMLName xml.Name `xml:"interface"`
		ifaceXML
	}{ifaceXML: d.Devices.Interfaces[1]}, "", "  ")
}

// ParseDomainXML reads back a domain (e.g. from `virsh dumpxml`).
func ParseDomainXML(b []byte) (Spec, error) {
	var d domainXML
	if err := xml.Unmarshal(b, &d); err != nil {
		return Spec{}, err
	}
	s := Spec{
		Name:       d.Name,
		UUID:       d.UUID,
		VCPUs:      d.VCPU.Value,
		HotCPUs:    d.Metadata.VM.HotCPUs,
		HotMemMiB:  d.Metadata.VM.HotMemMiB,
		MemoryMiB:  toMiB(d.Memory.Value, d.Memory.Unit),
		Template:   d.Metadata.VM.Template,
		DiskGB:     d.Metadata.VM.DiskGB,
		CreatedAt:  d.Metadata.VM.CreatedAt,
		OSType:     d.Metadata.VM.OSType,
		Owner:      d.Metadata.VM.Owner,
		DefVersion: d.Metadata.VM.DefVersion,
		DiskBus:    d.Metadata.VM.DiskBus,
		NICModel:   d.Metadata.VM.NICModel,
		Firmware:   d.Metadata.VM.Firmware,
	}
	if d.VCPU.Current > 0 {
		s.VCPUs = d.VCPU.Current
	}
	if d.Devices.Video != nil {
		s.Video = d.Devices.Video.Model.Type
	}
	if d.CPU.Mode == "custom" && d.CPU.Model != nil {
		s.CPUModel = strings.TrimSpace(d.CPU.Model.Name)
		for _, f := range d.CPU.Features {
			switch f.Policy {
			case "require", "force":
				s.CPUFeatures = append(s.CPUFeatures, "+"+f.Name)
			case "disable", "forbid":
				s.CPUFeatures = append(s.CPUFeatures, "-"+f.Name)
			}
		}
	}
	for _, disk := range d.Devices.Disks {
		switch disk.Device {
		case "disk":
			if s.DiskPath == "" {
				s.DiskPath = disk.Source.Dev
				s.Limits.read(disk.IOTune, nil)
			} else if i := extraIndex(disk.Source.Dev); i > 0 {
				e := ExtraDisk{Index: i, Path: disk.Source.Dev}
				for _, m := range d.Metadata.VM.Disks {
					if m.Index == i {
						e.SizeGB = m.SizeGB
					}
				}
				s.Extra = append(s.Extra, e)
			}
		case "cdrom":
			if disk.Target.Dev == "sdb" {
				s.InstallISO = disk.Source.File
			} else if disk.Target.Dev == "sdd" {
				s.DriversISO = disk.Source.File
			} else {
				s.SeedISO = disk.Source.File
			}
		}
	}
	if len(d.Devices.Interfaces) > 0 {
		s.MAC = d.Devices.Interfaces[0].MAC.Address
		s.Bridge = d.Devices.Interfaces[0].Source.Bridge
		s.Limits.read(nil, d.Devices.Interfaces[0].Bandwidth)
		for _, x := range d.Devices.Interfaces[1:] {
			s.NICs = append(s.NICs, ExtraNIC{MAC: x.MAC.Address, Bridge: x.Source.Bridge})
		}
	}
	s.readHostdevs(d.Devices.Hostdevs)
	return s, nil
}

// extraIndex reads N from a data disk's device ".../<vm>-diskN" (0 = not one).
func extraIndex(dev string) int {
	j := strings.LastIndex(dev, "-disk")
	if j < 0 {
		return 0
	}
	n, err := strconv.Atoi(dev[j+5:])
	if err != nil || n < 1 || n > MaxExtraDisks {
		return 0
	}
	return n
}

func toMiB(v int, unit string) int {
	switch unit {
	case "", "KiB", "k", "K":
		return v / 1024
	case "MiB", "M":
		return v
	case "GiB", "G":
		return v * 1024
	}
	return v
}

// RandomMAC returns a locally administered address in the QEMU OUI 52:54:00.
func RandomMAC() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return fmt.Sprintf("52:54:00:%02x:%02x:%02x", b[0], b[1], b[2])
}

// ConsoleLog is where a domain's serial console output is recorded.
func ConsoleLog(name string) string { return "/var/log/libvirt/qemu/" + name + "-console.log" }
