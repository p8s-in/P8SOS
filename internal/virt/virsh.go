package virt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"p8sos/internal/execx"
)

// State is a normalized VM power state.
type State string

const (
	Running  State = "running"
	Stopped  State = "stopped"
	Paused   State = "paused"
	Stopping State = "stopping"
	Crashed  State = "crashed"
	Unknown  State = "unknown"
)

var ErrNoDomain = errors.New("domain not found")

// Hypervisor is the narrow surface p8sd needs from libvirt. Phase 1 implements
// it with virsh; a native libvirt RPC backend can replace it without touching
// the API or service layers.
type Hypervisor interface {
	Define(ctx context.Context, xml []byte) error
	Start(ctx context.Context, name string) error
	Shutdown(ctx context.Context, name string) error
	Destroy(ctx context.Context, name string) error
	Undefine(ctx context.Context, name string) error
	State(ctx context.Context, name string) (State, error)
	XML(ctx context.Context, name string) ([]byte, error)
	List(ctx context.Context) ([]string, error)
	Reboot(ctx context.Context, name string) error
	SetAutostart(ctx context.Context, name string, on bool) error
	Autostart(ctx context.Context, name string) (bool, error)
	// Addresses returns the guest's IPv4 addresses (may be empty while booting).
	Addresses(ctx context.Context, name string) ([]string, error)
}

type Virsh struct {
	Run execx.Runner
	URI string // e.g. qemu:///system
}

func (v *Virsh) virsh(ctx context.Context, args ...string) ([]byte, error) {
	out, err := v.Run.Run(ctx, "virsh", append([]string{"-c", v.URI, "-q"}, args...)...)
	var ee *execx.Error
	if errors.As(err, &ee) && (strings.Contains(ee.Stderr, "failed to get domain") ||
		strings.Contains(ee.Stderr, "Domain not found")) {
		return out, ErrNoDomain
	}
	return out, err
}

func (v *Virsh) Define(ctx context.Context, xml []byte) error {
	f, err := os.CreateTemp("", "p8s-domain-*.xml")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(xml); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	_, err = v.virsh(ctx, "define", f.Name())
	return err
}

func (v *Virsh) Start(ctx context.Context, name string) error {
	_, err := v.virsh(ctx, "start", name)
	return err
}

func (v *Virsh) Shutdown(ctx context.Context, name string) error {
	_, err := v.virsh(ctx, "shutdown", name, "--mode", "acpi")
	return err
}

func (v *Virsh) Destroy(ctx context.Context, name string) error {
	_, err := v.virsh(ctx, "destroy", name)
	return err
}

func (v *Virsh) Undefine(ctx context.Context, name string) error {
	// --nvram covers UEFI guests later; storage is managed by us, not libvirt.
	_, err := v.virsh(ctx, "undefine", name, "--nvram")
	return err
}

func (v *Virsh) State(ctx context.Context, name string) (State, error) {
	out, err := v.virsh(ctx, "domstate", name)
	if err != nil {
		return Unknown, err
	}
	return ParseState(string(out)), nil
}

func (v *Virsh) XML(ctx context.Context, name string) ([]byte, error) {
	return v.virsh(ctx, "dumpxml", name)
}

func (v *Virsh) List(ctx context.Context) ([]string, error) {
	out, err := v.virsh(ctx, "list", "--all", "--name")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			names = append(names, l)
		}
	}
	return names, nil
}

func (v *Virsh) Reboot(ctx context.Context, name string) error {
	_, err := v.virsh(ctx, "reboot", name, "--mode", "acpi")
	return err
}

func (v *Virsh) SetAutostart(ctx context.Context, name string, on bool) error {
	args := []string{"autostart", name}
	if !on {
		args = append(args, "--disable")
	}
	_, err := v.virsh(ctx, args...)
	return err
}

func (v *Virsh) Autostart(ctx context.Context, name string) (bool, error) {
	out, err := v.virsh(ctx, "dominfo", name)
	if err != nil {
		return false, err
	}
	for _, l := range strings.Split(string(out), "\n") {
		if k, val, ok := strings.Cut(l, ":"); ok && strings.TrimSpace(k) == "Autostart" {
			return strings.TrimSpace(val) == "enable", nil
		}
	}
	return false, nil
}

// Addresses asks libvirt's DHCP leases first, then the guest agent, then the
// host ARP table — whichever knows the guest's address first.
func (v *Virsh) Addresses(ctx context.Context, name string) ([]string, error) {
	for _, src := range []string{"lease", "agent", "arp"} {
		out, err := v.virsh(ctx, "domifaddr", name, "--source", src)
		if errors.Is(err, ErrNoDomain) {
			return nil, err
		}
		if err != nil {
			continue // e.g. guest agent not running yet; try the next source
		}
		if ips := ParseIfAddr(string(out)); len(ips) > 0 {
			return ips, nil
		}
	}
	return nil, nil // no address yet is normal while the guest boots
}

// ParseIfAddr extracts IPv4 addresses from `virsh domifaddr` output.
func ParseIfAddr(out string) []string {
	var ips []string
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		for i := 0; i+1 < len(f); i++ {
			if f[i] == "ipv4" {
				ip, _, _ := strings.Cut(f[i+1], "/")
				if ip != "127.0.0.1" {
					ips = append(ips, ip)
				}
			}
		}
	}
	return ips
}

// ParseState maps virsh domstate output to State.
func ParseState(s string) State {
	switch strings.TrimSpace(s) {
	case "running", "idle", "blocked":
		return Running
	case "shut off":
		return Stopped
	case "paused", "pmsuspended":
		return Paused
	case "in shutdown":
		return Stopping
	case "crashed":
		return Crashed
	}
	return Unknown
}

func (s State) String() string { return string(s) }

var _ fmt.Stringer = State("")
