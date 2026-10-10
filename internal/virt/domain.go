// Package virt renders libvirt domain XML and drives the hypervisor.
package virt

import (
	"crypto/rand"
	"encoding/xml"
	"fmt"
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
	Bridge    string
	MAC       string
	Template  string
	CreatedAt string // RFC 3339
}

type domainXML struct {
	XMLName  xml.Name `xml:"domain"`
	Type     string   `xml:"type,attr"`
	Name     string   `xml:"name"`
	UUID     string   `xml:"uuid,omitempty"`
	Metadata struct {
		VM metaXML `xml:"https://p8s.dev/xmlns/vm/1 vm"`
	} `xml:"metadata"`
	Memory struct {
		Unit  string `xml:"unit,attr"`
		Value int    `xml:",chardata"`
	} `xml:"memory"`
	VCPU int `xml:"vcpu"`
	OS   struct {
		Type struct {
			Arch    string `xml:"arch,attr"`
			Machine string `xml:"machine,attr"`
			Value   string `xml:",chardata"`
		} `xml:"type"`
		Boot struct {
			Dev string `xml:"dev,attr"`
		} `xml:"boot"`
	} `xml:"os"`
	Features struct {
		ACPI *struct{} `xml:"acpi"`
		APIC *struct{} `xml:"apic"`
	} `xml:"features"`
	CPU struct {
		Mode string `xml:"mode,attr"`
	} `xml:"cpu"`
	OnPoweroff string     `xml:"on_poweroff"`
	OnReboot   string     `xml:"on_reboot"`
	OnCrash    string     `xml:"on_crash"`
	Devices    devicesXML `xml:"devices"`
}

type metaXML struct {
	Template  string `xml:"template"`
	DiskGB    int    `xml:"disk_gb"`
	CreatedAt string `xml:"created_at"`
}

type devicesXML struct {
	Disks      []diskXML  `xml:"disk"`
	Interfaces []ifaceXML `xml:"interface"`
	Serial     struct {
		Type string `xml:"type,attr"`
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
	RNG struct {
		Model   string `xml:"model,attr"`
		Backend struct {
			Model string `xml:"model,attr"`
			Value string `xml:",chardata"`
		} `xml:"backend"`
	} `xml:"rng"`
}

type diskXML struct {
	Type   string `xml:"type,attr"`
	Device string `xml:"device,attr"`
	Driver struct {
		Name    string `xml:"name,attr"`
		Type    string `xml:"type,attr"`
		Cache   string `xml:"cache,attr,omitempty"`
		Discard string `xml:"discard,attr,omitempty"`
	} `xml:"driver"`
	Source struct {
		Dev  string `xml:"dev,attr,omitempty"`
		File string `xml:"file,attr,omitempty"`
	} `xml:"source"`
	Target struct {
		Dev string `xml:"dev,attr"`
		Bus string `xml:"bus,attr"`
	} `xml:"target"`
	ReadOnly *struct{} `xml:"readonly"`
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
	d.Metadata.VM = metaXML{Template: s.Template, DiskGB: s.DiskGB, CreatedAt: s.CreatedAt}
	d.Memory.Unit, d.Memory.Value = "MiB", s.MemoryMiB
	d.VCPU = s.VCPUs
	d.OS.Type.Arch, d.OS.Type.Machine, d.OS.Type.Value = "x86_64", "q35", "hvm"
	d.OS.Boot.Dev = "hd"
	d.Features.ACPI, d.Features.APIC = &struct{}{}, &struct{}{}
	d.CPU.Mode = "host-passthrough"
	d.OnPoweroff, d.OnReboot, d.OnCrash = "destroy", "restart", "restart"

	root := diskXML{Type: "block", Device: "disk"}
	root.Driver.Name, root.Driver.Type, root.Driver.Cache, root.Driver.Discard = "qemu", "raw", "none", "unmap"
	root.Source.Dev = s.DiskPath
	root.Target.Dev, root.Target.Bus = "vda", "virtio"
	d.Devices.Disks = append(d.Devices.Disks, root)

	if s.SeedISO != "" {
		seed := diskXML{Type: "file", Device: "cdrom", ReadOnly: &struct{}{}}
		seed.Driver.Name, seed.Driver.Type = "qemu", "raw"
		seed.Source.File = s.SeedISO
		seed.Target.Dev, seed.Target.Bus = "sda", "sata"
		d.Devices.Disks = append(d.Devices.Disks, seed)
	}

	var nic ifaceXML
	nic.Type = "bridge"
	nic.MAC.Address = s.MAC
	nic.Source.Bridge = s.Bridge
	nic.Model.Type = "virtio"
	d.Devices.Interfaces = []ifaceXML{nic}

	d.Devices.Serial.Type = "pty"
	d.Devices.Console.Type = "pty"
	d.Devices.Channel.Type = "unix"
	d.Devices.Channel.Target.Type, d.Devices.Channel.Target.Name = "virtio", "org.qemu.guest_agent.0"
	d.Devices.Graphics.Type, d.Devices.Graphics.Port = "vnc", "-1"
	d.Devices.Graphics.Autoport, d.Devices.Graphics.Listen = "yes", "127.0.0.1"
	d.Devices.RNG.Model = "virtio"
	d.Devices.RNG.Backend.Model, d.Devices.RNG.Backend.Value = "random", "/dev/urandom"

	return xml.MarshalIndent(d, "", "  ")
}

// ParseDomainXML reads back a domain (e.g. from `virsh dumpxml`).
func ParseDomainXML(b []byte) (Spec, error) {
	var d domainXML
	if err := xml.Unmarshal(b, &d); err != nil {
		return Spec{}, err
	}
	s := Spec{
		Name:      d.Name,
		UUID:      d.UUID,
		VCPUs:     d.VCPU,
		MemoryMiB: toMiB(d.Memory.Value, d.Memory.Unit),
		Template:  d.Metadata.VM.Template,
		DiskGB:    d.Metadata.VM.DiskGB,
		CreatedAt: d.Metadata.VM.CreatedAt,
	}
	for _, disk := range d.Devices.Disks {
		switch disk.Device {
		case "disk":
			if s.DiskPath == "" {
				s.DiskPath = disk.Source.Dev
			}
		case "cdrom":
			s.SeedISO = disk.Source.File
		}
	}
	if len(d.Devices.Interfaces) > 0 {
		s.MAC = d.Devices.Interfaces[0].MAC.Address
		s.Bridge = d.Devices.Interfaces[0].Source.Bridge
	}
	return s, nil
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
