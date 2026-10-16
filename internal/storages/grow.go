package storages

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Grow makes a storage's VM-disk pool bigger while VMs keep running.
//   - NAS share (NFS): the pool file grows to sizeGB (sparse: it only takes
//     what VMs write) and the pool takes up the new space.
//   - SAN disk (iSCSI): after the LUN was made bigger on the SAN, the disk is
//     read again and the pool takes up the new space (sizeGB is ignored).
func (m *Manager) Grow(ctx context.Context, name string, sizeGB int) (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, err := m.load()
	if err != nil {
		return Status{}, err
	}
	i := -1
	for k := range l {
		if l[k].Name == name {
			i = k
		}
	}
	if i < 0 {
		return Status{}, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	s := l[i]
	pool := PoolName(name)
	if _, err := m.Run.Run(ctx, "zpool", "list", "-H", pool); err != nil {
		return Status{}, fmt.Errorf("%w: its VM-disk pool isn't open (Connect it first)", ErrInvalid)
	}
	var dev string
	switch {
	case s.Type == NFS && s.VMDisks:
		if sizeGB <= s.SizeGB {
			return Status{}, fmt.Errorf("%w: it has %d GB now; give a bigger size (pools can't shrink)", ErrInvalid, s.SizeGB)
		}
		if sizeGB > 1<<20 {
			return Status{}, fmt.Errorf("%w: size %d GB", ErrInvalid, sizeGB)
		}
		// room on the share for the whole file
		if out, err := m.Run.Run(ctx, "timeout", "10", "stat", "-f", "-c", "%a %S", m.PoolDir(name)); err == nil {
			if f := strings.Fields(string(out)); len(f) == 2 {
				a, _ := strconv.ParseInt(f[0], 10, 64)
				bs, _ := strconv.ParseInt(f[1], 10, 64)
				if free := a * bs >> 30; free > 0 && free < int64(sizeGB-s.SizeGB) {
					return Status{}, fmt.Errorf("%w: the share has %d GB free; it can grow by at most that (to %d GB)", ErrInvalid, free, int64(s.SizeGB)+free)
				}
			}
		}
		dev = m.PoolFile(name)
		if _, err := m.Run.Run(ctx, "truncate", "-s", fmt.Sprintf("%dG", sizeGB), dev); err != nil {
			return Status{}, fmt.Errorf("grow %s: %s", dev, stderr(err))
		}
	case s.Type == ISCSI:
		dev = m.Device(s)
		_, _ = m.iscsi(ctx, "-m", "session", "--rescan") // the SAN made the LUN bigger: read its size again
		_, _ = m.Run.Run(ctx, "partprobe", dev)
	default:
		return Status{}, fmt.Errorf("%w: %s holds no VM disks", ErrInvalid, name)
	}
	if _, err := m.Run.Run(ctx, "zpool", "online", "-e", pool, dev); err != nil {
		return Status{}, fmt.Errorf("the pool didn't take up the new space: %s", stderr(err))
	}
	if s.Type == NFS {
		l[i].SizeGB = sizeGB
		if err := m.save(l); err != nil {
			return Status{}, err
		}
	}
	return m.status(ctx, l[i]), nil
}
