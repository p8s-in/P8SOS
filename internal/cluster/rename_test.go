package cluster

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Renames show up by themselves, the master can rename a worker, and a
// removed worker is told to stand alone.
func TestRenameAndRemoveTellsWorker(t *testing.T) {
	var mu sync.Mutex
	var got []string
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/cluster/capacity":
			json.NewEncoder(w).Encode(Capacity{CPUs: 2, Hostname: "p8s-vm1"})
		case "/api/v1/node/rename":
			var b map[string]string
			json.NewDecoder(r.Body).Decode(&b)
			if b["name"] != "web-box" {
				http.Error(w, "bad", 400)
				return
			}
			w.WriteHeader(202)
		default:
			w.WriteHeader(204)
		}
	}))
	defer worker.Close()
	host := "p8s-node"
	m := &Manager{Dir: t.TempDir(), HTTP: func(string) *http.Client { return worker.Client() }, Hostname: func() string { return host }}
	m.loaded = true
	m.status = map[string]*Status{}
	m.st = State{Role: "master", Self: Node{ID: "n1", Name: "old-name"},
		Nodes: []Node{{ID: "n2", Name: "p8s-node", API: worker.URL, Token: "tok-n2"}}}
	ctx := context.Background()

	m.Poll(ctx)
	st := m.State()
	if st.Self.Name != "p8s-node" || st.Nodes[0].Name != "p8s-vm1" {
		t.Fatalf("names not refreshed: self %q, worker %q", st.Self.Name, st.Nodes[0].Name)
	}
	host = "laptop"
	m.Poll(ctx)
	if m.State().Self.Name != "laptop" {
		t.Fatal("master rename not picked up")
	}

	if err := m.Rename(ctx, "n2", "web-box"); err != nil {
		t.Fatal(err)
	}
	if err := m.Rename(ctx, "n9", "x"); err == nil {
		t.Fatal("unknown server renamed")
	}

	if err := m.Remove(ctx, "n2"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	all := strings.Join(got, "\n")
	mu.Unlock()
	if !strings.Contains(all, "POST /api/v1/node/rename Bearer tok-n2") || !strings.Contains(all, "POST /api/v1/cluster/leave Bearer tok-n2") {
		t.Fatalf("worker calls:\n%s", all)
	}
	if len(m.Workers()) != 0 {
		t.Fatal("still listed")
	}
}

// One missed answer is no outage; three are. A server that just joined is "connecting".
func TestPollDebounce(t *testing.T) {
	up := true
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up {
			http.Error(w, "down", 500)
			return
		}
		json.NewEncoder(w).Encode(Capacity{CPUs: 2})
	}))
	defer worker.Close()
	now := time.Now()
	m := &Manager{Dir: t.TempDir(), HTTP: func(string) *http.Client { return worker.Client() }, Now: func() time.Time { return now }}
	m.loaded, m.status = true, map[string]*Status{}
	m.st = State{Role: "master", Self: Node{ID: "n1", Name: "a"},
		Nodes: []Node{{ID: "n2", Name: "b", API: worker.URL, Token: "t", Joined: now.UTC().Format(time.RFC3339)}}}
	ctx := context.Background()
	up = false
	m.Poll(ctx)
	if s := m.NodeStatus("n2"); s.Online || !s.Connecting || s.Error != "" {
		t.Fatalf("just joined: %+v", s)
	}
	up = true
	m.Poll(ctx)
	if s := m.NodeStatus("n2"); !s.Online || s.Connecting {
		t.Fatalf("answering: %+v", s)
	}
	now = now.Add(10 * time.Minute)
	up = false
	for i := 1; i < offlineAfter; i++ {
		m.Poll(ctx)
		if !m.NodeStatus("n2").Online {
			t.Fatalf("offline after %d miss(es)", i)
		}
	}
	m.Poll(ctx)
	if s := m.NodeStatus("n2"); s.Online || s.Error == "" {
		t.Fatalf("still online after %d misses: %+v", offlineAfter, s)
	}
}
