package creds

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStore(t *testing.T) {
	s := &Store{File: filepath.Join(t.TempDir(), "d", "credentials.json")}
	if _, ok := s.Get("vm", "web1"); ok {
		t.Fatal("empty store")
	}
	if err := s.Set("vm", "web1", Login{User: "root", Password: "pw1", SSH: true}); err != nil {
		t.Fatal(err)
	}
	s.Set("ct", "web1", Login{User: "root", Password: "pw2"})
	if l, ok := s.Get("vm", "web1"); !ok || l.Password != "pw1" || !l.SSH || l.SetAt == "" {
		t.Fatalf("%+v", l)
	}
	if l, _ := s.Get("ct", "web1"); l.Password != "pw2" {
		t.Fatalf("%+v", l)
	}
	if fi, _ := os.Stat(s.File); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	s.Rename("ct", "web1", "web9")
	if _, ok := s.Get("ct", "web9"); !ok {
		t.Fatal("rename")
	}
	s.Delete("vm", "web1")
	if _, ok := s.Get("vm", "web1"); ok {
		t.Fatal("delete")
	}
	var nilStore *Store
	if _, ok := nilStore.Get("vm", "x"); ok || nilStore.Set("vm", "x", Login{}) != nil {
		t.Fatal("nil store")
	}
}

func TestGenerate(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		p := Generate(16)
		if len(p) != 16 || seen[p] || strings.ContainsAny(p, "01lIO") {
			t.Fatalf("bad %q", p)
		}
		seen[p] = true
	}
	if len(Generate(4)) != 12 {
		t.Fatal("minimum length")
	}
}
