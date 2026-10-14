package installer

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// NIC is a network port.
type NIC struct {
	Name     string `json:"name"`
	MAC      string `json:"mac"`
	Wireless bool   `json:"wireless"`
	Link     bool   `json:"link"` // cable plugged in (or associated)
	Driver   string `json:"driver,omitempty"`
}

// Label is a one-line description for menus.
func (n NIC) Label() string {
	s := n.Name + "  " + n.MAC
	if n.Wireless {
		s += "  Wi-Fi"
	} else if n.Link {
		s += "  cable plugged in"
	} else {
		s += "  no cable"
	}
	if n.Driver != "" {
		s += "  (" + n.Driver + ")"
	}
	return s
}

// ReadNICs lists the physical network ports under sysfs (/sys/class/net):
// ports with a device behind them, not bridges, bonds, VLANs or tunnels.
func ReadNICs(sysfs string) []NIC {
	ents, err := os.ReadDir(sysfs)
	if err != nil {
		return nil
	}
	read := func(p ...string) string {
		b, _ := os.ReadFile(filepath.Join(append([]string{sysfs}, p...)...))
		return strings.TrimSpace(string(b))
	}
	exists := func(p ...string) bool {
		_, err := os.Stat(filepath.Join(append([]string{sysfs}, p...)...))
		return err == nil
	}
	var out []NIC
	for _, e := range ents {
		n := e.Name()
		if n == "lo" || !exists(n, "device") {
			continue
		}
		nic := NIC{Name: n, MAC: read(n, "address"), Wireless: exists(n, "wireless") || exists(n, "phy80211"),
			Link: read(n, "carrier") == "1"}
		if l, err := os.Readlink(filepath.Join(sysfs, n, "device", "driver")); err == nil {
			nic.Driver = filepath.Base(l)
		}
		out = append(out, nic)
	}
	// wired with a cable first, then wired, then Wi-Fi
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := nicRank(out[i]), nicRank(out[j])
		if ri != rj {
			return ri < rj
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func nicRank(n NIC) int {
	switch {
	case !n.Wireless && n.Link:
		return 0
	case !n.Wireless:
		return 1
	}
	return 2
}

// PickNIC chooses the port for "auto": the first wired port with a cable,
// else the first wired port, else Wi-Fi.
func PickNIC(nics []NIC) (NIC, bool) {
	if len(nics) == 0 {
		return NIC{}, false
	}
	best := nics[0]
	for _, n := range nics[1:] {
		if nicRank(n) < nicRank(best) {
			best = n
		}
	}
	return best, true
}
