package ceph

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Getting Ceph back when it doesn't answer. Ceph's monitors need a majority:
// with 2 monitors and one server gone for good, nothing works. Two ways out,
// both run on a server that is still there:
//
//   - MonAlone: this server's monitor becomes the only one (the others are
//     taken out of the monitor map, the standard Ceph rescue). Data whose only
//     copies were on the gone server's disks is lost; the rest works again.
//   - Remove: Ceph is taken off this server (services off, configuration and
//     monitor/manager data put aside, not deleted), so it can be set up anew.
//     The disks Ceph had stay as they are unless Wipe asks to wipe them.

// More actions a server runs for itself.
const (
	ActMonAlone = "mon-alone"
	ActRemove   = "remove"
)

var reMonName = regexp.MustCompile(`\bmon\.([a-z0-9][a-z0-9-]*)`)

// ParseMonmap reads the monitor names from `monmaptool --print`.
func ParseMonmap(out string) []string {
	var l []string
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || !strings.Contains(t, "]") && !strings.Contains(t, ":6789") {
			continue
		}
		if m := reMonName.FindStringSubmatch(t); m != nil && !seen[m[1]] {
			seen[m[1]] = true
			l = append(l, m[1])
		}
	}
	return l
}

// CheckMonAlone says whether MonAlone can run here.
func (m *Manager) CheckMonAlone() error {
	if err := m.needConfigured(); err != nil {
		return err
	}
	if !m.exists(m.monDir()) {
		return fmt.Errorf("%w: this server has no Ceph monitor; run this on a server that has one", ErrConflict)
	}
	return nil
}

// MonAlone makes this server's monitor the only one and starts it.
func (m *Manager) MonAlone(ctx context.Context) error {
	if err := m.CheckMonAlone(); err != nil {
		return err
	}
	net, err := NetworkOf(m.confText())
	if err != nil {
		return err
	}
	ip, ok := m.AddrIn(net)
	if !ok {
		return fmt.Errorf("%w: this server has no address in the Ceph network %s", ErrInvalid, net)
	}
	fsid := FSIDOf(m.confText())
	tmp, err := m.workDir()
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	mp := filepath.Join(tmp, "monmap")
	unit := "ceph-mon@" + m.Name
	run := func(args ...string) ([]byte, error) {
		out, err := m.Run.Run(ctx, args[0], args[1:]...)
		if err != nil {
			return out, fmt.Errorf("%s: %w", strings.Join(args[:min(2, len(args))], " "), err)
		}
		return out, nil
	}
	if _, err := run("systemctl", "stop", unit); err != nil {
		return err
	}
	// whatever happens below, the monitor is started again
	defer func() {
		bg := context.WithoutCancel(ctx)
		_, _ = m.Run.Run(bg, "chown", "-R", "ceph:ceph", m.monDir())
		_, _ = m.Run.Run(bg, "systemctl", "reset-failed", unit)
		_, _ = m.Run.Run(bg, "systemctl", "start", unit)
	}()
	if _, err := run("ceph-mon", "-i", m.Name, "--extract-monmap", mp); err != nil {
		return err
	}
	out, err := run("monmaptool", "--print", mp)
	if err != nil {
		return err
	}
	names := ParseMonmap(string(out))
	var gone []string
	for _, n := range names {
		if n != m.Name {
			gone = append(gone, n)
		}
	}
	if len(gone) > 0 {
		for _, n := range gone {
			if _, err := run("monmaptool", mp, "--rm", n); err != nil {
				return err
			}
		}
		if _, err := run("ceph-mon", "-i", m.Name, "--inject-monmap", mp); err != nil {
			return err
		}
	}
	if err := writeAtomic(m.ConfFile(), Conf(fsid, net, []Mon{{Name: m.Name, IP: ip.String()}}), 0o644); err != nil {
		return err
	}
	_, _ = m.Run.Run(ctx, "chown", "-R", "ceph:ceph", m.monDir())
	_, _ = m.Run.Run(ctx, "systemctl", "reset-failed", unit)
	if _, err := run("systemctl", "start", unit); err != nil {
		return err
	}
	if m.exists(m.mgrDir()) {
		_, _ = m.Run.Run(ctx, "systemctl", "restart", "ceph-mgr@"+m.Name)
	}
	if err := m.waitUp(ctx); err != nil {
		return err
	}
	if len(gone) > 0 {
		m.mu.Lock()
		m.task.Detail = "monitors taken out: " + strings.Join(gone, ", ")
		m.mu.Unlock()
	}
	return nil
}

// CheckRemove says whether Ceph can be taken off this server.
func (m *Manager) CheckRemove(ctx context.Context) error {
	if !m.Configured() && !m.exists(m.monDir()) && len(m.localOSDs(ctx)) == 0 {
		return fmt.Errorf("%w: Ceph isn't set up on this server", ErrConflict)
	}
	if m.InUse != nil {
		if l := m.InUse(ctx); len(l) > 0 {
			return fmt.Errorf("%w: VMs on this server have disks on Ceph (%s); move or delete them first", ErrConflict, strings.Join(l, ", "))
		}
	}
	return nil
}

// Remove takes Ceph off this server: its services stop, and the configuration,
// keys and monitor/manager/disk data are moved to /var/lib/ceph/p8s-removed-<time>
// (nothing is deleted). wipe: also wipe the disks this server gave to Ceph.
func (m *Manager) Remove(ctx context.Context, wipe bool) error {
	if err := m.CheckRemove(ctx); err != nil {
		return err
	}
	osds := m.localOSDs(ctx)
	units := []string{"ceph-mon@" + m.Name, "ceph-mgr@" + m.Name}
	for _, id := range osds {
		units = append(units, fmt.Sprintf("ceph-osd@%d", id))
	}
	// the units that mount Ceph's disks at boot (ceph-volume@lvm-<id>-<uuid>)
	if out, err := m.Run.Run(ctx, "systemctl", "list-unit-files", "ceph-volume@*", "--no-legend"); err == nil {
		for _, l := range strings.Split(string(out), "\n") {
			if f := strings.Fields(l); len(f) > 0 && strings.HasPrefix(f[0], "ceph-volume@") {
				units = append(units, f[0])
			}
		}
	}
	for _, u := range units {
		_, _ = m.Run.Run(ctx, "systemctl", "disable", "--now", u)
	}
	for _, id := range osds { // a disk's data directory is a small mount
		_, _ = m.Run.Run(ctx, "umount", m.lib(fmt.Sprintf("osd/ceph-%d", id)))
	}
	for _, u := range []string{"ceph-osd.target", "ceph-mgr.target", "ceph-mon.target", "ceph.target"} {
		_, _ = m.Run.Run(ctx, "systemctl", "disable", "--now", u)
	}
	var problems []string
	if wipe {
		for _, id := range osds {
			// only the volumes Ceph made for this disk (found by their OSD tag)
			if _, err := m.Run.Run(ctx, "ceph-volume", "lvm", "zap", "--osd-id", fmt.Sprint(id), "--destroy"); err != nil {
				problems = append(problems, fmt.Sprintf("osd.%d not wiped: %v", id, err))
			}
		}
	}
	aside := m.lib("p8s-removed-" + time.Now().UTC().Format("20060102-150405"))
	if err := os.MkdirAll(aside, 0o700); err != nil {
		return err
	}
	for _, p := range []string{m.ConfFile(), m.AdminKeyring(), m.lib("mon"), m.lib("mgr"), m.lib("osd"), m.lib("bootstrap-osd")} {
		if !m.exists(p) {
			continue
		}
		dst := filepath.Join(aside, strings.ReplaceAll(strings.TrimPrefix(p, "/"), "/", "_"))
		if err := moveAside(p, dst); err != nil && !errors.Is(err, os.ErrNotExist) {
			problems = append(problems, fmt.Sprintf("move %s aside: %v", p, err))
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	m.mu.Lock()
	m.task.Detail = "Ceph removed from this server; its old files are in " + aside
	m.mu.Unlock()
	return nil
}

// moveAside renames p to dst; a file on another file system (/etc) is copied, then removed.
func moveAside(p, dst string) error {
	err := os.Rename(p, dst)
	if err == nil {
		return nil
	}
	st, serr := os.Stat(p)
	if serr != nil || st.IsDir() {
		return err
	}
	b, rerr := os.ReadFile(p)
	if rerr != nil {
		return rerr
	}
	if werr := os.WriteFile(dst, b, 0o600); werr != nil {
		return werr
	}
	return os.Remove(p)
}
