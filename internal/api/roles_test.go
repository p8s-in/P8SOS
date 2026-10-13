package api

import (
	"strings"
	"testing"

	"p8sos/internal/users"
)

func TestRoles(t *testing.T) {
	m := newMultiUser(t)
	m.req("POST", "/api/v1/users", secret, `{"name":"ops","role":"operator","password":"operator-pass-1"}`, 201)
	m.req("POST", "/api/v1/users", secret, `{"name":"aud","role":"auditor","password":"auditor-pass-1"}`, 201)
	m.req("POST", "/api/v1/users", secret, `{"name":"x","role":"king","password":"auditor-pass-1"}`, 400)
	ops := m.login("ops", "operator-pass-1", "")
	aud := m.login("aud", "auditor-pass-1", "")

	// operators run every VM but not the server
	m.req("GET", "/api/v1/vms/shop", ops, "", 200)
	m.req("POST", "/api/v1/vms/shop/stop", ops, `{"force":true}`, 202)
	m.req("GET", "/api/v1/users", ops, "", 200)
	m.req("POST", "/api/v1/users", ops, `{"name":"x2","role":"admin","password":"0123456789ab"}`, 403)
	m.req("PATCH", "/api/v1/users/aud", ops, `{"role":"admin"}`, 403)
	m.req("POST", "/api/v1/node/shell", ops, "", 403)
	m.req("POST", "/api/v1/updates/install", ops, "{}", 403)
	m.req("GET", "/api/v1/cluster/ha-sync", ops, "", 403)
	if v := m.req("GET", "/api/v1/me", ops, "", 200); v["role"] != users.Operator {
		t.Fatalf("me %v", v)
	}

	// auditors look only
	m.req("GET", "/api/v1/vms", aud, "", 200)
	m.req("GET", "/api/v1/audit", aud, "", 200)
	m.req("GET", "/api/v1/users", aud, "", 200)
	m.req("POST", "/api/v1/vms/shop/start", aud, "", 403)
	m.req("POST", "/api/v1/vms/web1/console", aud, "", 403)
	m.req("GET", "/api/v1/vms/web1/login", aud, "", 403)
	m.req("DELETE", "/api/v1/vms/web1", aud, "", 403)
	m.req("POST", "/api/v1/me/password", aud, `{"old_password":"auditor-pass-1","new_password":"auditor-pass-2"}`, 204)
	if v := m.req("GET", "/api/v1/me", aud, "", 200); v["role"] != users.Auditor {
		t.Fatalf("me %v", v)
	}
	if v, _ := m.vms.Get(t.Context(), "shop"); v.State != "stopped" {
		t.Fatalf("state %s", v.State)
	}
}

func TestAuditExport(t *testing.T) {
	m := newMultiUser(t)
	m.req("POST", "/api/v1/users", secret, `{"name":"aud","role":"auditor","password":"auditor-pass-1"}`, 201)
	aud := m.login("aud", "auditor-pass-1", "")
	if v := m.req("GET", "/api/v1/audit/verify", aud, "", 200); v["ok"] != true || v["entries"].(float64) < 2 {
		t.Fatalf("verify %v", v)
	}
	code, out := do(t, m.srv, "GET", "/api/v1/audit/export?from=2000-01-01", aud, "")
	if code != 200 || !strings.Contains(out, "POST /api/v1/users") || !strings.HasPrefix(out, "time,user,ip,action") {
		t.Fatalf("%d %s", code, out)
	}
	m.req("GET", "/api/v1/audit/export?from=yesterday", aud, "", 400)
	m.req("POST", "/api/v1/users", secret, `{"name":"bob","password":"customer-pass-1"}`, 201)
	bob := m.login("bob", "customer-pass-1", "")
	m.req("GET", "/api/v1/audit/export", bob, "", 403)
}
