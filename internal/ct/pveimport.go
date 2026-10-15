package ct

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"p8sos/internal/imports"
	"p8sos/internal/jobs"
	"p8sos/internal/vm"
)

// Importing Proxmox containers (vzdump-lxc backups): the archive's files are
// unpacked as the container's root (ids shifted into the unprivileged range,
// like templates), then it gets P8S's own LXC config.

// ImportRequest: the plan's fields, changed where the person wants.
type ImportRequest struct {
	File      string `json:"file"`
	Name      string `json:"name"`
	VCPUs     int    `json:"vcpus,omitempty"`
	MemoryMiB int    `json:"memory_mib,omitempty"`
	DiskGB    int    `json:"disk_gb,omitempty"`
	Network   string `json:"network,omitempty"`
	KeepMAC   bool   `json:"keep_mac,omitempty"`
	Owner     string `json:"owner,omitempty"`
	Start     *bool  `json:"start,omitempty"`
	Autostart *bool  `json:"autostart,omitempty"`
}

// unpackCmd builds the tar command that unpacks an archive (stdin) into dir
// as the container's root.
func unpackCmd(dir string) []string {
	return []string{"lxc-usernsexec", "-m", fmt.Sprintf("b:0:%d:65536", MapBase), "--",
		"tar", "--numeric-owner", "-xpf", "-", "-C", dir,
		"--xattrs", "--xattrs-include=user.*", "--xattrs-include=security.capability", "--warning=no-xattr-write",
		"--exclude=./dev/*", "--exclude=./etc/vzdump"}
}

// StartImport checks the request and imports the backup at path as a job.
func (s *Service) StartImport(ctx context.Context, path string, pl imports.Plan, r ImportRequest) (jobs.Job, error) {
	if s.Jobs == nil {
		return jobs.Job{}, fmt.Errorf("%w: background jobs aren't set up for containers", vm.ErrConflict)
	}
	if pl.Kind != imports.KindLXC {
		return jobs.Job{}, fmt.Errorf("%w: %s isn't a Proxmox container backup", vm.ErrInvalid, pl.File)
	}
	if r.Name == "" {
		r.Name = pl.Name
	}
	if r.VCPUs == 0 {
		r.VCPUs = pl.VCPUs
	}
	if r.MemoryMiB == 0 {
		r.MemoryMiB = pl.MemoryMiB
	}
	r.DiskGB = max(r.DiskGB, pl.DiskGB)
	cr := CreateRequest{Name: r.Name, VCPUs: r.VCPUs, MemoryMiB: r.MemoryMiB, DiskGB: r.DiskGB}
	if err := s.validate(&cr); err != nil {
		return jobs.Job{}, err
	}
	if r.Network == "" {
		r.Network = vm.NetNAT
	}
	if r.Network == vm.NetPublic {
		return jobs.Job{}, fmt.Errorf("%w: import it on the private network first; public addresses for imported containers aren't supported yet", vm.ErrInvalid)
	}
	if _, err := s.bridgeFor(r.Name, r.Network); err != nil {
		return jobs.Job{}, err
	}
	if s.Exists(r.Name) {
		return jobs.Job{}, fmt.Errorf("%w: container %q", vm.ErrExists, r.Name)
	}
	mac := newMAC()
	if r.KeepMAC && pl.MAC != "" {
		mac = pl.MAC
	}
	j, err := s.Jobs.Start("import", r.Name, func(ctx context.Context, p jobs.Progress) (string, error) {
		return s.runImport(ctx, path, pl, r, mac, p)
	})
	if errors.Is(err, jobs.ErrBusy) {
		return jobs.Job{}, fmt.Errorf("%w: %v", vm.ErrConflict, err)
	}
	return j, err
}

type countReader struct {
	r    io.Reader
	n    int64
	tick func(int64)
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	c.tick(c.n)
	return n, err
}

func (s *Service) runImport(ctx context.Context, path string, pl imports.Plan, r ImportRequest, mac string, p jobs.Progress) (string, error) {
	name := r.Name
	if s.Exists(name) {
		return "", fmt.Errorf("container %q was created meanwhile", name)
	}
	ds := s.dataset(name)
	root := s.rootfs(name)
	bg := context.WithoutCancel(ctx)
	_, _ = s.zfs(ctx, "create", "-p", "-o", "canmount=off", s.Pool+"/p8s/ct")
	p(fmt.Sprintf("creating a %d GB disk", r.DiskGB))
	if _, err := s.zfs(ctx, "create", "-o", "mountpoint="+root, "-o", fmt.Sprintf("refquota=%dG", r.DiskGB), ds); err != nil {
		return "", fmt.Errorf("create the container's disk: %w", err)
	}
	undo := func() {
		_, _ = s.zfs(bg, "destroy", "-r", ds)
		_ = os.RemoveAll(s.confDir(name))
	}
	_ = os.MkdirAll(root, 0o755)
	if err := s.chown(root, MapBase, MapBase); err != nil {
		undo()
		return "", err
	}
	_ = os.Chmod(root, 0o755)

	in, err := imports.OpenStream(ctx, path)
	if err != nil {
		undo()
		return "", err
	}
	defer in.Close()
	last := time.Time{}
	cr := &countReader{r: in, tick: func(n int64) {
		if time.Since(last) > 2*time.Second {
			last = time.Now()
			p(fmt.Sprintf("unpacking the container's files: %.1f GB", float64(n)/(1<<30)))
		}
	}}
	args := unpackCmd(root)
	c := s.cmd(ctx, args[0], args[1:]...)
	var stderr strings.Builder
	c.Stdin, c.Stderr = cr, &stderr
	p("unpacking the container's files")
	if err := c.Run(); err != nil {
		// tar exits 1 when some files differed while read (fine), 2 on real errors
		if c.ProcessState == nil || c.ProcessState.ExitCode() != 1 {
			undo()
			msg := strings.TrimSpace(stderr.String())
			if len(msg) > 600 {
				msg = msg[len(msg)-600:]
			}
			if strings.Contains(msg, "Disk quota exceeded") || strings.Contains(msg, "No space left") {
				return "", fmt.Errorf("the files don't fit in %d GB: import again with a bigger disk", r.DiskGB)
			}
			return "", fmt.Errorf("unpacking failed: %v %s", err, msg)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "etc")); err != nil {
		undo()
		return "", errors.New("the backup has no /etc: it isn't a container's files")
	}
	// hostname = the new name; a fixed address becomes DHCP on our network
	if err := s.prepare(name, nil, ""); err != nil {
		undo()
		return "", fmt.Errorf("prepare the container: %w", err)
	}
	if pl.StaticIP != "" {
		if err := s.writeNet(name, mac, nil); err != nil {
			undo()
			return "", fmt.Errorf("network setup: %w", err)
		}
	}
	m := &meta{Template: "import:" + pl.File, VCPUs: r.VCPUs, MemoryMiB: r.MemoryMiB, DiskGB: r.DiskGB, MAC: mac,
		Network: r.Network, Owner: r.Owner, Autostart: r.Autostart == nil || *r.Autostart, Created: s.now().UTC().Format(time.RFC3339)}
	br, err := s.bridgeFor(name, r.Network)
	if err != nil {
		undo()
		return "", err
	}
	if err := s.writeConfig(name, m, br); err != nil {
		undo()
		return "", err
	}
	if err := s.put(name, m); err != nil {
		undo()
		return "", err
	}
	if s.Refresh != nil {
		_ = s.Refresh(ctx)
	}
	msg := fmt.Sprintf("container %s imported from %s", name, pl.File)
	if r.Start == nil || *r.Start {
		p("starting")
		if err := s.start(ctx, name); err != nil {
			return "", fmt.Errorf("%s, but it didn't start: %w", msg, err)
		}
		msg += " and started"
	}
	return msg, nil
}
