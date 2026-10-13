package users

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ErrBadCode means an email link is wrong, used or expired.
var ErrBadCode = errors.New("this link is invalid or has expired")

// ErrUnverified means the user hasn't confirmed their email yet.
var ErrUnverified = errors.New("please confirm your email address first (check your inbox)")

// NormEmail checks and lower-cases an email address.
func NormEmail(e string) (string, error) {
	e = strings.ToLower(strings.TrimSpace(e))
	a, err := mail.ParseAddress(e)
	if err != nil || a.Address != e || len(e) > 254 || !strings.Contains(e[strings.LastIndex(e, "@"):], ".") {
		return "", fmt.Errorf("%w: %q is not an email address", ErrInvalid, e)
	}
	return e, nil
}

// ByEmail finds the user with this email.
func (s *Store) ByEmail(email string) (User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return User{}, err
	}
	for _, u := range s.users {
		if email != "" && u.Email == email {
			return *u, nil
		}
	}
	return User{}, fmt.Errorf("%w: no account with that email", ErrNotFound)
}

// BySSO finds the account linked to a company-login identity.
func (s *Store) BySSO(key string) (User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return User{}, err
	}
	for _, u := range s.users {
		if key != "" && u.SSO == key {
			return *u, nil
		}
	}
	return User{}, fmt.Errorf("%w: no account for that login", ErrNotFound)
}

// FreeName turns a wish ("Ravi.K") into an unused valid user name.
func (s *Store) FreeName(wish string) string {
	b := strings.Builder{}
	for _, r := range strings.ToLower(wish) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		}
	}
	base := strings.TrimLeft(b.String(), "0123456789._-")
	if len(base) > 24 {
		base = base[:24]
	}
	if len(base) < 2 {
		base = "user"
	}
	for i := 1; ; i++ {
		n := base
		if i > 1 {
			n = fmt.Sprintf("%s%d", base, i)
		}
		if _, err := s.Get(n); err != nil && ValidName(n) {
			return n
		}
	}
}

// RandomPassword is for accounts that sign in elsewhere (company login).
func RandomPassword() string { return randHex(24) }

// EmailTaken reports whether another account uses the address.
func (s *Store) EmailTaken(email, except string) bool {
	u, err := s.ByEmail(email)
	return err == nil && u.Name != except
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func sum(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

// NewCode makes a one-time code for an email link (purpose "verify" or
// "reset"), valid for ttl. Older codes for the same purpose stop working.
func (s *Store) NewCode(name, purpose string, ttl time.Duration) (string, error) {
	code := randHex(24)
	exp := s.now().Add(ttl).Unix()
	_, err := s.Update(name, func(u *User) error {
		keep := []string{}
		for _, c := range u.Codes {
			if !strings.HasPrefix(c, purpose+":") {
				keep = append(keep, c)
			}
		}
		u.Codes = append(keep, fmt.Sprintf("%s:%s:%d", purpose, sum(code), exp))
		return nil
	})
	return code, err
}

// UseCode finds the user a code belongs to and consumes it.
func (s *Store) UseCode(purpose, code string) (User, error) {
	if len(code) != 48 {
		return User{}, ErrBadCode
	}
	h := sum(code)
	s.mu.Lock()
	if err := s.load(); err != nil {
		s.mu.Unlock()
		return User{}, err
	}
	name := ""
	now := s.now().Unix()
	for _, u := range s.users {
		for _, c := range u.Codes {
			f := strings.Split(c, ":")
			if len(f) != 3 || f[0] != purpose {
				continue
			}
			exp, _ := strconv.ParseInt(f[2], 10, 64)
			if subtle.ConstantTimeCompare([]byte(f[1]), []byte(h)) == 1 && now <= exp {
				name = u.Name
			}
		}
	}
	s.mu.Unlock()
	if name == "" {
		return User{}, ErrBadCode
	}
	if _, err := s.Update(name, func(u *User) error {
		keep := []string{}
		for _, c := range u.Codes {
			if !strings.HasPrefix(c, purpose+":") {
				keep = append(keep, c)
			}
		}
		u.Codes = keep
		return nil
	}); err != nil {
		return User{}, err
	}
	return s.Get(name)
}

// --- personal API tokens ---

var reTokName = regexp.MustCompile(`^[\pL\pN ._-]{1,40}$`)

// NewToken makes an API token for the user; the secret is shown once.
func (s *Store) NewToken(name, label string) (APIToken, string, error) {
	label = strings.TrimSpace(label)
	if !reTokName.MatchString(label) {
		return APIToken{}, "", fmt.Errorf("%w: token name: up to 40 letters, digits, spaces, . _ -", ErrInvalid)
	}
	secret := "p8sk_" + randHex(32)
	t := APIToken{ID: randHex(6), Name: label, SHA256: sum(secret), Created: s.now().UTC().Format(time.RFC3339)}
	_, err := s.Update(name, func(u *User) error {
		if len(u.Tokens) >= 20 {
			return fmt.Errorf("%w: at most 20 API tokens", ErrInvalid)
		}
		u.Tokens = append(u.Tokens, t)
		return nil
	})
	return t, secret, err
}

// DeleteToken revokes one of the user's tokens.
func (s *Store) DeleteToken(name, id string) error {
	found := false
	_, err := s.Update(name, func(u *User) error {
		keep := []APIToken{}
		for _, t := range u.Tokens {
			if t.ID == id {
				found = true
				continue
			}
			keep = append(keep, t)
		}
		u.Tokens = keep
		return nil
	})
	if err == nil && !found {
		return fmt.Errorf("%w: token", ErrNotFound)
	}
	return err
}

// TokenUser returns the (enabled) user an API token belongs to and notes its use.
func (s *Store) TokenUser(secret string) (User, bool) {
	if !strings.HasPrefix(secret, "p8sk_") {
		return User{}, false
	}
	h := sum(secret)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.load() != nil {
		return User{}, false
	}
	for _, u := range s.users {
		for i, t := range u.Tokens {
			if subtle.ConstantTimeCompare([]byte(t.SHA256), []byte(h)) == 1 {
				if u.Disabled {
					return User{}, false
				}
				now := s.now().UTC()
				if last, err := time.Parse(time.RFC3339, t.LastUsed); err != nil || now.Sub(last) > time.Hour {
					u.Tokens[i].LastUsed = now.Format(time.RFC3339)
					_ = s.save()
				}
				return *u, true
			}
		}
	}
	return User{}, false
}
