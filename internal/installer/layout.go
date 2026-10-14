package installer

import (
	"fmt"
	"strings"
)

// Layout is how the chosen disks are combined.
type Layout string

const (
	Single Layout = "single" // ZFS on one disk
	Mirror Layout = "mirror" // ZFS mirror (RAID 1): every disk holds a full copy
	RAID10 Layout = "raid10" // ZFS striped mirrors (RAID 10): pairs of mirrors
	RAIDZ1 Layout = "raidz1" // ZFS RAID-Z1 (like RAID 5): survives 1 failed disk
	RAIDZ2 Layout = "raidz2" // ZFS RAID-Z2 (like RAID 6): survives 2 failed disks
	RAIDZ3 Layout = "raidz3" // ZFS RAID-Z3: survives 3 failed disks
	Ext4   Layout = "ext4"   // ext4 system partition + ZFS VM pool on the same disk
)

// Layouts in menu order.
var Layouts = []Layout{Single, Mirror, RAID10, RAIDZ1, RAIDZ2, RAIDZ3, Ext4}

// Title is the menu text.
func (l Layout) Title() string {
	switch l {
	case Single:
		return "ZFS, one disk (no redundancy)"
	case Mirror:
		return "ZFS mirror (RAID 1): every disk has a full copy"
	case RAID10:
		return "ZFS striped mirrors (RAID 10): pairs of mirrored disks"
	case RAIDZ1:
		return "ZFS RAID-Z1 (like RAID 5): survives 1 failed disk"
	case RAIDZ2:
		return "ZFS RAID-Z2 (like RAID 6): survives 2 failed disks"
	case RAIDZ3:
		return "ZFS RAID-Z3: survives 3 failed disks"
	case Ext4:
		return "ext4 system + ZFS VM storage, one disk (classic)"
	}
	return string(l)
}

// ZFSRoot reports whether the system itself lives on ZFS.
func (l Layout) ZFSRoot() bool { return l != Ext4 }

// MinDisks is how many disks the layout needs.
func (l Layout) MinDisks() int {
	switch l {
	case Mirror:
		return 2
	case RAIDZ1:
		return 3
	case RAID10, RAIDZ2:
		return 4
	case RAIDZ3:
		return 5
	}
	return 1
}

// Survives is how many failed disks the pool survives (for RAID 10: at least
// one; more if they are in different pairs).
func (l Layout) Survives(n int) int {
	switch l {
	case Mirror:
		return n - 1
	case RAID10:
		return 1
	case RAIDZ1:
		return 1
	case RAIDZ2:
		return 2
	case RAIDZ3:
		return 3
	}
	return 0
}

// Check reports whether n disks fit the layout.
func (l Layout) Check(n int) error {
	switch l {
	case Single, Ext4:
		if n != 1 {
			return fmt.Errorf("%s uses exactly one disk; pick one disk or a RAID layout", l.Title())
		}
	case Mirror, RAID10, RAIDZ1, RAIDZ2, RAIDZ3:
		if n < l.MinDisks() {
			return fmt.Errorf("%s needs at least %d disks (you picked %d)", strings.SplitN(l.Title(), ":", 2)[0], l.MinDisks(), n)
		}
		if l == RAID10 && n%2 != 0 {
			return fmt.Errorf("RAID 10 needs an even number of disks (you picked %d)", n)
		}
	default:
		return fmt.Errorf("unknown layout %q (use %s)", l, joinLayouts())
	}
	return nil
}

func joinLayouts() string {
	var s []string
	for _, l := range Layouts {
		s = append(s, string(l))
	}
	return strings.Join(s, ", ")
}

// Fits lists the layouts that work with n disks.
func Fits(n int) []Layout {
	var out []Layout
	for _, l := range Layouts {
		if l.Check(n) == nil {
			out = append(out, l)
		}
	}
	return out
}

// Usable estimates the pool's space for disks of these sizes: every disk
// counts as the smallest one, minus the parity or mirror copies.
func (l Layout) Usable(sizes []int64) int64 {
	if len(sizes) == 0 {
		return 0
	}
	min := sizes[0]
	for _, s := range sizes {
		if s < min {
			min = s
		}
	}
	n := int64(len(sizes))
	switch l {
	case Mirror:
		return min
	case RAID10:
		return min * (n / 2)
	case RAIDZ1:
		return min * (n - 1)
	case RAIDZ2:
		return min * (n - 2)
	case RAIDZ3:
		return min * (n - 3)
	}
	return min * n
}

// vdevs is the pool layout for zpool create: "mirror a b", "mirror a b mirror c d", "raidz2 a b c d".
func (l Layout) vdevs(parts []string) []string {
	switch l {
	case Mirror:
		return append([]string{"mirror"}, parts...)
	case RAID10:
		var v []string
		for i := 0; i+1 < len(parts); i += 2 {
			v = append(v, "mirror", parts[i], parts[i+1])
		}
		return v
	case RAIDZ1, RAIDZ2, RAIDZ3:
		return append([]string{string(l)}, parts...)
	}
	return parts
}
