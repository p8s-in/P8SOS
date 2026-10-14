package virt

import (
	"encoding/xml"
	"fmt"
	"regexp"
	"strings"
)

// Passthrough: a host's PCI device (graphics card, network card, NVMe,
// USB controller) or USB device given to a VM, like Proxmox's hostpci /
// usb options. PCI uses VFIO (libvirt managed="yes" detaches it from the
// host driver when the VM starts and gives it back when it stops).

// USBDev is a USB device by vendor and product id (it may be replugged into
// any port and the VM still gets it).
type USBDev struct {
	Vendor  string `json:"vendor"`  // 4 hex digits, e.g. 046d
	Product string `json:"product"` // 4 hex digits, e.g. c52b
}

func (u USBDev) ID() string { return u.Vendor + ":" + u.Product }

// MaxHostDevs is how many PCI + USB devices a VM can be given.
const MaxHostDevs = 8

var (
	rePCI = regexp.MustCompile(`^([0-9a-f]{4}):([0-9a-f]{2}):([0-9a-f]{2})\.([0-7])$`)
	reUSB = regexp.MustCompile(`^([0-9a-f]{4}):([0-9a-f]{4})$`)
)

// ParsePCI checks a PCI address "0000:01:00.0" (a short "01:00.0" gets
// domain 0000).
func ParsePCI(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if strings.Count(s, ":") == 1 {
		s = "0000:" + s
	}
	if !rePCI.MatchString(s) {
		return "", fmt.Errorf("PCI address must look like 0000:01:00.0, not %q", s)
	}
	return s, nil
}

// ParseUSB checks a USB id "046d:c52b".
func ParseUSB(s string) (USBDev, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	m := reUSB.FindStringSubmatch(s)
	if m == nil {
		return USBDev{}, fmt.Errorf("USB device must be vendor:product like 046d:c52b, not %q", s)
	}
	return USBDev{Vendor: m[1], Product: m[2]}, nil
}

type hostdevID struct {
	ID string `xml:"id,attr"`
}

type pciAddrXML struct {
	Domain   string `xml:"domain,attr"`
	Bus      string `xml:"bus,attr"`
	Slot     string `xml:"slot,attr"`
	Function string `xml:"function,attr"`
}

type hostdevXML struct {
	Mode    string `xml:"mode,attr"`
	Type    string `xml:"type,attr"`
	Managed string `xml:"managed,attr,omitempty"`
	Source  struct {
		Vendor  *hostdevID  `xml:"vendor"`
		Product *hostdevID  `xml:"product"`
		Address *pciAddrXML `xml:"address"`
	} `xml:"source"`
}

func pciHostdev(addr string) (hostdevXML, error) {
	a, err := ParsePCI(addr)
	if err != nil {
		return hostdevXML{}, err
	}
	m := rePCI.FindStringSubmatch(a)
	h := hostdevXML{Mode: "subsystem", Type: "pci", Managed: "yes"}
	h.Source.Address = &pciAddrXML{Domain: "0x" + m[1], Bus: "0x" + m[2], Slot: "0x" + m[3], Function: "0x" + m[4]}
	return h, nil
}

func usbHostdev(u USBDev) hostdevXML {
	h := hostdevXML{Mode: "subsystem", Type: "usb", Managed: "yes"}
	h.Source.Vendor, h.Source.Product = &hostdevID{"0x" + u.Vendor}, &hostdevID{"0x" + u.Product}
	return h
}

func (s Spec) hostdevs() ([]hostdevXML, error) {
	if len(s.PCI)+len(s.USB) > MaxHostDevs {
		return nil, fmt.Errorf("at most %d PCI/USB devices per VM", MaxHostDevs)
	}
	var out []hostdevXML
	for _, p := range s.PCI {
		h, err := pciHostdev(p)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	for _, u := range s.USB {
		if _, err := ParseUSB(u.ID()); err != nil {
			return nil, err
		}
		out = append(out, usbHostdev(u))
	}
	return out, nil
}

// readHostdevs fills PCI/USB from a domain's <hostdev> elements.
func (s *Spec) readHostdevs(hs []hostdevXML) {
	// "0x1" with n digits -> "0001"
	hex := func(v string, n int) string {
		v = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(v)), "0x")
		for len(v) < n {
			v = "0" + v
		}
		return v
	}
	for _, h := range hs {
		switch h.Type {
		case "pci":
			if a := h.Source.Address; a != nil {
				addr := hex(a.Domain, 4) + ":" + hex(a.Bus, 2) + ":" + hex(a.Slot, 2) + "." + strings.TrimLeft(hex(a.Function, 1), "0")
				if strings.HasSuffix(addr, ".") {
					addr += "0"
				}
				if p, err := ParsePCI(addr); err == nil {
					s.PCI = append(s.PCI, p)
				}
			}
		case "usb":
			if h.Source.Vendor != nil && h.Source.Product != nil {
				if u, err := ParseUSB(hex(h.Source.Vendor.ID, 4) + ":" + hex(h.Source.Product.ID, 4)); err == nil {
					s.USB = append(s.USB, u)
				}
			}
		}
	}
}

// USBXML is a USB device's <hostdev> element, for plugging it into a running
// VM (virsh attach-device / detach-device --live).
func USBXML(u USBDev) ([]byte, error) {
	if _, err := ParseUSB(u.ID()); err != nil {
		return nil, err
	}
	return xml.MarshalIndent(struct {
		XMLName xml.Name `xml:"hostdev"`
		hostdevXML
	}{hostdevXML: usbHostdev(u)}, "", "  ")
}
