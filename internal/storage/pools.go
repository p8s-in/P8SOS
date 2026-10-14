package storage

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// VM disks can also live on other ZFS pools (network storage: a pool on an
// iSCSI disk). Pools names them; a VM's disks are all on one pool, found by
// where its root disk <vm>-disk0 is.

// LocalStorage is the name of the server's own pool.
const LocalStorage = "local"

type placement struct {
	mu    sync.Mutex
	where map[string]string // vm -> parent dataset
}

func (z *ZFS) pools() map[string]string {
	if z.Pools == nil {
		return nil
	}
	return z.Pools()
}

// parent is the dataset that holds the VM's disks.
func (z *ZFS) parent(vm string) string {
	pools := z.pools()
	if len(pools) == 0 && z.Ceph == nil {
		return z.Dataset
	}
	z.place.mu.Lock()
	defer z.place.mu.Unlock()
	if z.place.where == nil {
		z.place.where = map[string]string{}
	}
	if p, ok := z.place.where[vm]; ok {
		// "local" without a local disk may be a VM that came on Ceph since: look again
		if p != z.Dataset || z.Ceph == nil || z.exists("/dev/zvol/"+z.Dataset+"/"+vm+"-disk0") {
			return p
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// look on every other pool once and remember what is where
	names := make([]string, 0, len(pools))
	for _, ds := range pools {
		names = append(names, ds)
	}
	sort.Strings(names)
	for _, ds := range names {
		out, err := z.Run.Run(ctx, "zfs", "list", "-H", "-o", "name", "-t", "volume", "-d", "1", ds)
		if err != nil {
			continue
		}
		for _, n := range strings.Fields(string(out)) {
			if v, ok := strings.CutSuffix(strings.TrimPrefix(n, ds+"/"), "-disk0"); ok && !strings.Contains(v, "/") {
				z.place.where[v] = ds
			}
		}
	}
	if z.cephReady(ctx) {
		if imgs, err := z.Ceph.Images(ctx); err == nil {
			for _, n := range imgs {
				if v, ok := strings.CutSuffix(n, "-disk0"); ok {
					z.place.where[v] = z.cephMarker()
				}
			}
		}
	}
	if _, ok := z.place.where[vm]; !ok {
		z.place.where[vm] = z.Dataset
	}
	return z.place.where[vm]
}

// Place says which storage a new VM's disks go to ("" or "local" = this server's pool).
func (z *ZFS) Place(vm, storage string) error {
	ds := z.Dataset
	if storage == CephStorage && z.Ceph != nil {
		if !z.cephReady(context.Background()) {
			return fmt.Errorf("Ceph storage isn't ready on this server (Server → Ceph)")
		}
		ds = z.cephMarker()
	} else if storage != "" && storage != LocalStorage {
		var ok bool
		if ds, ok = z.pools()[storage]; !ok {
			return fmt.Errorf("no storage %q for VM disks on this server", storage)
		}
	}
	z.place.mu.Lock()
	defer z.place.mu.Unlock()
	if z.place.where == nil {
		z.place.where = map[string]string{}
	}
	z.place.where[vm] = ds
	return nil
}

// StorageOf names the storage a VM's disks are on.
func (z *ZFS) StorageOf(vm string) string {
	p := z.parent(vm)
	if strings.HasPrefix(p, rbdMarker) {
		return CephStorage
	}
	for name, ds := range z.pools() {
		if ds == p {
			return name
		}
	}
	return LocalStorage
}

// Storages lists the storages VM disks can be put on.
func (z *ZFS) Storages() []string {
	l := []string{LocalStorage}
	for name := range z.pools() {
		l = append(l, name)
	}
	if z.cephReady(context.Background()) {
		l = append(l, CephStorage)
	}
	sort.Strings(l[1:])
	return l
}

func (z *ZFS) forget(vm string) {
	z.place.mu.Lock()
	delete(z.place.where, vm)
	z.place.mu.Unlock()
}

func poolOf(ds string) string { return strings.SplitN(ds, "/", 2)[0] }

// copyTemplate copies a template snapshot to another pool (a clone can't
// cross pools): zfs send | zfs receive. The copy's snapshot is dropped.
func (z *ZFS) copyTemplate(ctx context.Context, snapshot, ds string) error {
	send := StreamCmd(ctx, "zfs", "send", snapshot)
	recv := StreamCmd(ctx, "zfs", "receive", "-o", "volmode=dev", ds)
	var es, er strings.Builder
	send.Stderr, recv.Stderr = &es, &er
	pr, pw := io.Pipe()
	send.Stdout, recv.Stdin = pw, pr
	if err := recv.Start(); err != nil {
		return err
	}
	errS := send.Run()
	pw.CloseWithError(errS)
	errR := recv.Wait()
	if errS != nil {
		return fmt.Errorf("zfs send: %v %s", errS, strings.TrimSpace(es.String()))
	}
	if errR != nil {
		return fmt.Errorf("zfs receive: %v %s", errR, strings.TrimSpace(er.String()))
	}
	if _, snap, ok := strings.Cut(snapshot, "@"); ok {
		_, _ = z.Run.Run(ctx, "zfs", "destroy", ds+"@"+snap)
	}
	return nil
}

// Parent is the dataset that holds the VM's disks (for backups).
func (z *ZFS) Parent(vm string) string { return z.parent(vm) }
