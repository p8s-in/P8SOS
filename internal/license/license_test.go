package license

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func keypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

const srv = "P8S-AAAA-BBBB-CCCC-DDDD"

func lic(tier, expires string) License {
	return License{ID: "L12345678", Customer: "Acme", Tier: tier, Servers: []string{srv}, Issued: "2026-01-01", Expires: expires}
}

func TestSignParse(t *testing.T) {
	pub, priv := keypair(t)
	other, _ := keypair(t)
	key, err := Sign(priv, lic(Hosting, "2027-01-01"))
	if err != nil {
		t.Fatal(err)
	}
	l, err := Parse([]ed25519.PublicKey{other, pub}, key[:20]+"\n"+key[20:]) // wrapped paste
	if err != nil || l.Tier != Hosting || !l.Covers(srv) {
		t.Fatalf("parse: %v %+v", err, l)
	}
	if _, err := Parse([]ed25519.PublicKey{other}, key); !errors.Is(err, ErrKey) {
		t.Fatalf("wrong signer accepted: %v", err)
	}
	// tamper: change tier in the payload
	p, s, _ := strings.Cut(strings.TrimPrefix(key, KeyPrefix), ".")
	raw, _ := b64.DecodeString(p)
	raw = []byte(strings.Replace(string(raw), "hosting", "partner", 1))
	if _, err := Parse([]ed25519.PublicKey{pub}, KeyPrefix+b64.EncodeToString(raw)+"."+s); err == nil {
		t.Fatal("tampered key accepted")
	}
	if _, err := Sign(priv, lic(Home, "2027-01-01")); err == nil {
		t.Fatal("home licence signed")
	}
}

func TestServerID(t *testing.T) {
	a, b := ServerIDFrom("x"), ServerIDFrom("y")
	if a == b || !ValidServerID(a) || a != ServerIDFrom("x") {
		t.Fatal(a, b)
	}
}

func TestManagerStates(t *testing.T) {
	pub, priv := keypair(t)
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	m := &Manager{Path: filepath.Join(t.TempDir(), "license.json"), ServerID: srv, Keys: []ed25519.PublicKey{pub}, Now: func() time.Time { return now }}
	st := m.Status()
	if st.Tier != Home || !st.Notice || st.UserLimit != HomeUsers || st.Channel != "testing" || m.Allows(FeatSelling) {
		t.Fatalf("home: %+v", st)
	}
	other := lic(Business, "2027-01-01")
	other.Servers = []string{"P8S-0000-0000-0000-0000"}
	k, _ := Sign(priv, other)
	if _, err := m.Install(k); !errors.Is(err, ErrServer) {
		t.Fatalf("other server: %v", err)
	}
	k, _ = Sign(priv, lic(Hosting, "2026-06-20"))
	st, err := m.Install(k)
	if err != nil || st.State != "expiring" || st.Tier != Hosting || st.Notice || !m.Allows(FeatSelling) || st.UserLimit != 0 {
		t.Fatalf("install: %v %+v", err, st)
	}
	// survives a restart
	m2 := &Manager{Path: m.Path, Keys: []ed25519.PublicKey{pub}, Now: m.Now}
	if m2.ID() != srv || m2.Status().Tier != Hosting {
		t.Fatalf("reload: %+v", m2.Status())
	}
	now = time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC)
	if st := m.Status(); st.State != "grace" || st.Tier != Hosting {
		t.Fatalf("grace: %+v", st)
	}
	now = time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	if st := m.Status(); st.State != "expired" || st.Tier != Home || !st.Notice {
		t.Fatalf("expired: %+v", st)
	}
	if _, err := m.Install(k); err == nil {
		t.Fatal("expired key installed")
	}
	if err := m.Remove(); err != nil || m.Status().State != "none" {
		t.Fatal(err)
	}
}

func TestCheckRevokeRenew(t *testing.T) {
	pub, priv := keypair(t)
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	reply := map[string]string{"status": "active"}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["id"] != "L12345678" || in["server"] != srv {
			http.Error(w, "bad", 400)
			return
		}
		_ = json.NewEncoder(w).Encode(reply)
	}))
	defer ts.Close()
	m := &Manager{Path: filepath.Join(t.TempDir(), "l.json"), ServerID: srv, Keys: []ed25519.PublicKey{pub}, CheckURL: ts.URL, Now: func() time.Time { return now }}
	k, _ := Sign(priv, lic(Business, "2026-12-31"))
	if _, err := m.Install(k); err != nil {
		t.Fatal(err)
	}
	renewed, _ := Sign(priv, lic(Business, "2027-12-31"))
	reply["key"] = renewed
	if err := m.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := m.Status(); st.License.Expires != "2027-12-31" || st.Checked == "" {
		t.Fatalf("renew: %+v", st)
	}
	reply = map[string]string{"status": "revoked", "message": "refunded"}
	_ = m.Check(context.Background())
	if st := m.Status(); st.State != "revoked" || st.Tier != Home {
		t.Fatalf("revoke: %+v", st)
	}
}

func TestPubkeysFile(t *testing.T) {
	pub, _ := keypair(t)
	b := b64std(pub)
	if got := ParsePublicKeys("# comment\n\n" + b + "\nnot-a-key\n"); len(got) != 1 || !got[0].Equal(pub) {
		t.Fatalf("parsed %d keys", len(got))
	}
}

func b64std(k ed25519.PublicKey) string { return base64.StdEncoding.EncodeToString(k) }

func TestDevelopmentBuild(t *testing.T) {
	m := &Manager{Path: filepath.Join(t.TempDir(), "l.json"), ServerID: srv, Keys: []ed25519.PublicKey{}}
	st := m.Status()
	if st.State != "development" || st.Notice || st.UserLimit != 0 || !m.Allows(FeatSelling) {
		t.Fatalf("dev build: %+v", st)
	}
	if _, err := m.Install("P8S1-x.y"); err == nil {
		t.Fatal("key accepted in a dev build")
	}
}
