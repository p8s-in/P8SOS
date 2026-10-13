package audit

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLog(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	l := &Log{Path: filepath.Join(dir, "audit.log"), Now: func() time.Time { return now }, RetainDays: 30}
	for i := 0; i < 3; i++ {
		l.Write(Entry{User: "admin", Action: "POST /x", Status: 200})
	}
	if c, _ := l.Verify(); !c.OK || c.Entries != 3 {
		t.Fatalf("verify %+v", c)
	}
	// next day: a new file, the chain goes on
	now = now.Add(24 * time.Hour)
	l.Write(Entry{User: "bob", Action: "DELETE /y", Status: 204})
	if fs := l.files(); len(fs) != 2 {
		t.Fatalf("files %v", fs)
	}
	if r, _ := l.Recent(10, ""); len(r) != 4 || r[0].User != "bob" {
		t.Fatalf("recent %+v", r)
	}
	if r, _ := l.Recent(10, "bob"); len(r) != 1 {
		t.Fatal("by user")
	}
	// a fresh process continues the chain
	l2 := &Log{Path: l.Path, Now: l.Now, RetainDays: 30}
	l2.Write(Entry{User: "admin", Action: "POST /z"})
	if c, _ := l2.Verify(); !c.OK || c.Entries != 5 {
		t.Fatalf("verify after restart %+v", c)
	}
	var buf bytes.Buffer
	l2.WriteCSV(&buf, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), time.Time{})
	if lines := strings.Count(buf.String(), "\n"); lines != 3 || !strings.Contains(buf.String(), "DELETE /y") {
		t.Fatalf("csv %q", buf.String())
	}
	// tampering is found
	b, _ := os.ReadFile(l.Path)
	os.WriteFile(l.Path, bytes.Replace(b, []byte("DELETE /y"), []byte("DELETE /q"), 1), 0o600)
	if c, _ := l2.Verify(); c.OK || !strings.Contains(c.Problem, "changed") {
		t.Fatalf("tamper %+v", c)
	}
	os.WriteFile(l.Path, b, 0o600)
	lines := strings.SplitAfter(string(b), "\n")
	os.WriteFile(l.Path, []byte(lines[1]), 0o600) // first line of today deleted
	if c, _ := l2.Verify(); c.OK || !strings.Contains(c.Problem, "missing") {
		t.Fatalf("deleted line %+v", c)
	}
	os.WriteFile(l.Path, b, 0o600)
	// retention: rotated files older than 30 days go
	old := l.files()[0]
	if err := os.Chtimes(old, now.Add(-40*24*time.Hour), now.Add(-40*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(24 * time.Hour)
	l2.Write(Entry{User: "admin", Action: "POST /later"})
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("old file kept")
	}
	if c, _ := l2.Verify(); !c.OK {
		t.Fatalf("after pruning %+v", c)
	}
}
