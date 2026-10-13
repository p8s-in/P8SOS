package isos

import (
	"encoding/binary"
	"os"
)

// BootInfo reads an image's El Torito boot catalog: whether a VM can start
// from it with a BIOS (SeaBIOS: "windows", "linux") and/or UEFI ("windows11").
// A disc with neither — virtio-win, a data disc, a cut-off download — gives
// "Boot failed: Could not read from CDROM (code 0004)" in the VM.
func BootInfo(path string) (bios, uefi bool) {
	f, err := os.Open(path)
	if err != nil {
		return false, false
	}
	defer f.Close()
	const sector = 2048
	// sector 17: Boot Record Volume Descriptor
	brvd := make([]byte, sector)
	if _, err := f.ReadAt(brvd, 17*sector); err != nil {
		return false, false
	}
	if brvd[0] != 0 || string(brvd[1:6]) != "CD001" || string(brvd[7:30]) != "EL TORITO SPECIFICATION" {
		return false, false
	}
	cat := int64(binary.LittleEndian.Uint32(brvd[0x47:]))
	b := make([]byte, sector)
	if _, err := f.ReadAt(b, cat*sector); err != nil || b[0] != 0x01 || b[0x1e] != 0x55 || b[0x1f] != 0xAA {
		return false, false
	}
	platform := func(p byte) {
		switch p {
		case 0x00:
			bios = true
		case 0xEF:
			uefi = true
		}
	}
	plat := b[1] // validation entry: platform of the default entry
	if b[32] == 0x88 {
		platform(plat)
	}
	// section headers (0x90 more follow, 0x91 last) each with entries
	for off := 64; off+32 <= len(b); {
		h := b[off]
		if h != 0x90 && h != 0x91 {
			break
		}
		plat, n := b[off+1], int(binary.LittleEndian.Uint16(b[off+2:]))
		off += 32
		for i := 0; i < n && off+32 <= len(b); i++ {
			if b[off] == 0x88 {
				platform(plat)
			}
			off += 32
		}
		if h == 0x91 {
			break
		}
	}
	return bios, uefi
}

// Boot describes BootInfo in one word for the API: "bios+uefi", "bios",
// "uefi" or "none".
func Boot(path string) string {
	bios, uefi := BootInfo(path)
	switch {
	case bios && uefi:
		return "bios+uefi"
	case bios:
		return "bios"
	case uefi:
		return "uefi"
	}
	return "none"
}
