// Package installer is the P8S OS installer: it puts the system on disks the
// person picks, with the root file system on ZFS (one disk, a mirror, striped
// mirrors or RAID-Z) or on ext4 with the VM pool beside it. The installer ISO
// (iso/build-installer.sh) runs cmd/p8s-install, which uses this package.
//
// Everything here is plain data and command lists, so the whole plan can be
// checked in unit tests and printed with p8s-install --dry-run before a single
// disk is touched.
package installer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// LiveMount is where the live system (the installer USB/CD) is mounted.
const LiveMount = "/run/live/medium"

// MinDiskBytes is the smallest disk the installer offers.
const MinDiskBytes = 16 << 30

// Disk is one physical disk.
type Disk struct {
	Name      string   `json:"name"`            // kernel name: sda, nvme0n1
	Path      string   `json:"path"`            // /dev/sda
	ByID      string   `json:"by_id,omitempty"` // /dev/disk/by-id/ata-..., stable across reboots
	Size      int64    `json:"size"`            // bytes
	Model     string   `json:"model,omitempty"`
	Serial    string   `json:"serial,omitempty"`
	Tran      string   `json:"tran,omitempty"` // sata, nvme, usb, ...
	Rota      bool     `json:"rota"`           // spinning disk
	Removable bool     `json:"removable"`
	Parts     int      `json:"parts"`         // partitions on it now
	Contents  []string `json:"contents"`      // file systems / labels found: "ntfs Windows", "zfs_member tank"
	Mounted   []string `json:"mounted"`       // mount points in use right now
	Live      bool     `json:"live"`          // holds the installer itself
	Why       string   `json:"why,omitempty"` // why it can't be used ("" = usable)
}

// Empty reports whether the disk has no partitions and no file system.
func (d Disk) Empty() bool { return d.Parts == 0 && len(d.Contents) == 0 }

// Usable reports whether the installer may offer the disk.
func (d Disk) Usable() bool { return d.Why == "" }

// Dev is the path pools and boot loaders should use: by-id when the disk has
// one (it names the disk's model and serial, and survives renumbering).
func (d Disk) Dev() string {
	if d.ByID != "" {
		return d.ByID
	}
	return d.Path
}

// Part is partition n of the disk: by-id paths get "-partN", kernel paths
// get "N" ("pN" when the name ends in a digit, like nvme0n1p2).
func (d Disk) Part(n int) string {
	if d.ByID != "" {
		return fmt.Sprintf("%s-part%d", d.ByID, n)
	}
	if last := d.Path[len(d.Path)-1]; last >= '0' && last <= '9' {
		return fmt.Sprintf("%sp%d", d.Path, n)
	}
	return fmt.Sprintf("%s%d", d.Path, n)
}

// Label is a one-line description for menus: "sdb  476.9 GB  Samsung SSD 860 (S3Z9...)  SSD".
func (d Disk) Label() string {
	kind := "SSD"
	switch {
	case d.Tran == "nvme":
		kind = "NVMe"
	case d.Rota:
		kind = "HDD"
	}
	if d.Tran == "usb" || d.Removable {
		kind += ", USB"
	}
	s := fmt.Sprintf("%-8s %9s  %s", d.Name, HumanBytes(d.Size), strings.TrimSpace(d.Model))
	if d.Serial != "" {
		s += " (" + d.Serial + ")"
	}
	return s + "  " + kind
}

// State describes what is on the disk now, for the warning next to it.
func (d Disk) State() string {
	switch {
	case d.Why != "":
		return d.Why
	case d.Empty():
		return "empty"
	}
	var parts []string
	if d.Parts > 0 {
		parts = append(parts, fmt.Sprintf("%d partition(s)", d.Parts))
	}
	parts = append(parts, d.Contents...)
	return "HAS DATA: " + strings.Join(parts, ", ")
}

// HumanBytes prints sizes the way disk makers do (1 GB = 10^9 bytes), so the
// numbers match the label on the disk.
func HumanBytes(n int64) string {
	f := float64(n)
	for _, u := range []string{"B", "KB", "MB", "GB", "TB", "PB"} {
		if f < 1000 || u == "PB" {
			if u == "B" {
				return fmt.Sprintf("%d B", n)
			}
			return fmt.Sprintf("%.1f %s", f, u)
		}
		f /= 1000
	}
	return ""
}

// lsblk -J -b -o NAME,PATH,TYPE,SIZE,MODEL,SERIAL,TRAN,ROTA,RM,FSTYPE,LABEL,MOUNTPOINTS
type lsblkDev struct {
	Name        string     `json:"name"`
	Path        string     `json:"path"`
	Type        string     `json:"type"`
	Size        jsonInt    `json:"size"`
	Model       *string    `json:"model"`
	Serial      *string    `json:"serial"`
	Tran        *string    `json:"tran"`
	Rota        jsonBool   `json:"rota"`
	RM          jsonBool   `json:"rm"`
	FSType      *string    `json:"fstype"`
	Label       *string    `json:"label"`
	Mountpoints []*string  `json:"mountpoints"`
	Mountpoint  *string    `json:"mountpoint"` // older lsblk
	Children    []lsblkDev `json:"children"`
}

// LsblkArgs are the arguments ParseLsblk expects the output of.
var LsblkArgs = []string{"-J", "-b", "-o", "NAME,PATH,TYPE,SIZE,MODEL,SERIAL,TRAN,ROTA,RM,FSTYPE,LABEL,MOUNTPOINTS"}

// jsonInt accepts 123 and "123" (lsblk versions differ).
type jsonInt int64

func (j *jsonInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "null" || s == "" {
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	*j = jsonInt(n)
	return err
}

// jsonBool accepts true, "1" and 1.
type jsonBool bool

func (j *jsonBool) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	*j = s == "true" || s == "1"
	return nil
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}

func (d lsblkDev) mounts() []string {
	var m []string
	for _, p := range d.Mountpoints {
		if s := str(p); s != "" {
			m = append(m, s)
		}
	}
	if s := str(d.Mountpoint); s != "" && len(m) == 0 {
		m = append(m, s)
	}
	return m
}

// ParseLsblk turns lsblk's JSON into the list of disks, with the ones the
// installer must not touch marked (Why). byID maps /dev/sdX to its by-id link.
func ParseLsblk(out []byte, byID map[string]string) ([]Disk, error) {
	var doc struct {
		Blockdevices []lsblkDev `json:"blockdevices"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("reading lsblk: %w", err)
	}
	var disks []Disk
	for _, b := range doc.Blockdevices {
		if b.Type != "disk" || strings.HasPrefix(b.Name, "zram") || strings.HasPrefix(b.Name, "loop") {
			continue
		}
		path := b.Path
		if path == "" {
			path = "/dev/" + b.Name
		}
		d := Disk{Name: b.Name, Path: path, ByID: byID[path], Size: int64(b.Size), Model: str(b.Model),
			Serial: str(b.Serial), Tran: str(b.Tran), Rota: bool(b.Rota), Removable: bool(b.RM),
			Contents: []string{}, Mounted: []string{}}
		var walk func(n lsblkDev, top bool)
		walk = func(n lsblkDev, top bool) {
			if !top && n.Type == "part" {
				d.Parts++
			}
			if fs := str(n.FSType); fs != "" {
				c := fs
				if l := str(n.Label); l != "" {
					c += " " + l
				}
				d.Contents = append(d.Contents, c)
			}
			for _, m := range n.mounts() {
				d.Mounted = append(d.Mounted, m)
				if m == LiveMount || strings.HasPrefix(m, LiveMount+"/") {
					d.Live = true
				}
			}
			for _, c := range n.Children {
				walk(c, false)
			}
		}
		walk(b, true)
		d.Contents = dedupe(d.Contents)
		switch {
		case d.Live:
			d.Why = "the installer is running from this disk"
		case len(d.Mounted) > 0:
			d.Why = "in use (mounted at " + strings.Join(d.Mounted, ", ") + ")"
		case d.Size < MinDiskBytes:
			d.Why = "too small (needs 16 GB)"
		}
		disks = append(disks, d)
	}
	sort.SliceStable(disks, func(i, j int) bool { return disks[i].Name < disks[j].Name })
	return disks, nil
}

func dedupe(s []string) []string {
	seen := map[string]bool{}
	out := s[:0]
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// ByIDLinks reads /dev/disk/by-id (dir) and returns the best stable name for
// each whole disk: model+serial names (ata-, nvme-, scsi-, virtio-, usb-) win
// over wwn- and eui- numbers.
func ByIDLinks(dir string) map[string]string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return map[string]string{}
	}
	best := map[string]string{}
	rank := map[string]int{}
	for _, e := range ents {
		n := e.Name()
		if strings.Contains(n, "-part") {
			continue
		}
		target, err := filepath.EvalSymlinks(filepath.Join(dir, n))
		if err != nil {
			continue
		}
		r := idRank(n)
		if r < 0 {
			continue
		}
		if cur, ok := rank[target]; !ok || r < cur || (r == cur && n < filepath.Base(best[target])) {
			rank[target] = r
			best[target] = filepath.Join(dir, n)
		}
	}
	return best
}

func idRank(n string) int {
	switch {
	case strings.HasPrefix(n, "nvme-eui.") || strings.HasPrefix(n, "nvme-nvme."):
		return 5
	case strings.HasPrefix(n, "ata-"), strings.HasPrefix(n, "nvme-"):
		return 0
	case strings.HasPrefix(n, "scsi-0"), strings.HasPrefix(n, "scsi-1"), strings.HasPrefix(n, "scsi-2"), strings.HasPrefix(n, "scsi-3"):
		return 3
	case strings.HasPrefix(n, "scsi-"), strings.HasPrefix(n, "virtio-"), strings.HasPrefix(n, "usb-"):
		return 1
	case strings.HasPrefix(n, "wwn-"):
		return 4
	case strings.HasPrefix(n, "dm-"), strings.HasPrefix(n, "md-"), strings.HasPrefix(n, "lvm-"):
		return -1
	}
	return 6
}

// Find returns the disk with that name, path or by-id path.
func Find(disks []Disk, name string) (Disk, bool) {
	for _, d := range disks {
		if name == d.Name || name == d.Path || (d.ByID != "" && name == d.ByID) {
			return d, true
		}
	}
	return Disk{}, false
}
