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
