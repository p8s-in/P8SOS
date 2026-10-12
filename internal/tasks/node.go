package tasks

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"p8sos/internal/execx"
)

// Node gathers what the dashboard's Server page shows.
type Node struct {
	Tasks     *Manager
	Run       execx.Runner
	Pool      string          // ZFS pool, e.g. "tank"
	DataDir   string          // /var/lib/p8s
	Templates func() []string // installed template names
	// CTTemplates lists installed container templates; nil = containers off.
	CTTemplates func() []string
	LAN         func() bool // home-network bridge is up
}

// Storage is the VM pool.
type Storage struct {
	Pool       string `json:"pool"`
	Health     string `json:"health"`
	Size       int64  `json:"size"`
	Used       int64  `json:"used"`
	Free       int64  `json:"free"`
	File       bool   `json:"file"`        // pool lives in a file on the system disk (can grow)
	SystemFree int64  `json:"system_free"` // free space on the system disk
	CanGrow    int64  `json:"can_grow"`    // about how much "Grow" would add
	Error      string `json:"error,omitempty"`
}

// CatalogEntry is an OS image that can be added as a template.
type CatalogEntry struct {
	Name      string `json:"name"`
	Label     string `json:"label"`
	Installed bool   `json:"installed"`
}

// OSUpdates is the last Debian update check (written by os-upgrade.sh).
type OSUpdates struct {
	Checked        string `json:"checked,omitempty"`
	Upgradable     int    `json:"upgradable"`
	Security       int    `json:"security"`
	RebootRequired bool   `json:"reboot_required"`
	Kernel         string `json:"kernel,omitempty"`
}

// Overview is the Server page.
type Overview struct {
	Storage    Storage        `json:"storage"`
	Catalog    []CatalogEntry `json:"catalog"`
	CTCatalog  []CatalogEntry `json:"ct_catalog"` // empty when containers are off
	Containers bool           `json:"containers"` // LXC installed
	LAN        bool           `json:"lan"`
	OS         OSUpdates      `json:"os"`
	Running    *Task          `json:"running,omitempty"`
	Tasks      []Task         `json:"tasks"`
	Hostname   string         `json:"hostname"`
}

const gib = 1 << 30

func (n *Node) storage(ctx context.Context) Storage {
	s := Storage{Pool: n.Pool}
	out, err := n.Run.Run(ctx, "zpool", "list", "-Hp", "-o", "name,size,alloc,free,health", n.Pool)
	if err != nil {
		s.Error = "the VM storage pool could not be read"
		return s
	}
	f := strings.Fields(string(out))
	if len(f) >= 5 {
		s.Size, _ = strconv.ParseInt(f[1], 10, 64)
		s.Used, _ = strconv.ParseInt(f[2], 10, 64)
		s.Free, _ = strconv.ParseInt(f[3], 10, 64)
		s.Health = f[4]
	}
	var fs syscall.Statfs_t
	if syscall.Statfs(n.DataDir, &fs) == nil {
		s.SystemFree = int64(fs.Bavail) * fs.Bsize
		img := filepath.Join(n.DataDir, n.Pool+".img")
		if st, err := os.Stat(img); err == nil {
			s.File = true
			sys := st.Sys().(*syscall.Stat_t)
			used := sys.Blocks * 512
			total := int64(fs.Blocks) * fs.Bsize
			reserve := total / 10
			if reserve < 20*gib {
				reserve = 20 * gib
			}
			target := (used + s.SystemFree - reserve) / gib * gib
			if target > st.Size() {
				s.CanGrow = target - st.Size()
			}
		}
	}
	return s
}

func (n *Node) catalog(ctx context.Context, script string, installed func() []string) []CatalogEntry {
	have := map[string]bool{}
	if installed != nil {
		for _, t := range installed() {
			have[t] = true
		}
	}
	out := []CatalogEntry{}
	b, err := n.Run.Run(ctx, "/bin/bash", filepath.Join(n.Tasks.Scripts, script), "--list")
	if err != nil {
		return out
	}
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) >= 2 && reArg.MatchString(f[0]) {
			out = append(out, CatalogEntry{Name: f[0], Label: strings.Join(f[1:], " "), Installed: have[f[0]]})
		}
	}
	return out
}

// Overview collects everything for the Server page.
func (n *Node) Overview(ctx context.Context) Overview {
	o := Overview{Storage: n.storage(ctx), Catalog: n.catalog(ctx, "make-template.sh", n.Templates), Tasks: n.Tasks.List(ctx),
		CTCatalog: []CatalogEntry{}}
	if n.CTTemplates != nil {
		o.Containers = true
		o.CTCatalog = n.catalog(ctx, "make-ct-template.sh", n.CTTemplates)
	}
	if n.LAN != nil {
		o.LAN = n.LAN()
	}
	if b, err := os.ReadFile(filepath.Join(n.DataDir, "os-updates.json")); err == nil {
		_ = json.Unmarshal(b, &o.OS)
	}
	if _, err := os.Stat("/var/run/reboot-required"); err == nil {
		o.OS.RebootRequired = true
	}
	for i := range o.Tasks {
		if o.Tasks[i].State == "running" {
			t := o.Tasks[i]
			o.Running = &t
			break
		}
	}
	if len(o.Tasks) > 15 {
		o.Tasks = o.Tasks[:15]
	}
	o.Hostname, _ = os.Hostname()
	return o
}
