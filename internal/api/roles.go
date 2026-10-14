package api

import (
	"net/http"
	"regexp"
	"strings"

	"p8sos/internal/users"
)

// Staff roles besides admin:
//   - operator: runs every VM and container (create, power, console, backups,
//     snapshots, firewall, moves, HA per VM) but can't change accounts, plans,
//     billing, server settings, updates, the cluster or open the server shell;
//   - auditor: sees everything an admin sees, including the Activity log, but
//     changes nothing and can't open consoles, shells or login passwords.
// The checks run for every API call, after sign-in, before the handler.

type rule struct {
	method string // "" = any
	re     *regexp.Regexp
}

func rules(spec ...string) []rule {
	var out []rule
	for _, s := range spec {
		m, p, _ := strings.Cut(s, " ")
		if m == "*" {
			m = ""
		}
		out = append(out, rule{m, regexp.MustCompile("^/api/v1" + p + "$")})
	}
	return out
}

var (
	// operators may not do these
	operatorDeny = rules(
		"POST /users", "PATCH /users/[^/]+", "DELETE /users/[^/]+",
		"PUT /plans/[^/]+", "DELETE /plans/[^/]+",
		"PUT /cloud/settings", "POST /cloud/test-email", "GET /cloud/outbox",
		"POST /billing/adjust",
		"POST /ippools", "PATCH /ippools/[^/]+", "DELETE /ippools/[^/]+",
		"POST /tunnel/setup", "DELETE /tunnel", "PUT /tunnel/dashboard",
		"PUT /license", "DELETE /license", "POST /license/check",
		"POST /cluster/join-code", "POST /cluster/join", "POST /cluster/leave", "DELETE /cluster/nodes/[^/]+",
		"POST /cluster/invite-send", "POST /cluster/invites/[^/]+/[^/]+", "POST /cluster/check", "GET /cluster/shell",
		"POST /node/shell", "POST /node/tasks",
		"POST /updates/check", "POST /updates/install", "PUT /updates/channel",
		"PUT /alerts/settings", "PUT /ha/settings",
		"PUT /offsite/targets/[^/]+", "DELETE /offsite/targets/[^/]+",
		"PUT /sso", "PUT /audit/settings", "PUT /security", "PUT /ldap", "POST /ldap/test",
		"POST /storages", "POST /storages/[^/]+/connect", "DELETE /storages/[^/]+",
		"POST /ceph/.+", "DELETE /ceph/.+",
		"POST /vms/[^/]+/template", "DELETE /templates/[^/]+",
		"PUT /remote/vpn", "POST /remote/vpn/devices", "DELETE /remote/vpn/devices/[^/]+", "POST /remote/forward",
		// server-to-server calls of the cluster (the master's token is an admin)
		"* /cluster/(vm-export|replica|replica-send)/.+", "* /cluster/(vm-import|replica-recv|ha-sync)",
		"* /cluster/live-[a-z]+(/.+)?", "GET /cluster/cpu",
		"POST /cluster/(book|vote|promote)", "GET /cluster/peer",
	)
	// auditors may change only their own account (password, 2FA, profile)
	auditorAllow = rules("POST /logout", "POST /me/password", "PUT /me/profile", "POST /me/2fa/[a-z]+")
	// ...and may not read these (secrets, screens, shells)
	auditorDenyRead = rules(
		"GET /vms/[^/]+/login", "GET /containers/[^/]+/login", "GET /vms/[^/]+/screenshot",
		"GET /cloud/outbox", "GET /cluster/shell", "GET /cluster/vnc/[^/]+", "GET /cluster/vm-export/[^/]+",
		"GET /cluster/replica-send/[^/]+", "GET /me/tokens", "GET /cluster/live-header/[^/]+", "GET /cluster/peer",
	)
)

func matches(rs []rule, r *http.Request) bool {
	for _, x := range rs {
		if (x.method == "" || x.method == r.Method) && x.re.MatchString(r.URL.Path) {
			return true
		}
	}
	return false
}

// roleGuard enforces the operator and auditor limits.
func (s *Server) roleGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := who(r)
		if s.need2FA(p) && !matches(setup2FAPaths, r) {
			writeErr(w, http.StatusForbidden, "2fa_required", "turn on two-factor sign-in first (your name, top right): this server requires it for staff")
			return
		}
		switch p.Role {
		case users.Operator:
			if matches(operatorDeny, r) {
				writeErr(w, http.StatusForbidden, "forbidden", "operators can't change accounts or server settings; ask an admin")
				return
			}
		case users.Auditor:
			read := r.Method == http.MethodGet || r.Method == http.MethodHead
			if read && matches(auditorDenyRead, r) || !read && !matches(auditorAllow, r) {
				writeErr(w, http.StatusForbidden, "forbidden", "auditors can look but not change anything")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
