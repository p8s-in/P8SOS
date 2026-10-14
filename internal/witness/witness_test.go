package witness

import (
	"context"
	"crypto/tls"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestWitness(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := &Server{File: filepath.Join(t.TempDir(), "w.json"), Token: "secret", Now: func() time.Time { return now }}
	ts := httptest.NewTLSServer(s.Handler())
	defer ts.Close()
	pin := Fingerprint(ts.Certificate().Raw)
	c := &Client{Conf: Conf{URL: ts.URL, Token: "secret", Pin: pin, Cluster: "c1"}}
	ctx := context.Background()
	if r, err := c.Ping(ctx); err != nil || !r.OK || r.MasterSeen != -1 {
		t.Fatalf("ping %v %+v", err, r)
	}
	// the master beats; a worker asking for a vote is told the master lives
	if r, err := c.Beat(ctx, 3, "n1"); err != nil || !r.OK {
		t.Fatalf("beat %v %+v", err, r)
	}
	if r, _ := c.Vote(ctx, 4, "n2"); r.OK {
		t.Fatal("voted while the master is alive")
	}
	// the master goes quiet: the vote is given, once per term
	now = now.Add(time.Minute)
	if r, _ := c.Vote(ctx, 4, "n2"); !r.OK {
		t.Fatalf("no vote: %+v", r)
	}
	if r, _ := c.Vote(ctx, 4, "n3"); r.OK {
		t.Fatal("two votes in one term")
	}
	if r, _ := c.Vote(ctx, 2, "n2"); r.OK {
		t.Fatal("old term")
	}
	// the new master (term 4) beats; the old one (term 3) is told it is stale
	if r, _ := c.Beat(ctx, 4, "n2"); !r.OK {
		t.Fatal("new master")
	}
	if r, _ := c.Beat(ctx, 3, "n1"); r.OK || r.Term != 4 {
		t.Fatalf("old master accepted: %+v", r)
	}
	// state survives a restart
	s2 := &Server{File: s.File, Token: "secret", Now: s.Now}
	if r := s2.Status("c1"); r.Term != 4 {
		t.Fatalf("%+v", r)
	}
	// wrong token, wrong pin
	bad := &Client{Conf: Conf{URL: ts.URL, Token: "nope", Pin: pin, Cluster: "c1"}}
	if _, err := bad.Ping(ctx); err == nil {
		t.Fatal("wrong token")
	}
	other := &Client{Conf: Conf{URL: ts.URL, Token: "secret", Pin: "00", Cluster: "c1"}}
	if _, err := other.Ping(ctx); err == nil {
		t.Fatal("wrong certificate accepted")
	}
	_ = tls.VersionTLS12
}
