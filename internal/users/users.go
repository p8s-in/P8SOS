// Package users keeps P8S accounts: who can sign in to the dashboard, what
// they may do (admin or customer), how much they may use (quota) and their
// second factor (TOTP). Stored as one JSON file (mode 0600); passwords only as
// PBKDF2-SHA256 hashes.
package users

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Roles.
const (
	Admin    = "admin"    // everything, every VM, the host
	Customer = "customer" // own VMs only, within the quota
)

// Quota limits what a customer may have in total. Admins have no quota.
type Quota struct {
	VMs       int  `json:"vms"`
	VCPUs     int  `json:"vcpus"`
	MemoryMiB int  `json:"memory_mib"`
	DiskGB    int  `json:"disk_gb"`
	LAN       bool `json:"lan"` // may put VMs on the home/LAN network
}

// DefaultQuota is what a new customer gets unless the admin says otherwise.
var DefaultQuota = Quota{VMs: 2, VCPUs: 4, MemoryMiB: 4096, DiskGB: 60}

// User is one account as stored.
type User struct {
	Name      string `json:"name"`
	Role      string `json:"role"`
	PassHash  string `json:"pass_hash"`
	Quota     Quota  `json:"quota"`
	Disabled  bool   `json:"disabled,omitempty"`
	CreatedAt string `json:"created_at"`
	// TOTP (authenticator app). TOTPPending holds a secret until the user has
	// proved with one code that their app has it.
	TOTPSecret  string `json:"totp_secret,omitempty"`
	TOTPPending string `json:"totp_pending,omitempty"`
	TOTPLast    int64  `json:"totp_last,omitempty"` // last accepted time step (no replays)
}

// Public is what the API shows about a user (no secrets).
type Public struct {
	Name      string `json:"name"`
	Role      string `json:"role"`
	Quota     Quota  `json:"quota"`
	Disabled  bool   `json:"disabled"`
	TwoFactor bool   `json:"two_factor"`
	CreatedAt string `json:"created_at"`
}

func (u User) Public() Public {
	return Public{u.Name, u.Role, u.Quota, u.Disabled, u.TOTPSecret != "", u.CreatedAt}
}

var (
	ErrNotFound    = errors.New("user not found")
	ErrExists      = errors.New("user already exists")
	ErrInvalid     = errors.New("invalid")
	ErrBadLogin    = errors.New("wrong user name or password")
	ErrOTPRequired = errors.New("enter the 6-digit code from your authenticator app")
	ErrBadOTP      = errors.New("that code is wrong or expired")
)

var reName = regexp.MustCompile(`^[a-z][a-z0-9._-]{1,31}$`)

// ValidName reports whether s can be a user name.
func ValidName(s string) bool { return reName.MatchString(s) && !strings.HasPrefix(s, "token") }

// CheckPassword enforces the password rules.
func CheckPassword(pw string) error {
	if len(pw) < 10 || len(pw) > 128 {
		return fmt.Errorf("%w: password must be 10-128 characters", ErrInvalid)
	}
	return nil
}

// Store is the user database. The zero value with Path set is ready to use.
type Store struct {
	Path string
	Now  func() time.Time

	mu     sync.Mutex
	loaded bool
	users  map[string]*User
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Store) load() error {
	if s.loaded {
		return nil
	}
	s.users = map[string]*User{}
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		s.loaded = true
		return nil
	}
	if err != nil {
		return err
	}
	var f struct {
		Users []*User `json:"users"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return fmt.Errorf("%s: %w", s.Path, err)
	}
	for _, u := range f.Users {
		s.users[u.Name] = u
	}
	s.loaded = true
	return nil
}

func (s *Store) save() error {
	list := make([]*User, 0, len(s.users))
	for _, u := range s.users {
		list = append(list, u)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	b, err := json.MarshalIndent(map[string]any{"users": list}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o750); err != nil {
		return err
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.Path)
}

// List returns every user, sorted by name.
func (s *Store) List() ([]Public, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return nil, err
	}
	out := []Public{}
	for _, u := range s.users {
		out = append(out, u.Public())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Count is the number of accounts.
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.load() != nil {
		return 0
	}
	return len(s.users)
}

// Get returns a copy of one user.
func (s *Store) Get(name string) (User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return User{}, err
	}
	u, ok := s.users[name]
	if !ok {
		return User{}, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	return *u, nil
}

// Add creates an account.
func (s *Store) Add(name, role, password string, q Quota) (Public, error) {
	if !ValidName(name) {
		return Public{}, fmt.Errorf("%w: user name must be 2-32 chars: lowercase letters, digits, . _ -; start with a letter", ErrInvalid)
	}
	if role != Admin && role != Customer {
		return Public{}, fmt.Errorf("%w: role must be admin or customer", ErrInvalid)
	}
	if err := CheckPassword(password); err != nil {
		return Public{}, err
	}
	if err := checkQuota(q); err != nil {
		return Public{}, err
	}
	h := HashPassword(password)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return Public{}, err
	}
	if _, ok := s.users[name]; ok {
		return Public{}, fmt.Errorf("%w: %q", ErrExists, name)
	}
	u := &User{Name: name, Role: role, PassHash: h, Quota: q, CreatedAt: s.now().UTC().Format(time.RFC3339)}
	s.users[name] = u
	if err := s.save(); err != nil {
		delete(s.users, name)
		return Public{}, err
	}
	return u.Public(), nil
}

func checkQuota(q Quota) error {
	if q.VMs < 0 || q.VCPUs < 0 || q.MemoryMiB < 0 || q.DiskGB < 0 {
		return fmt.Errorf("%w: quota values can't be negative", ErrInvalid)
	}
	return nil
}

// Update changes a user under the store's lock and saves. fn may return an
// error to cancel.
func (s *Store) Update(name string, fn func(u *User) error) (Public, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return Public{}, err
	}
	u, ok := s.users[name]
	if !ok {
		return Public{}, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	c := *u
	if err := fn(&c); err != nil {
		return Public{}, err
	}
	if c.Role != Admin && c.Role != Customer {
		return Public{}, fmt.Errorf("%w: role must be admin or customer", ErrInvalid)
	}
	if err := checkQuota(c.Quota); err != nil {
		return Public{}, err
	}
	s.users[name] = &c
	if err := s.save(); err != nil {
		s.users[name] = u
		return Public{}, err
	}
	return c.Public(), nil
}

// SetPassword replaces a user's password.
func (s *Store) SetPassword(name, password string) error {
	if err := CheckPassword(password); err != nil {
		return err
	}
	h := HashPassword(password)
	_, err := s.Update(name, func(u *User) error { u.PassHash = h; return nil })
	return err
}

// Delete removes an account.
func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return err
	}
	u, ok := s.users[name]
	if !ok {
		return fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	delete(s.users, name)
	if err := s.save(); err != nil {
		s.users[name] = u
		return err
	}
	return nil
}

// AdminCount is the number of enabled admins (the last one can't be removed).
func (s *Store) AdminCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.load() != nil {
		return 0
	}
	n := 0
	for _, u := range s.users {
		if u.Role == Admin && !u.Disabled {
			n++
		}
	}
	return n
}

// dummyHash keeps sign-in time the same whether or not the user exists.
var dummyHash = HashPassword("p8s-not-a-real-password")

// Authenticate checks a sign-in. otp is the authenticator code; when the user
// has 2FA on and otp is empty, ErrOTPRequired is returned (password was right).
func (s *Store) Authenticate(name, password, otp string) (Public, error) {
	s.mu.Lock()
	if err := s.load(); err != nil {
		s.mu.Unlock()
		return Public{}, err
	}
	u, ok := s.users[name]
	var c User
	if ok {
		c = *u
	}
	s.mu.Unlock()
	if !ok {
		VerifyPassword(dummyHash, password)
		return Public{}, ErrBadLogin
	}
	if !VerifyPassword(c.PassHash, password) || c.Disabled {
		return Public{}, ErrBadLogin
	}
	if c.TOTPSecret == "" {
		return c.Public(), nil
	}
	if strings.TrimSpace(otp) == "" {
		return Public{}, ErrOTPRequired
	}
	if err := s.useCode(name, func(u *User) string { return u.TOTPSecret }, otp); err != nil {
		return Public{}, err
	}
	return c.Public(), nil
}

// useCode checks a TOTP code against the secret picked by sel and records its
// time step so the same code can't be used twice.
func (s *Store) useCode(name string, sel func(*User) string, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[name]
	if !ok {
		return ErrNotFound
	}
	step, ok := CheckTOTP(sel(u), code, s.now(), u.TOTPLast)
	if !ok {
		return ErrBadOTP
	}
	old := u.TOTPLast
	u.TOTPLast = step
	if err := s.save(); err != nil {
		u.TOTPLast = old
		return err
	}
	return nil
}

// StartTOTP makes a new secret for the user and returns it with an
// otpauth:// link for authenticator apps. It is used once ConfirmTOTP passes.
func (s *Store) StartTOTP(name, issuer string) (secret, uri string, err error) {
	secret = NewTOTPSecret()
	if _, err := s.Update(name, func(u *User) error { u.TOTPPending = secret; return nil }); err != nil {
		return "", "", err
	}
	return secret, TOTPURI(issuer, name, secret), nil
}

// ConfirmTOTP turns 2FA on once the user has typed a valid code for the pending secret.
func (s *Store) ConfirmTOTP(name, code string) error {
	u, err := s.Get(name)
	if err != nil {
		return err
	}
	if u.TOTPPending == "" {
		return fmt.Errorf("%w: start the 2FA setup first", ErrInvalid)
	}
	if err := s.useCode(name, func(u *User) string { return u.TOTPPending }, code); err != nil {
		return err
	}
	_, err = s.Update(name, func(u *User) error { u.TOTPSecret, u.TOTPPending = u.TOTPPending, ""; return nil })
	return err
}

// DisableTOTP turns 2FA off. code must be a valid current code unless the
// caller is an admin resetting someone else (code == "" and force).
func (s *Store) DisableTOTP(name, code string, force bool) error {
	if !force {
		u, err := s.Get(name)
		if err != nil {
			return err
		}
		if u.TOTPSecret == "" {
			return nil
		}
		if err := s.useCode(name, func(u *User) string { return u.TOTPSecret }, code); err != nil {
			return err
		}
	}
	_, err := s.Update(name, func(u *User) error { u.TOTPSecret, u.TOTPPending, u.TOTPLast = "", "", 0; return nil })
	return err
}

// --- passwords ---

const pbkdfIter = 310000

// HashPassword returns "pbkdf2-sha256$<iter>$<salt>$<hash>" (base64, no padding).
func HashPassword(pw string) string {
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	k, _ := pbkdf2.Key(sha256.New, pw, salt, pbkdfIter, 32)
	e := base64.RawStdEncoding
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdfIter, e.EncodeToString(salt), e.EncodeToString(k))
}

// VerifyPassword checks pw against a HashPassword result in constant time.
func VerifyPassword(hash, pw string) bool {
	p := strings.Split(hash, "$")
	if len(p) != 4 || p[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(p[1])
	if err != nil || iter < 1000 || iter > 10_000_000 {
		return false
	}
	e := base64.RawStdEncoding
	salt, err1 := e.DecodeString(p[2])
	want, err2 := e.DecodeString(p[3])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}
