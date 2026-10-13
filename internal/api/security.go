package api

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"p8sos/internal/ldap"
	"p8sos/internal/secpol"
	"p8sos/internal/storage"
	"p8sos/internal/users"
)

// --- who may connect (Settings → Security → allowed networks) ---

// paths reachable from anywhere even with an allow-list (each has its own check)
var openPaths = map[string]bool{
	"/api/v1/billing/razorpay/webhook": true, // signed by Razorpay
	"/api/v1/cluster/register":         true, // one-time join code
	"/api/v1/cluster/invite":           true, // confirmed by an admin
	"/api/v1/health":                   true,
}

func (s *Server) ipAllow(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Security != nil && !openPaths[r.URL.Path] && !s.Security.Allowed(clientIP(r)) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeErr(w, http.StatusForbidden, "ip_blocked", "this address may not use this server")
			} else {
				http.Error(w, "This address may not use this P8S OS server.", http.StatusForbidden)
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- two-factor required for staff ---

// may still be used before 2FA is on
var setup2FAPaths = rules("GET /me", "POST /me/2fa/(setup|enable)", "POST /me/password", "PUT /me/profile", "POST /logout", "GET /auth")

func (s *Server) need2FA(p principal) bool {
	if s.Security == nil || !p.User || p.ViaToken || !users.Staff(p.Role) || p.HasTOTP || p.ExtMFA {
		return false
	}
	return s.Security.Load().Staff2FA
}

// --- settings ---

func (s *Server) getSecurity(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	if s.Security == nil {
		writeErr(w, http.StatusNotFound, "not_found", "security settings aren't available")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": s.Security.Load(), "your_ip": clientIP(r)})
}

func (s *Server) putSecurity(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	if s.Security == nil {
		writeErr(w, http.StatusNotFound, "not_found", "security settings aren't available")
		return
	}
	var body secpol.Settings
	if !decode(w, r, &body) {
		return
	}
	st, err := s.Security.Save(body, clientIP(r))
	if errors.Is(err, secpol.ErrInvalid) {
		writeErr(w, http.StatusBadRequest, "invalid", err.Error())
		return
	} else if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": st, "your_ip": clientIP(r)})
}

func ldapView(st ldap.Settings) map[string]any {
	has := st.BindPassword != ""
	st.BindPassword = ""
	return map[string]any{"settings": st, "has_password": has, "default_user_filter": ldap.DefaultUserFilter}
}

func (s *Server) getLDAP(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	if s.LDAP == nil {
		writeErr(w, http.StatusNotFound, "not_found", "directory sign-in isn't available")
		return
	}
	writeJSON(w, http.StatusOK, ldapView(s.LDAP.Settings()))
}

func (s *Server) putLDAP(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	if s.LDAP == nil {
		writeErr(w, http.StatusNotFound, "not_found", "directory sign-in isn't available")
		return
	}
	var body ldap.Settings
	if !decode(w, r, &body) {
		return
	}
	st, err := s.LDAP.SetSettings(body)
	if errors.Is(err, ldap.ErrInvalid) {
		writeErr(w, http.StatusBadRequest, "invalid", err.Error())
		return
	} else if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ldapView(st))
}

func (s *Server) testLDAP(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	if s.LDAP == nil {
		writeErr(w, http.StatusNotFound, "not_found", "directory sign-in isn't available")
		return
	}
	var body ldap.Settings
	if !decode(w, r, &body) {
		return
	}
	msg, err := s.LDAP.Test(r.Context(), body)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "directory", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": msg})
}

// --- sign-in through the directory ---

// useDirectory: the typed name has no local account, or one made by the directory.
func (s *Server) useDirectory(name string) bool {
	if s.LDAP == nil || !s.LDAP.On() {
		return false
	}
	u, err := s.Users.Get(name)
	return err != nil || strings.HasPrefix(u.SSO, "ldap|")
}

func (s *Server) ldapLogin(w http.ResponseWriter, r *http.Request, name, password, otp string, keys []string) {
	id, err := s.LDAP.Authenticate(r.Context(), name, password)
	if errors.Is(err, ldap.ErrBadLogin) {
		s.limiter().Fail(keys...)
		writeErr(w, http.StatusUnauthorized, "bad_login", users.ErrBadLogin.Error())
		s.audit(r, name, http.StatusUnauthorized, "sign-in failed (directory)")
		return
	}
	if err != nil {
		s.audit(r, name, http.StatusBadGateway, "directory: "+err.Error())
		writeErr(w, http.StatusBadGateway, "directory", "the company directory can't be reached right now: "+err.Error())
		return
	}
	u, err := s.externalAccount(r, extID{key: id.Key, email: id.Email, verified: id.Email != "", name: id.Name,
		username: id.Username, role: id.Role, via: "directory"})
	if errors.Is(err, errRefused) {
		s.audit(r, name, http.StatusForbidden, "sign-in refused: "+err.Error())
		writeErr(w, http.StatusForbidden, "forbidden", strings.TrimPrefix(err.Error(), "refused: "))
		return
	} else if err != nil {
		s.fail(w, r, err)
		return
	}
	switch err := s.Users.CheckOTP(u.Name, otp); {
	case errors.Is(err, users.ErrOTPRequired):
		writeErr(w, http.StatusUnauthorized, "otp_required", err.Error())
		return
	case errors.Is(err, users.ErrBadOTP):
		s.limiter().Fail(keys...)
		writeErr(w, http.StatusUnauthorized, "bad_login", err.Error())
		s.audit(r, u.Name, http.StatusUnauthorized, "sign-in failed (2FA code)")
		return
	case err != nil:
		s.fail(w, r, err)
		return
	}
	s.limiter().Reset(keys[0])
	tok, ttl := s.Sessions.New(u.Name)
	s.audit(r, u.Name, http.StatusOK, "signed in (directory)")
	writeJSON(w, http.StatusOK, map[string]any{"token": tok, "idle_timeout": int(ttl.Seconds()), "user": u.Public(),
		"note": fmt.Sprintf("signed in as %s through the company directory", u.Name)})
}

// --- encryption at rest ---

func (s *Server) getEncryption(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	if s.Encryption == nil {
		writeJSON(w, http.StatusOK, storage.Encryption{})
		return
	}
	e, err := s.Encryption(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

// encryptionKey hands the storage key to a full admin who confirms their
// password (or 2FA code for accounts that sign in elsewhere), for safe keeping.
func (s *Server) encryptionKey(w http.ResponseWriter, r *http.Request) {
	p := who(r)
	if !s.adminOnly(w, r) || p.Role != users.Admin {
		if p.Admin {
			writeErr(w, http.StatusForbidden, "forbidden", "only an admin can download the key")
		}
		return
	}
	var body struct {
		Password string `json:"password"`
		OTP      string `json:"otp"`
	}
	if !decode(w, r, &body) {
		return
	}
	if p.User {
		u, err := s.Users.Get(p.Name)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		ok := users.VerifyPassword(u.PassHash, body.Password)
		if !ok && u.SSO != "" && u.TOTPSecret != "" {
			ok = s.Users.CheckOTP(u.Name, body.OTP) == nil && body.OTP != ""
		}
		if !ok {
			s.audit(r, p.Name, http.StatusForbidden, "encryption key: confirmation failed")
			writeErr(w, http.StatusForbidden, "bad_login", "confirm with your password (or your 2FA code if you sign in with the company login)")
			return
		}
	}
	if s.Encryption == nil {
		writeErr(w, http.StatusNotFound, "not_found", "the VM storage isn't encrypted")
		return
	}
	e, err := s.Encryption(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !e.On || e.KeyFile == "" || !strings.HasPrefix(e.KeyFile, "/") {
		writeErr(w, http.StatusNotFound, "not_found", "the VM storage isn't encrypted with a key file")
		return
	}
	b, err := os.ReadFile(e.KeyFile)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r, p.Name, http.StatusOK, "encryption key downloaded")
	writeJSON(w, http.StatusOK, map[string]string{"key_hex": hex.EncodeToString(b), "dataset": e.Root, "key_file": e.KeyFile,
		"restore": fmt.Sprintf("echo %s | xxd -r -p > %s && chmod 400 %s && zfs load-key %s", hex.EncodeToString(b), e.KeyFile, e.KeyFile, e.Root)})
}
