package license

import (
	"bytes"
	"crypto/ed25519"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// License is what a key says. Expires is the last day it is valid (UTC).
type License struct {
	ID       string   `json:"id"`
	Customer string   `json:"customer"`
	Email    string   `json:"email,omitempty"`
	Tier     string   `json:"tier"`
	Servers  []string `json:"servers"`
	Issued   string   `json:"issued"`  // YYYY-MM-DD
	Expires  string   `json:"expires"` // YYYY-MM-DD
}

// KeyPrefix starts every licence key.
const KeyPrefix = "P8S1-"

var (
	ErrKey    = errors.New("invalid licence key")
	ErrServer = errors.New("this licence key is for a different server")
)

var (
	reServer = regexp.MustCompile(`^P8S(-[0-9A-F]{4}){4}$`)
	reLicID  = regexp.MustCompile(`^L[0-9A-Z]{6,20}$`)
	reDate   = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// ValidServerID reports whether s looks like a server ID (P8S-XXXX-XXXX-XXXX-XXXX).
func ValidServerID(s string) bool { return reServer.MatchString(s) }

// Check validates the fields (not the signature).
func (l License) Check() error {
	if !reLicID.MatchString(l.ID) {
		return fmt.Errorf("%w: id", ErrKey)
	}
	if _, ok := Tier(l.Tier); !ok || l.Tier == Home {
		return fmt.Errorf("%w: tier %q", ErrKey, l.Tier)
	}
	if len(l.Servers) == 0 || len(l.Servers) > 100 {
		return fmt.Errorf("%w: 1-100 servers", ErrKey)
	}
	for _, s := range l.Servers {
		if !ValidServerID(s) {
			return fmt.Errorf("%w: server id %q", ErrKey, s)
		}
	}
	if !reDate.MatchString(l.Issued) || !reDate.MatchString(l.Expires) {
		return fmt.Errorf("%w: dates", ErrKey)
	}
	if _, err := time.Parse("2006-01-02", l.Expires); err != nil {
		return fmt.Errorf("%w: expiry", ErrKey)
	}
	if len(l.Customer) > 200 || len(l.Email) > 200 {
		return fmt.Errorf("%w: customer", ErrKey)
	}
	return nil
}

// Covers reports whether the licence names a server.
func (l License) Covers(server string) bool {
	for _, s := range l.Servers {
		if s == server {
			return true
		}
	}
	return false
}

// End is the moment the licence stops being valid (end of its last day, UTC).
func (l License) End() time.Time {
	t, _ := time.Parse("2006-01-02", l.Expires)
	return t.Add(24 * time.Hour)
}

var b64 = base64.RawURLEncoding

// Sign makes a licence key: P8S1-<payload>.<signature>.
func Sign(priv ed25519.PrivateKey, l License) (string, error) {
	if err := l.Check(); err != nil {
		return "", err
	}
	payload, err := json.Marshal(l)
	if err != nil {
		return "", err
	}
	return KeyPrefix + b64.EncodeToString(payload) + "." + b64.EncodeToString(ed25519.Sign(priv, payload)), nil
}

// Parse checks a key's signature against the trusted keys and returns the licence.
func Parse(keys []ed25519.PublicKey, key string) (License, error) {
	key = strings.Join(strings.Fields(key), "") // pasted keys may wrap
	rest, ok := strings.CutPrefix(key, KeyPrefix)
	if !ok || len(key) > 8192 {
		return License{}, ErrKey
	}
	p, s, ok := strings.Cut(rest, ".")
	if !ok {
		return License{}, ErrKey
	}
	payload, err1 := b64.DecodeString(p)
	sig, err2 := b64.DecodeString(s)
	if err1 != nil || err2 != nil || len(sig) != ed25519.SignatureSize {
		return License{}, ErrKey
	}
	good := false
	for _, k := range keys {
		if len(k) == ed25519.PublicKeySize && ed25519.Verify(k, payload, sig) {
			good = true
			break
		}
	}
	if !good {
		return License{}, fmt.Errorf("%w: not signed by P8S", ErrKey)
	}
	var l License
	dec := json.NewDecoder(bytes.NewReader(payload))
	if err := dec.Decode(&l); err != nil {
		return License{}, ErrKey
	}
	return l, l.Check()
}

//go:embed pubkeys.txt
var pubkeysTxt string

// Trusted are the P8S signing keys built into this version (pubkeys.txt:
// one base64 Ed25519 public key per line; # comments).
var Trusted = ParsePublicKeys(pubkeysTxt)

// ParsePublicKeys reads base64 public keys, one per line.
func ParsePublicKeys(txt string) []ed25519.PublicKey {
	var out []ed25519.PublicKey
	for _, line := range strings.Split(txt, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		b, err := base64.StdEncoding.DecodeString(line)
		if err == nil && len(b) == ed25519.PublicKeySize {
			out = append(out, ed25519.PublicKey(b))
		}
	}
	return out
}
