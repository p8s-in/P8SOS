package vm

import (
	"context"
	"os"
	"time"

	"p8sos/internal/virt"
)

// Installer CDs of Windows (and some others) ask "Press any key to boot from
// CD or DVD..." for 5 seconds. Nobody has the console open that fast, so the
// VM falls through to an empty disk and ends on a boot error or the UEFI boot
// manager page. While a VM with an installer CD still has a blank disk, P8S
// presses Enter for it during the first seconds after start (like a person
// at the keyboard). Once anything is installed the disk has a partition table
// and nothing is pressed.

type keySender interface {
	SendKey(ctx context.Context, name string, keys ...string) error
}

// BootKeyWindow is how long after start Enter is pressed (once a second);
// installers' own screens appear later than this.
var BootKeyWindow = 15 * time.Second

var bootKeyEvery = time.Second

// blankDisk: no MBR/GPT boot signature in the first sector (a new, empty disk).
var blankDisk = func(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	b := make([]byte, 512)
	if _, err := f.ReadAt(b, 0); err != nil {
		return false
	}
	return !(b[510] == 0x55 && b[511] == 0xAA)
}

func (s *Service) pressBootKey(ctx context.Context, dom string) {
	ks, ok := s.HV.(keySender)
	if !ok {
		return
	}
	x, err := s.HV.XML(ctx, dom)
	if err != nil {
		return
	}
	sp, err := virt.ParseDomainXML(x)
	if err != nil || sp.InstallISO == "" || !blankDisk(sp.DiskPath) {
		return
	}
	go func() {
		end := time.Now().Add(BootKeyWindow)
		for time.Now().Before(end) {
			time.Sleep(bootKeyEvery)
			if st, err := s.HV.State(ctx, dom); err != nil || st != virt.Running {
				return
			}
			_ = ks.SendKey(ctx, dom, "KEY_ENTER")
		}
	}()
}
