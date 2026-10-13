package imports

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
)

// BuildVMA writes a small VMA archive in memory, as the format describes it
// (for tests and fixtures; real archives come from Proxmox's vzdump).
// disks: device name (e.g. drive-scsi0) -> content.
func BuildVMA(configs map[string]string, names []string, disks map[string][]byte) []byte {
	be := binary.BigEndian
	uuid := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	blob := []byte{0}
	add := func(b []byte) uint32 {
		p := uint32(len(blob))
		blob = append(blob, byte(len(b)), byte(len(b)>>8))
		blob = append(blob, b...)
		return p
	}
	head := make([]byte, vmaHeaderFixed)
	copy(head, "VMA\x00")
	be.PutUint32(head[4:], 1)
	copy(head[8:], uuid[:])
	be.PutUint64(head[24:], 1_700_000_000)
	i := 0
	for name, data := range configs {
		be.PutUint32(head[2044+4*i:], add(append([]byte(name), 0)))
		be.PutUint32(head[3068+4*i:], add([]byte(data)))
		i++
	}
	for id, name := range names {
		e := head[4096+32*(id+1):]
		be.PutUint32(e[0:], add(append([]byte(name), 0)))
		be.PutUint64(e[8:], uint64(len(disks[name])))
	}
	for len(blob)%512 != 0 {
		blob = append(blob, 0)
	}
	be.PutUint32(head[48:], vmaHeaderFixed)
	be.PutUint32(head[52:], uint32(len(blob)))
	be.PutUint32(head[56:], uint32(vmaHeaderFixed+len(blob)))
	full := append(head, blob...)
	sum := md5.Sum(full)
	copy(full[32:], sum[:])

	// one entry per cluster; extents of up to 59 entries
	type ent struct {
		info uint64
		data []byte
	}
	var ents []ent
	for id, name := range names {
		d := disks[name]
		for c := 0; c*vmaCluster < len(d); c++ {
			var mask uint16
			var data []byte
			for j := 0; j < 16; j++ {
				lo := c*vmaCluster + j*vmaBlock
				if lo >= len(d) {
					break
				}
				blk := make([]byte, vmaBlock)
				copy(blk, d[lo:min(lo+vmaBlock, len(d))])
				if !allZero(blk) {
					mask |= 1 << j
					data = append(data, blk...)
				}
			}
			ents = append(ents, ent{uint64(mask)<<48 | uint64(id+1)<<32 | uint64(c), data})
		}
	}
	out := bytes.NewBuffer(full)
	for len(ents) > 0 {
		n := min(len(ents), vmaInfos)
		eh := make([]byte, vmaExtentHead)
		copy(eh, "VMAE")
		copy(eh[8:], uuid[:])
		var data []byte
		for k, e := range ents[:n] {
			be.PutUint64(eh[40+8*k:], e.info)
			data = append(data, e.data...)
		}
		be.PutUint16(eh[6:], uint16(len(data)/vmaBlock))
		s := md5.Sum(eh)
		copy(eh[24:], s[:])
		out.Write(eh)
		out.Write(data)
		ents = ents[n:]
	}
	return out.Bytes()
}
