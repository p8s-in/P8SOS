package imports

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Kinds of file that can be imported.
const (
	KindVMA  = "proxmox-vma" // vzdump backup of a Proxmox VM
	KindDisk = "disk-image"  // qcow2, vmdk, vhd(x), raw/img
)

// Plan is what an import would create; the dashboard shows it for checking
// and the person can change every field before starting.
type Plan struct {
	File      string `json:"file"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	VCPUs     int    `json:"vcpus"`
	MemoryMiB int    `json:"memory_mib"`
	DiskGB    int    `json:"disk_gb"`
	OSType    string `json:"os_type"`   // linux | windows | windows11
	Firmware  string `json:"firmware"`  // bios | uefi
	DiskBus   string `json:"disk_bus"`  // virtio | scsi | sata
	NICModel  string `json:"nic_model"` // virtio | e1000e | e1000 | rtl8139 | vmxnet3
	MAC       string `json:"mac,omitempty"`
	// VMA: the stream that becomes the VM's disk, and whether UEFI boot
	// entries (efidisk) come along.
	BootDevice string   `json:"boot_device,omitempty"`
	EFIVars    bool     `json:"efi_vars"`
	Devices    []Device `json:"devices,omitempty"`
	Format     string   `json:"format,omitempty"` // disk images: qcow2, vmdk, ...
	// Disks of an .ova (the first is the system disk).
	Disks      []OVADisk `json:"ova_disks,omitempty"`
	SourceInfo string    `json:"source_info,omitempty"`
	Config     string    `json:"config,omitempty"` // Proxmox qemu-server.conf as in the backup
	Warnings   []string  `json:"warnings"`
}

// OVMF_VARS_4M.fd: the size of a 4m Proxmox efidisk and of Debian's UEFI
// variable store, so the boot entries can be carried over as they are.
const efiVarsSize = 540672

var (
	reVMA  = regexp.MustCompile(`\.vma(\.zst|\.lzo|\.gz)?$`)
	reDisk = regexp.MustCompile(`(?i)\.(qcow2|vmdk|vhd|vhdx|img|raw|vdi)$`)
	reOVA  = regexp.MustCompile(`(?i)\.ova$`)
	reName = regexp.MustCompile(`[^a-z0-9-]+`)
)

// KindOf tells what a file name is, or "".
func KindOf(name string) string {
	switch {
	case reVMA.MatchString(name):
		return KindVMA
	case reDisk.MatchString(name):
		return KindDisk
	case reOVA.MatchString(name):
		return KindOVA
	}
	return ""
}

// OpenStream opens a (possibly compressed) VMA file as a plain stream.
func OpenStream(ctx context.Context, path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	var tool []string
	switch {
	case strings.HasSuffix(path, ".gz"):
		zr, err := gzip.NewReader(bufio.NewReaderSize(f, 1<<20))
		if err != nil {
			f.Close()
			return nil, err
		}
		return struct {
			io.Reader
			io.Closer
		}{zr, f}, nil
	case strings.HasSuffix(path, ".zst"):
		tool = []string{"zstd", "-dc", "-q"}
	case strings.HasSuffix(path, ".lzo"):
		tool = []string{"lzop", "-dc"}
	default:
		return f, nil
	}
	if _, err := exec.LookPath(tool[0]); err != nil {
		f.Close()
		return nil, fmt.Errorf("%s isn't installed (needed for %s files); run: sudo apt install %s", tool[0], filepath.Ext(path), tool[0])
	}
	cmd := exec.CommandContext(ctx, tool[0], tool[1:]...)
	cmd.Stdin = f
	out, err := cmd.StdoutPipe()
	if err != nil {
		f.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		f.Close()
		return nil, err
	}
	return &procReader{ReadCloser: out, cmd: cmd, f: f}, nil
}

type procReader struct {
	io.ReadCloser
	cmd *exec.Cmd
	f   *os.File
}

func (p *procReader) Close() error {
	p.ReadCloser.Close()
	_ = p.cmd.Process.Kill()
	_ = p.cmd.Wait()
	return p.f.Close()
}

// Inspect reads a file and proposes how to import it.
func Inspect(path string) (Plan, error) {
	base := filepath.Base(path)
	switch KindOf(base) {
	case KindVMA:
		r, err := OpenStream(context.Background(), path)
		if err != nil {
			return Plan{}, err
		}
		defer r.Close()
		v, err := OpenVMA(r)
		if err != nil {
			return Plan{}, err
		}
		p := PlanFromVMA(v)
		p.File = base
		return p, nil
	case KindDisk:
		return inspectDisk(path)
	case KindOVA:
		return inspectOVA(path)
	}
	return Plan{}, fmt.Errorf("%s: not a file P8S can import (.vma[.zst|.lzo|.gz], .ova, .qcow2, .vmdk, .vhd, .vhdx, .vdi, .img, .raw)", base)
}

// PlanFromVMA maps a Proxmox VM's config to P8S settings.
func PlanFromVMA(v *VMA) Plan {
	p := Plan{Kind: KindVMA, Devices: v.Devices, Warnings: []string{}}
	conf := string(v.Configs["qemu-server.conf"])
	p.Config = conf
	c := ParsePVEConfig(conf)
	p.Name = cleanName(c["name"])
	cores, _ := strconv.Atoi(firstOf(c["cores"], "1"))
	sockets, _ := strconv.Atoi(firstOf(c["sockets"], "1"))
	p.VCPUs = max(cores*max(sockets, 1), 1)
	if n, err := strconv.Atoi(c["vcpus"]); err == nil && n > 0 {
		p.VCPUs = n
	}
	p.MemoryMiB, _ = strconv.Atoi(firstOf(c["memory"], "2048"))
	p.Firmware = "bios"
	if c["bios"] == "ovmf" {
		p.Firmware = "uefi"
	}
	switch os := c["ostype"]; {
	case os == "win11":
		p.OSType = "windows11"
	case strings.HasPrefix(os, "win") || strings.HasPrefix(os, "w2k") || os == "wxp" || os == "wvista":
		p.OSType = "windows"
	default:
		p.OSType = "linux"
	}
	if p.OSType == "windows11" && p.Firmware != "uefi" {
		p.OSType = "windows"
		p.Warnings = append(p.Warnings, "Windows 11 on BIOS in Proxmox: imported as \"Windows\" (BIOS)")
	}

	// the boot disk: the first disk in the boot order, else the first one found
	devs := map[string]Device{}
	for _, d := range v.Devices {
		devs[d.Name] = d
	}
	var order []string
	if b := c["boot"]; strings.Contains(b, "order=") {
		for _, k := range strings.Split(strings.TrimPrefix(b[strings.Index(b, "order="):], "order="), ";") {
			order = append(order, strings.TrimSpace(strings.Split(k, ",")[0]))
		}
	}
	if bd := c["bootdisk"]; bd != "" {
		order = append(order, bd)
	}
	for _, k := range diskKeys() {
		order = append(order, k)
	}
	for _, k := range order {
		if d, ok := devs["drive-"+k]; ok && !strings.Contains(c[k], "media=cdrom") {
			p.BootDevice = d.Name
			p.DiskGB = int((d.Size + (1 << 30) - 1) >> 30)
			switch {
			case strings.HasPrefix(k, "virtio"):
				p.DiskBus = "virtio"
			case strings.HasPrefix(k, "scsi"):
				p.DiskBus = "scsi"
				if hw := c["scsihw"]; hw != "" && !strings.HasPrefix(hw, "virtio-scsi") {
					p.Warnings = append(p.Warnings, fmt.Sprintf("the SCSI controller was %s; P8S uses virtio-scsi (Linux: fine; Windows needs the virtio-scsi driver)", hw))
				}
			case strings.HasPrefix(k, "sata"):
				p.DiskBus = "sata"
			default: // ide
				p.DiskBus = "sata"
				p.Warnings = append(p.Warnings, "the disk was on IDE; it becomes SATA (Linux: fine; Windows may need the AHCI driver enabled first)")
			}
			break
		}
	}
	if p.BootDevice == "" {
		p.Warnings = append(p.Warnings, "no disk found in the backup")
	}
	var extra []string
	for _, d := range v.Devices {
		switch {
		case d.Name == p.BootDevice || d.Name == "vmstate":
		case d.Name == "drive-efidisk0":
			if d.Size == efiVarsSize && p.Firmware == "uefi" {
				p.EFIVars = true
			} else if p.Firmware == "uefi" {
				p.Warnings = append(p.Warnings, "the UEFI variables (efidisk, "+strconv.FormatInt(d.Size, 10)+" bytes) aren't the 4M format: the VM boots via the default EFI path")
			}
		case d.Name == "drive-tpmstate0":
			p.Warnings = append(p.Warnings, "the TPM state isn't carried over: BitLocker or Windows Hello may ask for the recovery key")
		default:
			extra = append(extra, strings.TrimPrefix(d.Name, "drive-"))
		}
	}
	if len(extra) > 0 {
		p.Warnings = append(p.Warnings, "only the boot disk is imported for now; not imported: "+strings.Join(extra, ", "))
	}
	// network card: model and MAC of net0
	p.NICModel = "virtio"
	if n := c["net0"]; n != "" {
		first := strings.Split(n, ",")[0]
		if m, mac, ok := strings.Cut(first, "="); ok {
			if NICModels[m] {
				p.NICModel = m
			} else {
				p.Warnings = append(p.Warnings, "network card "+m+" isn't available; virtio is used")
			}
			p.MAC = strings.ToLower(mac)
		}
		if strings.Contains(n, "tag=") {
			p.Warnings = append(p.Warnings, "net0 had a VLAN tag; VLANs aren't supported yet (the VM joins the chosen network untagged)")
		}
	}
	if p.Name == "" {
		p.Name = "imported"
	}
	return p
}

// NICModels P8S can give a VM.
var NICModels = map[string]bool{"virtio": true, "e1000e": true, "e1000": true, "rtl8139": true, "vmxnet3": true}

func diskKeys() []string {
	var k []string
	for _, b := range []struct {
		p string
		n int
	}{{"scsi", 31}, {"virtio", 16}, {"sata", 6}, {"ide", 4}} {
		for i := 0; i < b.n; i++ {
			k = append(k, b.p+strconv.Itoa(i))
		}
	}
	return k
}

// ParsePVEConfig reads "key: value" lines of the current config (snapshot
// sections in [brackets] are ignored).
func ParsePVEConfig(s string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			break
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, ":"); ok {
			m[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return m
}

func cleanName(s string) string {
	s = strings.Trim(reName.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > 49 {
		s = strings.Trim(s[:49], "-")
	}
	if s != "" && (s[0] < 'a' || s[0] > 'z') {
		s = "vm-" + s
	}
	return s
}

func firstOf(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// QemuImg runs qemu-img; tests replace it.
var QemuImg = func(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "qemu-img", args...).Output()
}

func inspectDisk(path string) (Plan, error) {
	out, err := QemuImg(context.Background(), "info", "--output=json", path)
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return Plan{}, fmt.Errorf("qemu-img can't read %s: %s", filepath.Base(path), strings.TrimSpace(string(ee.Stderr)))
		}
		return Plan{}, fmt.Errorf("qemu-img: %w (install qemu-utils)", err)
	}
	var info struct {
		Format      string `json:"format"`
		VirtualSize int64  `json:"virtual-size"`
		Backing     string `json:"backing-filename"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return Plan{}, err
	}
	if info.Backing != "" {
		return Plan{}, fmt.Errorf("%s depends on another image (%s); merge it first: qemu-img convert -O qcow2 %s merged.qcow2", filepath.Base(path), info.Backing, filepath.Base(path))
	}
	base := filepath.Base(path)
	p := Plan{File: base, Kind: KindDisk, Format: info.Format, VCPUs: 2, MemoryMiB: 2048, OSType: "linux", Firmware: "bios",
		DiskBus: "virtio", NICModel: "virtio", DiskGB: int((info.VirtualSize + (1 << 30) - 1) >> 30), Warnings: []string{}}
	p.Name = cleanName(strings.TrimSuffix(base, filepath.Ext(base)))
	if p.Name == "" {
		p.Name = "imported"
	}
	p.SourceInfo = fmt.Sprintf("%s image, %d GB disk", info.Format, p.DiskGB)
	p.Warnings = append(p.Warnings, "a disk image has no machine settings: check CPUs, RAM, OS type and firmware (UEFI or BIOS) before importing")
	if info.Format == "vmdk" {
		p.Warnings = append(p.Warnings, "VMware disk: Linux usually boots on virtio; Windows from VMware: choose disk SATA and network e1000e (or vmxnet3), then install the virtio drivers")
	}
	return p, nil
}

// WriteDisk copies a disk image onto a block device (raw), skipping zeros.
// progress gets 0-100.
func WriteDisk(ctx context.Context, src, dev string, progress func(pct float64)) error {
	cmd := exec.CommandContext(ctx, "qemu-img", "convert", "-p", "-n", "--target-is-zero", "-O", "raw", src, dev)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("qemu-img: %w (install qemu-utils)", err)
	}
	sc := bufio.NewScanner(out)
	sc.Split(scanCR)
	re := regexp.MustCompile(`\(([0-9.]+)/100%\)`)
	for sc.Scan() {
		if m := re.FindStringSubmatch(sc.Text()); m != nil && progress != nil {
			f, _ := strconv.ParseFloat(m[1], 64)
			progress(f)
		}
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("qemu-img convert: %v %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// scanCR splits on \r or \n (qemu-img redraws its progress with \r).
func scanCR(data []byte, eof bool) (int, []byte, error) {
	for i, b := range data {
		if b == '\r' || b == '\n' {
			return i + 1, data[:i], nil
		}
	}
	if eof && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}
