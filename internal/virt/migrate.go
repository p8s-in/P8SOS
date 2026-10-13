package virt

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// Live migration: the running VM, its memory and its disks go to another
// server over the cluster tunnel while it keeps running (libvirt + QEMU; the
// disks are mirrored over NBD). The other server's libvirt listens on its
// tunnel address only (scripts/libvirt-migrate.sh).

// MigrateCmd builds the virsh process (tests replace it).
var MigrateCmd = exec.CommandContext

// MigratableXML is the domain's definition as the other server needs it
// (running state, or the saved definition with inactive).
func (v *Virsh) MigratableXML(ctx context.Context, name string, inactive bool) ([]byte, error) {
	if inactive {
		return v.virsh(ctx, "dumpxml", "--migratable", "--inactive", name)
	}
	return v.virsh(ctx, "dumpxml", "--migratable", name)
}

var rePct = regexp.MustCompile(`Migration: \[\s*(\d+)\s*%\]`)

// MigrateArgs is the virsh command line for a live move to the server at ip.
func MigrateArgs(uri, name, ip, xmlFile, persistentFile string) []string {
	return []string{"-c", uri, "migrate", "--live", "--persistent", "--undefinesource",
		"--copy-storage-all", "--auto-converge", "--verbose",
		"--xml", xmlFile, "--persistent-xml", persistentFile,
		"--migrateuri", "tcp://" + ip, "--disks-uri", "tcp://" + ip, "--listen-address", ip,
		name, "qemu+tcp://" + ip + "/system"}
}

// Migrate moves the running domain to the server at ip (its tunnel address).
// xml / persistentXML are the definitions to use there. progress gets 0-100.
func (v *Virsh) Migrate(ctx context.Context, name, ip string, xml, persistentXML []byte, progress func(int)) error {
	dir, err := os.MkdirTemp("", "p8s-migrate-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	xf, pf := dir+"/live.xml", dir+"/persistent.xml"
	if err := os.WriteFile(xf, xml, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(pf, persistentXML, 0o600); err != nil {
		return err
	}
	cmd := MigrateCmd(ctx, "virsh", MigrateArgs(v.URI, name, ip, xf, pf)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	sc := bufio.NewScanner(out)
	sc.Split(splitCR)
	last := -1
	for sc.Scan() {
		if m := rePct.FindStringSubmatch(sc.Text()); m != nil && progress != nil {
			if p, _ := strconv.Atoi(m[1]); p != last {
				last = p
				progress(p)
			}
		}
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil { // cancelled: stop the job; the VM keeps running here
			_, _ = v.virsh(context.Background(), "domjobabort", name)
		}
		msg := strings.TrimSpace(stderr.String())
		msg = strings.TrimPrefix(msg, "error: ")
		return fmt.Errorf("live move: %s", firstLine(msg, err.Error()))
	}
	return nil
}

func firstLine(s, fallback string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(strings.TrimPrefix(l, "error: ")); l != "" {
			return l
		}
	}
	return fallback
}

// splitCR splits on \r as well as \n (virsh --verbose redraws one line).
func splitCR(data []byte, atEOF bool) (int, []byte, error) {
	for i, b := range data {
		if b == '\n' || b == '\r' {
			return i + 1, data[:i], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// RewriteXML replaces attribute values in a domain definition (disk and CD
// paths, bridge names): old -> new, as 'old' or "old".
func RewriteXML(x []byte, repl map[string]string) []byte {
	s := string(x)
	for o, n := range repl {
		if o == "" || o == n {
			continue
		}
		s = strings.ReplaceAll(s, "'"+o+"'", "'"+n+"'")
		s = strings.ReplaceAll(s, `"`+o+`"`, `"`+n+`"`)
	}
	return []byte(s)
}

// CPU is a CPU model with features, as libvirt describes it.
type CPU struct {
	Model    string   `json:"model"`
	Features []string `json:"features,omitempty"` // "+name" required, "-name" off
}

// HostCPU is this server's CPU as libvirt sees it (domcapabilities), for
// working out the common CPU of a cluster.
func (v *Virsh) HostCPU(ctx context.Context) ([]byte, error) {
	return v.virsh(ctx, "domcapabilities")
}

// CPUBaseline is the newest CPU model every given server can run
// (virsh hypervisor-cpu-baseline over their domcapabilities).
func (v *Virsh) CPUBaseline(ctx context.Context, caps [][]byte) (CPU, error) {
	f, err := os.CreateTemp("", "p8s-cpus-*.xml")
	if err != nil {
		return CPU{}, err
	}
	defer os.Remove(f.Name())
	for _, c := range caps {
		_, _ = f.Write(c)
		_, _ = f.Write([]byte("\n"))
	}
	f.Close()
	out, err := v.virsh(ctx, "hypervisor-cpu-baseline", "--migratable", f.Name())
	if err != nil {
		return CPU{}, err
	}
	return ParseCPU(out)
}

// ParseCPU reads a <cpu> element (the answer of hypervisor-cpu-baseline).
func ParseCPU(b []byte) (CPU, error) {
	var c struct {
		Model    *cpuModel `xml:"model"`
		Features []cpuFeat `xml:"feature"`
	}
	if err := xml.Unmarshal(b, &c); err != nil || c.Model == nil || strings.TrimSpace(c.Model.Name) == "" {
		return CPU{}, fmt.Errorf("no CPU model in %q", firstLine(string(b), "(empty)"))
	}
	out := CPU{Model: strings.TrimSpace(c.Model.Name)}
	for _, f := range c.Features {
		if f.Policy == "disable" || f.Policy == "forbid" {
			out.Features = append(out.Features, "-"+f.Name)
		} else {
			out.Features = append(out.Features, "+"+f.Name)
		}
	}
	return out, nil
}
