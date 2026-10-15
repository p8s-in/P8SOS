// Package diskmgr is the server's disk management (like Proxmox's Disks
// view): every physical disk with what is on it and its SMART health, wiping
// a disk that isn't in use, protecting disks that must never be touched, and
// making a new ZFS pool from empty disks that becomes a storage for VM disks.
//
// Safety rules, checked again on the server for every action:
//   - a disk that is mounted, holds the system, belongs to an imported ZFS
//     pool, to LVM (Ceph) or software RAID, or is swap can't be wiped or used;
//   - a disk marked protected can't be wiped or used;
//   - a disk with data is only wiped when its name is typed to confirm;
//   - new pools are only made from empty disks.
package diskmgr

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
	"p8sos/internal/installer"
)

var (
	// ErrInvalid is a bad request.
	ErrInvalid = errors.New("invalid")
	// ErrRefused is an action the safety rules don't allow.
	ErrRefused = errors.New("refused")
	// ErrConfirm means the disk has data: type its name to confirm.
	ErrConfirm = errors.New("confirm")
	// ErrNotFound is an unknown disk or pool.
	ErrNotFound = errors.New("not found")
)

// Disk is one physical disk as the Disks view shows it.
type Disk struct {
	Name      string   `json:"name"`
	Path      string   `json:"path"`
	ByID      string   `json:"by_id,omitempty"`
	Size      int64    `json:"size"`
	Model     string   `json:"model,omitempty"`
	Serial    string   `json:"serial,omitempty"`
	Kind      string   `json:"kind"` // NVMe, SSD, HDD (+ USB)
	Tran      string   `json:"tran,omitempty"`
	Parts     int      `json:"parts"`
	Contents  []string `json:"contents"`
	Mounted   []string `json:"mounted"`
	Usage     string   `json:"usage"`          // empty, system, pool NAME, ceph/LVM, RAID, data, ...
	Pool      string   `json:"pool,omitempty"` // the imported ZFS pool it belongs to
	InUse     string   `json:"in_use,omitempty"`
	Protected bool     `json:"protected"`
	Empty     bool     `json:"empty"`
	SMART     *SMART   `json:"smart,omitempty"`
}

// SMART is a disk's health summary.
type SMART struct {
	Available   bool   `json:"available"`
	Passed      bool   `json:"passed"`
	TempC       int    `json:"temp_c,omitempty"`
	PowerOnHrs  int    `json:"power_on_hours,omitempty"`
	WearPct     int    `json:"wear_pct,omitempty"`    // NVMe/SSD percentage used
	Reallocated int    `json:"reallocated,omitempty"` // sectors (ATA 5)
	Pending     int    `json:"pending,omitempty"`     // sectors (ATA 197)
	MediaErrors int    `json:"media_errors,omitempty"`
	Note        string `json:"note,omitempty"`
}

// LocalPool is a ZFS pool made here for VM disks.
type LocalPool struct {
	Name    string   `json:"name"`   // storage name (VMs pick it)
	Pool    string   `json:"pool"`   // ZFS pool: p8s-<name>
	Layout  string   `json:"layout"` // single, mirror, raidz1, raidz2
	Disks   []string `json:"disks"`  // the devices it was made from
	Created string   `json:"created"`
	// filled when listed
	Health string `json:"health,omitempty"`
	Size   int64  `json:"size,omitempty"`
	Free   int64  `json:"free,omitempty"`
	VMs    int    `json:"vm_disks"`
}

// Manager does it on this server.
type Manager struct {
	Run     execx.Runner
	DataDir string // disks-protected.json, localpools.json
	// ByID maps /dev/sdX to its stable name; nil = /dev/disk/by-id.
	ByID func() map[string]string
	// Resolve follows a device link to /dev/sdX; nil = filepath.EvalSymlinks.
	Resolve func(string) (string, error)
	// Taken lists other storage names in use (network storages, "local", "ceph").
	Taken func() []string
	Now   func() time.Time

	mu    sync.Mutex
	smart map[string]smartCache
}

type smartCache struct {
	at time.Time
	s  *SMART
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) resolve(p string) string {
	f := filepath.EvalSymlinks
	if m.Resolve != nil {
		f = m.Resolve
	}
	if r, err := f(p); err == nil {
		return r
	}
	return p
}

// lsblk children: every partition / holder path → its whole disk.
type node struct {
	Name     string  `json:"name"`
	Path     string  `json:"path"`
	Type     string  `json:"type"`
	FSType   *string `json:"fstype"`
	Children []node  `json:"children"`
}

func childPaths(out []byte) (map[string]string, map[string][]string) {
	var doc struct {
		Blockdevices []node `json:"blockdevices"`
	}
	_ = json.Unmarshal(out, &doc)
	owner := map[string]string{}
	types := map[string][]string{}
	for _, d := range doc.Blockdevices {
		if d.Type != "disk" {
			continue
		}
		var walk func(n node)
		walk = func(n node) {
			p := n.Path
			if p == "" {
				p = "/dev/" + n.Name
			}
			owner[p] = d.Name
			types[d.Name] = append(types[d.Name], n.Type)
			for _, c := range n.Children {
				walk(c)
			}
		}
		walk(d)
	}
	return owner, types
}

// pools maps each disk name to the imported ZFS pool using it.
func (m *Manager) pools(ctx context.Context, owner map[string]string) map[string]string {
	out, err := m.Run.Run(ctx, "zpool", "status", "-P")
	if err != nil {
		return map[string]string{}
	}
	res := map[string]string{}
	pool := ""
	for _, line := range strings.Split(string(out), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "pool:") {
			pool = strings.TrimSpace(strings.TrimPrefix(t, "pool:"))
			continue
		}
		f := strings.Fields(t)
		if len(f) == 0 || !strings.HasPrefix(f[0], "/") {
			continue
		}
		dev := f[0]
		if d, ok := owner[dev]; ok {
			res[d] = pool
			continue
		}
		if d, ok := owner[m.resolve(dev)]; ok {
			res[d] = pool
			continue
		}
		// a file-backed pool (/var/lib/p8s/tank.img) has no disk
	}
	return res
}

func kindOf(d installer.Disk) string {
	k := "SSD"
	switch {
	case d.Tran == "nvme":
		k = "NVMe"
	case d.Rota:
		k = "HDD"
	}
	if d.Tran == "usb" || d.Removable {
		k += ", USB"
	}
	return k
}

// List is every physical disk with what is on it.
func (m *Manager) List(ctx context.Context, withSMART bool) ([]Disk, error) {
	out, err := m.Run.Run(ctx, "lsblk", installer.LsblkArgs...)
	if err != nil {
		return nil, err
	}
	byID := map[string]string{}
	if m.ByID != nil {
		byID = m.ByID()
	} else {
		byID = installer.ByIDLinks("/dev/disk/by-id")
	}
	raw, err := installer.ParseLsblk(out, byID)
	if err != nil {
		return nil, err
	}
	owner, types := childPaths(out)
	inPool := m.pools(ctx, owner)
	prot := m.protected()
	var l []Disk
	for _, d := range raw {
		x := Disk{Name: d.Name, Path: d.Path, ByID: d.ByID, Size: d.Size, Model: d.Model, Serial: d.Serial,
			Kind: kindOf(d), Tran: d.Tran, Parts: d.Parts, Contents: d.Contents, Mounted: d.Mounted,
			Pool: inPool[d.Name], Empty: d.Empty()}
		x.Protected = prot[keyOf(x)]
		x.Usage, x.InUse = usage(x, types[d.Name])
		if withSMART {
			x.SMART = m.smartOf(ctx, x.Path)
		}
		l = append(l, x)
	}
	return l, nil
}

func keyOf(d Disk) string {
	if d.ByID != "" {
		return d.ByID
	}
	if d.Serial != "" {
		return "serial:" + d.Serial
	}
	return d.Path
}

func has(l []string, prefix string) bool {
	for _, s := range l {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

// usage says what the disk is used for, and why it is in use ("" = free to wipe).
func usage(d Disk, types []string) (string, string) {
	sys := false
	for _, m := range d.Mounted {
		if m == "/" || m == "/boot" || m == "/boot/efi" || m == "[SWAP]" || strings.HasPrefix(m, "/usr") || strings.HasPrefix(m, "/var") {
			sys = true
		}
	}
	switch {
	case d.Pool == "rpool" || d.Pool == "bpool" || sys:
		return "system", "the server's own system is on it"
	case d.Pool != "":
		return "ZFS pool " + d.Pool, "it belongs to the ZFS pool " + d.Pool
	case has(d.Contents, "LVM2_member"):
		return "LVM / Ceph", "it is an LVM disk (Ceph puts its disks in LVM): remove it from Ceph first"
	case has(d.Contents, "linux_raid_member"):
		return "software RAID", "it is part of a software RAID"
	case has(d.Contents, "swap"):
		return "swap", "it is swap space"
	case len(d.Mounted) > 0:
		return "mounted", "it is mounted at " + strings.Join(d.Mounted, ", ")
	case d.Tran == "iscsi":
		return "network disk (iSCSI)", "it is a network disk: manage it under Network storage"
	case has(d.Contents, "zfs_member"):
		return "old ZFS pool (not imported)", ""
	case d.Empty:
		return "empty", ""
	}
	for _, t := range types {
		if t == "crypt" || t == "lvm" || t == "raid1" || t == "md" {
			return "in use (" + t + ")", "it is in use (" + t + ")"
		}
	}
	return "data", ""
}

func (m *Manager) find(ctx context.Context, name string) (Disk, error) {
	l, err := m.List(ctx, false)
	if err != nil {
		return Disk{}, err
	}
	for _, d := range l {
		if d.Name == name || d.Path == name || d.ByID != "" && d.ByID == name {
			return d, nil
		}
	}
	return Disk{}, fmt.Errorf("%w: no disk %q", ErrNotFound, name)
}

// Wipe clears a disk (partition tables, file systems, old ZFS labels). A disk
// with data needs confirm = its name.
func (m *Manager) Wipe(ctx context.Context, name, confirm string) error {
	d, err := m.find(ctx, name)
	if err != nil {
		return err
	}
	switch {
	case d.Protected:
		return fmt.Errorf("%w: %s is protected (unprotect it first)", ErrRefused, d.Name)
	case d.InUse != "":
		return fmt.Errorf("%w: %s can't be wiped: %s", ErrRefused, d.Name, d.InUse)
	case !d.Empty && confirm != d.Name:
		return fmt.Errorf("%w: %s has data (%s): type its name to wipe it", ErrConfirm, d.Name, strings.Join(d.Contents, ", "))
	}
	out, _ := m.Run.Run(ctx, "lsblk", "-lnpo", "NAME,TYPE", d.Path)
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == "part" {
			_, _ = m.Run.Run(ctx, "zpool", "labelclear", "-f", f[0])
			if _, err := m.Run.Run(ctx, "wipefs", "-af", f[0]); err != nil {
				return err
			}
		}
	}
	_, _ = m.Run.Run(ctx, "zpool", "labelclear", "-f", d.Path)
	if _, err := m.Run.Run(ctx, "wipefs", "-af", d.Path); err != nil {
		return err
	}
	_, _ = m.Run.Run(ctx, "blockdev", "--rereadpt", d.Path)
	_, _ = m.Run.Run(ctx, "udevadm", "settle", "--timeout=10")
	return nil
}

// --- protection -------------------------------------------------------------

func (m *Manager) protFile() string { return filepath.Join(m.DataDir, "disks-protected.json") }

func (m *Manager) protected() map[string]bool {
	out := map[string]bool{}
	var l []string
	if b, err := os.ReadFile(m.protFile()); err == nil {
		_ = json.Unmarshal(b, &l)
	}
	for _, k := range l {
		out[k] = true
	}
	return out
}

func writeJSON(path string, v any) error {
	b, _ := json.MarshalIndent(v, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Protect marks (or unmarks) a disk as never to be wiped or used.
func (m *Manager) Protect(ctx context.Context, name string, on bool) error {
	d, err := m.find(ctx, name)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.protected()
	if on {
		p[keyOf(d)] = true
	} else {
		delete(p, keyOf(d))
	}
	l := make([]string, 0, len(p))
	for k := range p {
		l = append(l, k)
	}
	sort.Strings(l)
	return writeJSON(m.protFile(), l)
}

// --- SMART ------------------------------------------------------------------

func (m *Manager) smartOf(ctx context.Context, dev string) *SMART {
	m.mu.Lock()
	if m.smart == nil {
		m.smart = map[string]smartCache{}
	}
	if c, ok := m.smart[dev]; ok && m.now().Sub(c.at) < 10*time.Minute {
		m.mu.Unlock()
		return c.s
	}
	m.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := m.Run.Run(cctx, "smartctl", "-j", "-H", "-A", "-i", dev)
	s := ParseSMART(out)
	if !s.Available && err != nil && len(out) == 0 {
		s.Note = "smartctl isn't installed or can't read this disk"
	}
	m.mu.Lock()
	m.smart[dev] = smartCache{at: m.now(), s: s}
	m.mu.Unlock()
	return s
}

// ParseSMART reads smartctl -j output (smartctl exits non-zero for many
// harmless reasons, so the JSON decides).
func ParseSMART(out []byte) *SMART {
	var j struct {
		SmartStatus *struct {
			Passed bool `json:"passed"`
		} `json:"smart_status"`
		Temperature struct {
			Current int `json:"current"`
		} `json:"temperature"`
		PowerOnTime struct {
			Hours int `json:"hours"`
		} `json:"power_on_time"`
		ATA struct {
			Table []struct {
				ID  int `json:"id"`
				Raw struct {
					Value int `json:"value"`
				} `json:"raw"`
			} `json:"table"`
		} `json:"ata_smart_attributes"`
		NVMe *struct {
			PercentageUsed int `json:"percentage_used"`
			MediaErrors    int `json:"media_errors"`
		} `json:"nvme_smart_health_information_log"`
		Smartctl struct {
			Messages []struct {
				String string `json:"string"`
			} `json:"messages"`
		} `json:"smartctl"`
	}
	s := &SMART{}
	if json.Unmarshal(out, &j) != nil {
		return s
	}
	if j.SmartStatus == nil {
		if len(j.Smartctl.Messages) > 0 {
			s.Note = j.Smartctl.Messages[0].String
		} else {
			s.Note = "no SMART data (virtual disk?)"
		}
		return s
	}
	s.Available, s.Passed = true, j.SmartStatus.Passed
	s.TempC, s.PowerOnHrs = j.Temperature.Current, j.PowerOnTime.Hours
	for _, a := range j.ATA.Table {
		switch a.ID {
		case 5:
			s.Reallocated = a.Raw.Value
		case 197:
			s.Pending = a.Raw.Value
		case 177, 231, 233: // SSD wear (vendor-specific, best effort)
		}
	}
	if j.NVMe != nil {
		s.WearPct, s.MediaErrors = j.NVMe.PercentageUsed, j.NVMe.MediaErrors
	}
	return s
}

// --- local pools ------------------------------------------------------------

var reName = regexp.MustCompile(`^[a-z][a-z0-9-]{1,19}$`)

func (m *Manager) poolFile() string { return filepath.Join(m.DataDir, "localpools.json") }

// Pools are the pools made here.
func (m *Manager) Pools() []LocalPool {
	var l []LocalPool
	if b, err := os.ReadFile(m.poolFile()); err == nil {
		_ = json.Unmarshal(b, &l)
	}
	return l
}

// VMPools maps each local pool's storage name to its dataset for VM disks
// (merged with the network storages' pools in storage.ZFS).
func (m *Manager) VMPools() map[string]string {
	out := map[string]string{}
	for _, p := range m.Pools() {
		out[p.Name] = p.Pool + "/vms"
	}
	return out
}

// PoolsInfo is the pools with their health, size and VM disks.
func (m *Manager) PoolsInfo(ctx context.Context) []LocalPool {
	l := m.Pools()
	for i := range l {
		p := &l[i]
		if out, err := m.Run.Run(ctx, "zpool", "list", "-Hp", "-o", "size,free,health", p.Pool); err == nil {
			f := strings.Fields(string(out))
			if len(f) == 3 {
				fmt.Sscan(f[0], &p.Size)
				fmt.Sscan(f[1], &p.Free)
				p.Health = f[2]
			}
		} else {
			p.Health = "MISSING"
		}
		if out, err := m.Run.Run(ctx, "zfs", "list", "-H", "-o", "name", "-t", "volume", "-r", p.Pool+"/vms"); err == nil {
			p.VMs = len(strings.Fields(string(out)))
		}
	}
	return l
}

// CreatePool makes a ZFS pool p8s-<name> from empty disks and a dataset for VM disks.
func (m *Manager) CreatePool(ctx context.Context, name, layout string, disks []string) (LocalPool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !reName.MatchString(name) {
		return LocalPool{}, fmt.Errorf("%w: name: 2-20 lowercase letters, digits and -, starting with a letter", ErrInvalid)
	}
	taken := map[string]bool{"local": true, "ceph": true}
	if m.Taken != nil {
		for _, t := range m.Taken() {
			taken[t] = true
		}
	}
	for _, p := range m.Pools() {
		taken[p.Name] = true
	}
	if taken[name] {
		return LocalPool{}, fmt.Errorf("%w: a storage called %s exists already", ErrInvalid, name)
	}
	need := map[string]int{"single": 1, "mirror": 2, "raidz1": 3, "raidz2": 4}
	min, ok := need[layout]
	if !ok {
		return LocalPool{}, fmt.Errorf("%w: layout must be single, mirror, raidz1 or raidz2", ErrInvalid)
	}
	if len(disks) < min || layout == "single" && len(disks) != 1 || len(disks) > 12 {
		return LocalPool{}, fmt.Errorf("%w: %s needs %s", ErrInvalid, layout, map[string]string{"single": "exactly 1 disk", "mirror": "2 or more disks", "raidz1": "3 or more disks", "raidz2": "4 or more disks"}[layout])
	}
	all, err := m.List(ctx, false)
	if err != nil {
		return LocalPool{}, err
	}
	byName := map[string]Disk{}
	for _, d := range all {
		byName[d.Name] = d
	}
	seen := map[string]bool{}
	var devs []string
	for _, n := range disks {
		d, ok := byName[strings.TrimPrefix(n, "/dev/")]
		switch {
		case !ok:
			return LocalPool{}, fmt.Errorf("%w: no disk %q", ErrNotFound, n)
		case seen[d.Name]:
			return LocalPool{}, fmt.Errorf("%w: %s twice", ErrInvalid, d.Name)
		case d.Protected:
			return LocalPool{}, fmt.Errorf("%w: %s is protected", ErrRefused, d.Name)
		case d.InUse != "":
			return LocalPool{}, fmt.Errorf("%w: %s: %s", ErrRefused, d.Name, d.InUse)
		case !d.Empty:
			return LocalPool{}, fmt.Errorf("%w: %s isn't empty (%s): wipe it first", ErrRefused, d.Name, strings.Join(d.Contents, ", "))
		case strings.Contains(d.Kind, "USB"):
			return LocalPool{}, fmt.Errorf("%w: %s is a USB disk: not for VM storage", ErrRefused, d.Name)
		}
		seen[d.Name] = true
		dev := d.ByID
		if dev == "" {
			dev = d.Path
		}
		devs = append(devs, dev)
	}
	pool := "p8s-" + name
	args := []string{"create", "-o", "ashift=12", "-O", "compression=lz4", "-O", "atime=off", "-O", "xattr=sa", "-m", "none", pool}
	if layout != "single" {
		args = append(args, layout)
	}
	args = append(args, devs...)
	if _, err := m.Run.Run(ctx, "zpool", args...); err != nil {
		return LocalPool{}, fmt.Errorf("making the pool: %w", err)
	}
	if _, err := m.Run.Run(ctx, "zfs", "create", "-o", "canmount=off", pool+"/vms"); err != nil {
		_, _ = m.Run.Run(ctx, "zpool", "destroy", pool)
		return LocalPool{}, fmt.Errorf("making the VM dataset: %w", err)
	}
	p := LocalPool{Name: name, Pool: pool, Layout: layout, Disks: devs, Created: m.now().UTC().Format(time.RFC3339)}
	l := append(m.Pools(), p)
	if err := writeJSON(m.poolFile(), l); err != nil {
		return p, err
	}
	return p, nil
}

// DestroyPool removes a pool made here (no VM disks may be on it; confirm = its name).
func (m *Manager) DestroyPool(ctx context.Context, name, confirm string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.Pools()
	idx := -1
	for i, p := range l {
		if p.Name == name {
			idx = i
		}
	}
	if idx < 0 {
		return fmt.Errorf("%w: no storage %q made here", ErrNotFound, name)
	}
	p := l[idx]
	if out, err := m.Run.Run(ctx, "zfs", "list", "-H", "-o", "name", "-t", "volume", "-r", p.Pool+"/vms"); err == nil && len(strings.Fields(string(out))) > 0 {
		return fmt.Errorf("%w: VM disks are still on %s (%s): move them first", ErrRefused, name, strings.Join(strings.Fields(string(out)), ", "))
	}
	if confirm != name {
		return fmt.Errorf("%w: type the storage name to destroy it", ErrConfirm)
	}
	if out, err := m.Run.Run(ctx, "zpool", "list", "-H", "-o", "name", p.Pool); err == nil && strings.TrimSpace(string(out)) == p.Pool {
		if _, err := m.Run.Run(ctx, "zpool", "destroy", p.Pool); err != nil {
			return err
		}
	}
	l = append(l[:idx], l[idx+1:]...)
	return writeJSON(m.poolFile(), l)
}
