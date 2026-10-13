package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"p8sos/internal/audit"
	"p8sos/internal/backup"
	"p8sos/internal/license"
	"p8sos/internal/users"
	"p8sos/internal/vm"
)

// principal is who is making a request.
type principal struct {
	Name     string // user name, or "api-token:<name>"
	Admin    bool
	Quota    users.Quota
	Session  string // the sign-in token (empty for API tokens)
	User     bool   // a P8S user account (can have a password / 2FA)
	ViaToken bool   // a personal API token (scripts): no account changes
	Cluster  bool   // the cluster master acting on this server (users live on the master)
	Role     string // users.Admin, Operator, Auditor or Customer (API tokens: admin)
	HasTOTP  bool   // two-factor sign-in is on
	ExtMFA   bool   // signs in with the company login (its 2FA is the provider's)
}

type ctxKey struct{}

func who(r *http.Request) principal {
	p, _ := r.Context().Value(ctxKey{}).(principal)
	return p
}

// requireAuth accepts a dashboard sign-in (session) or an API token. API tokens
// act as an admin (the `p8` command on the host uses one).
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		var p principal
		if ok && tok != "" {
			if s.Sessions != nil && s.Users != nil {
				if name, found := s.Sessions.User(tok); found {
					if u, err := s.Users.Get(name); err == nil && !u.Disabled {
						p = principal{Name: u.Name, Admin: users.Staff(u.Role), Role: u.Role, Quota: u.Quota, Session: tok, User: true,
							HasTOTP: u.TOTPSecret != "", ExtMFA: u.SSO != "" && !strings.HasPrefix(u.SSO, "ldap|")}
					}
				}
			}
			if p.Name == "" && s.Users != nil {
				if u, ok := s.Users.TokenUser(tok); ok {
					p = principal{Name: u.Name, Admin: users.Staff(u.Role), Role: u.Role, Quota: u.Quota, User: true, ViaToken: true}
				}
			}
			if p.Name == "" {
				if name, valid := s.tokenName(tok); valid {
					p = principal{Name: "api-token:" + name, Admin: true, Role: users.Admin}
				}
			}
			if p.Name == "" && s.Cluster != nil && s.Cluster.Accepts(tok) {
				p = principal{Name: "cluster-master", Admin: true, Cluster: true, Role: users.Admin}
			}
		}
		if p.Name == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="p8sd"`)
			writeErr(w, http.StatusUnauthorized, "unauthorized", "please sign in again")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)))
	})
}

func (s *Server) tokenName(tok string) (string, bool) {
	sum := sha256.Sum256([]byte(tok))
	got := []byte(hex.EncodeToString(sum[:]))
	name, match := "", 0
	for _, t := range s.Tokens { // check all to keep timing constant
		if subtle.ConstantTimeCompare(got, []byte(t.SHA256)) == 1 {
			match, name = 1, t.Name
		}
	}
	return name, match == 1
}

// audited records every change (anything but GET) with who did it.
func (s *Server) audited(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || s.Audit == nil {
			next.ServeHTTP(w, r)
			return
		}
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(sw, r)
		s.audit(r, who(r).Name, sw.code, "")
	})
}

func (s *Server) audit(r *http.Request, user string, status int, detail string) {
	if s.Audit == nil {
		return
	}
	err := s.Audit.Write(audit.Entry{User: user, IP: clientIP(r), Action: r.Method + " " + r.URL.Path, Status: status, Detail: detail})
	if err != nil && s.Log != nil {
		s.Log.Error("audit log", "err", err)
	}
}

// clientIP is the caller's address. Requests through the Cloudflare Tunnel
// arrive from cloudflared on this host; for those, Cloudflare's header has the
// real visitor (trusted only from loopback, where cloudflared runs).
func clientIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		h = r.RemoteAddr
	}
	if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
		if cf := net.ParseIP(strings.TrimSpace(r.Header.Get("Cf-Connecting-Ip"))); cf != nil {
			return cf.String()
		}
	}
	return h
}

func (s *Server) adminOnly(w http.ResponseWriter, r *http.Request) bool {
	if who(r).Admin {
		return true
	}
	writeErr(w, http.StatusForbidden, "forbidden", "only an admin can do that")
	return false
}

func (s *Server) usersOn(w http.ResponseWriter) bool {
	if s.Users != nil && s.Sessions != nil {
		return true
	}
	writeErr(w, http.StatusNotFound, "not_found", "user accounts are not enabled on this host")
	return false
}

// vmFor loads the VM named in the path, if the caller may use it. Someone
// else's VM looks exactly like a VM that doesn't exist.
func (s *Server) vmFor(w http.ResponseWriter, r *http.Request) (vm.VM, bool) {
	name := r.PathValue("name")
	v, err := s.VMs.Get(r.Context(), name)
	if err == nil && !mayUse(who(r), v.Owner) {
		err = fmt.Errorf("%w: vm %q", vm.ErrNotFound, name)
	}
	if err != nil {
		s.fail(w, r, err)
		return vm.VM{}, false
	}
	return v, true
}

func mayUse(p principal, owner string) bool { return p.Admin || (owner != "" && owner == p.Name) }

// --- quotas ---

// Usage is what a user has now.
type Usage struct {
	VMs       int `json:"vms"`
	VCPUs     int `json:"vcpus"`
	MemoryMiB int `json:"memory_mib"`
	DiskGB    int `json:"disk_gb"`
	Sites     int `json:"sites"`
	PublicIPs int `json:"public_ips"`
}

// usage is what a user has now, websites included.
func (s *Server) usage(vms []vm.VM, owner string) Usage {
	u := usageOf(vms, owner)
	if s.CTs != nil { // containers count like VMs
		if cts, err := s.CTs.List(context.Background()); err == nil {
			for _, c := range cts {
				if c.Owner == owner {
					u.VMs++
					u.VCPUs += c.VCPUs
					u.MemoryMiB += c.MemoryMiB
					u.DiskGB += c.DiskGB
					if c.PublicIP != "" {
						u.PublicIPs++
					}
				}
			}
		}
	}
	if s.Tunnel != nil {
		if l, err := s.Tunnel.Sites("", owner); err == nil {
			u.Sites = len(l)
		}
	}
	return u
}

func usageOf(vms []vm.VM, owner string) Usage {
	var u Usage
	for _, v := range vms {
		if v.Owner == owner {
			u.VMs++
			u.VCPUs += v.VCPUs
			u.MemoryMiB += v.MemoryMiB
			u.DiskGB += v.DiskGB
			if v.Public != nil {
				u.PublicIPs++
			}
		}
	}
	return u
}

// fits checks that adding (or growing by) d stays within q.
func fits(q users.Quota, now Usage, d Usage) error {
	var over []string
	if now.VMs+d.VMs > q.VMs {
		over = append(over, fmt.Sprintf("VMs %d of %d", now.VMs+d.VMs, q.VMs))
	}
	if now.VCPUs+d.VCPUs > q.VCPUs {
		over = append(over, fmt.Sprintf("CPUs %d of %d", now.VCPUs+d.VCPUs, q.VCPUs))
	}
	if now.MemoryMiB+d.MemoryMiB > q.MemoryMiB {
		over = append(over, fmt.Sprintf("RAM %d of %d MB", now.MemoryMiB+d.MemoryMiB, q.MemoryMiB))
	}
	if now.DiskGB+d.DiskGB > q.DiskGB {
		over = append(over, fmt.Sprintf("disk %d of %d GB", now.DiskGB+d.DiskGB, q.DiskGB))
	}
	if d.PublicIPs > 0 && now.PublicIPs+d.PublicIPs > q.PublicIPs {
		over = append(over, fmt.Sprintf("public IPs %d of %d", now.PublicIPs+d.PublicIPs, q.PublicIPs))
	}
	if d.Sites > 0 && now.Sites+d.Sites > q.Sites {
		over = append(over, fmt.Sprintf("websites %d of %d", now.Sites+d.Sites, q.Sites))
	}
	if len(over) > 0 {
		return fmt.Errorf("%w: that would go over your plan (%s)", errQuota, strings.Join(over, ", "))
	}
	return nil
}

var errQuota = errors.New("quota exceeded")

// quotaLock serializes check-then-create per user, so two requests at once
// can't both squeeze under the limit.
func (s *Server) quotaLock(user string) func() {
	m, _ := s.qlocks.LoadOrStore(user, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// checkQuota is for customers adding d to what they have.
func (s *Server) checkQuota(ctx context.Context, p principal, d Usage) error {
	if p.Admin {
		return nil
	}
	all, err := s.VMs.List(ctx)
	if err != nil {
		return err
	}
	return fits(p.Quota, s.usage(all, p.Name), d)
}

// --- sign in / out ---

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !s.usersOn(w) {
		return
	}
	var body struct {
		User     string `json:"user"`
		Password string `json:"password"`
		OTP      string `json:"otp"`
	}
	if !decode(w, r, &body) {
		return
	}
	name := strings.ToLower(strings.TrimSpace(body.User))
	keys := []string{"u:" + name, "ip:" + clientIP(r)}
	if wait, blocked := s.limiter().Blocked(keys...); blocked {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeErr(w, http.StatusTooManyRequests, "too_many_attempts",
			fmt.Sprintf("too many failed sign-ins; try again in %d minutes", int(wait.Minutes())+1))
		s.audit(r, name, http.StatusTooManyRequests, "sign-in blocked")
		return
	}
	if s.useDirectory(name) {
		s.ldapLogin(w, r, name, body.Password, body.OTP, keys)
		return
	}
	u, err := s.Users.Authenticate(name, body.Password, body.OTP)
	switch {
	case errors.Is(err, users.ErrOTPRequired):
		writeErr(w, http.StatusUnauthorized, "otp_required", err.Error())
		return
	case errors.Is(err, users.ErrBadLogin), errors.Is(err, users.ErrBadOTP):
		s.limiter().Fail(keys...)
		writeErr(w, http.StatusUnauthorized, "bad_login", err.Error())
		s.audit(r, name, http.StatusUnauthorized, "sign-in failed")
		return
	case err != nil:
		s.fail(w, r, err)
		return
	}
	if u.Email != "" && !u.Verified {
		writeErr(w, http.StatusForbidden, "unverified", users.ErrUnverified.Error())
		return
	}
	s.limiter().Reset(keys[0])
	tok, ttl := s.Sessions.New(u.Name)
	s.audit(r, u.Name, http.StatusOK, "signed in")
	writeJSON(w, http.StatusOK, map[string]any{"token": tok, "idle_timeout": int(ttl.Seconds()), "user": u})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if p := who(r); p.Session != "" && s.Sessions != nil {
		s.Sessions.End(p.Session)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) limiter() *users.Limiter {
	s.lmOnce.Do(func() {
		if s.Limiter == nil {
			s.Limiter = &users.Limiter{}
		}
	})
	return s.Limiter
}

// authInfo is public: tells the sign-in page which form to show.
func (s *Server) authInfo(w http.ResponseWriter, r *http.Request) {
	n := 0
	if s.Users != nil {
		n = s.Users.Count()
	}
	out := map[string]any{"accounts": s.Users != nil && s.Sessions != nil && n > 0 || s.LDAP != nil && s.LDAP.On(), "brand": "P8S OS"}
	if s.SSO != nil {
		if on, name := s.SSO.On(); on {
			out["sso"] = name
		}
	}
	if s.Cloud != nil {
		st := s.Cloud.Settings()
		out["brand"], out["signup"], out["currency"] = s.brand().Name, st.SignupOpen && n > 0 && s.License.Allows(license.FeatSelling), st.Currency
		out["reset"] = st.Email()
	}
	writeJSON(w, http.StatusOK, out)
}

// --- me ---

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	p := who(r)
	all, err := s.VMs.List(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := map[string]any{
		"name": p.Name, "role": users.Customer, "quota": p.Quota, "usage": s.usage(all, p.Name),
		"two_factor": false, "account": p.User, "lan": p.Admin || p.Quota.LAN,
		"accounts_on": s.Users != nil && s.Sessions != nil && s.Users.Count() > 0,
	}
	if p.Admin {
		out["role"] = p.Role
	}
	out["ssh_keys"] = []string{}
	out["via_token"] = p.ViaToken
	out["need_2fa"] = s.need2FA(p)
	if p.User {
		if u, err := s.Users.Get(p.Name); err == nil {
			out["email"], out["verified"], out["full_name"], out["phone"] = u.Email, u.Verified, u.FullName, u.Phone
			out["company"], out["gstin"], out["address"] = u.Company, u.GSTIN, u.Address
			out["two_factor"] = u.TOTPSecret != ""
			if u.SSHKeys != nil {
				out["ssh_keys"] = u.SSHKeys
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) accountOnly(w http.ResponseWriter, r *http.Request) bool {
	if p := who(r); p.User && !p.ViaToken {
		return true
	}
	writeErr(w, http.StatusBadRequest, "invalid", "signed in with an API token: there is no account password or 2FA to change")
	return false
}

func (s *Server) changeMyPassword(w http.ResponseWriter, r *http.Request) {
	if !s.accountOnly(w, r) {
		return
	}
	var body struct {
		Old string `json:"old_password"`
		New string `json:"new_password"`
	}
	if !decode(w, r, &body) {
		return
	}
	p := who(r)
	u, err := s.Users.Get(p.Name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !users.VerifyPassword(u.PassHash, body.Old) {
		writeErr(w, http.StatusForbidden, "bad_login", "the current password is wrong")
		return
	}
	if err := s.Users.SetPassword(p.Name, body.New); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Sessions.EndUser(p.Name, p.Session) // sign out everywhere else
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) setup2FA(w http.ResponseWriter, r *http.Request) {
	if !s.accountOnly(w, r) {
		return
	}
	secret, uri, err := s.Users.StartTOTP(who(r).Name, s.issuer())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"secret": secret, "uri": uri})
}

func (s *Server) enable2FA(w http.ResponseWriter, r *http.Request) {
	if !s.accountOnly(w, r) {
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &body) {
		return
	}
	if err := s.Users.ConfirmTOTP(who(r).Name, body.Code); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) disable2FA(w http.ResponseWriter, r *http.Request) {
	if !s.accountOnly(w, r) {
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &body) {
		return
	}
	if err := s.Users.DisableTOTP(who(r).Name, body.Code, false); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) issuer() string {
	if s.Issuer != "" {
		return s.Issuer
	}
	return "P8S OS"
}

// --- user admin ---

type userView struct {
	users.Public
	Usage Usage `json:"usage"`
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.usersOn(w) {
		return
	}
	list, err := s.Users.List()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	all, err := s.VMs.List(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := []userView{}
	for _, u := range list {
		out = append(out, userView{u, s.usage(all, u.Name)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

func (s *Server) addUser(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.usersOn(w) || s.userLimitReached(w) {
		return
	}
	var body struct {
		Name     string       `json:"name"`
		Role     string       `json:"role"`
		Password string       `json:"password"`
		Quota    *users.Quota `json:"quota"`
		Email    string       `json:"email"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Role == "" {
		body.Role = users.Customer
	}
	q := users.DefaultQuota
	if body.Quota != nil {
		q = *body.Quota
	}
	email := ""
	if strings.TrimSpace(body.Email) != "" {
		e, err := users.NormEmail(body.Email)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if s.Users.EmailTaken(e, "") {
			writeErr(w, http.StatusConflict, "exists", "another account uses that email")
			return
		}
		email = e
	}
	name := strings.ToLower(strings.TrimSpace(body.Name))
	u, err := s.Users.Add(name, body.Role, body.Password, q)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if email != "" { // an admin vouches for the address
		u, _ = s.Users.Update(name, func(x *users.User) error { x.Email, x.Verified = email, true; return nil })
	}
	writeJSON(w, http.StatusCreated, u)
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.usersOn(w) {
		return
	}
	var body struct {
		Role     *string      `json:"role"`
		Quota    *users.Quota `json:"quota"`
		Disabled *bool        `json:"disabled"`
		Password *string      `json:"password"`
		Reset2FA bool         `json:"reset_2fa"`
	}
	if !decode(w, r, &body) {
		return
	}
	name := r.PathValue("user")
	cur, err := s.Users.Get(name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	losesAdmin := cur.Role == users.Admin && !cur.Disabled &&
		((body.Role != nil && *body.Role != users.Admin) || (body.Disabled != nil && *body.Disabled))
	if losesAdmin && s.Users.AdminCount() <= 1 {
		writeErr(w, http.StatusConflict, "conflict", "this is the last admin; make another admin first")
		return
	}
	if body.Password != nil {
		if err := users.CheckPassword(*body.Password); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	u, err := s.Users.Update(name, func(u *users.User) error {
		if body.Role != nil {
			u.Role = *body.Role
		}
		if body.Quota != nil {
			u.Quota = *body.Quota
		}
		if body.Disabled != nil {
			u.Disabled = *body.Disabled
		}
		if body.Password != nil {
			u.PassHash = users.HashPassword(*body.Password)
		}
		if body.Reset2FA {
			u.TOTPSecret, u.TOTPPending, u.TOTPLast = "", "", 0
		}
		return nil
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if body.Password != nil || (body.Disabled != nil && *body.Disabled) {
		s.Sessions.EndUser(name, who(r).Session)
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.usersOn(w) {
		return
	}
	name := r.PathValue("user")
	cur, err := s.Users.Get(name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if cur.Role == users.Admin && !cur.Disabled && s.Users.AdminCount() <= 1 {
		writeErr(w, http.StatusConflict, "conflict", "this is the last admin; make another admin first")
		return
	}
	all, err := s.VMs.List(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if n := s.usage(all, name).VMs; n > 0 {
		writeErr(w, http.StatusConflict, "conflict", fmt.Sprintf("%s still has %d server(s) (VMs or containers); delete them or give them to someone else first", name, n))
		return
	}
	if err := s.Users.Delete(name); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Sessions.EndUser(name, "")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) setOwner(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	var body struct {
		Owner string `json:"owner"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Owner != "" && !who(r).Cluster { // the master checked its own accounts
		if s.Users == nil {
			writeErr(w, http.StatusBadRequest, "invalid", "user accounts are not enabled on this host")
			return
		}
		if _, err := s.Users.Get(body.Owner); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	v, err := s.VMs.SetOwner(r.Context(), r.PathValue("name"), body.Owner)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) auditLog(w http.ResponseWriter, r *http.Request) {
	p := who(r)
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if n <= 0 || n > 1000 {
		n = 200
	}
	user := r.URL.Query().Get("user")
	if !p.Admin {
		user = p.Name // customers see their own actions
	}
	list, err := s.Audit.Recent(n, user)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := map[string]any{"entries": list}
	if p.Admin && s.Audit != nil {
		out["retain_days"] = s.Audit.RetainDays
		if s.Audit.RetainDays <= 0 {
			out["retain_days"] = 180
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// auditExport: the Activity log from..to (YYYY-MM-DD, host time) as CSV.
func (s *Server) auditExport(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	var from, to time.Time
	var err error
	if v := r.URL.Query().Get("from"); v != "" {
		if from, err = time.ParseInLocation("2006-01-02", v, time.Local); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid", "from: use YYYY-MM-DD")
			return
		}
	}
	if v := r.URL.Query().Get("to"); v != "" {
		if to, err = time.ParseInLocation("2006-01-02", v, time.Local); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid", "to: use YYYY-MM-DD")
			return
		}
		to = to.Add(24 * time.Hour) // the whole day
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="p8s-activity.csv"`)
	if err := s.Audit.WriteCSV(w, from, to); err != nil && s.Log != nil {
		s.Log.Error("audit export", "err", err)
	}
}

// auditVerify checks the log's hash chain (nothing changed or removed).
func (s *Server) auditVerify(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	c, err := s.Audit.Verify()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// --- jobs and backups by owner ---

func (s *Server) noteJob(r *http.Request, id string) { s.jobOwners.Store(id, who(r).Name) }

func (s *Server) mayJob(r *http.Request, j vm.Job, owned map[string]bool) bool {
	p := who(r)
	if p.Admin {
		return true
	}
	if o, ok := s.jobOwners.Load(j.ID); ok {
		return o.(string) == p.Name
	}
	return owned[j.Target]
}

func (s *Server) ownedNames(ctx context.Context, p principal) map[string]bool {
	m := map[string]bool{}
	if p.Admin {
		return m
	}
	if all, err := s.VMs.List(ctx); err == nil {
		for _, v := range all {
			if v.Owner == p.Name {
				m[v.Name] = true
			}
		}
	}
	return m
}

// backupFor finds one backup the caller may use.
func (s *Server) backupFor(w http.ResponseWriter, r *http.Request) (vm.Backup, bool) {
	name, id := r.PathValue("name"), r.PathValue("id")
	list, err := s.VMs.ListBackups(name)
	if err == nil {
		for _, b := range list {
			if b.ID == id && b.VM == name && mayUse(who(r), b.Owner) {
				return b, true
			}
		}
		err = fmt.Errorf("%w: backup %s of %q", vm.ErrNotFound, id, name)
	}
	s.fail(w, r, err)
	return backup.Meta{}, false
}

// setMyKeys saves the SSH public keys put into the user's new VMs.
func (s *Server) setMyKeys(w http.ResponseWriter, r *http.Request) {
	if !s.accountOnly(w, r) {
		return
	}
	var body struct {
		Keys []string `json:"keys"`
	}
	if !decode(w, r, &body) {
		return
	}
	keys := []string{}
	for _, k := range body.Keys {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if !validKey(k) {
			writeErr(w, http.StatusBadRequest, "invalid", "that doesn't look like an SSH public key (ssh-ed25519 AAAA… or ssh-rsa AAAA…)")
			return
		}
		keys = append(keys, k)
	}
	if len(keys) > 10 {
		writeErr(w, http.StatusBadRequest, "invalid", "at most 10 keys")
		return
	}
	if _, err := s.Users.Update(who(r).Name, func(u *users.User) error { u.SSHKeys = keys; return nil }); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

func validKey(k string) bool {
	f := strings.Fields(k)
	if len(f) < 2 || len(k) > 16384 || strings.ContainsAny(k, "\r\n") {
		return false
	}
	switch f[0] {
	case "ssh-ed25519", "ssh-rsa", "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521",
		"sk-ssh-ed25519@openssh.com", "sk-ecdsa-sha2-nistp256@openssh.com":
	default:
		return false
	}
	_, err := base64.StdEncoding.DecodeString(f[1])
	return err == nil
}
