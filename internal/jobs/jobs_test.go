package jobs

import (
	"context"
	"errors"
	"testing"
)

func TestJobs(t *testing.T) {
	var m Manager
	release := make(chan struct{})
	j, err := m.Start("backup", "web1", func(ctx context.Context, p Progress) (string, error) {
		p("sending")
		<-release
		return "saved 1 GB", nil
	})
	if err != nil || j.State != Running {
		t.Fatalf("start: %v %+v", err, j)
	}
	if _, err := m.Start("backup", "web1", nil); !errors.Is(err, ErrBusy) {
		t.Fatalf("second job on same target: %v", err)
	}
	other, err := m.Start("backup", "db1", func(context.Context, Progress) (string, error) { return "", errors.New("disk full") })
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	m.Wait()
	if g, _ := m.Get(j.ID); g.State != Done || g.Message != "saved 1 GB" || g.Finished == "" {
		t.Fatalf("done job: %+v", g)
	}
	if g, _ := m.Get(other.ID); g.State != Failed || g.Error != "disk full" {
		t.Fatalf("failed job: %+v", g)
	}
	if len(m.List()) != 2 {
		t.Fatalf("list %+v", m.List())
	}
	if _, ok := m.Get("nope"); ok {
		t.Fatal("unknown job found")
	}
}

func TestPrune(t *testing.T) {
	m := Manager{Keep: 3}
	for i := 0; i < 10; i++ {
		_, _ = m.Start("x", string(rune('a'+i)), func(context.Context, Progress) (string, error) { return "", nil })
		m.Wait()
	}
	if n := len(m.List()); n > 4 {
		t.Fatalf("kept %d jobs", n)
	}
}
