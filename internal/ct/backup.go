package ct

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"p8sos/internal/backup"
	"p8sos/internal/execx"
	"p8sos/internal/jobs"
	"p8sos/internal/vm"
)

// Container backups: the disk (a ZFS filesystem) is snapshotted for an
// instant (processes frozen when it runs) and streamed into a compressed file
// next to the VM backups, under <backups>/_ct/<name>/. A backup comes back as
// a new container (or over the old one when it was deleted).

const backupSnap = "p8s-backup-"

// Backups returns the store for container backups (nil when not configured).
func (s *Service) backups() *backup.Store {
	if s.Backups == nil {
		return nil
	}
	b := *s.Backups
	b.Dir = filepath.Join(b.Dir, "_ct") // "_" never matches a VM name
	b.Disk = s.dataset
	b.RecvOpts = []string{} // filesystem: no zvol options; mountpoint/quota set after
	// zfs in the host's mount namespace, like every other container disk command
	b.Run = nsRunner{s.Run}
	cmd := b.Cmd
	b.Cmd = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name == "zfs" {
			name, args = "nsenter", append([]string{"--mount=/proc/1/ns/mnt", "--", "zfs"}, args...)
		}
		if cmd != nil {
			return cmd(ctx, name, args...)
		}
		return exec.CommandContext(ctx, name, args...)
	}
	return &b
}

type nsRunner struct{ r execx.Runner }

func (n nsRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if name == "zfs" {
		return n.r.Run(ctx, "nsenter", append([]string{"--mount=/proc/1/ns/mnt", "--", "zfs"}, args...)...)
	}
	return n.r.Run(ctx, name, args...)
}

var errBackupsOff = fmt.Errorf("%w: backups are not configured on this host", vm.ErrConflict)

// ListBackups returns a container's backups, newest first ("" = all).
func (s *Service) ListBackups(name string) ([]backup.Meta, error) {
	b := s.backups()
	if b == nil {
		return []backup.Meta{}, nil
	}
	return b.List(name)
}

// DeleteBackup removes one backup.
func (s *Service) DeleteBackup(name, id string) error {
	b := s.backups()
	if b == nil {
		return errBackupsOff
	}
	err := b.Delete(name, id)
	if errors.Is(err, backup.ErrNotFound) {
		return fmt.Errorf("%w: backup %s of %s", vm.ErrNotFound, id, name)
	}
	return err
}

// StartBackup backs a container up in the background (keep > 0 prunes to
// that many afterwards).
func (s *Service) StartBackup(ctx context.Context, name string, keep int) (jobs.Job, error) {
	b := s.backups()
	if b == nil || s.Jobs == nil {
		return jobs.Job{}, errBackupsOff
	}
	if _, err := s.get(name); err != nil {
		return jobs.Job{}, err
	}
	j, err := s.Jobs.Start("ct-backup", name, func(ctx context.Context, p jobs.Progress) (string, error) {
		return s.runBackup(ctx, b, name, keep, p)
	})
	if errors.Is(err, jobs.ErrBusy) {
		return jobs.Job{}, fmt.Errorf("%w: %v", vm.ErrConflict, err)
	}
	return j, err
}

func (s *Service) runBackup(ctx context.Context, b *backup.Store, name string, keep int, p jobs.Progress) (string, error) {
	m, err := s.get(name)
	if err != nil {
		return "", err
	}
	id := b.NewID()
	snap := backupSnap + id
	p("taking a snapshot")
	running := s.state(ctx, name) == "running"
	frozen := false
	if running {
		_, ferr := s.Run.Run(ctx, "lxc-freeze", "-n", name, "-P", s.LXCDir)
		frozen = ferr == nil
	}
	_, err = s.zfs(ctx, "snapshot", s.dataset(name)+"@"+snap)
	if frozen {
		_, _ = s.Run.Run(context.WithoutCancel(ctx), "lxc-unfreeze", "-n", name, "-P", s.LXCDir)
	}
	if err != nil {
		return "", fmt.Errorf("snapshot: %w", err)
	}
	defer func() { _, _ = s.zfs(context.WithoutCancel(ctx), "destroy", s.dataset(name)+"@"+snap) }()

	p("copying the disk")
	bm, err := b.Save(ctx, name, snap, backup.Meta{
		ID: id, VM: name, Template: m.Template, User: "root", Network: m.Network, OSType: "ct",
		VCPUs: m.VCPUs, MemoryMiB: m.MemoryMiB, DiskGB: m.DiskGB, Owner: m.Owner,
		Consistent: !running || frozen,
	})
	if err != nil {
		return "", err
	}
	msg := fmt.Sprintf("backup %s of container %s saved (%s)", bm.ID, name, human(bm.Bytes))
	if keep > 0 {
		if n, err := b.Prune(name, keep); err == nil && n > 0 {
			msg += fmt.Sprintf(", %d old one(s) removed", n)
		}
	}
	return msg, nil
}

// StartRestore brings backup id of container name back as container newName
// (which must not exist; name itself works once the original is deleted).
func (s *Service) StartRestore(ctx context.Context, name, id, newName string, exists func(string) bool) (jobs.Job, error) {
	b := s.backups()
	if b == nil || s.Jobs == nil {
		return jobs.Job{}, errBackupsOff
	}
	bm, err := b.Get(name, id)
	if err != nil {
		return jobs.Job{}, fmt.Errorf("%w: backup %s of %s", vm.ErrNotFound, id, name)
	}
	if newName == "" {
		newName = name
	}
	if !reName.MatchString(newName) {
		return jobs.Job{}, fmt.Errorf("%w: name: 2-49 lowercase letters, digits and dashes, starting with a letter", vm.ErrInvalid)
	}
	if s.Exists(newName) || (exists != nil && exists(newName)) {
		return jobs.Job{}, fmt.Errorf("%w: %q is taken; pick a new name for the restored container", vm.ErrExists, newName)
	}
	j, err := s.Jobs.Start("ct-restore", newName, func(ctx context.Context, p jobs.Progress) (string, error) {
		return s.runRestore(ctx, b, bm, newName, p)
	})
	if errors.Is(err, jobs.ErrBusy) {
		return jobs.Job{}, fmt.Errorf("%w: %v", vm.ErrConflict, err)
	}
	return j, err
}

func (s *Service) runRestore(ctx context.Context, b *backup.Store, bm backup.Meta, name string, p jobs.Progress) (string, error) {
	br, err := s.bridgeFor(bm.Network)
	if err != nil { // home network gone: fall back to the private one
		bm.Network, br = vm.NetNAT, s.Bridge
	}
	p("unpacking the disk")
	ds := s.dataset(name)
	if err := b.Restore(ctx, bm, name, false); err != nil {
		return "", err
	}
	undo := func() {
		_, _ = s.zfs(context.Background(), "destroy", "-r", ds)
		_ = os.RemoveAll(s.confDir(name))
	}
	if _, err := s.zfs(ctx, "set", "mountpoint="+s.rootfs(name), fmt.Sprintf("refquota=%dG", bm.DiskGB), ds); err != nil {
		undo()
		return "", fmt.Errorf("set up the disk: %w", err)
	}
	if _, err := s.zfs(ctx, "mount", ds); err != nil && !alreadyMounted(err) {
		undo()
		return "", fmt.Errorf("mount the disk: %w", err)
	}
	if bm.VM != name { // restored under a new name
		if err := s.prepare(name, nil, ""); err != nil {
			undo()
			return "", err
		}
	}
	m := &meta{Template: bm.Template, VCPUs: bm.VCPUs, MemoryMiB: bm.MemoryMiB, DiskGB: bm.DiskGB, MAC: newMAC(),
		Network: bm.Network, Owner: bm.Owner, Autostart: true, Created: s.now().UTC().Format(time.RFC3339)}
	if err := s.writeConfig(name, m, br); err != nil {
		undo()
		return "", err
	}
	if err := s.put(name, m); err != nil {
		undo()
		return "", err
	}
	p("starting")
	if err := s.start(ctx, name); err != nil {
		return fmt.Sprintf("container %s restored from backup %s, but it didn't start: %v", name, bm.ID, err), nil
	}
	return fmt.Sprintf("container %s restored from backup %s of %s", name, bm.ID, bm.VM), nil
}

func alreadyMounted(err error) bool {
	return err != nil && strings.Contains(err.Error(), "already mounted")
}

func human(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d KB", n/1024)
}
