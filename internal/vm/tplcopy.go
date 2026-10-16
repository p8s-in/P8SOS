package vm

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
)

// Copying a template made from a VM to another server of the cluster, so
// new VMs from it can go on either. The stream is one JSON line (the
// template) and then its disks (storage.ZFS SendTemplate).

// templateMover is the storage part (storage.ZFS).
type templateMover interface {
	SendTemplate(ctx context.Context, snaps []string, w io.Writer) error
	RecvTemplate(ctx context.Context, ds string, idx []int, r io.Reader) error
}

// TemplateNode is a server that has a template.
type TemplateNode struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

var tplSave sync.Mutex // the templates file, while a copy comes in

func (s *Service) custom(name string) (customTemplate, bool) {
	for _, c := range s.customTemplates() {
		if c.Name == name {
			return c, true
		}
	}
	return customTemplate{}, false
}

// TemplateCopyCheck says whether template name can be copied from here.
func (s *Service) TemplateCopyCheck(name string) error {
	if _, ok := s.Disks.(templateMover); !ok {
		return fmt.Errorf("%w: this server's storage can't copy templates", ErrConflict)
	}
	if _, ok := s.custom(name); !ok {
		if _, ok := s.template(name); ok {
			return fmt.Errorf("%w: %q is a built-in template: every server can add it (Server → OS templates)", ErrConflict, name)
		}
		return fmt.Errorf("%w: template %q", ErrNotFound, name)
	}
	return nil
}

// ExportTemplate writes template name (made from a VM) to w.
func (s *Service) ExportTemplate(ctx context.Context, name string, w io.Writer) error {
	if err := s.TemplateCopyCheck(name); err != nil {
		return err
	}
	c, _ := s.custom(name)
	b, _ := json.Marshal(c)
	if _, err := w.Write(append(b, '\n')); err != nil {
		return err
	}
	snaps := []string{c.Snapshot}
	for _, e := range c.Extra {
		snaps = append(snaps, e.Snapshot)
	}
	return s.Disks.(templateMover).SendTemplate(ctx, snaps, w)
}

func snapName(s string) string { _, n, _ := strings.Cut(s, "@"); return n }

// ImportTemplate makes the template from an ExportTemplate stream.
func (s *Service) ImportTemplate(ctx context.Context, r io.Reader) (TemplateInfo, error) {
	tm, ok := s.Disks.(templateMover)
	if !ok || s.TemplatesFile == "" {
		return TemplateInfo{}, fmt.Errorf("%w: this server can't take templates", ErrConflict)
	}
	br := bufio.NewReaderSize(r, 64<<10)
	line, err := br.ReadBytes('\n')
	if err != nil {
		return TemplateInfo{}, fmt.Errorf("%w: template stream: %v", ErrInvalid, err)
	}
	var c customTemplate
	if err := json.Unmarshal(line, &c); err != nil || !reName.MatchString(c.Name) || snapName(c.Snapshot) == "" {
		return TemplateInfo{}, fmt.Errorf("%w: template stream: bad header", ErrInvalid)
	}
	if _, ok := s.template(c.Name); ok {
		return TemplateInfo{}, fmt.Errorf("%w: a template %q exists on this server", ErrExists, c.Name)
	}
	ds := s.templatesDataset() + "/" + c.Name
	var idx []int
	for _, e := range c.Extra {
		if e.Index < 1 || e.Index > 8 || snapName(e.Snapshot) == "" {
			return TemplateInfo{}, fmt.Errorf("%w: template stream: bad data disk", ErrInvalid)
		}
		idx = append(idx, e.Index)
	}
	if err := tm.RecvTemplate(ctx, ds, idx, br); err != nil {
		return TemplateInfo{}, err
	}
	c.Snapshot = ds + "@" + snapName(c.Snapshot)
	for i, e := range c.Extra {
		c.Extra[i].Snapshot = fmt.Sprintf("%s-disk%d@%s", ds, e.Index, snapName(e.Snapshot))
	}
	tplSave.Lock()
	defer tplSave.Unlock()
	if _, ok := s.template(c.Name); ok {
		s.logErr("drop template copy", s.Disks.(templateDisks).DestroyTemplate(context.WithoutCancel(ctx), ds))
		return TemplateInfo{}, fmt.Errorf("%w: a template %q exists on this server", ErrExists, c.Name)
	}
	if err := s.saveCustom(append(s.customTemplates(), c)); err != nil {
		if td, ok := s.Disks.(templateDisks); ok {
			s.logErr("drop template copy", td.DestroyTemplate(context.WithoutCancel(ctx), ds))
		}
		return TemplateInfo{}, err
	}
	return TemplateInfo{Name: c.Name, Label: labelOf(c.Template), MinDiskGB: c.MinDiskGB, OSVariant: c.OSVariant,
		DefaultUser: c.User(), Custom: true, From: c.From, Created: c.Created}, nil
}
