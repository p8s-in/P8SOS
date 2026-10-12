package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"p8sos/internal/backup"
	"p8sos/internal/cloudinit"
	"p8sos/internal/jobs"
	"p8sos/internal/virt"
)

// Backups are full copies of a VM's disk in files (see package backup). Taking
// one or restoring one runs as a background job.

const backupSnapPrefix = "p8s-backup-"

// Backup describes one stored backup.
type Backup = backup.Meta

// Job is a background operation.
type Job = jobs.Job

// Schedule is a VM's automatic backup setting.
type Schedule struct {
	Daily bool `json:"daily"`
	Keep  int  `json:"keep"` // how many backups to keep; older ones are deleted
}

type deviceWaiter interface {
	WaitDevice(ctx context.Context, vm string) (string, error)
}

var errBackupsOff = fmt.Errorf("%w: backups are not configured on this host", ErrConflict)

func (s *Service) backupsReady() error {
	if s.Backups == nil || s.Jobs == nil {
		return errBackupsOff
	}
	return nil
}

// StartBackup begins backing up a VM. It works while the VM runs: the guest's
// filesystems are frozen for the instant the snapshot is taken.
func (s *Service) StartBackup(ctx context.Context, name string) (Job, error) {
	return s.startBackup(ctx, name, 0)
}

func (s *Service) startBackup(ctx context.Context, name string, keep int) (Job, error) {
	if err := s.backupsReady(); err != nil {
		return Job{}, err
	}
	if _, err := s.Get(ctx, name); err != nil {
		return Job{}, err
	}
	j, err := s.Jobs.Start("backup", name, func(ctx context.Context, p jobs.Progress) (string, error) {
		return s.runBackup(ctx, name, keep, p)
	})
	if errors.Is(err, jobs.ErrBusy) {
		return Job{}, fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return j, err
}

func (s *Service) runBackup(ctx context.Context, name string, keep int, p jobs.Progress) (string, error) {
	sd, err := s.snapDisks()
	if err != nil {
		return "", err
	}
	dom := Prefix + name
	id := s.Backups.NewID()
	snap := backupSnapPrefix + id

	// Snapshot under the VM lock (instant), copy without it (minutes), so the
	// VM can be used, stopped or started while its backup is written.
	p("taking a snapshot")
	unlock := s.lock(dom)
	v, err := s.get(ctx, dom)
	if err != nil {
		unlock()
		return "", err
	}
	consistent := v.State == virt.Stopped
	if v.State == virt.Running {
		consistent = s.HV.FSFreeze(ctx, dom) == nil // thawed right after the snapshot below
	}
	err = sd.Snapshot(ctx, dom, snap)
	if consistent && v.State == virt.Running {
		s.logErr("thaw "+dom, s.HV.FSThaw(context.WithoutCancel(ctx), dom))
	}
	unlock()
	if err != nil {
		return "", err
	}
	defer func() { s.logErr("remove backup snapshot", sd.DestroySnapshot(context.WithoutCancel(ctx), dom, snap)) }()

	p("copying the disk")
	m, err := s.Backups.Save(ctx, dom, snap, Backup{
		ID: id, VM: name, Template: v.Template, User: v.User, Network: v.Network, OSType: v.OSType,
		VCPUs: v.VCPUs, MemoryMiB: v.MemoryMiB, DiskGB: v.DiskGB, Consistent: consistent, Owner: v.Owner,
		Pool: poolOf(v), PublicIP: publicIPOf(v),
	})
	if err != nil {
		return "", err
	}
	msg := fmt.Sprintf("backup %s of %s saved (%s)", m.ID, name, humanBytes(m.Bytes))
	if keep > 0 {
		if n, err := s.Backups.Prune(name, keep); err != nil {
			s.logErr("prune backups", err)
		} else if n > 0 {
			msg += fmt.Sprintf(", %d old one(s) removed", n)
		}
	}
	if s.AfterBackup != nil {
		s.AfterBackup("vm", name, m.ID)
	}
	return msg, nil
}

func humanBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(b)/(1<<20))
	}
	return fmt.Sprintf("%d KB", b>>10)
}

// ListBackups returns stored backups, newest first; name "" = all VMs
// (including VMs that were deleted since).
func (s *Service) ListBackups(name string) ([]Backup, error) {
	if err := s.backupsReady(); err != nil {
		return nil, err
	}
	if name != "" && !reName.MatchString(name) {
		return nil, fmt.Errorf("%w: vm %q", ErrNotFound, name)
	}
	return s.Backups.List(name)
}

// DeleteBackup removes one backup file.
func (s *Service) DeleteBackup(name, id string) error {
	if err := s.backupsReady(); err != nil {
		return err
	}
	if err := s.Backups.Delete(name, id); errors.Is(err, backup.ErrNotFound) {
		return fmt.Errorf("%w: backup %s of %q", ErrNotFound, id, name)
	} else if err != nil {
		return err
	}
	return nil
}

// StartRestore brings a backup back. With newName empty (or the VM's own name)
// the VM's disk is replaced — the VM must be stopped; if the VM was deleted it
// is recreated. With another name a new VM is made from the backup (a copy).
func (s *Service) StartRestore(ctx context.Context, name, id, newName string) (Job, error) {
	if err := s.backupsReady(); err != nil {
		return Job{}, err
	}
	m, err := s.Backups.Get(name, id)
	if errors.Is(err, backup.ErrNotFound) {
		return Job{}, fmt.Errorf("%w: backup %s of %q", ErrNotFound, id, name)
	} else if err != nil {
		return Job{}, err
	}
	target := newName
	if target == "" {
		target = name
	}
	if !reName.MatchString(target) {
		return Job{}, fmt.Errorf("%w: name must be 2-49 chars: lowercase letters, digits, hyphens; start with a letter", ErrInvalid)
	}
	dom := Prefix + target
	st, err := s.HV.State(ctx, dom)
	exists := err == nil
	if err != nil && !errors.Is(err, virt.ErrNoDomain) {
		return Job{}, err
	}
	switch {
	case exists && target != name:
		return Job{}, fmt.Errorf("%w: vm %q already exists; pick another name", ErrExists, target)
	case exists && st != virt.Stopped:
		return Job{}, fmt.Errorf("%w: stop vm %q before restoring over it (it is %s)", ErrConflict, target, st)
	}
	j, err := s.Jobs.Start("restore", target, func(ctx context.Context, p jobs.Progress) (string, error) {
		if exists {
			return s.restoreInPlace(ctx, target, m, p)
		}
		return s.restoreAsNew(ctx, target, m, p)
	})
	if errors.Is(err, jobs.ErrBusy) {
		return Job{}, fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return j, err
}

func (s *Service) restoreInPlace(ctx context.Context, name string, m Backup, p jobs.Progress) (string, error) {
	dom := Prefix + name
	defer s.lock(dom)()
	if st, err := s.state(ctx, dom); err != nil {
		return "", err
	} else if st != virt.Stopped {
		return "", fmt.Errorf("vm %q was started meanwhile; stop it and try again", name)
	}
	p("writing the disk back")
	if err := s.Backups.Restore(ctx, m, dom, true); err != nil {
		return "", err
	}
	if w, ok := s.Disks.(deviceWaiter); ok {
		if _, err := w.WaitDevice(ctx, dom); err != nil {
			return "", err
		}
	}
	if sd, err := s.snapDisks(); err == nil {
		if gb, err := sd.DiskSizeGB(ctx, dom); err == nil {
			s.logErr("record disk size", s.redefine(ctx, dom, func(sp *virt.Spec) { sp.DiskGB = gb }))
		}
	}
	return fmt.Sprintf("%s is back at backup %s (snapshots taken after it are gone)", name, m.ID), nil
}

func (s *Service) restoreAsNew(ctx context.Context, name string, m Backup, p jobs.Progress) (string, error) {
	dom := Prefix + name
	defer s.lock(dom)()
	if _, err := s.HV.State(ctx, dom); err == nil {
		return "", fmt.Errorf("vm %q was created meanwhile", name)
	}
	var undo []func()
	fail := func(err error) (string, error) {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
		return "", err
	}
	bg := context.WithoutCancel(ctx)
	p("writing the disk")
	if err := s.Backups.Restore(ctx, m, dom, false); err != nil {
		return fail(err)
	}
	undo = append(undo, func() { s.logErr("rollback restored disk", s.Disks.DestroyDisk(bg, dom)) })
	disk := "/dev/zvol/" + s.Backups.Dataset + "/" + dom + "-disk0"
	if w, ok := s.Disks.(deviceWaiter); ok {
		d, err := w.WaitDevice(ctx, dom)
		if err != nil {
			return fail(err)
		}
		disk = d
	}
	diskGB := m.DiskGB
	if sd, err := s.snapDisks(); err == nil {
		if gb, err := sd.DiskSizeGB(ctx, dom); err == nil && gb > 0 {
			diskGB = gb
		}
	}
	uuid := newUUID()
	// A new instance-id makes cloud-init treat it as a new machine: new hostname,
	// new SSH host keys. Users, passwords and data come from the backup.
	ci := cloudinit.Spec{InstanceID: uuid, Hostname: name, SSHKeys: s.defaultKeys(m.Owner)}
	network := m.Network
	if network == NetLAN && !s.LANAvailable() || network == NetPublic {
		network = NetNAT
	}
	mac, note, bridge := virt.RandomMAC(), "", s.bridgeFor(network)
	// A deleted VM recreated under its own name gets its public address back
	// (or another one from the pool); copies under a new name stay private.
	if m.Pool != "" && name == m.VM && s.Pools != nil {
		if l, release, err := s.lease(m.Pool, name, m.PublicIP); err == nil {
			undo = append(undo, release)
			ci.Static, bridge = static(l, mac), l.Bridge
			if l.IP != m.PublicIP {
				note = fmt.Sprintf(" (new public IP %s; %s was taken)", l.IP, m.PublicIP)
			}
		} else {
			note = fmt.Sprintf(" (no public IP: %v; it is on the private network)", err)
		}
	}
	seed, err := s.Seeds.Build(ctx, dom, ci)
	if err != nil {
		return fail(err)
	}
	undo = append(undo, func() { s.logErr("rollback seed", s.Seeds.Remove(dom)) })
	spec := virt.Spec{
		Name: dom, UUID: uuid, VCPUs: max(m.VCPUs, 1), MemoryMiB: max(m.MemoryMiB, 256), DiskGB: diskGB,
		DiskPath: disk, SeedISO: seed, Bridge: bridge, MAC: mac,
		Template: m.Template, OSType: m.OSType, CreatedAt: time.Now().UTC().Format(time.RFC3339), Owner: m.Owner,
	}
	x, err := virt.DomainXML(spec)
	if err != nil {
		return fail(err)
	}
	if err := s.HV.Define(ctx, x); err != nil {
		return fail(err)
	}
	undo = append(undo, func() { s.logErr("rollback define", s.HV.Undefine(bg, dom)) })
	if err := s.HV.SetAutostart(ctx, dom, true); err != nil {
		return fail(err)
	}
	p("starting")
	if err := s.HV.Start(ctx, dom); err != nil {
		return fail(err)
	}
	s.afterStart(ctx, name)
	return fmt.Sprintf("%s created from backup %s of %s and started%s", name, m.ID, m.VM, note), nil
}

// --- daily schedule ---

var schedMu sync.Mutex

func (s *Service) loadSchedules() (map[string]Schedule, error) {
	m := map[string]Schedule{}
	if s.ScheduleFile == "" {
		s.memSched.Range(func(k, v any) bool { m[k.(string)] = v.(Schedule); return true })
		return m, nil
	}
	b, err := os.ReadFile(s.ScheduleFile)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	return m, json.Unmarshal(b, &m)
}

// GetSchedule returns a VM's backup schedule (zero value = none).
func (s *Service) GetSchedule(name string) (Schedule, error) {
	schedMu.Lock()
	defer schedMu.Unlock()
	m, err := s.loadSchedules()
	return m[name], err
}

// SetSchedule changes a VM's daily backup. Daily=false removes it.
func (s *Service) SetSchedule(name string, sch Schedule) error {
	if sch.Daily && (sch.Keep < 1 || sch.Keep > 365) {
		return fmt.Errorf("%w: keep must be 1-365", ErrInvalid)
	}
	schedMu.Lock()
	defer schedMu.Unlock()
	m, err := s.loadSchedules()
	if err != nil {
		return err
	}
	if sch.Daily {
		m[name] = sch
	} else {
		delete(m, name)
	}
	if s.ScheduleFile == "" {
		s.memSched.Delete(name)
		if sch.Daily {
			s.memSched.Store(name, sch)
		}
		return nil
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.MkdirAll(filepath.Dir(s.ScheduleFile), 0o755); err != nil {
		return err
	}
	tmp := s.ScheduleFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.ScheduleFile)
}

// RunSchedule takes the daily backups at `at` (HH:MM, host time) until ctx ends.
func (s *Service) RunSchedule(ctx context.Context, at string, now func() time.Time) {
	if now == nil {
		now = time.Now
	}
	last := ""
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		if n := now(); n.Format("15:04") == at && n.Format("2006-01-02") != last {
			last = n.Format("2006-01-02")
			s.RunScheduledBackups(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// RunScheduledBackups starts the backup of every VM that has a daily schedule.
func (s *Service) RunScheduledBackups(ctx context.Context) []string {
	schedMu.Lock()
	m, err := s.loadSchedules()
	schedMu.Unlock()
	if err != nil {
		s.logErr("read backup schedule", err)
		return nil
	}
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	var started []string
	for _, n := range names {
		if !m[n].Daily {
			continue
		}
		if strings.Contains(n, ":") { // another kind of guest, e.g. "ct:web2"
			if s.OtherBackup == nil {
				continue
			}
			if err := s.OtherBackup(ctx, n, m[n].Keep); err != nil {
				if !errors.Is(err, ErrNotFound) {
					s.logErr("scheduled backup of "+n, err)
				}
				continue
			}
			started = append(started, n)
			continue
		}
		if _, err := s.startBackup(ctx, n, m[n].Keep); err != nil {
			if !errors.Is(err, ErrNotFound) {
				s.logErr("scheduled backup of "+n, err)
			}
			continue
		}
		started = append(started, n)
	}
	if s.Log != nil && len(started) > 0 {
		s.Log.Info("scheduled backups started", "vms", strings.Join(started, ","))
	}
	return started
}

// Job returns one background job.
func (s *Service) Job(id string) (Job, bool) {
	if s.Jobs == nil {
		return Job{}, false
	}
	return s.Jobs.Get(id)
}

// ListJobs lists recent background jobs, newest first.
func (s *Service) ListJobs() []Job {
	if s.Jobs == nil {
		return []Job{}
	}
	return s.Jobs.List()
}

// defaultKeys are the host's SSH keys for admin VMs; customer VMs don't get them.
func (s *Service) defaultKeys(owner string) []string {
	if owner != "" {
		return nil
	}
	return s.SSHKeys
}

func poolOf(v VM) string {
	if v.Public != nil {
		return v.Public.Pool
	}
	return ""
}

func publicIPOf(v VM) string {
	if v.Public != nil {
		return v.Public.IP
	}
	return ""
}
