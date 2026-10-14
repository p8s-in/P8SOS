package vm

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"p8sos/internal/hostdevs"
	"p8sos/internal/virt"
)

// Passthrough: give a VM one of the server's PCI devices (graphics card,
// network card, NVMe, USB controller) or a USB device.

// HostDevices lists the server's devices (hostdevs.Lister).
type HostDevices interface {
	List(ctx context.Context) hostdevs.Info
}

// Devices is a VM's passthrough view: what it has and what the server offers.
type Devices struct {
	PCI       []string          `json:"pci"` // given to this VM
	USB       []string          `json:"usb"`
	Available hostdevs.Info     `json:"available"`
	UsedBy    map[string]string `json:"used_by"` // device -> other VM that has it
}

func (s *Service) hostDevices() (HostDevices, error) {
	if s.HostDevs == nil {
		return nil, fmt.Errorf("%w: passthrough isn't available on this server", ErrInvalid)
	}
	return s.HostDevs, nil
}

// usedBy maps every device given to some VM on this server to that VM.
func (s *Service) usedBy(ctx context.Context) map[string]string {
	m := map[string]string{}
	doms, err := s.HV.List(ctx)
	if err != nil {
		return m
	}
	for _, d := range doms {
		sp, err := s.spec(ctx, d)
		if err != nil {
			continue
		}
		n := strings.TrimPrefix(d, Prefix)
		for _, p := range sp.PCI {
			m[p] = n
		}
		for _, u := range sp.USB {
			m[u.ID()] = n
		}
	}
	return m
}

// Devices returns the VM's passthrough devices and the server's.
func (s *Service) Devices(ctx context.Context, name string) (Devices, error) {
	hd, err := s.hostDevices()
	if err != nil {
		return Devices{}, err
	}
	sp, err := s.spec(ctx, Prefix+name)
	if err != nil {
		return Devices{}, err
	}
	out := Devices{PCI: append([]string{}, sp.PCI...), USB: []string{}, Available: hd.List(ctx), UsedBy: map[string]string{}}
	for _, u := range sp.USB {
		out.USB = append(out.USB, u.ID())
	}
	for dev, vm := range s.usedBy(ctx) {
		if vm != name {
			out.UsedBy[dev] = vm
		}
	}
	return out, nil
}

// AddPCI gives the VM a PCI device, with the other devices of its IOMMU
// group (they can only go together). The VM must be stopped.
func (s *Service) AddPCI(ctx context.Context, name, addr string) (VM, string, error) {
	hd, err := s.hostDevices()
	if err != nil {
		return VM{}, "", err
	}
	a, err := virt.ParsePCI(addr)
	if err != nil {
		return VM{}, "", fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	dom := Prefix + name
	defer s.lock(dom)()
	st, err := s.state(ctx, dom)
	if err != nil {
		return VM{}, "", err
	}
	if st != virt.Stopped {
		return VM{}, "", fmt.Errorf("%w: shut vm %q down first; PCI devices are given while it is off", ErrConflict, name)
	}
	in := hd.List(ctx)
	var dev hostdevs.PCI
	found := false
	for _, p := range in.PCI {
		if p.Addr == a {
			dev, found = p, true
		}
	}
	switch {
	case !found:
		return VM{}, "", fmt.Errorf("%w: this server has no PCI device %s", ErrNotFound, a)
	case dev.Bridge:
		return VM{}, "", fmt.Errorf("%w: %s is a bridge inside the server; it can't be given to a VM", ErrInvalid, a)
	case !in.IOMMU:
		return VM{}, "", fmt.Errorf("%w: the CPU's IOMMU is off, so PCI devices can't be given to VMs: Server page → Summary → Turn on IOMMU (then reboot)", ErrConflict)
	}
	sp, err := s.spec(ctx, dom)
	if err != nil {
		return VM{}, "", err
	}
	if slices.Contains(sp.PCI, a) {
		return VM{}, "", fmt.Errorf("%w: vm %q already has %s", ErrExists, name, a)
	}
	want := append([]string{a}, dev.Others...)
	used := s.usedBy(ctx)
	for _, w := range want {
		if o := used[w]; o != "" && o != name {
			return VM{}, "", fmt.Errorf("%w: %s is given to vm %q; take it away there first", ErrConflict, w, o)
		}
	}
	var added []string
	for _, w := range want {
		if !slices.Contains(sp.PCI, w) {
			added = append(added, w)
		}
	}
	if len(sp.PCI)+len(added)+len(sp.USB) > virt.MaxHostDevs {
		return VM{}, "", fmt.Errorf("%w: at most %d PCI/USB devices per VM", ErrConflict, virt.MaxHostDevs)
	}
	if err := s.redefine(ctx, dom, func(x *virt.Spec) { x.PCI = append(x.PCI, added...) }); err != nil {
		return VM{}, "", err
	}
	note := fmt.Sprintf("%s (%s) is given to %s; it leaves the server when the VM starts and comes back when it stops", a, dev.Name, name)
	if len(added) > 1 {
		note += fmt.Sprintf(". Its IOMMU group partners go too: %s", strings.Join(added[1:], ", "))
	}
	if dev.Warn != "" {
		note += ". Careful: " + dev.Warn
	}
	if dev.Kind == "graphics" {
		note += ". In the VM install the card's own driver; the browser console keeps showing the VM's standard screen"
	}
	v, err := s.get(ctx, dom)
	return v, note, err
}

// RemovePCI takes a PCI device away from a stopped VM.
func (s *Service) RemovePCI(ctx context.Context, name, addr string) (VM, error) {
	a, err := virt.ParsePCI(addr)
	if err != nil {
		return VM{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	dom := Prefix + name
	defer s.lock(dom)()
	st, err := s.state(ctx, dom)
	if err != nil {
		return VM{}, err
	}
	if st != virt.Stopped {
		return VM{}, fmt.Errorf("%w: shut vm %q down first", ErrConflict, name)
	}
	sp, err := s.spec(ctx, dom)
	if err != nil {
		return VM{}, err
	}
	if !slices.Contains(sp.PCI, a) {
		return VM{}, fmt.Errorf("%w: vm %q has no PCI device %s", ErrNotFound, name, a)
	}
	if err := s.redefine(ctx, dom, func(x *virt.Spec) {
		x.PCI = slices.DeleteFunc(x.PCI, func(p string) bool { return p == a })
	}); err != nil {
		return VM{}, err
	}
	return s.get(ctx, dom)
}

// AddUSB gives the VM a USB device (vendor:product). A running VM gets it
// at once when it is plugged in; otherwise when it is plugged in and the VM
// starts.
func (s *Service) AddUSB(ctx context.Context, name, id string) (VM, string, error) {
	hd, err := s.hostDevices()
	if err != nil {
		return VM{}, "", err
	}
	u, err := virt.ParseUSB(id)
	if err != nil {
		return VM{}, "", fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	dom := Prefix + name
	defer s.lock(dom)()
	st, err := s.state(ctx, dom)
	if err != nil {
		return VM{}, "", err
	}
	sp, err := s.spec(ctx, dom)
	if err != nil {
		return VM{}, "", err
	}
	for _, x := range sp.USB {
		if x == u {
			return VM{}, "", fmt.Errorf("%w: vm %q already has USB %s", ErrExists, name, u.ID())
		}
	}
	if o := s.usedBy(ctx)[u.ID()]; o != "" && o != name {
		return VM{}, "", fmt.Errorf("%w: USB %s is given to vm %q; take it away there first", ErrConflict, u.ID(), o)
	}
	if len(sp.PCI)+len(sp.USB) >= virt.MaxHostDevs {
		return VM{}, "", fmt.Errorf("%w: at most %d PCI/USB devices per VM", ErrConflict, virt.MaxHostDevs)
	}
	plugged, label := false, u.ID()
	for _, x := range hd.List(ctx).USB {
		if x.ID == u.ID() {
			plugged, label = true, x.Name+" ("+u.ID()+")"
		}
	}
	note := label + " is given to " + name
	if st == virt.Running {
		switch at, ok := s.HV.(nicAttacher); {
		case !plugged:
			note += "; plug it in and restart the VM to use it"
		case ok:
			x, err := virt.USBXML(u)
			if err != nil {
				return VM{}, "", err
			}
			if err := at.AttachDisk(ctx, dom, x); err != nil {
				return VM{}, "", fmt.Errorf("plugging USB %s into the running VM: %w", u.ID(), err)
			}
			note += "; it is in the VM now"
		default:
			note += "; it arrives when the VM restarts"
		}
	} else if !plugged {
		note += "; it isn't plugged in now: the VM won't start until it is (or take it away again)"
	}
	if err := s.redefine(ctx, dom, func(x *virt.Spec) { x.USB = append(x.USB, u) }); err != nil {
		return VM{}, "", err
	}
	v, err := s.get(ctx, dom)
	return v, note, err
}

// RemoveUSB takes a USB device away (unplugged from a running VM at once).
func (s *Service) RemoveUSB(ctx context.Context, name, id string) (VM, error) {
	u, err := virt.ParseUSB(id)
	if err != nil {
		return VM{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	dom := Prefix + name
	defer s.lock(dom)()
	st, err := s.state(ctx, dom)
	if err != nil {
		return VM{}, err
	}
	sp, err := s.spec(ctx, dom)
	if err != nil {
		return VM{}, err
	}
	if !slices.Contains(sp.USB, u) {
		return VM{}, fmt.Errorf("%w: vm %q has no USB %s", ErrNotFound, name, u.ID())
	}
	if at, ok := s.HV.(nicAttacher); ok && st == virt.Running {
		if x, err := virt.USBXML(u); err == nil {
			s.logErr("unplug USB", at.DetachDevice(ctx, dom, x)) // not plugged in: nothing to unplug
		}
	}
	if err := s.redefine(ctx, dom, func(x *virt.Spec) {
		x.USB = slices.DeleteFunc(x.USB, func(d virt.USBDev) bool { return d == u })
	}); err != nil {
		return VM{}, err
	}
	return s.get(ctx, dom)
}

// hostDevsNote is why a VM with passthrough devices can't move.
func hostDevsNote(sp virt.Spec) string {
	if len(sp.PCI)+len(sp.USB) == 0 {
		return ""
	}
	return "it has PCI/USB devices of this server; take them away first (Hardware → PCI and USB devices)"
}

// AddDevice / RemoveDevice dispatch on kind "pci" or "usb" (the API's form).
func (s *Service) AddDevice(ctx context.Context, name, kind, id string) (VM, string, error) {
	switch kind {
	case "pci":
		return s.AddPCI(ctx, name, id)
	case "usb":
		return s.AddUSB(ctx, name, id)
	}
	return VM{}, "", fmt.Errorf("%w: kind must be \"pci\" or \"usb\"", ErrInvalid)
}

func (s *Service) RemoveDevice(ctx context.Context, name, kind, id string) (VM, error) {
	switch kind {
	case "pci":
		return s.RemovePCI(ctx, name, id)
	case "usb":
		return s.RemoveUSB(ctx, name, id)
	}
	return VM{}, fmt.Errorf("%w: kind must be \"pci\" or \"usb\"", ErrInvalid)
}
