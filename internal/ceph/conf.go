package ceph

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

// Mon is one monitor.
type Mon struct {
	Name string `json:"name"`
	IP   string `json:"ip"`
}

// Conf is the ceph.conf every server gets. Traffic between servers and to the
// VM disks is encrypted (msgr2 secure mode); everything authenticates with keys.
func Conf(fsid string, network netip.Prefix, mons []Mon) string {
	ms := append([]Mon(nil), mons...)
	sort.Slice(ms, func(i, j int) bool { return ms[i].Name < ms[j].Name })
	var hosts, names []string
	for _, m := range ms {
		hosts = append(hosts, monAddrv(m.IP))
		names = append(names, m.Name)
	}
	return "# Written by P8S OS (Server → Ceph). Every server has the same file.\n" +
		"[global]\n" +
		"fsid = " + fsid + "\n" +
		"mon_initial_members = " + strings.Join(names, ",") + "\n" +
		"mon_host = " + strings.Join(hosts, " ") + "\n" +
		"public_network = " + network.String() + "\n" +
		"cluster_network = " + network.String() + "\n" +
		"auth_cluster_required = cephx\n" +
		"auth_service_required = cephx\n" +
		"auth_client_required = cephx\n" +
		"ms_cluster_mode = secure\n" +
		"ms_service_mode = secure\n" +
		"ms_client_mode = secure\n" +
		"ms_mon_cluster_mode = secure\n" +
		"ms_mon_service_mode = secure\n" +
		"ms_mon_client_mode = secure\n" +
		"osd_pool_default_size = 3\n" +
		"osd_pool_default_min_size = 2\n" +
		"\n[client]\n" +
		"keyring = /etc/ceph/ceph.client.admin.keyring\n" +
		"rbd_default_features = 61\n" // layering, exclusive-lock, object-map, fast-diff, deep-flatten
}

func confValue(conf, key string) string {
	for _, l := range strings.Split(conf, "\n") {
		k, v, ok := strings.Cut(l, "=")
		if ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// NetworkOf reads public_network from a ceph.conf.
func NetworkOf(conf string) (netip.Prefix, error) {
	v := confValue(conf, "public_network")
	p, err := netip.ParsePrefix(strings.SplitN(v, ",", 2)[0])
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("ceph.conf has no public_network")
	}
	return p.Masked(), nil
}

// FSIDOf reads the cluster id from a ceph.conf.
func FSIDOf(conf string) string { return confValue(conf, "fsid") }

// ParseMonDump reads `ceph mon dump -f json`: the monitors and the cluster id.
func ParseMonDump(out []byte) ([]Mon, string, error) {
	var d struct {
		FSID string `json:"fsid"`
		Mons []struct {
			Name        string `json:"name"`
			PublicAddrs struct {
				Addrvec []struct {
					Type string `json:"type"`
					Addr string `json:"addr"`
				} `json:"addrvec"`
			} `json:"public_addrs"`
			Addr string `json:"addr"`
		} `json:"mons"`
	}
	if err := json.Unmarshal(out, &d); err != nil {
		return nil, "", fmt.Errorf("reading the monitor list: %w", err)
	}
	var mons []Mon
	for _, m := range d.Mons {
		addr := m.Addr
		for _, a := range m.PublicAddrs.Addrvec {
			addr = a.Addr
			if a.Type == "v2" {
				break
			}
		}
		addr = strings.SplitN(addr, "/", 2)[0]
		ap, err := netip.ParseAddrPort(addr)
		if err != nil {
			continue
		}
		mons = append(mons, Mon{Name: m.Name, IP: ap.Addr().String()})
	}
	return mons, d.FSID, nil
}

// Status is the Ceph cluster at a glance.
type Status struct {
	Health    string    `json:"health"`   // HEALTH_OK, HEALTH_WARN, HEALTH_ERR
	Problems  []string  `json:"problems"` // "1/3 mons down, quorum n1,n2"
	FSID      string    `json:"fsid"`
	Mons      int       `json:"mons"`
	Quorum    []string  `json:"quorum"`
	MgrUp     bool      `json:"mgr_up"`
	MgrSpare  int       `json:"mgr_spare"`
	OSDCount  int       `json:"osd_count"`
	OSDUp     int       `json:"osd_up"`
	OSDIn     int       `json:"osd_in"`
	PGs       int       `json:"pgs"`
	PGStates  []string  `json:"pg_states"` // "33 active+clean"
	UsedBytes int64     `json:"used_bytes"`
	FreeBytes int64     `json:"free_bytes"`
	SizeBytes int64     `json:"size_bytes"`
	OSDs      []OSD     `json:"osds"`
	Pools     []PoolUse `json:"pools"`
}

// OSD is one disk in Ceph.
type OSD struct {
	ID    int     `json:"id"`
	Host  string  `json:"host"`
	Up    bool    `json:"up"`
	In    bool    `json:"in"`
	Class string  `json:"class,omitempty"` // hdd, ssd, nvme
	TiB   float64 `json:"tib"`
}

// PoolUse is a Ceph pool's use.
type PoolUse struct {
	Name      string  `json:"name"`
	Stored    int64   `json:"stored"`
	MaxAvail  int64   `json:"max_avail"`
	UsedRatio float64 `json:"used_ratio"`
}

// ParseStatus reads `ceph status -f json`.
func ParseStatus(out []byte) (Status, error) {
	var d struct {
		FSID   string `json:"fsid"`
		Health struct {
			Status string `json:"status"`
			Checks map[string]struct {
				Severity string `json:"severity"`
				Summary  struct {
					Message string `json:"message"`
				} `json:"summary"`
			} `json:"checks"`
		} `json:"health"`
		Quorum []string `json:"quorum_names"`
		Monmap struct {
			NumMons int `json:"num_mons"`
			Mons    []struct {
				Name string `json:"name"`
			} `json:"mons"`
		} `json:"monmap"`
		Osdmap struct {
			NumOSDs   int `json:"num_osds"`
			NumUpOSDs int `json:"num_up_osds"`
			NumInOSDs int `json:"num_in_osds"`
			// older layout
			Osdmap *struct {
				NumOSDs   int `json:"num_osds"`
				NumUpOSDs int `json:"num_up_osds"`
				NumInOSDs int `json:"num_in_osds"`
			} `json:"osdmap"`
		} `json:"osdmap"`
		Pgmap struct {
			PGsByState []struct {
				State string `json:"state_name"`
				Count int    `json:"count"`
			} `json:"pgs_by_state"`
			NumPGs     int   `json:"num_pgs"`
			BytesUsed  int64 `json:"bytes_used"`
			BytesAvail int64 `json:"bytes_avail"`
			BytesTotal int64 `json:"bytes_total"`
		} `json:"pgmap"`
		Mgrmap struct {
			Available   bool `json:"available"`
			NumStandbys int  `json:"num_standbys"`
		} `json:"mgrmap"`
	}
	if err := json.Unmarshal(out, &d); err != nil {
		return Status{}, fmt.Errorf("reading ceph status: %w", err)
	}
	s := Status{Health: d.Health.Status, FSID: d.FSID, Mons: d.Monmap.NumMons, Quorum: d.Quorum,
		MgrUp: d.Mgrmap.Available, MgrSpare: d.Mgrmap.NumStandbys,
		OSDCount: d.Osdmap.NumOSDs, OSDUp: d.Osdmap.NumUpOSDs, OSDIn: d.Osdmap.NumInOSDs,
		PGs: d.Pgmap.NumPGs, UsedBytes: d.Pgmap.BytesUsed, FreeBytes: d.Pgmap.BytesAvail, SizeBytes: d.Pgmap.BytesTotal,
		Problems: []string{}, PGStates: []string{}}
	if s.Mons == 0 {
		s.Mons = len(d.Monmap.Mons)
	}
	if o := d.Osdmap.Osdmap; o != nil && s.OSDCount == 0 {
		s.OSDCount, s.OSDUp, s.OSDIn = o.NumOSDs, o.NumUpOSDs, o.NumInOSDs
	}
	if s.Quorum == nil {
		s.Quorum = []string{}
	}
	keys := make([]string, 0, len(d.Health.Checks))
	for k := range d.Health.Checks {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		c := d.Health.Checks[k]
		sev := "warning"
		if c.Severity == "HEALTH_ERR" {
			sev = "error"
		}
		s.Problems = append(s.Problems, sev+": "+c.Summary.Message)
	}
	for _, p := range d.Pgmap.PGsByState {
		s.PGStates = append(s.PGStates, fmt.Sprintf("%d %s", p.Count, p.State))
	}
	s.OSDs, s.Pools = []OSD{}, []PoolUse{}
	return s, nil
}

// ParseOSDTree reads `ceph osd tree -f json`.
func ParseOSDTree(out []byte) []OSD {
	var d struct {
		Nodes []struct {
			ID       int     `json:"id"`
			Name     string  `json:"name"`
			Type     string  `json:"type"`
			Children []int   `json:"children"`
			Status   string  `json:"status"`
			Reweight float64 `json:"reweight"`
			Class    string  `json:"device_class"`
			Weight   float64 `json:"crush_weight"`
		} `json:"nodes"`
		Stray []struct {
			ID       int     `json:"id"`
			Status   string  `json:"status"`
			Reweight float64 `json:"reweight"`
		} `json:"stray"`
	}
	l := []OSD{}
	if json.Unmarshal(out, &d) != nil {
		return l
	}
	host := map[int]string{}
	for _, n := range d.Nodes {
		if n.Type == "host" {
			for _, c := range n.Children {
				host[c] = n.Name
			}
		}
	}
	for _, n := range d.Nodes {
		if n.Type == "osd" {
			l = append(l, OSD{ID: n.ID, Host: host[n.ID], Up: n.Status == "up", In: n.Reweight > 0, Class: n.Class, TiB: n.Weight})
		}
	}
	for _, n := range d.Stray {
		l = append(l, OSD{ID: n.ID, Up: n.Status == "up", In: n.Reweight > 0})
	}
	sort.Slice(l, func(i, j int) bool { return l[i].ID < l[j].ID })
	return l
}

// ParseDF reads `ceph df -f json`.
func ParseDF(out []byte) []PoolUse {
	var d struct {
		Pools []struct {
			Name  string `json:"name"`
			Stats struct {
				Stored      int64   `json:"stored"`
				MaxAvail    int64   `json:"max_avail"`
				PercentUsed float64 `json:"percent_used"`
			} `json:"stats"`
		} `json:"pools"`
	}
	l := []PoolUse{}
	if json.Unmarshal(out, &d) != nil {
		return l
	}
	for _, p := range d.Pools {
		l = append(l, PoolUse{Name: p.Name, Stored: p.Stats.Stored, MaxAvail: p.Stats.MaxAvail, UsedRatio: p.Stats.PercentUsed})
	}
	return l
}
