package api

import (
	"crypto/ed25519"
	"path/filepath"
	"testing"
	"time"

	"p8sos/internal/billing"
	"p8sos/internal/cloud"
	"p8sos/internal/license"
	"p8sos/internal/testfake"
	"p8sos/internal/updates"
)

func TestLicense(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	const id = "P8S-1111-2222-3333-4444"
	var lm *license.Manager
	m := newMultiUserCfg(t, func(s *Server) {
		dir := t.TempDir()
		lm = &license.Manager{Path: filepath.Join(dir, "license.json"), ServerID: id, Keys: []ed25519.PublicKey{pub},
			CheckURL: "off", Now: func() time.Time { return time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC) }}
		s.License = lm
		s.Cloud = &cloud.Store{Path: filepath.Join(dir, "cloud.json")}
		s.Billing = &billing.Store{Dir: filepath.Join(dir, "billing")}
	})
	st := m.server.Cloud.Settings()
	st.SignupOpen, st.Brand = true, "Acme Enterprise"
	if _, err := m.server.Cloud.SetSettings(st); err != nil {
		t.Fatal(err)
	}

	// Home: 3 accounts, no selling, default brand, notice
	m.req("POST", "/api/v1/users", secret, `{"name":"boss","role":"admin","password":"boss-password-1"}`, 201)
	m.req("POST", "/api/v1/users", secret, `{"name":"alice","password":"alice-password"}`, 201)
	m.req("POST", "/api/v1/users", secret, `{"name":"bob","password":"bob-password-1"}`, 201)
	m.req("POST", "/api/v1/users", secret, `{"name":"carol","password":"carol-password"}`, 402)
	if v := m.req("GET", "/api/v1/auth", "", "", 200); v["signup"] != false || v["brand"] != "P8S Cloud" {
		t.Fatalf("home auth info: %v", v)
	}
	m.req("POST", "/api/v1/signup", "", `{"user":"dave","email":"d@example.com","password":"dave-password"}`, 403)
	m.req("PUT", "/api/v1/plans/small", secret, `{"name":"Small","vcpus":1,"memory_mib":1024,"disk_gb":20,"price_monthly":50000}`, 402)
	boss := m.login("boss", "boss-password-1", "")
	alice := m.login("alice", "alice-password", "")
	v := m.req("GET", "/api/v1/license", boss, "", 200)
	if v["tier"] != "home" || v["notice"] != true || v["server_id"] != id {
		t.Fatalf("status: %v", v)
	}
	m.req("GET", "/api/v1/license", alice, "", 403)

	// a key for another server, a forged key, then the right one
	other, _ := license.Sign(priv, license.License{ID: "LTEST0001", Customer: "x", Tier: license.Enterprise,
		Servers: []string{"P8S-0000-0000-0000-0000"}, Issued: "2026-01-01", Expires: "2027-01-01"})
	m.req("PUT", "/api/v1/license", boss, `{"key":"`+other+`"}`, 400)
	_, priv2, _ := ed25519.GenerateKey(nil)
	forged, _ := license.Sign(priv2, license.License{ID: "LTEST0002", Customer: "x", Tier: license.Enterprise,
		Servers: []string{id}, Issued: "2026-01-01", Expires: "2027-01-01"})
	m.req("PUT", "/api/v1/license", boss, `{"key":"`+forged+`"}`, 400)
	m.req("PUT", "/api/v1/license", alice, `{"key":"x"}`, 403)
	good, _ := license.Sign(priv, license.License{ID: "LTEST0003", Customer: "Acme", Tier: license.Enterprise,
		Servers: []string{id}, Issued: "2026-01-01", Expires: "2027-01-01"})
	v = m.req("PUT", "/api/v1/license", boss, `{"key":"`+good+`"}`, 200)
	if v["tier"] != "enterprise" || v["notice"] != false || v["state"] != "active" {
		t.Fatalf("installed: %v", v)
	}

	// Enterprise unlocks it all
	m.req("POST", "/api/v1/users", secret, `{"name":"carol","password":"carol-password"}`, 201)
	if v := m.req("GET", "/api/v1/auth", "", "", 200); v["signup"] != true || v["brand"] != "Acme Enterprise" {
		t.Fatalf("hosting auth info: %v", v)
	}
	m.req("PUT", "/api/v1/plans/small", secret, `{"name":"Small","vcpus":1,"memory_mib":1024,"disk_gb":20,"price_monthly":50000}`, 200)

	// removing it goes back to Home (existing accounts stay)
	if v := m.req("DELETE", "/api/v1/license", boss, "", 200); v["tier"] != "home" {
		t.Fatalf("removed: %v", v)
	}
	m.req("POST", "/api/v1/users", secret, `{"name":"erin","password":"erin-password-1"}`, 402)
	m.login("carol", "carol-password", "")
}

func TestUpdatesAPI(t *testing.T) {
	m := newMultiUserCfg(t, func(s *Server) {
		s.Updates = &updates.Manager{Dir: t.TempDir(), Current: "0.6.0", Run: &testfake.Runner{},
			Info: func() updates.Channel {
				return updates.Channel{Allowed: []string{"testing"}, Server: "P8S-1111-2222-3333-4444"}
			}}
	})
	m.req("POST", "/api/v1/users", secret, `{"name":"alice","password":"alice-password"}`, 201)
	alice := m.login("alice", "alice-password", "")
	m.req("GET", "/api/v1/updates", alice, "", 403)
	v := m.req("GET", "/api/v1/updates", secret, "", 200)
	if v["enabled"] != false || v["current"] != "0.6.0" || v["channel"] != "testing" {
		t.Fatalf("status %v", v)
	}
	m.req("POST", "/api/v1/updates/check", secret, "", 409)
	m.req("POST", "/api/v1/updates/install", secret, "", 409)
	m.req("PUT", "/api/v1/updates/channel", secret, `{"channel":"stable"}`, 400)
	m.req("PUT", "/api/v1/updates/channel", secret, `{"channel":"testing"}`, 200)
}
