// Package config loads the p8sd configuration file (JSON).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

type Config struct {
	Listen       string     `json:"listen"`           // e.g. "127.0.0.1:8006"
	DataDir      string     `json:"data_dir"`         // e.g. "/var/lib/p8s"
	LibvirtURI   string     `json:"libvirt_uri"`      // e.g. "qemu:///system"
	VMDataset    string     `json:"vm_dataset"`       // ZFS dataset holding VM disks, e.g. "tank/p8s/vms"
	Bridge       string     `json:"bridge"`           // NAT bridge VMs attach to by default, e.g. "virbr0"
	LANBridge    string     `json:"lan_bridge"`       // optional bridge onto the home network, e.g. "br0"
	TLSCert      string     `json:"tls_cert"`         // optional; plain HTTP if empty
	TLSKey       string     `json:"tls_key"`          // optional
	TLSAuto      bool       `json:"tls_auto"`         // HTTPS with a self-signed cert in data_dir/tls
	SSHKeys      []string   `json:"default_ssh_keys"` // added to VMs created without keys (e.g. from the dashboard)
	Tokens       []Token    `json:"tokens"`           // API tokens (hashed)
	Templates    []Template `json:"templates"`        // VM templates
	MaxVCPUs     int        `json:"max_vcpus"`        // per-VM ceiling
	MaxMemoryMiB int        `json:"max_memory_mib"`   // per-VM ceiling
	MaxDiskGB    int        `json:"max_disk_gb"`      // per-VM ceiling
	BackupDir    string     `json:"backup_dir"`       // backup files; default data_dir/backups (put it on another disk!)
	ISODir       string     `json:"iso_dir"`          // installer ISO library; default data_dir/isos
	VMGuard      *bool      `json:"vm_guard"`         // keep VMs off the host and the home LAN (default true)
	VMAllow      []string   `json:"vm_allow"`         // private addresses VMs may still reach, e.g. a NAS
	BackupTime   string     `json:"backup_time"`      // daily backups at HH:MM host time; default 02:30
	BackupKeep   int        `json:"backup_keep"`      // default number of daily backups kept per VM; default 7
	// LicenseServer is the P8S licence server, e.g. "https://licence.example.com";
	// p8sd asks it once a day whether this server's key was renewed or cancelled.
	// Empty = keys are only checked offline.
	LicenseServer string `json:"license_server"`
}

// GuardOn reports whether VM isolation rules are wanted (default yes).
func (c *Config) GuardOn() bool { return c.VMGuard == nil || *c.VMGuard }

// Token is an API credential. Only the SHA-256 hex of the secret is stored.
type Token struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

// Template is a golden image stored as a ZFS zvol snapshot.
type Template struct {
	Name      string `json:"name"`        // e.g. "debian-13"
	Snapshot  string `json:"snapshot"`    // e.g. "tank/p8s/templates/debian-13@base"
	MinDiskGB int    `json:"min_disk_gb"` // must be >= the zvol's volsize
	OSVariant string `json:"os_variant"`  // informational, e.g. "debian13"
	Label     string `json:"label"`       // shown in the dashboard, e.g. "Ubuntu 24.04 LTS"
	// DefaultUser is the image's login user that gets the SSH keys ("debian", "ubuntu", "rocky").
	DefaultUser string `json:"default_user"`
}

// User returns the template's login user (older configs have none: Debian).
func (t Template) User() string {
	if t.DefaultUser != "" {
		return t.DefaultUser
	}
	return "debian"
}

var (
	reDataset = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]*$`)
	reSnap    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]*@[A-Za-z0-9_.:-]+$`)
	reHex64   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	reClock   = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
)

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &Config{
		Listen:       "127.0.0.1:8006",
		DataDir:      "/var/lib/p8s",
		LibvirtURI:   "qemu:///system",
		Bridge:       "vmbr0",
		MaxVCPUs:     16,
		MaxMemoryMiB: 65536,
		MaxDiskGB:    2048,
	}
	if err := json.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.BackupDir == "" {
		c.BackupDir = filepath.Join(c.DataDir, "backups")
	}
	if c.ISODir == "" {
		c.ISODir = filepath.Join(c.DataDir, "isos")
	}
	if c.BackupTime == "" {
		c.BackupTime = "02:30"
	}
	if c.BackupKeep == 0 {
		c.BackupKeep = 7
	}
	return c, c.Validate()
}

func (c *Config) Validate() error {
	var errs []error
	if !reDataset.MatchString(c.VMDataset) {
		errs = append(errs, fmt.Errorf("vm_dataset %q is invalid", c.VMDataset))
	}
	if c.Bridge == "" {
		errs = append(errs, errors.New("bridge is required"))
	}
	if c.LANBridge != "" && c.LANBridge == c.Bridge {
		errs = append(errs, errors.New("lan_bridge must differ from bridge"))
	}
	if len(c.Tokens) == 0 {
		errs = append(errs, errors.New("at least one token is required (run: p8sd token)"))
	}
	for _, t := range c.Tokens {
		if !reHex64.MatchString(t.SHA256) {
			errs = append(errs, fmt.Errorf("token %q: sha256 must be 64 lowercase hex chars", t.Name))
		}
	}
	seen := map[string]bool{}
	for _, t := range c.Templates {
		if t.Name == "" || seen[t.Name] {
			errs = append(errs, fmt.Errorf("template name %q empty or duplicated", t.Name))
		}
		seen[t.Name] = true
		if !reSnap.MatchString(t.Snapshot) {
			errs = append(errs, fmt.Errorf("template %q: snapshot %q must look like pool/ds@snap", t.Name, t.Snapshot))
		}
		if t.MinDiskGB <= 0 {
			errs = append(errs, fmt.Errorf("template %q: min_disk_gb must be > 0", t.Name))
		}
	}
	if c.BackupTime != "" && !reClock.MatchString(c.BackupTime) {
		errs = append(errs, fmt.Errorf("backup_time %q must be HH:MM (24h)", c.BackupTime))
	}
	if c.BackupKeep < 0 || c.BackupKeep > 365 {
		errs = append(errs, errors.New("backup_keep must be 1-365"))
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		errs = append(errs, errors.New("tls_cert and tls_key must be set together"))
	}
	return errors.Join(errs...)
}
