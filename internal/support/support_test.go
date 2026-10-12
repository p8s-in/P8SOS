package support

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestTickets(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "tickets.json")}
	if _, err := s.Open("alice", "", "", "x"); !errors.Is(err, ErrInvalid) {
		t.Fatal("empty subject")
	}
	if _, err := s.Open("alice", "a\nBcc: x", "", "x"); !errors.Is(err, ErrInvalid) {
		t.Fatal("multi-line subject")
	}
	tk, err := s.Open("alice", "Server won't boot", "web1", "help please")
	if err != nil || tk.Status != "open" || len(tk.Messages) != 1 {
		t.Fatalf("%+v %v", tk, err)
	}
	tk, _ = s.Reply(tk.ID, "boss", true, "Try the console")
	if tk.Status != "answered" || !tk.Messages[1].Staff {
		t.Fatalf("staff reply %+v", tk)
	}
	tk, _ = s.Reply(tk.ID, "alice", false, "still broken")
	if tk.Status != "open" {
		t.Fatal("customer reply should reopen")
	}
	s.Close(tk.ID)
	s.Open("bob", "Billing question", "", "invoice?")
	s2 := &Store{Path: s.Path}
	if l, _ := s2.List("alice"); len(l) != 1 || l[0].Status != "closed" || l[0].Messages != nil {
		t.Fatalf("alice list %+v", l)
	}
	if l, _ := s2.List(""); len(l) != 2 {
		t.Fatal("all tickets")
	}
	if g, _ := s2.Get(tk.ID); len(g.Messages) != 3 {
		t.Fatal("messages kept")
	}
	if _, err := s2.Reply("nope", "a", false, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatal("reply to missing ticket")
	}
}
