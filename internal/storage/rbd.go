package storage

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
	"sync"
	"time"

	"p8sos/internal/execx"
)

// VM disks on Ceph: RBD images <vm>-disk0 ... <vm>-disk8 in one pool, used
// as kernel block devices (/dev/rbd/POOL/IMAGE, "krbd"), so the rest of P8S
// sees a disk like a zvol. Every server of the cluster sees the same images:
// a VM moves without copying its disks and starts elsewhere when its server dies.
//
// Safety rule: an image is mapped (opened) on the server where its VM is
// defined, and is only ever deleted when no server has it mapped.

// CephStorage is the storage name of the Ceph pool.
const CephStorage = "ceph"

const rbdMarker = "rbd:"

// ErrInUse means an image is open on another server (it was kept).
var ErrInUse = errors.New("in use on another server")

// RBD is the Ceph pool for VM disks.
type RBD struct {
	Run  execx.Runner
	Pool string // p8s-vms
	// Ready reports whether Ceph and the pool are usable on this server.
	Ready func(ctx context.Context) bool
	// MapsFile lists the images to map at boot (scripts/ceph-map.sh); "" = none.
	MapsFile string
	Exists   func(string) bool // nil = os.Stat
	Wait     time.Duration     // for /dev/rbd/...; 0 = 30 s

	importMu sync.Mutex // template imports
	mapsMu   sync.Mutex // the maps file
	readyMu  sync.Mutex
	readyAt  time.Time
	ready    bool
}

func (r *RBD) exists(p string) bool {
	if r.Exists != nil {
		return r.Exists(p)
	}
	_, err := os.Stat(p)
	return err == nil
}

// usable caches Ready for 30 seconds.
func (r *RBD) usable(ctx context.Context) bool {
	if r == nil {
		return false
	}
	if r.Ready == nil {
		return r.Pool != ""
	}
	r.readyMu.Lock()
	defer r.readyMu.Unlock()
	if time.Since(r.readyAt) > 30*time.Second {
		r.ready, r.readyAt = r.Ready(ctx), time.Now()
	}
	return r.ready
}

func (r *RBD) image(vm string, i int) string { return fmt.Sprintf("%s-disk%d", vm, i) }
func (r *RBD) spec(img string) string        { return r.Pool + "/" + img }

// Dev is the block device of an image.
func (r *RBD) Dev(img string) string { return "/dev/rbd/" + r.Pool + "/" + img }

func (r *RBD) rbd(ctx context.Context, args ...string) ([]byte, error) {
	return r.Run.Run(ctx, "rbd", args...)
}

// Images lists the pool's images.
func (r *RBD) Images(ctx context.Context) ([]string, error) {
	out, err := r.rbd(ctx, "ls", "--pool", r.Pool)
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

func notFound(err error) bool {
	s := errStderr(err)
	return strings.Contains(s, "No such file") || strings.Contains(s, "not found") || strings.Contains(s, "does not exist")
}

// Map opens the image on this server and returns its device.
func (r *RBD) Map(ctx context.Context, img string) (string, error) {
	dev := r.Dev(img)
	if !r.exists(dev) {
		if _, err := r.rbd(ctx, "device", "map", "--options", "ms_mode=secure", r.spec(img)); err != nil &&
			!strings.Contains(errStderr(err), "already mapped") {
			return "", fmt.Errorf("open Ceph disk %s: %w", img, err)
		}
	}
	if err := r.remember(img, true); err != nil {
		return "", err
	}
	limit := r.Wait
	if limit == 0 {
		limit = 30 * time.Second
	}
	for deadline := time.Now().Add(limit); !r.exists(dev); {
		if time.Now().After(deadline) || ctx.Err() != nil {
			return "", fmt.Errorf("Ceph disk device %s did not appear (udev)", dev)
		}
		_, _ = r.Run.Run(ctx, "udevadm", "settle", "--timeout=5")
		time.Sleep(200 * time.Millisecond)
	}
	return dev, nil
}

// Unmap closes the image on this server (not mapped = fine).
func (r *RBD) Unmap(ctx context.Context, img string) error {
	dev := r.Dev(img)
	for attempt := 1; r.exists(dev); attempt++ {
		_, err := r.rbd(ctx, "device", "unmap", dev)
		if err == nil || strings.Contains(errStderr(err), "not mapped") {
			break
		}
		if !strings.Contains(errStderr(err), "busy") || attempt == 15 || ctx.Err() != nil {
			return fmt.Errorf("close Ceph disk %s: %w", img, err)
		}
		_, _ = r.Run.Run(ctx, "udevadm", "settle", "--timeout=5")
		time.Sleep(time.Second)
	}
	return r.remember(img, false)
}

// remember keeps the boot-time map list up to date.
func (r *RBD) remember(img string, on bool) error {
	if r.MapsFile == "" {
		return nil
	}
	r.mapsMu.Lock()
	defer r.mapsMu.Unlock()
	b, err := os.ReadFile(r.MapsFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	want := r.spec(img)
	var lines []string
	found := false
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if l == want {
			found = true
			if !on {
				continue
			}
		}
		lines = append(lines, l)
	}
	if on == found {
		return nil
	}
	if on {
		lines = append(lines, want)
	}
	sort.Strings(lines)
	data := "# Ceph disks this server opens at boot (P8S OS keeps this list).\n" + strings.Join(lines, "\n") + "\n"
	if err := os.MkdirAll(filepath.Dir(r.MapsFile), 0o755); err != nil {
		return err
	}
	tmp := r.MapsFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(data), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, r.MapsFile)
}

// Watchers counts the servers that have the image open.
func (r *RBD) Watchers(ctx context.Context, img string) (int, error) {
	out, err := r.rbd(ctx, "status", "--format", "json", r.spec(img))
	if err != nil {
		return 0, err
	}
	var st struct {
		Watchers []json.RawMessage `json:"watchers"`
	}
	if err := json.Unmarshal(out, &st); err != nil {
		return 0, fmt.Errorf("rbd status: %w", err)
	}
	return len(st.Watchers), nil
}

// Remove closes the image here and deletes it with its snapshots — unless
// another server has it open (ErrInUse; the image is kept). Missing = fine.
func (r *RBD) Remove(ctx context.Context, img string) error {
	if err := r.Unmap(ctx, img); err != nil {
		return err
	}
	n, err := r.Watchers(ctx, img)
	if notFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("Ceph disk %s: %w", img, ErrInUse)
	}
	if _, err := r.rbd(ctx, "snap", "purge", r.spec(img)); err != nil && !notFound(err) {
		return fmt.Errorf("delete snapshots of %s: %w", img, err)
	}
	if _, err := r.rbd(ctx, "rm", "--no-progress", r.spec(img)); err != nil && !notFound(err) {
		return fmt.Errorf("delete Ceph disk %s: %w", img, err)
	}
	return nil
}

// Create makes an empty image of bytes (rounded up to a MiB).
func (r *RBD) Create(ctx context.Context, img string, bytes int64) error {
	mib := (bytes + (1<<20 - 1)) >> 20
	if _, err := r.rbd(ctx, "create", "--size", fmt.Sprintf("%dM", mib), r.spec(img)); err != nil {
		return fmt.Errorf("create Ceph disk %s: %w", img, err)
	}
	return nil
}

// Size is the image's size in bytes.
func (r *RBD) Size(ctx context.Context, img string) (int64, error) {
	out, err := r.rbd(ctx, "info", "--format", "json", r.spec(img))
	if err != nil {
		return 0, err
	}
	var in struct {
		Size int64 `json:"size"`
	}
	if err := json.Unmarshal(out, &in); err != nil {
		return 0, fmt.Errorf("rbd info: %w", err)
	}
	return in.Size, nil
}

// Grow sets the image's size (never smaller).
func (r *RBD) Grow(ctx context.Context, img string, sizeGB int) error {
	cur, err := r.Size(ctx, img)
	if err != nil {
		return err
	}
	if int64(sizeGB)<<30 <= cur {
		return nil
	}
	if _, err := r.rbd(ctx, "resize", "--no-progress", "--size", fmt.Sprintf("%dG", sizeGB), r.spec(img)); err != nil {
		return fmt.Errorf("resize Ceph disk %s: %w", img, err)
	}
	return nil
}

// rbdSnap is one line of `rbd snap ls --format json`.
type rbdSnap struct {
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	Timestamp string `json:"timestamp"`
}

func (r *RBD) snaps(ctx context.Context, img string) ([]rbdSnap, error) {
	out, err := r.rbd(ctx, "snap", "ls", "--format", "json", r.spec(img))
	if err != nil {
		return nil, err
	}
	var l []rbdSnap
	if err := json.Unmarshal(out, &l); err != nil {
		return nil, fmt.Errorf("rbd snap ls: %w", err)
	}
	return l, nil
}

// ParseRBDTime reads rbd's snapshot time ("Mon Oct 14 12:03:04 2026").
func ParseRBDTime(s string) time.Time {
	for _, f := range []string{"Mon Jan _2 15:04:05 2006", time.ANSIC, time.RFC3339} {
		if t, err := time.ParseInLocation(f, strings.TrimSpace(s), time.Local); err == nil {
			return t
		}
	}
	return time.Time{}
}

var reTplName = regexp.MustCompile(`[^a-z0-9.-]+`)

// templateImage is the pool's copy of a template snapshot (guid = the ZFS
// snapshot's id, so a rebuilt template gets a new copy).
func (r *RBD) templateImage(snapshot, guid string) string {
	base := snapshot
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	base = strings.Trim(reTplName.ReplaceAllString(strings.ToLower(base), "-"), "-")
	if len(guid) > 12 {
		guid = guid[:12]
	}
	return "p8s-tpl-" + base + "-" + guid
}

// HasImageSnap reports whether img@snap exists.
func (r *RBD) HasImageSnap(ctx context.Context, img, snap string) bool {
	l, err := r.snaps(ctx, img)
	if err != nil {
		return false
	}
	for _, s := range l {
		if s.Name == snap {
			return true
		}
	}
	return false
}

// --- the ZFS storage, with VMs on Ceph routed here ---

func (z *ZFS) cephReady(ctx context.Context) bool { return z.Ceph != nil && z.Ceph.usable(ctx) }

func (z *ZFS) cephMarker() string { return rbdMarker + z.Ceph.Pool }

// onCeph reports whether the VM's disks are Ceph images.
func (z *ZFS) onCeph(vm string) bool { return strings.HasPrefix(z.parent(vm), rbdMarker) }

// Shared reports whether the VM's disks are on storage every server sees (Ceph).
func (z *ZFS) Shared(vm string) bool { return z.Ceph != nil && z.onCeph(vm) }

func (z *ZFS) cephImages(ctx context.Context, vm string) []string {
	l := []string{z.Ceph.image(vm, 0)}
	for _, i := range z.Extras(ctx, vm) {
		l = append(l, z.Ceph.image(vm, i))
	}
	return l
}

// Release closes the VM's Ceph disks on this server (they stay in Ceph); for
// local disks it does nothing.
func (z *ZFS) Release(ctx context.Context, vm string) error {
	if !z.Shared(vm) {
		return nil
	}
	for _, img := range z.cephImages(ctx, vm) {
		if err := z.Ceph.Unmap(ctx, img); err != nil {
			return err
		}
	}
	return nil
}

func (z *ZFS) cephExtras(ctx context.Context, vm string) []int {
	imgs, err := z.Ceph.Images(ctx)
	if err != nil {
		return nil
	}
	return parseExtras(strings.Join(imgs, "\n"), vm+"-disk")
}

// cephTemplate makes sure the pool has a copy of a template snapshot (made
// once: the zvol is copied in, then snapshotted and protected for clones).
func (z *ZFS) cephTemplate(ctx context.Context, snapshot string) (string, error) {
	r := z.Ceph
	out, err := z.Run.Run(ctx, "zfs", "get", "-H", "-o", "value", "guid", snapshot)
	if err != nil {
		return "", fmt.Errorf("template %s: %w", snapshot, err)
	}
	img := r.templateImage(snapshot, strings.TrimSpace(string(out)))
	if r.HasImageSnap(ctx, img, "base") {
		return img, nil
	}
	r.importMu.Lock() // one import at a time on this server
	defer r.importMu.Unlock()
	if r.HasImageSnap(ctx, img, "base") {
		return img, nil
	}
	tmp := z.Dataset + "/p8s-ceph-import"
	_ = z.destroy(ctx, tmp)
	if _, err := z.Run.Run(ctx, "zfs", "clone", "-o", "volmode=dev", snapshot, tmp); err != nil {
		return "", fmt.Errorf("template copy: %w", err)
	}
	defer func() { _ = z.destroy(context.WithoutCancel(ctx), tmp) }()
	dev, err := z.waitPath(ctx, "/dev/zvol/"+tmp)
	if err != nil {
		return "", err
	}
	// another server may be importing the same template: leave a half copy alone
	if _, err := r.rbd(ctx, "info", r.spec(img)); err == nil {
		_ = r.Remove(ctx, img)
	}
	if _, err := r.rbd(ctx, "import", "--no-progress", "--image-format", "2", dev, r.spec(img)); err != nil {
		return "", fmt.Errorf("copy template to Ceph: %w", err)
	}
	for _, a := range [][]string{{"snap", "create", r.spec(img) + "@base"}, {"snap", "protect", r.spec(img) + "@base"}} {
		if _, err := r.rbd(ctx, a...); err != nil {
			return "", fmt.Errorf("template on Ceph: %w", err)
		}
	}
	return img, nil
}

func (z *ZFS) cephClone(ctx context.Context, snapshot, vm string, i, sizeGB int) (string, error) {
	r := z.Ceph
	tpl, err := z.cephTemplate(ctx, snapshot)
	if err != nil {
		return "", err
	}
	img := r.image(vm, i)
	if _, err := r.rbd(ctx, "clone", r.spec(tpl)+"@base", r.spec(img)); err != nil {
		if strings.Contains(errStderr(err), "exists") {
			return "", fmt.Errorf("a Ceph disk %s already exists (another VM of that name?)", img)
		}
		return "", fmt.Errorf("clone disk: %w", err)
	}
	if err := r.Grow(ctx, img, sizeGB); err != nil {
		_ = r.Remove(context.WithoutCancel(ctx), img)
		return "", err
	}
	dev, err := r.Map(ctx, img)
	if err != nil {
		_ = r.Remove(context.WithoutCancel(ctx), img)
		return "", err
	}
	return dev, nil
}

func (z *ZFS) cephCreate(ctx context.Context, vm string, i int, bytes int64) (string, error) {
	r := z.Ceph
	img := r.image(vm, i)
	if err := r.Create(ctx, img, bytes); err != nil {
		if strings.Contains(errStderr(err), "exists") {
			return "", fmt.Errorf("a Ceph disk %s already exists (another VM of that name?)", img)
		}
		return "", err
	}
	dev, err := r.Map(ctx, img)
	if err != nil {
		_ = r.Remove(context.WithoutCancel(ctx), img)
		return "", err
	}
	return dev, nil
}

// cephDestroy closes and deletes the VM's images; images another server has
// open are kept (the VM lives there now).
func (z *ZFS) cephDestroy(ctx context.Context, vm string) error {
	var kept []string
	imgs := z.cephImages(ctx, vm)
	for i := len(imgs) - 1; i >= 0; i-- { // data disks first, root last
		if err := z.Ceph.Remove(ctx, imgs[i]); errors.Is(err, ErrInUse) {
			kept = append(kept, imgs[i])
		} else if err != nil {
			return err
		}
	}
	z.forget(vm)
	if len(kept) > 0 && len(kept) < len(imgs) {
		return fmt.Errorf("some Ceph disks are open on another server and were kept: %s", strings.Join(kept, ", "))
	}
	return nil
}

func (z *ZFS) cephSnapshots(ctx context.Context, vm string) ([]Snapshot, error) {
	l, err := z.Ceph.snaps(ctx, z.Ceph.image(vm, 0))
	if err != nil {
		return nil, fmt.Errorf("list snapshots: %w", err)
	}
	out := []Snapshot{}
	for _, s := range l {
		if hidden(s.Name) {
			continue
		}
		at := ParseRBDTime(s.Timestamp)
		sn := Snapshot{Name: s.Name}
		if !at.IsZero() {
			sn.CreatedAt = at.UTC().Format(time.RFC3339)
		}
		out = append(out, sn)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out, nil
}

func (z *ZFS) cephRollback(ctx context.Context, vm, name string, destroyNewer bool) error {
	r := z.Ceph
	imgs := z.cephImages(ctx, vm)
	// the kept backup snapshot is newer: the next backup is then a full one
	for _, img := range imgs {
		l, _ := r.snaps(ctx, img)
		for _, s := range l {
			if strings.HasPrefix(s.Name, "p8s-backup-") {
				_, _ = r.rbd(ctx, "snap", "rm", "--no-progress", r.spec(img)+"@"+s.Name)
			}
		}
	}
	if destroyNewer { // same rule as on ZFS: later snapshots go
		all, err := z.cephSnapshots(ctx, vm)
		if err != nil {
			return err
		}
		at := ""
		for _, s := range all {
			if s.Name == name {
				at = s.CreatedAt
			}
		}
		for _, s := range all {
			if at != "" && s.CreatedAt > at {
				if err := z.cephDestroySnap(ctx, vm, s.Name); err != nil {
					return err
				}
			}
		}
	}
	// a disk is rolled back closed: QEMU is off, and the kernel must not cache old blocks
	for _, img := range imgs {
		if err := r.Unmap(ctx, img); err != nil {
			return err
		}
	}
	var rerr error
	for _, img := range imgs {
		if !r.HasImageSnap(ctx, img, name) {
			continue // a data disk added after the snapshot keeps its data
		}
		if _, err := r.rbd(ctx, "snap", "rollback", "--no-progress", r.spec(img)+"@"+name); err != nil {
			rerr = fmt.Errorf("rollback %s: %w", img, err)
			break
		}
	}
	for _, img := range imgs {
		if _, err := r.Map(ctx, img); err != nil && rerr == nil {
			rerr = err
		}
	}
	return rerr
}

func (z *ZFS) cephDestroySnap(ctx context.Context, vm, name string) error {
	for _, img := range z.cephImages(ctx, vm) {
		if _, err := z.Ceph.rbd(ctx, "snap", "rm", "--no-progress", z.Ceph.spec(img)+"@"+name); err != nil && !notFound(err) {
			return fmt.Errorf("delete snapshot of %s: %w", img, err)
		}
	}
	return nil
}

func (z *ZFS) cephSizeGB(ctx context.Context, img string) (int, error) {
	b, err := z.Ceph.Size(ctx, img)
	return int(b >> 30), err
}
