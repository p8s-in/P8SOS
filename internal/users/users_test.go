package users

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPasswordHash(t *testing.T) {
	h := HashPassword("correct horse battery")
	if !VerifyPassword(h, "correct horse battery") {
		t.Fatal("right password rejected")
	}
	if VerifyPassword(h, "correct horse batterY") || VerifyPassword("garbage", "x") || VerifyPassword("", "") {
		t.Fatal("wrong password accepted")
	}
	if HashPassword("a") == HashPassword("a") {
		t.Fatal("salt not random")
	}
}

// RFC 6238 appendix B (SHA1), cut to 6 digits.
func TestTOTPVectors(t *testing.T) {
	secret := b32.EncodeToString([]byte("12345678901234567890"))
	for _, c := range []struct {
		t    int64
		want string
	}{{59, "287082"}, {1111111109, "081804"}, {1111111111, "050471"}, {1234567890, "005924"}, {2000000000, "279037"}} {
		got, err := TOTPCode(secret, time.Unix(c.t, 0))
		if err != nil || got != c.want {
			t.Errorf("t=%d: %s, want %s (%v)", c.t, got, c.want, err)
		}
	}
	now := time.Unix(1111111109, 0)
	if _, ok := CheckTOTP(secret, "081804", now, 0); !ok {
		t.Error("current code rejected")
	}
	prev, _ := TOTPCode(secret, now.Add(-30*time.Second))
	if _, ok := CheckTOTP(secret, prev, now, 0); !ok {
		t.Error("previous step (clock drift) rejected")
	}
	old, _ := TOTPCode(secret, now.Add(-90*time.Second))
	if _, ok := CheckTOTP(secret, old, now, 0); ok {
		t.Error("old code accepted")
	}
	step, _ := CheckTOTP(secret, "081804", now, 0)
	if _, ok := CheckTOTP(secret, "081804", now, step); ok {
		t.Error("replayed code accepted")
	}
}

func TestStore(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_800_000_000, 0)
	s := &Store{Path: filepath.Join(dir, "users.json"), Now: func() time.Time { return now }}
	if _, err := s.Add("alice", Customer, "short", DefaultQuota); !errors.Is(err, ErrInvalid) {
		t.Fatal("short password accepted")
	}
	if _, err := s.Add("Bad Name", Customer, "longenough1", DefaultQuota); !errors.Is(err, ErrInvalid) {
		t.Fatal("bad name accepted")
	}
	if _, err := s.Add("alice", Customer, "alice-password", DefaultQuota); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add("alice", Customer, "alice-password", DefaultQuota); !errors.Is(err, ErrExists) {
		t.Fatal("duplicate accepted")
	}
	if st, _ := os.Stat(s.Path); st.Mode().Perm() != 0o600 {
		t.Fatalf("users file mode %v", st.Mode().Perm())
	}
	// reload from disk
	s2 := &Store{Path: s.Path, Now: s.Now}
	if _, err := s2.Authenticate("alice", "alice-password", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Authenticate("alice", "nope-nope-nope", ""); !errors.Is(err, ErrBadLogin) {
		t.Fatal("wrong password accepted")
	}
	if _, err := s2.Authenticate("bob", "alice-password", ""); !errors.Is(err, ErrBadLogin) {
		t.Fatal("unknown user accepted")
	}

	// 2FA
	secret, uri, err := s2.StartTOTP("alice", "P8S OS")
	if err != nil || secret == "" || uri == "" {
		t.Fatal(err)
	}
	if _, err := s2.Authenticate("alice", "alice-password", ""); err != nil {
		t.Fatal("pending 2FA must not be required yet")
	}
	code, _ := TOTPCode(secret, now)
	if err := s2.ConfirmTOTP("alice", "000000"); !errors.Is(err, ErrBadOTP) {
		t.Fatal("bad confirm code accepted")
	}
	if err := s2.ConfirmTOTP("alice", code); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Authenticate("alice", "alice-password", ""); !errors.Is(err, ErrOTPRequired) {
		t.Fatalf("want OTP required, got %v", err)
	}
	now = now.Add(30 * time.Second)
	code, _ = TOTPCode(secret, now)
	if _, err := s2.Authenticate("alice", "alice-password", code); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Authenticate("alice", "alice-password", code); !errors.Is(err, ErrBadOTP) {
		t.Fatal("code replay accepted")
	}
	if err := s2.DisableTOTP("alice", "", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Authenticate("alice", "alice-password", ""); err != nil {
		t.Fatal("2FA still on after reset")
	}
	// disabled account
	if _, err := s2.Update("alice", func(u *User) error { u.Disabled = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Authenticate("alice", "alice-password", ""); !errors.Is(err, ErrBadLogin) {
		t.Fatal("disabled user signed in")
	}
}

func TestSessionsAndLimiter(t *testing.T) {
	now := time.Unix(1000, 0)
	s := &Sessions{Idle: time.Hour, Max: 3 * time.Hour, Now: func() time.Time { return now }}
	tok, _ := s.New("alice")
	if u, ok := s.User(tok); !ok || u != "alice" {
		t.Fatal("session not found")
	}
	now = now.Add(50 * time.Minute)
	s.User(tok) // keeps it alive
	now = now.Add(50 * time.Minute)
	if _, ok := s.User(tok); !ok {
		t.Fatal("active session expired")
	}
	now = now.Add(61 * time.Minute)
	if _, ok := s.User(tok); ok {
		t.Fatal("idle session still valid")
	}
	a, _ := s.New("bob")
	b, _ := s.New("bob")
	s.EndUser("bob", b)
	if _, ok := s.User(a); ok {
		t.Fatal("EndUser kept the other session")
	}
	if _, ok := s.User(b); !ok {
		t.Fatal("EndUser removed the kept session")
	}

	l := &Limiter{Max: 3, Window: time.Minute, Now: func() time.Time { return now }}
	for i := 0; i < 3; i++ {
		if _, b := l.Blocked("u:x"); b {
			t.Fatal("blocked too early")
		}
		l.Fail("u:x")
	}
	if _, b := l.Blocked("ip:1", "u:x"); !b {
		t.Fatal("not blocked after 3 failures")
	}
	now = now.Add(61 * time.Second)
	if _, b := l.Blocked("u:x"); b {
		t.Fatal("still blocked after the window")
	}
}

func TestSessionsSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	a := &Sessions{Path: path}
	tok, _ := a.New("alice")
	gone, _ := a.New("bob")
	a.End(gone)
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("sessions file: %v", err)
	}
	b := &Sessions{Path: path} // p8sd restarted
	if u, ok := b.User(tok); !ok || u != "alice" {
		t.Fatal("session lost across restart")
	}
	if _, ok := b.User(gone); ok {
		t.Fatal("ended session came back")
	}
	b.EndUser("alice", "")
	if _, ok := (&Sessions{Path: path}).User(tok); ok {
		t.Fatal("signed-out user still signed in after restart")
	}
}
