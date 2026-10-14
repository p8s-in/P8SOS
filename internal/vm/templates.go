package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"p8sos/internal/config"
	"p8sos/internal/jobs"
	"p8sos/internal/virt"
)

// Templates made from VMs (like Proxmox's "Convert to template", but the
// VM stays). The VM's root disk is copied into the templates dataset; new VMs
// are instant copies of it, exactly like the built-in OS templates. Before the
// copy a running VM is cleaned through its guest agent — cloud-init state,
// machine id, SSH host keys — and shut down, so every VM made from it gets
// its own identity.

// templateDisks is the storage part (storage.ZFS).
type templateDisks interface {
	MakeTemplate(ctx context.Context, vm, snap, ds string) (string, error)
	DestroyTemplate(ctx context.Context, ds string) error
}

// guestExec runs a script in a guest through its agent (virt.Virsh).
type guestExec interface {
	GuestExec(ctx context.Context, name, script string, timeout time.Duration) (int, string, error)
}

// TemplateInfo is a template as the dashboard lists it.
type TemplateInfo struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	MinDiskGB   int    `json:"min_disk_gb"`
	OSVariant   string `json:"os_variant"`
	DefaultUser string `json:"default_user"`
	Custom      bool   `json:"custom,omitempty"` // made from a VM (can be deleted)
	From        string `json:"from,omitempty"`   // the VM it was made from
	Created     string `json:"created,omitempty"`
	Node        string `json:"node,omitempty"` // cluster: the server that has it
	NodeName    string `json:"node_name,omitempty"`
}

// customTemplate is one entry of TemplatesFile.
type customTemplate struct {
	config.Template
	From    string `json:"from"`
	Created string `json:"created"`
}

// TemplateRequest makes a template from a VM.
type TemplateRequest struct {
	Name  string `json:"name"`  // template name, like the VM names
	Label string `json:"label"` // shown when making VMs ("" = the name)
}

var reLabel = regexp.MustCompile(`^[^\x00-\x1f<>]{1,60}$`)

// SealScript cleans a guest before it becomes a template: the next boot is
// a first boot (cloud-init runs again, a new machine id and SSH host keys).
const SealScript = `set +e
command -v cloud-init >/dev/null && { cloud-init clean --logs --seed --machine-id 2>/dev/null || cloud-init clean --logs --seed; }
truncate -s 0 /etc/machine-id 2>/dev/null; rm -f /var/lib/dbus/machine-id
rm -f /etc/ssh/ssh_host_*
rm -f /var/lib/dhcp/*.leases /var/lib/NetworkManager/*.lease 2>/dev/null
history -c 2>/dev/null; rm -f /root/.bash_history
sync; exit 0`

func (s *Service) customTemplates() []customTemplate {
	if s.TemplatesFile == "" {
		return nil
	}
	b, err := os.ReadFile(s.TemplatesFile)
	if err != nil {
		return nil
	}
	var l []customTemplate
	_ = json.Unmarshal(b, &l)
	return l
}

func (s *Service) saveCustom(l []customTemplate) error {
	sort.Slice(l, func(i, j int) bool { return l[i].Name < l[j].Name })
	b, _ := json.MarshalIndent(l, "", "  ")
	if err := os.MkdirAll(filepath.Dir(s.TemplatesFile), 0o750); err != nil {
		return err
	}
	tmp := s.TemplatesFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.TemplatesFile)
}

// AllTemplates are the built-in templates and those made from VMs.
func (s *Service) AllTemplates() []config.Template {
	l := append([]config.Template(nil), s.Templates...)
	for _, c := range s.customTemplates() {
		l = append(l, c.Template)
	}
	return l
}

// ListTemplates lists every template for the dashboard.
func (s *Service) ListTemplates(ctx context.Context) []TemplateInfo {
	out := []TemplateInfo{}
	for _, t := range s.Templates {
		out = append(out, TemplateInfo{Name: t.Name, Label: labelOf(t), MinDiskGB: t.MinDiskGB, OSVariant: t.OSVariant, DefaultUser: t.User()})
	}
	for _, c := range s.customTemplates() {
		out = append(out, TemplateInfo{Name: c.Name, Label: labelOf(c.Template), MinDiskGB: c.MinDiskGB, OSVariant: c.OSVariant,
			DefaultUser: c.User(), Custom: true, From: c.From, Created: c.Created})
	}
	return out
}

func labelOf(t config.Template) string {
	if t.Label != "" {
		return t.Label
	}
	return t.Name
}

// templatesDataset is where templates made from VMs go: next to the built-in ones.
func (s *Service) templatesDataset() string {
	for _, t := range s.Templates {
		if ds, _, ok := strings.Cut(t.Snapshot, "@"); ok {
			if i := strings.LastIndex(ds, "/"); i > 0 {
				return ds[:i]
			}
		}
	}
	return "tank/p8s/templates"
}

// StartMakeTemplate makes a template from a VM in the background.
func (s *Service) StartMakeTemplate(ctx context.Context, name string, r TemplateRequest) (Job, error) {
	if !reName.MatchString(r.Name) {
		return Job{}, fmt.Errorf("%w: template name: 2-49 chars, lowercase letters, digits, hyphens; start with a letter", ErrInvalid)
	}
	if r.Label != "" && !reLabel.MatchString(r.Label) {
		return Job{}, fmt.Errorf("%w: label: up to 60 characters", ErrInvalid)
	}
	if s.TemplatesFile == "" || s.Jobs == nil {
		return Job{}, fmt.Errorf("%w: templates from VMs aren't set up on this server", ErrConflict)
	}
	if _, ok := s.Disks.(templateDisks); !ok {
		return Job{}, fmt.Errorf("%w: this server's storage can't make templates", ErrConflict)
	}
	if _, ok := s.template(r.Name); ok {
		return Job{}, fmt.Errorf("%w: a template %q exists", ErrExists, r.Name)
	}
	v, err := s.Get(ctx, name)
	if err != nil {
		return Job{}, err
	}
	if v.Storage == SharedStorage {
		return Job{}, fmt.Errorf("%w: templates are made from VMs on local storage (%s is on Ceph)", ErrConflict, name)
	}
	if v.State == virt.Running {
		if _, ok := s.HV.(guestExec); !ok {
			return Job{}, fmt.Errorf("%w: shut it down first", ErrConflict)
		}
	} else if v.State != virt.Stopped {
		return Job{}, fmt.Errorf("%w: %s is %s; start or shut it down first", ErrConflict, name, v.State)
	}
	j, err := s.Jobs.Start("template", name, func(ctx context.Context, p jobs.Progress) (string, error) {
		return s.makeTemplate(ctx, name, r, p)
	})
	if errors.Is(err, jobs.ErrBusy) {
		return Job{}, fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return j, err
}

func (s *Service) makeTemplate(ctx context.Context, name string, r TemplateRequest, p jobs.Progress) (string, error) {
	td := s.Disks.(templateDisks)
	sd, err := s.snapDisks()
	if err != nil {
		return "", err
	}
	dom := Prefix + name
	bg := context.WithoutCancel(ctx)
	unlock := s.lock(dom)
	defer func() {
		if unlock != nil {
			unlock()
		}
	}()
	st, err := s.state(ctx, dom)
	if err != nil {
		return "", err
	}
	sp, err := s.spec(ctx, dom)
	if err != nil {
		return "", err
	}
	note := ""
	if st == virt.Running {
		p("cleaning the guest (cloud-init, machine id, SSH host keys)")
		code, out, err := s.HV.(guestExec).GuestExec(ctx, dom, SealScript, 2*time.Minute)
		if err != nil {
			return "", fmt.Errorf("%w (is qemu-guest-agent running in the VM? Or shut it down and try again — then it is copied as it is)", err)
		}
		if code != 0 {
			return "", fmt.Errorf("cleaning the guest failed (exit %d): %s", code, strings.TrimSpace(out))
		}
		p("shutting it down")
		if err := s.HV.Shutdown(ctx, dom); err != nil {
			return "", err
		}
		for deadline := time.Now().Add(3 * time.Minute); ; {
			if st, err := s.HV.State(ctx, dom); err == nil && st == virt.Stopped {
				break
			}
			if time.Now().After(deadline) || ctx.Err() != nil {
				return "", fmt.Errorf("%s didn't shut down in 3 minutes; shut it down and try again", name)
			}
			time.Sleep(2 * time.Second)
		}
		note = "; " + name + " was cleaned and shut down (it gets new SSH host keys when started)"
	} else if sp.SeedISO != "" {
		note = "; " + name + " was off, so it was copied as it is: VMs made from it share its machine id (start it once and make the template while it runs to clean it)"
	}
	snap := fmt.Sprintf("p8s-tpl-%d", time.Now().Unix())
	if err := sd.Snapshot(ctx, dom, snap); err != nil {
		return "", err
	}
	defer func() { s.logErr("drop template snapshot", sd.DestroySnapshot(bg, dom, snap)) }()
	unlock()
	unlock = nil

	p("copying the disk")
	ds := s.templatesDataset() + "/" + r.Name
	tsnap, err := td.MakeTemplate(ctx, dom, snap, ds)
	if err != nil {
		return "", err
	}
	src, _ := s.template(sp.Template)
	t := config.Template{Name: r.Name, Snapshot: tsnap, MinDiskGB: max(sp.DiskGB, 1), OSVariant: src.OSVariant,
		Label: r.Label, DefaultUser: src.DefaultUser}
	if t.Label == "" {
		t.Label = r.Name + " (from " + name + ")"
	}
	l := s.customTemplates()
	l = append(l, customTemplate{Template: t, From: name, Created: time.Now().UTC().Format(time.RFC3339)})
	if err := s.saveCustom(l); err != nil {
		s.logErr("drop template", td.DestroyTemplate(bg, ds))
		return "", err
	}
	return fmt.Sprintf("template %s made from %s (%d GB)%s", r.Name, name, t.MinDiskGB, note), nil
}

// DeleteTemplate deletes a template made from a VM.
func (s *Service) DeleteTemplate(ctx context.Context, name string) error {
	l := s.customTemplates()
	for i, c := range l {
		if c.Name != name {
			continue
		}
		td, ok := s.Disks.(templateDisks)
		if !ok {
			return fmt.Errorf("%w: this server's storage can't delete templates", ErrConflict)
		}
		ds, _, _ := strings.Cut(c.Snapshot, "@")
		if err := td.DestroyTemplate(ctx, ds); err != nil {
			return fmt.Errorf("%w: %v (delete those VMs first)", ErrConflict, err)
		}
		return s.saveCustom(append(l[:i:i], l[i+1:]...))
	}
	if _, ok := s.template(name); ok {
		return fmt.Errorf("%w: %q is a built-in template", ErrConflict, name)
	}
	return fmt.Errorf("%w: template %q", ErrNotFound, name)
}

func (s *Service) isCustomTemplate(name string) bool {
	for _, c := range s.customTemplates() {
		if c.Name == name {
			return true
		}
	}
	return false
}
