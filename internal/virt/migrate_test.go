package virt

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"p8sos/internal/testfake"
)

func TestCPUModelXML(t *testing.T) {
	s := Spec{Name: "p8s-a", DiskPath: "/d", Bridge: "b", MAC: "52:54:00:00:00:01", VCPUs: 2, MemoryMiB: 512,
		CPUModel: "Skylake-Client-IBRS", CPUFeatures: []string{"+aes", "-hle"}}
	b, err := DomainXML(s)
	if err != nil {
		t.Fatal(err)
	}
	x := string(b)
	if !strings.Contains(x, `<cpu mode="custom" match="exact" check="partial">`) || !strings.Contains(x, `<model fallback="forbid">Skylake-Client-IBRS</model>`) ||
		!strings.Contains(x, `<feature policy="disable" name="hle">`) || !strings.Contains(x, `<topology sockets="1"`) {
		t.Fatalf("xml:\n%s", x)
	}
	back, _ := ParseDomainXML(b)
	if back.CPUModel != s.CPUModel || !reflect.DeepEqual(back.CPUFeatures, s.CPUFeatures) {
		t.Fatalf("round trip %q %v", back.CPUModel, back.CPUFeatures)
	}
	s.CPUModel, s.CPUFeatures = "", nil
	b, _ = DomainXML(s)
	if !strings.Contains(string(b), `<cpu mode="host-passthrough">`) {
		t.Fatal("default must stay host-passthrough")
	}
}

func TestParseCPUAndRewrite(t *testing.T) {
	c, err := ParseCPU([]byte(`<cpu mode='custom' match='exact'>
  <model fallback='forbid'>Haswell-noTSX</model>
  <vendor>Intel</vendor>
  <feature policy='require' name='vmx'/>
  <feature policy='disable' name='rtm'/>
</cpu>`))
	if err != nil || c.Model != "Haswell-noTSX" || !reflect.DeepEqual(c.Features, []string{"+vmx", "-rtm"}) {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := ParseCPU([]byte("error: something")); err == nil {
		t.Fatal("garbage accepted")
	}
	x := RewriteXML([]byte(`<source dev='/dev/zvol/a/p8s-x-disk0'/><source bridge="br0"/><source dev='/dev/zvol/a/p8s-x-disk01'/>`),
		map[string]string{"/dev/zvol/a/p8s-x-disk0": "/dev/zvol/b/p8s-x-disk0", "br0": "br1"})
	if string(x) != `<source dev='/dev/zvol/b/p8s-x-disk0'/><source bridge="br1"/><source dev='/dev/zvol/a/p8s-x-disk01'/>` {
		t.Fatalf("rewrite %s", x)
	}
}

func TestMigrate(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "virsh")
	os.WriteFile(script, []byte("#!/bin/sh\necho \"$@\" > "+dir+"/args\nprintf 'Migration: [  10 %%]\\rMigration: [ 55 %%]\\rMigration: [100 %%]\\n'\n"+
		"case \"$*\" in *p8s-bad*) echo 'error: operation failed: job: unexpectedly failed' >&2; exit 1;; esac\n"), 0o755)
	MigrateCmd = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, script, args...)
	}
	defer func() { MigrateCmd = exec.CommandContext }()
	v := &Virsh{Run: &testfake.Runner{}, URI: "qemu:///system"}
	var got []int
	if err := v.Migrate(context.Background(), "p8s-a", "10.250.0.3", []byte("<x/>"), []byte("<y/>"), func(p int) { got = append(got, p) }); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []int{10, 55, 100}) {
		t.Fatalf("progress %v", got)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	for _, w := range []string{"migrate --live --persistent --undefinesource --copy-storage-all", "--migrateuri tcp://10.250.0.3", "--disks-uri tcp://10.250.0.3",
		"--listen-address 10.250.0.3", "p8s-a qemu+tcp://10.250.0.3/system"} {
		if !strings.Contains(string(args), w) {
			t.Errorf("args lack %q: %s", w, args)
		}
	}
	err := v.Migrate(context.Background(), "p8s-bad", "10.250.0.3", nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "unexpectedly failed") {
		t.Fatalf("failure: %v", err)
	}
}

func TestParseExecStatus(t *testing.T) {
	done, code, out, err := ParseExecStatus([]byte(`{"return":{"exited":true,"exitcode":3,"out-data":"aGVsbG8K","err-data":"b29wcwo="}}`))
	if err != nil || !done || code != 3 || out != "hello\noops\n" {
		t.Fatalf("%v %d %q %v", done, code, out, err)
	}
	if done, _, _, _ := ParseExecStatus([]byte(`{"return":{"exited":false}}`)); done {
		t.Fatal("still running")
	}
}
