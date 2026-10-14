package imports

import (
	"archive/tar"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// OVA files (VMware, VirtualBox, appliances): a tar with an .ovf description
// and the disks (.vmdk). The machine settings come from the OVF; every disk
// is imported, the first as the system disk.

// KindOVA is an .ova appliance.
const KindOVA = "ova"

// OVADisk is one disk of an appliance.
type OVADisk struct {
	Href  string `json:"href"`  // file name inside the .ova
	Bytes int64  `json:"bytes"` // its capacity (virtual size)
}

type ovfEnvelope struct {
	References struct {
		Files []struct {
			ID   string `xml:"id,attr"`
			Href string `xml:"href,attr"`
		} `xml:"File"`
	} `xml:"References"`
	DiskSection struct {
		Disks []struct {
			DiskID   string `xml:"diskId,attr"`
			FileRef  string `xml:"fileRef,attr"`
			Capacity string `xml:"capacity,attr"`
			Units    string `xml:"capacityAllocationUnits,attr"`
		} `xml:"Disk"`
	} `xml:"DiskSection"`
	System struct {
		ID   string `xml:"id,attr"`
		Name string `xml:"Name"`
		OS   struct {
			ID     string `xml:"id,attr"`
			OSType string `xml:"osType,attr"`
			Desc   string `xml:"Description"`
		} `xml:"OperatingSystemSection"`
		Hardware struct {
			Items   []ovfItem `xml:"Item"`
			Configs []struct {
				Key   string `xml:"key,attr"`
				Value string `xml:"value,attr"`
			} `xml:"Config"`
		} `xml:"VirtualHardwareSection"`
	} `xml:"VirtualSystem"`
}

type ovfItem struct {
	ResourceType    int    `xml:"ResourceType"`
	ResourceSubType string `xml:"ResourceSubType"`
	VirtualQuantity int64  `xml:"VirtualQuantity"`
	AllocationUnits string `xml:"AllocationUnits"`
	HostResource    string `xml:"HostResource"`
	Address         string `xml:"Address"`
	ElementName     string `xml:"ElementName"`
}

// units turns OVF allocation units ("byte * 2^20", "MegaBytes", "GB") into bytes per unit.
func units(u string) int64 {
	u = strings.ToLower(strings.ReplaceAll(u, " ", ""))
	switch {
	case u == "" || u == "byte" || u == "bytes":
		return 1
	case strings.HasPrefix(u, "byte*2^"):
		n, err := strconv.Atoi(strings.TrimPrefix(u, "byte*2^"))
		if err == nil && n >= 0 && n < 63 {
			return 1 << n
		}
	case strings.HasPrefix(u, "kilo") || u == "kb":
		return 1 << 10
	case strings.HasPrefix(u, "mega") || u == "mb":
		return 1 << 20
	case strings.HasPrefix(u, "giga") || u == "gb":
		return 1 << 30
	}
	return 1
}

// PlanFromOVF maps an OVF description to P8S settings.
func PlanFromOVF(b []byte) (Plan, error) {
	var e ovfEnvelope
	if err := xml.Unmarshal(b, &e); err != nil {
		return Plan{}, fmt.Errorf("the .ovf description can't be read: %w", err)
	}
	p := Plan{Kind: KindOVA, VCPUs: 1, MemoryMiB: 1024, OSType: "linux", Firmware: "bios", DiskBus: "virtio", NICModel: "virtio", Warnings: []string{}}
	p.Name = cleanName(firstOf(e.System.Name, e.System.ID))
	if p.Name == "" {
		p.Name = "imported"
	}
	files := map[string]string{}
	for _, f := range e.References.Files {
		files[f.ID] = f.Href
	}
	disks := map[string]OVADisk{}
	var order []string
	for _, d := range e.DiskSection.Disks {
		n, _ := strconv.ParseInt(d.Capacity, 10, 64)
		disks[d.DiskID] = OVADisk{Href: files[d.FileRef], Bytes: n * units(d.Units)}
		order = append(order, d.DiskID)
	}
	controllers := map[string]int{} // Item address/ElementName isn't reliable; remember the kinds seen
	for _, it := range e.System.Hardware.Items {
		switch it.ResourceType {
		case 3: // CPUs
			if it.VirtualQuantity > 0 {
				p.VCPUs = int(it.VirtualQuantity)
			}
		case 4: // memory
			if it.VirtualQuantity > 0 {
				p.MemoryMiB = int(it.VirtualQuantity * units(firstOf(it.AllocationUnits, "byte * 2^20")) >> 20)
			}
		case 10: // network card
			switch st := strings.ToLower(it.ResourceSubType); {
			case strings.Contains(st, "vmxnet3"):
				p.NICModel = "vmxnet3"
			case strings.Contains(st, "e1000e"):
				p.NICModel = "e1000e"
			case strings.Contains(st, "e1000"), strings.Contains(st, "pcnet"):
				p.NICModel = "e1000"
			}
		case 5, 20: // IDE, SATA controller
			controllers["sata"]++
		case 6: // SCSI controller
			controllers["scsi"]++
		}
	}
	for _, c := range e.System.Hardware.Configs {
		if c.Key == "firmware" && c.Value == "efi" {
			p.Firmware = "uefi"
		}
	}
	os := strings.ToLower(e.System.OS.OSType + " " + e.System.OS.Desc)
	if strings.Contains(os, "windows") {
		p.OSType = "windows"
		p.DiskBus, p.NICModel = "sata", "e1000e"
		if strings.Contains(os, "11") && p.Firmware == "uefi" {
			p.OSType = "windows11"
		}
		p.Warnings = append(p.Warnings, "Windows: it starts with a SATA disk and an e1000e card (drivers Windows has); install the virtio drivers later for speed")
	}
	for _, id := range order {
		if d := disks[id]; d.Href != "" {
			p.Disks = append(p.Disks, d)
		}
	}
	if len(p.Disks) == 0 {
		return Plan{}, errors.New("the appliance describes no disks")
	}
	p.DiskGB = int((p.Disks[0].Bytes + (1 << 30) - 1) >> 30)
	if len(p.Disks) > 1 {
		p.Warnings = append(p.Warnings, fmt.Sprintf("%d disks: the first is the system disk, the others become data disks", len(p.Disks)))
	}
	p.SourceInfo = fmt.Sprintf("appliance %q, %d CPUs, %d MB RAM, %d disk(s), %s", firstOf(e.System.Name, e.System.ID), p.VCPUs, p.MemoryMiB, len(p.Disks), firstOf(e.System.OS.Desc, "unknown OS"))
	return p, nil
}

// inspectOVA reads the .ovf inside an .ova.
func inspectOVA(file string) (Plan, error) {
	f, err := os.Open(file)
	if err != nil {
		return Plan{}, err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	have := map[string]bool{}
	var plan *Plan
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Plan{}, fmt.Errorf("%s isn't a valid .ova (tar) file: %w", filepath.Base(file), err)
		}
		name := path.Base(h.Name)
		have[name] = true
		if strings.HasSuffix(strings.ToLower(name), ".ovf") && plan == nil {
			b, err := io.ReadAll(io.LimitReader(tr, 4<<20))
			if err != nil {
				return Plan{}, err
			}
			p, err := PlanFromOVF(b)
			if err != nil {
				return Plan{}, err
			}
			plan = &p
		}
	}
	if plan == nil {
		return Plan{}, fmt.Errorf("%s has no .ovf description", filepath.Base(file))
	}
	for _, d := range plan.Disks {
		if !have[path.Base(d.Href)] {
			return Plan{}, fmt.Errorf("the appliance's disk %s isn't in the .ova", d.Href)
		}
	}
	plan.File = filepath.Base(file)
	return *plan, nil
}

// ExtractOVA writes the appliance's disk files into dir and returns
// href → extracted path. Only files the plan names are written.
func ExtractOVA(ctx context.Context, file string, pl Plan, dir string, progress func(string)) (map[string]string, error) {
	want := map[string]bool{}
	for _, d := range pl.Disks {
		want[path.Base(d.Href)] = true
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	tr := tar.NewReader(f)
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		name := path.Base(h.Name)
		if !want[name] || h.Typeflag != tar.TypeReg {
			continue
		}
		progress(fmt.Sprintf("unpacking %s", name))
		dst := filepath.Join(dir, name) // path.Base: never outside dir
		w, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return nil, err
		}
		if _, err := io.Copy(w, tr); err != nil {
			w.Close()
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		for _, d := range pl.Disks {
			if path.Base(d.Href) == name {
				out[d.Href] = dst
			}
		}
	}
	for _, d := range pl.Disks {
		if out[d.Href] == "" {
			return nil, fmt.Errorf("disk %s not found in the .ova", d.Href)
		}
	}
	return out, nil
}
