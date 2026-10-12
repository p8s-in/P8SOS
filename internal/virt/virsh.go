package virt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
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
	// FSFreeze / FSThaw pause guest filesystem writes (via the guest agent) so a
	// disk snapshot is clean. Freeze fails if the agent isn't running.
	FSFreeze(ctx context.Context, name string) error
	FSThaw(ctx context.Context, name string) error
	// VNCAddr is the host:port of a running domain's VNC screen (127.0.0.1 only).
	VNCAddr(ctx context.Context, name string) (string, error)
	// Screenshot grabs the VM's screen straight from QEMU (PPM or PNG).
	Screenshot(ctx context.Context, name string) ([]byte, error)
	// SetUserPassword sets a guest account's password via the guest agent.
	SetUserPassword(ctx context.Context, name, user, password string) error
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

func (v *Virsh) FSFreeze(ctx context.Context, name string) error {
	_, err := v.virsh(ctx, "domfsfreeze", name)
	return err
}

func (v *Virsh) FSThaw(ctx context.Context, name string) error {
	_, err := v.virsh(ctx, "domfsthaw", name)
	return err
}

func (v *Virsh) VNCAddr(ctx context.Context, name string) (string, error) {
	out, err := v.virsh(ctx, "domdisplay", "--type", "vnc", name)
	if err != nil {
		return "", err
	}
	return ParseVNCDisplay(string(out))
}

// ParseVNCDisplay turns `virsh domdisplay` output (vnc://localhost:0) into
// 127.0.0.1:5900. Only loopback displays are accepted: p8sd is the only way in.
func ParseVNCDisplay(out string) (string, error) {
	s := strings.TrimSpace(out)
	rest, ok := strings.CutPrefix(s, "vnc://")
	if !ok {
		return "", fmt.Errorf("no VNC display (got %q)", s)
	}
	host, disp, ok := strings.Cut(rest, ":")
	n, err := strconv.Atoi(strings.TrimRight(disp, "/"))
	if !ok || err != nil || n < 0 {
		return "", fmt.Errorf("bad VNC display %q", s)
	}
	if host != "localhost" && host != "127.0.0.1" {
		return "", fmt.Errorf("VNC display %q is not on loopback", s)
	}
	return fmt.Sprintf("127.0.0.1:%d", 5900+n), nil
}

// Screenshot returns the VM's screen as it is right now (PPM or PNG bytes,
// straight from QEMU's framebuffer, independent of any VNC client).
func (v *Virsh) Screenshot(ctx context.Context, name string) ([]byte, error) {
	f, err := os.CreateTemp("", "p8s-shot-*")
	if err != nil {
		return nil, err
	}
	f.Close()
	defer os.Remove(f.Name())
	if _, err := v.virsh(ctx, "screenshot", name, f.Name()); err != nil {
		return nil, err
	}
	return os.ReadFile(f.Name())
}

// DomStats returns `virsh domstats --raw` for all running domains (one call).
func (v *Virsh) DomStats(ctx context.Context) (string, error) {
	out, err := v.virsh(ctx, "domstats", "--raw", "--cpu-total", "--balloon", "--vcpu", "--block", "--interface", "--list-running")
	return string(out), err
}

func (v *Virsh) SetUserPassword(ctx context.Context, name, user, password string) error {
	_, err := v.virsh(ctx, "set-user-password", name, user, password)
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
