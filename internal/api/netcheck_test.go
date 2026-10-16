package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"p8sos/internal/tasks"
	"p8sos/internal/testfake"
)

func TestNetCheckAPI(t *testing.T) {
	dir := t.TempDir()
	out := "==> Host internet\n  ✔ the server reaches the internet\n==> Firewall\n  ✘ the FORWARD policy is DROP\n    allow it\n==> From outside\n  · the internet sees this server as 203.0.113.5\n\n1 problem(s) found.\n"
	run := &testfake.Runner{Out: map[string]string{"/bin/bash " + filepath.Join(dir, "netcheck.sh"): out}}
	os.WriteFile(filepath.Join(dir, "netcheck.sh"), []byte("#!/bin/bash\n"), 0o755)
	m := newMultiUserCfg(t, func(s *Server) {
		s.Node = &tasks.Node{Tasks: &tasks.Manager{Dir: t.TempDir(), Scripts: dir, Run: run}}
	})
	b := m.req("GET", "/api/v1/cluster/netcheck", secret, "", 200)
	srv := b["servers"].([]any)
	if len(srv) != 1 {
		t.Fatalf("%v", b)
	}
	rep := srv[0].(map[string]any)["report"].(map[string]any)
	if rep["problems"] != 1.0 || len(rep["sections"].([]any)) != 3 {
		t.Fatalf("%v", rep)
	}
	if b := m.req("GET", "/api/v1/node/netcheck", secret, "", 200); b["summary"] != "1 problem(s) found." {
		t.Fatalf("%v", b)
	}
	m.req("POST", "/api/v1/cluster/netcheck/self/repair", secret, "", 202)
	if c := strings.Join(run.Calls, "\n"); !strings.Contains(c, "netcheck.sh") {
		t.Fatal(c)
	}
}
