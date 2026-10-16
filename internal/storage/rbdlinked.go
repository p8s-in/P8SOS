package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Linked clones on Ceph: the new VM's images are rbd clones of the source's
// images at a protected snapshot (p8s-link-…). Like on ZFS they are instant
// and take no space until they differ; the snapshot stays protected while a
// clone uses it, so the source can't be deleted meanwhile. Unlink flattens
// the clone (copies the shared blocks into it).

func (z *ZFS) cephLink(ctx context.Context, vm, snap, newVM string, idx []int) error {
	r := z.Ceph
	var made, protected []string
	undo := func() {
		bg := context.WithoutCancel(ctx)
		for _, img := range made {
			_ = r.Remove(bg, img)
		}
		for _, s := range protected {
			_, _ = r.rbd(bg, "snap", "unprotect", s)
		}
	}
	for _, i := range idx {
		src, dst := r.spec(r.image(vm, i))+"@"+snap, r.image(newVM, i)
		if _, err := r.rbd(ctx, "snap", "protect", src); err != nil && !strings.Contains(errStderr(err), "already protected") {
			undo()
			return fmt.Errorf("link disk %d: %w", i, err)
		}
		protected = append(protected, src)
		if _, err := r.rbd(ctx, "clone", src, r.spec(dst)); err != nil {
			undo()
			if strings.Contains(errStderr(err), "exists") {
				return fmt.Errorf("a Ceph disk %s already exists (another VM of that name?)", dst)
			}
			return fmt.Errorf("link disk %d: %w", i, err)
		}
		made = append(made, dst)
		if _, err := r.Map(ctx, dst); err != nil {
			undo()
			return err
		}
	}
	return nil
}

// rbdParent is an image's parent (rbd info --format json).
type rbdParent struct {
	Pool     string `json:"pool"`
	Image    string `json:"image"`
	Snapshot string `json:"snapshot"`
}

func (r *RBD) parent(ctx context.Context, img string) (rbdParent, bool) {
	out, err := r.rbd(ctx, "info", "--format", "json", r.spec(img))
	if err != nil {
		return rbdParent{}, false
	}
	var info struct {
		Parent *rbdParent `json:"parent"`
	}
	if json.Unmarshal(out, &info) != nil || info.Parent == nil || info.Parent.Image == "" {
		return rbdParent{}, false
	}
	return *info.Parent, true
}

// children lists the images cloned from img@snap.
func (r *RBD) children(ctx context.Context, img, snap string) []string {
	out, err := r.rbd(ctx, "children", "--format", "json", r.spec(img)+"@"+snap)
	if err != nil {
		return nil
	}
	var objs []struct {
		Image string `json:"image"`
	}
	if json.Unmarshal(out, &objs) == nil && (len(objs) == 0 || objs[0].Image != "") {
		var l []string
		for _, o := range objs {
			l = append(l, o.Image)
		}
		return l
	}
	var names []string // older Ceph: ["pool/image", …]
	_ = json.Unmarshal(out, &names)
	for i, n := range names {
		names[i] = n[strings.LastIndex(n, "/")+1:]
	}
	return names
}

// vmOfImage turns "p8s-web2-disk0" into "p8s-web2".
func vmOfImage(img string) string {
	if i := strings.LastIndex(img, "-disk"); i > 0 {
		return img[:i]
	}
	return img
}

func (z *ZFS) cephLinkedClones(ctx context.Context, vm string) []string {
	seen := map[string]bool{}
	for _, img := range z.cephImages(ctx, vm) {
		l, _ := z.Ceph.snaps(ctx, img)
		for _, s := range l {
			if !strings.HasPrefix(s.Name, LinkSnapPrefix) {
				continue
			}
			for _, c := range z.Ceph.children(ctx, img, s.Name) {
				if n := vmOfImage(c); n != vm {
					seen[n] = true
				}
			}
		}
	}
	var out []string
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (z *ZFS) cephLinkedTo(ctx context.Context, vm string) string {
	p, ok := z.Ceph.parent(ctx, z.Ceph.image(vm, 0))
	if !ok || !strings.HasPrefix(p.Snapshot, LinkSnapPrefix) {
		return ""
	}
	return vmOfImage(p.Image)
}

// cephOrigins are the link snapshots (pool/image@snap) the VM's images were cloned from.
func (z *ZFS) cephOrigins(ctx context.Context, imgs []string) []string {
	var l []string
	for _, img := range imgs {
		if p, ok := z.Ceph.parent(ctx, img); ok && strings.HasPrefix(p.Snapshot, LinkSnapPrefix) {
			pool := p.Pool
			if pool == "" {
				pool = z.Ceph.Pool
			}
			l = append(l, pool+"/"+p.Image+"@"+p.Snapshot)
		}
	}
	return l
}

// cephDropOrigins removes link snapshots no clone uses any more.
func (z *ZFS) cephDropOrigins(ctx context.Context, origins []string) {
	for _, o := range origins {
		if _, err := z.Ceph.rbd(ctx, "snap", "unprotect", o); err != nil {
			continue // another clone still uses it
		}
		_, _ = z.Ceph.rbd(ctx, "snap", "rm", "--no-progress", o)
	}
}

func (z *ZFS) cephUnlink(ctx context.Context, vm string, progress func(string)) error {
	imgs := z.cephImages(ctx, vm)
	origins := z.cephOrigins(ctx, imgs)
	if len(origins) == 0 {
		return fmt.Errorf("%s isn't a linked clone", vm)
	}
	for _, img := range imgs {
		if p, ok := z.Ceph.parent(ctx, img); !ok || !strings.HasPrefix(p.Snapshot, LinkSnapPrefix) {
			continue
		}
		if progress != nil {
			progress("copying " + img)
		}
		if _, err := z.Ceph.rbd(ctx, "flatten", "--no-progress", z.Ceph.spec(img)); err != nil {
			return fmt.Errorf("copy %s: %w", img, err)
		}
	}
	z.cephDropOrigins(ctx, origins)
	return nil
}
