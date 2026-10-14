package virt

import (
	"context"
	"fmt"
	"strconv"
)

// IOLimits caps how fast a VM may use its disks and network (like Proxmox's
// disk "bandwidth/IOPS" and NIC "rate" options), so one busy VM can't slow
// the others down. Zero = no limit. Each disk and each network card gets the
// limit on its own.
type IOLimits struct {
	ReadMBs     int `json:"read_mbs,omitempty"`      // disk reads, MB/s
	WriteMBs    int `json:"write_mbs,omitempty"`     // disk writes, MB/s
	IOPS        int `json:"iops,omitempty"`          // disk operations per second (reads + writes)
	NetDownMbit int `json:"net_down_mbit,omitempty"` // network into the VM, Mbit/s
	NetUpMbit   int `json:"net_up_mbit,omitempty"`   // network out of the VM, Mbit/s
}

// Check keeps the limits sensible (a typo like 1 MB/s would make a VM unusable).
func (l IOLimits) Check() error {
	for _, c := range []struct {
		v, min, max int
		what        string
	}{{l.ReadMBs, 5, 100000, "disk read MB/s"}, {l.WriteMBs, 5, 100000, "disk write MB/s"}, {l.IOPS, 100, 10000000, "disk IOPS"},
		{l.NetDownMbit, 1, 400000, "network download Mbit/s"}, {l.NetUpMbit, 1, 400000, "network upload Mbit/s"}} {
		if c.v != 0 && (c.v < c.min || c.v > c.max) {
			return fmt.Errorf("%s must be 0 (no limit) or %d-%d", c.what, c.min, c.max)
		}
	}
	return nil
}

func (l IOLimits) Zero() bool { return l == IOLimits{} }

type ioTuneXML struct {
	ReadBytes  int64 `xml:"read_bytes_sec,omitempty"`
	WriteBytes int64 `xml:"write_bytes_sec,omitempty"`
	TotalIOPS  int64 `xml:"total_iops_sec,omitempty"`
}

type bwXML struct {
	Average int `xml:"average,attr"` // KB/s
}

type bandwidthXML struct {
	Inbound  *bwXML `xml:"inbound"`
	Outbound *bwXML `xml:"outbound"`
}

const mb = 1000 * 1000

func (l IOLimits) ioTune() *ioTuneXML {
	if l.ReadMBs == 0 && l.WriteMBs == 0 && l.IOPS == 0 {
		return nil
	}
	return &ioTuneXML{ReadBytes: int64(l.ReadMBs) * mb, WriteBytes: int64(l.WriteMBs) * mb, TotalIOPS: int64(l.IOPS)}
}

// Mbit/s -> KB/s (libvirt's unit)
func kbs(mbit int) int { return mbit * 125 }

func (l IOLimits) bandwidth() *bandwidthXML {
	if l.NetDownMbit == 0 && l.NetUpMbit == 0 {
		return nil
	}
	b := &bandwidthXML{}
	if l.NetDownMbit > 0 {
		b.Inbound = &bwXML{kbs(l.NetDownMbit)}
	}
	if l.NetUpMbit > 0 {
		b.Outbound = &bwXML{kbs(l.NetUpMbit)}
	}
	return b
}

func (l *IOLimits) read(t *ioTuneXML, b *bandwidthXML) {
	if t != nil {
		l.ReadMBs, l.WriteMBs, l.IOPS = int(t.ReadBytes/mb), int(t.WriteBytes/mb), int(t.TotalIOPS)
	}
	if b != nil {
		if b.Inbound != nil {
			l.NetDownMbit = b.Inbound.Average / 125
		}
		if b.Outbound != nil {
			l.NetUpMbit = b.Outbound.Average / 125
		}
	}
}

// SetDiskLimits changes a running VM's disk limits at once (virsh blkdeviotune --live).
func (v *Virsh) SetDiskLimits(ctx context.Context, name, target string, l IOLimits) error {
	_, err := v.virsh(ctx, "blkdeviotune", name, target, "--live",
		"--read-bytes-sec", strconv.FormatInt(int64(l.ReadMBs)*mb, 10),
		"--write-bytes-sec", strconv.FormatInt(int64(l.WriteMBs)*mb, 10),
		"--total-iops-sec", strconv.Itoa(l.IOPS))
	return err
}

// SetNICLimits changes a running VM's network card limits at once (virsh domiftune --live).
func (v *Virsh) SetNICLimits(ctx context.Context, name, mac string, l IOLimits) error {
	_, err := v.virsh(ctx, "domiftune", name, mac, "--live",
		"--inbound", strconv.Itoa(kbs(l.NetDownMbit)), "--outbound", strconv.Itoa(kbs(l.NetUpMbit)))
	return err
}

// DiskTargets are the VM's disk device names (vda, vdb …) for SetDiskLimits.
func DiskTargets(s Spec) []string {
	root := "vda" // as DomainXML: virtio vda, SATA sdc, virtio-scsi sde
	switch b := ExtraTarget(s, 1); {
	case b == "sdf" && s.DiskBus == "scsi":
		root = "sde"
	case b == "sdf":
		root = "sdc"
	}
	out := []string{root}
	for _, e := range s.Extra {
		out = append(out, ExtraTarget(s, e.Index))
	}
	return out
}

// SetVCPUs turns a running VM's vCPUs on or off (hot-plug, up to its ceiling).
func (v *Virsh) SetVCPUs(ctx context.Context, name string, n int) error {
	_, err := v.virsh(ctx, "setvcpus", name, strconv.Itoa(n), "--live")
	return err
}

// AddMemory plugs a memory module of mib MiB into a running VM.
func (v *Virsh) AddMemory(ctx context.Context, name string, mib int) error {
	return v.AttachDisk(ctx, name, []byte(fmt.Sprintf("<memory model='dimm'><target><size unit='MiB'>%d</size><node>0</node></target></memory>", mib)))
}
