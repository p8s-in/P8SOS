package imports

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Proxmox container backups (vzdump-lxc-ID-DATE.tar[.zst|.gz|.lzo], .tgz):
// a tar of the container's files, its settings in ./etc/vzdump/pct.conf
// (vzdump puts that first). The files become a P8S container.

// KindLXC marks a Proxmox container backup.
const KindLXC = "proxmox-lxc"

var reLXC = regexp.MustCompile(`\.(tar(\.zst|\.gz|\.lzo)?|tgz)$`)

// isLXC: a tar file that looks like a container backup.
func isLXC(name string) bool {
	return reLXC.MatchString(name) && strings.Contains(strings.ToLower(name), "lxc")
}

// ReadPCTConf reads the container settings from the start of a backup.
func ReadPCTConf(ctx context.Context, file string) (string, error) {
	r, err := OpenStream(ctx, file)
	if err != nil {
		return "", err
	}
	defer r.Close()
	tr := tar.NewReader(r)
	for n := 0; n < 200; n++ { // it comes first; don't read a whole big archive looking for it
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("%s isn't a readable tar archive: %w", filepath.Base(file), err)
		}
		if path.Clean("/"+h.Name) == "/etc/vzdump/pct.conf" {
			b, err := io.ReadAll(io.LimitReader(tr, 1<<20))
			return string(b), err
		}
	}
	return "", fmt.Errorf("%s has no Proxmox container settings (etc/vzdump/pct.conf): is it a vzdump container backup?", filepath.Base(file))
}

func inspectLXC(file string) (Plan, error) {
	conf, err := ReadPCTConf(context.Background(), file)
	if err != nil {
		return Plan{}, err
	}
	p := PlanFromLXC(conf)
	p.File = filepath.Base(file)
	return p, nil
}

// PlanFromLXC maps a Proxmox container's settings to a P8S container.
func PlanFromLXC(conf string) Plan {
	c := ParsePVEConfig(conf)
	p := Plan{Kind: KindLXC, Config: conf, Warnings: []string{}, OSType: "linux", Firmware: "bios", DiskBus: "virtio", NICModel: "virtio"}
	p.Name = cleanName(c["hostname"])
	if p.Name == "" {
		p.Name = "imported-ct"
	}
	p.VCPUs, _ = strconv.Atoi(c["cores"])
	if p.VCPUs < 1 {
		p.VCPUs = 1
		if c["cores"] == "" {
			p.Warnings = append(p.Warnings, "the container had no CPU limit in Proxmox; it gets 1 CPU here (change it below)")
		}
	}
	p.MemoryMiB, _ = strconv.Atoi(firstOf(c["memory"], "512"))
	p.DiskGB = max(sizeGB(optOf(c["rootfs"], "size")), 1)
	p.CTOS = c["ostype"]
	if a := c["arch"]; a != "" && a != "amd64" {
		p.Warnings = append(p.Warnings, "the container is "+a+"; this server runs amd64 programs only")
	}
	if c["unprivileged"] != "1" {
		p.Warnings = append(p.Warnings, "it was a privileged container in Proxmox; P8S runs every container unprivileged (most software works; direct device access and some mounts won't)")
	}
	// mount points: those with backup=1 are inside the archive and land in the root disk
	for i := 0; i < 256; i++ {
		v, ok := c["mp"+strconv.Itoa(i)]
		if !ok {
			continue
		}
		mp := optOf(v, "mp")
		if optOf(v, "backup") == "1" {
			p.DiskGB += sizeGB(optOf(v, "size"))
			p.Warnings = append(p.Warnings, fmt.Sprintf("mount point mp%d (%s) is in the backup: its files go into the container's disk, which is made bigger for it", i, mp))
		} else {
			p.Warnings = append(p.Warnings, fmt.Sprintf("mount point mp%d (%s) wasn't in the backup: that folder will be empty", i, mp))
		}
	}
	if n := c["net0"]; n != "" {
		if mac := optOf(n, "hwaddr"); mac != "" {
			p.MAC = strings.ToLower(mac)
		}
		if ip := optOf(n, "ip"); ip != "" && ip != "dhcp" && ip != "manual" {
			p.StaticIP = ip
			p.Warnings = append(p.Warnings, "it had a fixed address ("+ip+"); here it gets its address by DHCP on the network you pick (the container's network settings are switched to DHCP)")
		}
		if optOf(n, "tag") != "" {
			p.Warnings = append(p.Warnings, "net0 had a VLAN tag; the container joins the chosen network untagged")
		}
	} else {
		p.Warnings = append(p.Warnings, "the container had no network card; it gets one here")
	}
	for i := 1; i < 32; i++ {
		if _, ok := c["net"+strconv.Itoa(i)]; ok {
			p.Warnings = append(p.Warnings, "only the first network card (eth0) is set up here; others were left out")
			break
		}
	}
	for _, k := range []string{"lxc.mount.entry", "dev0", "features"} {
		if v, ok := c[k]; ok && v != "" {
			p.Warnings = append(p.Warnings, fmt.Sprintf("the Proxmox setting %q (%s) isn't carried over", k, v))
		}
	}
	p.SourceInfo = strings.TrimSpace(fmt.Sprintf("Proxmox container, %s, %d GB", firstOf(p.CTOS, "Linux"), p.DiskGB))
	return p
}

// optOf reads key from "a,key=v,b=w" (the first unnamed part counts as volume).
func optOf(v, key string) string {
	for _, part := range strings.Split(v, ",") {
		if k, x, ok := strings.Cut(strings.TrimSpace(part), "="); ok && k == key {
			return x
		}
	}
	return ""
}

// sizeGB reads Proxmox sizes ("8G", "512M", "1T"; no unit = GiB), rounded up.
func sizeGB(s string) int {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0
	}
	mul := 1.0
	switch s[len(s)-1] {
	case 'T':
		mul, s = 1024, s[:len(s)-1]
	case 'G':
		s = s[:len(s)-1]
	case 'M':
		mul, s = 1.0/1024, s[:len(s)-1]
	case 'K':
		mul, s = 1.0/(1024*1024), s[:len(s)-1]
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 {
		return 0
	}
	n := f * mul
	if n != float64(int(n)) {
		return int(n) + 1
	}
	return int(n)
}
