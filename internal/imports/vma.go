// Package imports brings virtual machines from other hypervisors into P8S OS:
// Proxmox VE backups (vzdump .vma archives, also .zst/.lzo/.gz compressed)
// and plain disk images (qcow2, vmdk, vhd/vhdx, raw) from VMware, VirtualBox,
// Hyper-V, Proxmox or anywhere else.
//
// The VMA reader below is written from the published VMA format description
// (file layout only; no Proxmox code is used), so archives are read directly
// without Proxmox tools.
package imports

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"
)

const (
	vmaBlock       = 4096
	vmaCluster     = 16 * vmaBlock // 64 KiB
	vmaExtentHead  = 512
	vmaInfos       = 59 // block infos per extent
	vmaHeaderFixed = 12288
)

// Device is one disk (stream) in a VMA archive.
type Device struct {
	ID   int    `json:"id"`
	Name string `json:"name"` // e.g. drive-scsi0, drive-efidisk0
	Size int64  `json:"size"` // bytes
}

// VMA is an opened archive, positioned at the first extent.
type VMA struct {
	r       *bufio.Reader
	uuid    [16]byte
	Created time.Time
	Configs map[string][]byte // qemu-server.conf, qemu-server.fw, ...
	Devices []Device
}

// ErrNotVMA: the stream isn't a VMA archive.
var ErrNotVMA = errors.New("not a Proxmox VMA backup")

// OpenVMA reads and checks the header.
func OpenVMA(r io.Reader) (*VMA, error) {
	br := bufio.NewReaderSize(r, 1<<20)
	head := make([]byte, vmaHeaderFixed)
	if _, err := io.ReadFull(br, head); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotVMA, err)
	}
	if string(head[0:4]) != "VMA\x00" {
		return nil, ErrNotVMA
	}
	be := binary.BigEndian
	if v := be.Uint32(head[4:]); v != 1 {
		return nil, fmt.Errorf("VMA version %d is not supported", v)
	}
	size := be.Uint32(head[56:])
	if size < vmaHeaderFixed || size > 64<<20 {
		return nil, fmt.Errorf("%w: bad header size %d", ErrNotVMA, size)
	}
	full := make([]byte, size)
	copy(full, head)
	if _, err := io.ReadFull(br, full[vmaHeaderFixed:]); err != nil {
		return nil, fmt.Errorf("VMA header: %w", err)
	}
	// MD5 over the whole header with the md5 field zeroed
	var want [16]byte
	copy(want[:], full[32:48])
	clear(full[32:48])
	if md5.Sum(full) != want {
		return nil, errors.New("VMA header checksum is wrong (damaged file?)")
	}
	v := &VMA{r: br, Configs: map[string][]byte{}}
	copy(v.uuid[:], full[8:24])
	v.Created = time.Unix(int64(be.Uint64(full[24:])), 0).UTC()

	// blob buffer: 1 reserved byte, then [2-byte little-endian length][data]...
	off, bsize := be.Uint32(full[48:]), be.Uint32(full[52:])
	if off < vmaHeaderFixed || uint64(off)+uint64(bsize) > uint64(size) {
		return nil, fmt.Errorf("%w: bad blob buffer", ErrNotVMA)
	}
	blobs := map[uint32][]byte{}
	for p := off + 1; p+2 <= off+bsize; {
		n := uint32(full[p]) | uint32(full[p+1])<<8
		if p+2+n > off+bsize {
			break
		}
		blobs[p-off] = full[p+2 : p+2+n]
		p += 2 + n
	}
	str := func(ptr uint32) (string, bool) {
		b, ok := blobs[ptr]
		if !ok || len(b) == 0 || b[len(b)-1] != 0 {
			return "", false
		}
		return string(b[:len(b)-1]), true
	}
	for i := 0; i < 256; i++ {
		np, dp := be.Uint32(full[2044+4*i:]), be.Uint32(full[3068+4*i:])
		if np == 0 || dp == 0 {
			continue
		}
		name, ok := str(np)
		data, ok2 := blobs[dp]
		if ok && ok2 {
			v.Configs[name] = bytes.Clone(data)
		}
	}
	for i := 1; i < 256; i++ {
		e := full[4096+32*i:]
		sz := int64(be.Uint64(e[8:]))
		name, ok := str(be.Uint32(e[0:]))
		if sz > 0 && ok {
			v.Devices = append(v.Devices, Device{ID: i, Name: name, Size: sz})
		}
	}
	return v, nil
}

// Total is the sum of all device sizes (for progress).
func (v *VMA) Total() int64 {
	var t int64
	for _, d := range v.Devices {
		if d.Name != "vmstate" {
			t += d.Size
		}
	}
	return t
}

// Extract writes the disks' data to targets (by device ID; devices without a
// target are read past). Zero blocks are not written: targets must start out
// zeroed (a new ZFS volume does). progress gets bytes of disk covered so far.
func (v *VMA) Extract(targets map[int]io.WriterAt, progress func(done int64)) error {
	be := binary.BigEndian
	sizes := map[int]int64{}
	for _, d := range v.Devices {
		sizes[d.ID] = d.Size
	}
	head := make([]byte, vmaExtentHead)
	data := make([]byte, vmaInfos*vmaCluster)
	var done int64
	for {
		if _, err := io.ReadFull(v.r, head); err == io.EOF {
			return nil
		} else if err != nil {
			return fmt.Errorf("VMA data: %w (file cut off?)", err)
		}
		if string(head[0:4]) != "VMAE" || head[4] != 0 || head[5] != 0 {
			return errors.New("VMA extent header is wrong (damaged file?)")
		}
		var want [16]byte
		copy(want[:], head[24:40])
		clear(head[24:40])
		if md5.Sum(head) != want {
			return errors.New("VMA extent checksum is wrong (damaged file?)")
		}
		if !bytes.Equal(head[8:24], v.uuid[:]) {
			return errors.New("VMA extent belongs to another archive")
		}
		blocks := int(be.Uint16(head[6:]))
		if blocks > vmaInfos*16 {
			return errors.New("VMA extent too large")
		}
		buf := data[:blocks*vmaBlock]
		if _, err := io.ReadFull(v.r, buf); err != nil {
			return fmt.Errorf("VMA data: %w (file cut off?)", err)
		}
		pos := 0
		for i := 0; i < vmaInfos; i++ {
			bi := be.Uint64(head[40+8*i:])
			dev := int(bi>>32) & 0xff
			if dev == 0 {
				continue
			}
			cluster := int64(bi & 0xffffffff)
			mask := uint16(bi >> 48)
			size, known := sizes[dev]
			if !known {
				return fmt.Errorf("VMA data for unknown device %d", dev)
			}
			w := targets[dev]
			base := cluster * vmaCluster
			for j := 0; j < 16; j++ {
				if mask&(1<<j) == 0 {
					continue
				}
				if pos+vmaBlock > len(buf) {
					return errors.New("VMA extent is short (damaged file?)")
				}
				at := base + int64(j)*vmaBlock
				if w != nil && at < size {
					chunk := buf[pos : pos+vmaBlock]
					if at+vmaBlock > size {
						chunk = chunk[:size-at]
					}
					if !allZero(chunk) {
						if _, err := w.WriteAt(chunk, at); err != nil {
							return fmt.Errorf("write disk: %w", err)
						}
					}
				}
				pos += vmaBlock
			}
			if w != nil {
				done += vmaCluster
				if progress != nil {
					progress(done)
				}
			}
		}
	}
}

func allZero(b []byte) bool {
	for len(b) >= 8 {
		if binary.LittleEndian.Uint64(b) != 0 {
			return false
		}
		b = b[8:]
	}
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
