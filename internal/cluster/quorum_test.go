package cluster

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestVotesAndBooks(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	m := &Manager{Dir: t.TempDir(), Now: func() time.Time { return now }}
	m.st = State{Role: "worker", Term: 2, Self: Node{ID: "n3"}, Nodes: []Node{{ID: "n1"}}, Mesh: []Node{{ID: "n2"}}}
	m.loaded = true
	m.lastMaster = now
	if r := m.Vote(VoteRequest{Term: 3, Candidate: "n2"}); r.Granted || r.Why != "I still hear the master" {
		t.Fatalf("hears master: %+v", r)
	}
	now = now.Add(time.Minute)
	if r := m.Vote(VoteRequest{Term: 2, Candidate: "n2"}); r.Granted {
		t.Fatal("old term granted")
	}
	if r := m.Vote(VoteRequest{Term: 3, Candidate: "n2"}); !r.Granted {
		t.Fatalf("vote: %+v", r)
	}
	if r := m.Vote(VoteRequest{Term: 3, Candidate: "n4"}); r.Granted {
		t.Fatal("two votes in one term")
	}
	// books: older terms are refused, a newer master is followed
	if _, err := m.ReceiveBook(ctx, Book{Term: 1, MasterID: "n1", Nodes: []Node{{ID: "n1"}, {ID: "n3"}}}); !errors.Is(err, ErrStale) {
		t.Fatalf("stale book: %v", err)
	}
	if _, err := m.ReceiveBook(ctx, Book{Term: 3, MasterID: "n2", Nodes: []Node{{ID: "n1"}, {ID: "n2"}, {ID: "n3", Endpoint: "192.168.0.9:51820"}},
		Files: map[string][]byte{"users.json": []byte("[]"), "../evil": []byte("x")}}); err != nil {
		t.Fatal(err)
	}
	if st := m.State(); st.Nodes[0].ID != "n2" || st.Term != 3 || len(st.Mesh) != 1 || st.Mesh[0].ID != "n1" || st.Self.Endpoint != "192.168.0.9:51820" {
		t.Fatalf("after book %+v", st)
	}
	if p := m.Peer(); p.MasterSeen != 0 || p.MasterID != "n2" {
		t.Fatalf("peer %+v", p)
	}
	// a master never votes for someone else, and steps down for a newer book
	mm := &Manager{Dir: t.TempDir(), Now: func() time.Time { return now }}
	mm.st = State{Role: "master", Term: 3, Self: Node{ID: "n1"}, Nodes: []Node{{ID: "n2"}, {ID: "n3"}}}
	mm.loaded = true
	if r := mm.Vote(VoteRequest{Term: 9, Candidate: "n2"}); r.Granted {
		t.Fatal("master voted")
	}
	var role string
	mm.OnRoleChange = func(r string) { role = r }
	if _, err := mm.ReceiveBook(ctx, Book{Term: 4, MasterID: "n2", Nodes: []Node{{ID: "n1"}, {ID: "n2"}, {ID: "n3"}}}); err != nil || mm.Role() != "worker" || role != "worker" {
		t.Fatalf("step down: %v %s %s", err, mm.Role(), role)
	}
	if mm.ClusterSize() != 3 {
		t.Fatalf("size %d", mm.ClusterSize())
	}
}
