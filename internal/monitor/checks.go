package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func sev(v, warn, crit int) string {
	switch {
	case crit > 0 && v >= crit:
		return Critical
	case warn > 0 && v >= warn:
		return Warning
	}
	return "ok"
}

// ZFS pools: health and how full they are.
func (m *Monitor) checkPools(ctx context.Context, s Settings) []finding {
	if m.Run == nil {
		return nil
	}
	out, err := m.Run.Run(ctx, "zpool", "list", "-H", "-p", "-o", "name,health,capacity")
	if err != nil {
		return nil // no ZFS here
	}
	var fs []finding
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		name, health := f[0], f[1]
		capPct, _ := strconv.Atoi(strings.TrimSuffix(f[2], "%"))
		st := "ok"
		switch health {
		case "ONLINE":
		case "DEGRADED":
			st = Critical
		default: // FAULTED, UNAVAIL, SUSPENDED, OFFLINE
			st = Critical
		}
		fs = append(fs, finding{key: "pool:" + name, name: "ZFS pool " + name, status: st, value: health,
			title:  fmt.Sprintf("ZFS pool %s is %s", name, health),
			detail: fmt.Sprintf("Pool %s reports %s. A disk has failed or is missing: replace it soon (zpool status %s shows which). With a second failure data can be lost.", name, health, name)})
		cs := sev(capPct, s.PoolWarnPct, s.PoolCritPct)
		fs = append(fs, finding{key: "poolfull:" + name, name: "ZFS pool " + name + " space", status: cs, value: fmt.Sprintf("%d%% full", capPct),
			title:  fmt.Sprintf("ZFS pool %s is %d%% full", name, capPct),
			detail: fmt.Sprintf("Pool %s is %d%% full. ZFS slows down above 80%% and VMs stop writing when it is full: delete old snapshots/backups or add disks.", name, capPct)})
	}
	return fs
}

// Free space of the system disk (and other watched paths).
func (m *Monitor) checkSpace(s Settings) []finding {
	paths := m.Paths
	if len(paths) == 0 {
		paths = []string{"/"}
	}
	var fs []finding
	for _, p := range paths {
		var st syscall.Statfs_t
		if syscall.Statfs(m.path(p), &st) != nil || st.Blocks == 0 {
			continue
		}
		used := int(100 - st.Bavail*100/st.Blocks)
		freeGB := float64(st.Bavail) * float64(st.Bsize) / 1e9
		crit := min(s.DiskWarnPct+10, 97)
		fs = append(fs, finding{key: "space:" + p, name: "Free space " + p, status: sev(used, s.DiskWarnPct, crit),
			value: fmt.Sprintf("%d%% used, %.1f GB free", used, freeGB),
			title: fmt.Sprintf("%s is %d%% full", p, used),
			detail: fmt.Sprintf("Only %.1f GB free on %s. When it is full, logs, updates and P8S itself stop working: "+
				"remove old files from the import folder, old ISOs or local backups.", freeGB, p)})
	}
	return fs
}

func (m *Monitor) checkMemory(s Settings) []finding {
	if m.MemUsedPct == nil {
		return nil
	}
	pct := m.MemUsedPct()
	return []finding{{key: "memory", name: "Memory", status: sev(pct, s.MemWarnPct, 98), value: fmt.Sprintf("%d%% used", pct),
		title:  fmt.Sprintf("memory is %d%% used", pct),
		detail: fmt.Sprintf("%d%% of the RAM is really in use (caches not counted). Linux may start killing VMs when it runs out: give VMs less RAM or move some to another server.", pct)}}
}

// readInt reads a number from a sysfs file.
func (m *Monitor) readInt(p string) (int64, bool) {
	b, err := os.ReadFile(m.path(p))
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return n, err == nil
}

func (m *Monitor) readStr(p string) string {
	b, _ := os.ReadFile(m.path(p))
	return strings.TrimSpace(string(b))
}

// CPU temperature from hwmon (coretemp, k10temp, ...), else thermal zones.
func (m *Monitor) checkTemps(s Settings) (float64, []finding) {
	cpu := map[string]bool{"coretemp": true, "k10temp": true, "zenpower": true, "cpu_thermal": true, "acpitz": true, "x86_pkg_temp": true}
	var max float64
	found := false
	dirs, _ := filepath.Glob(m.path("/sys/class/hwmon/hwmon*"))
	for _, d := range dirs {
		rel, _ := filepath.Rel(m.path("/"), d)
		if !cpu[m.readStr("/"+rel+"/name")] {
			continue
		}
		inputs, _ := filepath.Glob(filepath.Join(d, "temp*_input"))
		for _, in := range inputs {
			r, _ := filepath.Rel(m.path("/"), in)
			if v, ok := m.readInt("/" + r); ok && v > 0 {
				found = true
				max = maxf(max, float64(v)/1000)
			}
		}
	}
	if !found {
		zones, _ := filepath.Glob(m.path("/sys/class/thermal/thermal_zone*"))
		for _, z := range zones {
			r, _ := filepath.Rel(m.path("/"), z)
			if t := m.readStr("/" + r + "/type"); t != "x86_pkg_temp" && t != "acpitz" && !strings.Contains(t, "cpu") {
				continue
			}
			if v, ok := m.readInt("/" + r + "/temp"); ok && v > 0 {
				found = true
				max = maxf(max, float64(v)/1000)
			}
		}
	}
	if !found {
		return 0, nil
	}
	st := "ok"
	if max >= float64(s.CPUTempWarnC) {
		st = Warning
	}
	if max >= float64(s.CPUTempWarnC+10) || (s.ThermalShutdownC > 0 && max >= float64(s.ThermalShutdownC)) {
		st = Critical
	}
	return max, []finding{{key: "cputemp", name: "CPU temperature", status: st, value: fmt.Sprintf("%.0f °C", max),
		title:  fmt.Sprintf("the CPU is at %.0f °C", max),
		detail: fmt.Sprintf("The CPU is %.0f °C hot. Check the room temperature, the air flow and the fans (dust).", max)}}
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// VMs that crashed.
func (m *Monitor) checkGuests(ctx context.Context) []finding {
	if m.Guests == nil {
		return nil
	}
	var fs []finding
	n := 0
	for _, g := range m.Guests(ctx) {
		n++
		if g.State == "crashed" {
			fs = append(fs, finding{key: "vm:" + g.Name, status: Critical, title: "VM " + g.Name + " crashed",
				detail: "VM " + g.Name + " crashed. Open its console or log (journalctl / /var/log/libvirt/qemu) and start it again."})
		}
	}
	fs = append(fs, finding{key: "vms", name: "VMs", status: map[bool]string{true: Critical, false: "ok"}[len(fs) > 0],
		value: fmt.Sprintf("%d, %d crashed", n, len(fs)), display: true})
	return fs
}

// Backups and restores that failed (kept as a warning for a day).
func (m *Monitor) checkJobs() []finding {
	if m.FailedJobs == nil {
		return nil
	}
	l := m.FailedJobs(m.now().Add(-24 * time.Hour))
	if len(l) == 0 {
		return []finding{{key: "jobs", name: "Backups (last 24 h)", status: "ok", value: "no failures"}}
	}
	return []finding{{key: "jobs", name: "Backups (last 24 h)", status: Warning, value: fmt.Sprintf("%d failed", len(l)),
		title:  fmt.Sprintf("%d backup job(s) failed", len(l)),
		detail: "Failed in the last 24 hours:\n- " + strings.Join(l, "\n- ")}}
}

type powerState struct {
	critical bool
	why      string
}

// Power: a UPS through NUT (upsc), or the laptop's own battery.
func (m *Monitor) checkPower(ctx context.Context, s Settings) (powerState, []finding) {
	if s.UPS != "" && m.Run != nil {
		out, err := m.Run.Run(ctx, "upsc", s.UPS)
		if err != nil {
			return powerState{}, []finding{{key: "power", name: "UPS " + s.UPS, status: Warning, value: "no answer",
				title: "the UPS " + s.UPS + " doesn't answer", detail: "upsc " + s.UPS + " failed: is NUT running and the USB cable plugged in? (" + strings.TrimSpace(err.Error()) + ")"}}
		}
		v := map[string]string{}
		for _, l := range strings.Split(string(out), "\n") {
			if k, val, ok := strings.Cut(l, ":"); ok {
				v[strings.TrimSpace(k)] = strings.TrimSpace(val)
			}
		}
		status := " " + v["ups.status"] + " "
		charge := v["battery.charge"]
		value := strings.TrimSpace(status) + ", battery " + charge + "%"
		switch {
		case strings.Contains(status, " LB ") || strings.Contains(status, " FSD "):
			return powerState{critical: true, why: "the UPS battery is almost empty"}, []finding{{key: "power", name: "UPS", status: Critical, value: value,
				title: "UPS battery almost empty", detail: "The power is off and the UPS battery is nearly empty (" + charge + "%)."}}
		case strings.Contains(status, " OB "):
			return powerState{}, []finding{{key: "power", name: "UPS", status: Warning, value: value,
				title: "power cut: running on the UPS battery (" + charge + "%)", detail: "The server runs on the UPS battery. Runtime left: " + v["battery.runtime"] + " s."}}
		case strings.Contains(status, " RB "):
			return powerState{}, []finding{{key: "power", name: "UPS", status: Warning, value: value,
				title: "the UPS battery needs replacing", detail: "The UPS reports its battery should be replaced (RB)."}}
		}
		return powerState{}, []finding{{key: "power", name: "UPS", status: "ok", value: value}}
	}
	// laptop battery
	bats, _ := filepath.Glob(m.path("/sys/class/power_supply/BAT*"))
	if len(bats) == 0 {
		return powerState{}, nil
	}
	r, _ := filepath.Rel(m.path("/"), bats[0])
	b := "/" + r
	pct, ok := m.readInt(b + "/capacity")
	if !ok {
		return powerState{}, nil
	}
	status := m.readStr(b + "/status") // Charging, Discharging, Full, Not charging
	value := fmt.Sprintf("battery %d%%, %s", pct, strings.ToLower(status))
	if status != "Discharging" {
		return powerState{}, []finding{{key: "power", name: "Power (battery)", status: "ok", value: value}}
	}
	if int(pct) <= s.BatteryShutdownPct {
		return powerState{critical: true, why: fmt.Sprintf("the battery is at %d%%", pct)}, []finding{{key: "power", name: "Power (battery)", status: Critical, value: value,
			title: fmt.Sprintf("power cut: battery at %d%%", pct), detail: fmt.Sprintf("The server runs on its battery, now at %d%%.", pct)}}
	}
	return powerState{}, []finding{{key: "power", name: "Power (battery)", status: Warning, value: value,
		title:  fmt.Sprintf("power cut: running on the battery (%d%%)", pct),
		detail: fmt.Sprintf("The server is unplugged or the power is off; battery %d%%. Below %d%% P8S shuts everything down cleanly.", pct, s.BatteryShutdownPct)}}
}

// SMART: disk health, bad sectors, wear and temperature (smartctl, every 30 min).
func (m *Monitor) checkSmart(ctx context.Context, s Settings) []finding {
	if m.Run == nil {
		return nil
	}
	out, err := m.Run.Run(ctx, "smartctl", "--scan", "-j")
	if err != nil && len(out) == 0 {
		return []finding{{key: "smart", name: "Disk health (SMART)", status: "ok", value: "smartmontools not installed"}}
	}
	var scan struct {
		Devices []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"devices"`
	}
	_ = json.Unmarshal(out, &scan)
	var fs []finding
	for _, d := range scan.Devices {
		args := []string{"-j", "-H", "-A", "-i", d.Name}
		if d.Type != "" {
			args = append([]string{"-d", d.Type}, args...)
		}
		out, _ := m.Run.Run(ctx, "smartctl", args...) // exit status is a bit mask; the JSON says what happened
		var r struct {
			Model  string `json:"model_name"`
			Serial string `json:"serial_number"`
			Status *struct {
				Passed bool `json:"passed"`
			} `json:"smart_status"`
			Temp struct {
				Current int `json:"current"`
			} `json:"temperature"`
			ATA struct {
				Table []struct {
					ID  int `json:"id"`
					Raw struct {
						Value int64 `json:"value"`
					} `json:"raw"`
				} `json:"table"`
			} `json:"ata_smart_attributes"`
			NVMe *struct {
				Critical    int   `json:"critical_warning"`
				PercentUsed int   `json:"percentage_used"`
				MediaErrors int64 `json:"media_errors"`
			} `json:"nvme_smart_health_information_log"`
		}
		if json.Unmarshal(out, &r) != nil || r.Status == nil {
			continue // USB sticks, virtual disks: no SMART
		}
		who := d.Name
		if r.Model != "" {
			who += " (" + r.Model + ")"
		}
		st, problems := "ok", []string{}
		if !r.Status.Passed {
			st = Critical
			problems = append(problems, "the disk says it is FAILING")
		}
		for _, a := range r.ATA.Table {
			switch a.ID {
			case 5:
				if a.Raw.Value > 0 {
					problems = append(problems, fmt.Sprintf("%d reallocated sectors", a.Raw.Value))
				}
			case 197:
				if a.Raw.Value > 0 {
					problems = append(problems, fmt.Sprintf("%d sectors waiting to be reallocated", a.Raw.Value))
				}
			case 198:
				if a.Raw.Value > 0 {
					problems = append(problems, fmt.Sprintf("%d unreadable sectors", a.Raw.Value))
				}
			}
		}
		if n := r.NVMe; n != nil {
			if n.Critical != 0 {
				st = Critical
				problems = append(problems, fmt.Sprintf("critical warning flags %#x", n.Critical))
			}
			if n.MediaErrors > 0 {
				problems = append(problems, fmt.Sprintf("%d media errors", n.MediaErrors))
			}
			if n.PercentUsed >= 90 {
				problems = append(problems, fmt.Sprintf("%d%% of its write life used", n.PercentUsed))
			}
		}
		if r.Temp.Current >= s.DiskTempWarn {
			problems = append(problems, fmt.Sprintf("%d °C hot", r.Temp.Current))
		}
		if len(problems) > 0 && st == "ok" {
			st = Warning
		}
		value := "healthy"
		if r.Temp.Current > 0 {
			value += fmt.Sprintf(", %d °C", r.Temp.Current)
		}
		if len(problems) > 0 {
			value = strings.Join(problems, ", ")
		}
		fs = append(fs, finding{key: "smart:" + d.Name, name: "Disk " + who, status: st, value: value,
			title: "disk " + who + ": " + strings.Join(problems, ", "),
			detail: "SMART reports for " + who + " (serial " + r.Serial + "): " + strings.Join(problems, ", ") +
				". Copy important data off and plan a replacement; with a ZFS mirror replace it with zpool replace."})
	}
	return fs
}

// checkClock: the clock must be right (NTP) for logs, TLS and 2FA codes; CERT-In
// asks for logs with synced time. Warns after 10 minutes unsynced.
func (m *Monitor) checkClock(ctx context.Context) []finding {
	out, err := m.Run.Run(ctx, "timedatectl", "show", "-p", "NTPSynchronized", "--value")
	v := strings.TrimSpace(string(out))
	if err != nil || v == "" {
		return nil
	}
	m.mu.Lock()
	if v == "yes" {
		m.unsynced = 0
	} else {
		m.unsynced++
	}
	n := m.unsynced
	m.mu.Unlock()
	f := finding{key: "clock", name: "Clock (NTP)", status: "ok", value: "synced"}
	if v != "yes" {
		f.value = "not synced"
		if n >= 10 {
			f.status, f.title = Warning, "the clock is not synced (NTP)"
			f.detail = "Times in logs, backups and 2FA codes may be wrong. Check internet access to NTP (UDP 123) and run: timedatectl set-ntp true"
		}
	}
	return []finding{f}
}
