package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"strings"

	"p8sos/internal/tasks"
	"p8sos/internal/users"
)

// Boot security (admins, Server page → Storage): Secure Boot, unlocking an
// encrypted system over SSH at start, the disk passphrase.
//
//	GET    /node/boot-security                  {uefi, secure_boot, mok, zfs_signer, sb_entry, encrypted, unlock_ssh, unlock_keys}
//	POST   /node/boot-security/secureboot       → task (signed boot loader, signed ZFS module, key enrolment)
//	POST   /node/boot-security/unlock-keys      {"key"} → task
//	DELETE /node/boot-security/unlock-keys/{n}  → task
//	POST   /node/boot-security/passphrase       {"password" (yours) | "otp", "passphrase"} (full admins)

// BootSecCmd runs scripts/bootsec.sh with args (stdin fed when not "").
type BootSecCmd func(ctx context.Context, stdin string, args ...string) ([]byte, error)

var reSSHKey = regexp.MustCompile(`^(ssh-(ed25519|rsa)|ecdsa-sha2-nistp(256|384|521)) [A-Za-z0-9+/=]{20,} ?[^\r\n]{0,100}$`)

func (s *Server) bootSecOn(w http.ResponseWriter, r *http.Request) bool {
	if !s.nodeOn(w, r) {
		return false
	}
	if s.BootSec == nil {
		writeErr(w, http.StatusNotFound, "not_found", "boot security isn't available on this host")
		return false
	}
	return true
}

func (s *Server) getBootSec(w http.ResponseWriter, r *http.Request) {
	if !s.bootSecOn(w, r) {
		return
	}
	out, err := s.BootSec(r.Context(), "", "json")
	line := ""
	for _, l := range strings.Split(string(out), "\n") { // stray warnings may come before it
		if strings.HasPrefix(strings.TrimSpace(l), "{") {
			line = l
		}
	}
	var st map[string]any
	if err != nil || json.Unmarshal([]byte(line), &st) != nil {
		writeErr(w, http.StatusBadGateway, "unavailable", "couldn't read the boot state: "+strings.TrimSpace(string(out)))
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) secureBootSetup(w http.ResponseWriter, r *http.Request) {
	if s.bootSecOn(w, r) {
		s.taskStarted(w, r, tasks.SecureBoot, "")
	}
}

func (s *Server) addUnlockKey(w http.ResponseWriter, r *http.Request) {
	if !s.bootSecOn(w, r) {
		return
	}
	var b struct {
		Key string `json:"key"`
	}
	if !decode(w, r, &b) {
		return
	}
	b.Key = strings.TrimSpace(b.Key)
	if !reSSHKey.MatchString(b.Key) {
		writeErr(w, http.StatusBadRequest, "invalid", "paste one SSH public key (ssh-ed25519 AAAA… you@pc)")
		return
	}
	if err := os.MkdirAll(strings.TrimSuffix(s.Node.Tasks.UnlockKeyFile(), "/boot-unlock.key"), 0o700); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := os.WriteFile(s.Node.Tasks.UnlockKeyFile(), []byte(b.Key+"\n"), 0o600); err != nil {
		s.fail(w, r, err)
		return
	}
	s.taskStarted(w, r, tasks.UnlockAdd, "")
}

func (s *Server) removeUnlockKey(w http.ResponseWriter, r *http.Request) {
	if s.bootSecOn(w, r) {
		s.taskStarted(w, r, tasks.UnlockRemove, r.PathValue("n"))
	}
}

// changePassphrase: a full admin who confirms their own password sets a new
// disk passphrase (never stored, never logged).
func (s *Server) changePassphrase(w http.ResponseWriter, r *http.Request) {
	p := who(r)
	if !s.bootSecOn(w, r) {
		return
	}
	if p.Role != users.Admin {
		writeErr(w, http.StatusForbidden, "forbidden", "only an admin can change the disk passphrase")
		return
	}
	var b struct {
		Password   string `json:"password"`
		OTP        string `json:"otp"`
		Passphrase string `json:"passphrase"`
	}
	if !decode(w, r, &b) {
		return
	}
	if p.User {
		u, err := s.Users.Get(p.Name)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		ok := users.VerifyPassword(u.PassHash, b.Password)
		if !ok && u.SSO != "" && u.TOTPSecret != "" {
			ok = b.OTP != "" && s.Users.CheckOTP(u.Name, b.OTP) == nil
		}
		if !ok {
			s.audit(r, p.Name, http.StatusForbidden, "disk passphrase: confirmation failed")
			writeErr(w, http.StatusForbidden, "bad_login", "confirm with your password (or your 2FA code if you sign in with the company login)")
			return
		}
	}
	if len(b.Passphrase) < 8 || len(b.Passphrase) > 512 || strings.ContainsAny(b.Passphrase, "\r\n") {
		writeErr(w, http.StatusBadRequest, "invalid", "the passphrase needs 8 or more characters (no line breaks)")
		return
	}
	if out, err := s.BootSec(r.Context(), b.Passphrase+"\n", "passphrase-stdin"); err != nil {
		writeErr(w, http.StatusConflict, "failed", "changing the passphrase failed: "+strings.TrimSpace(string(out)))
		return
	}
	s.audit(r, p.Name, http.StatusOK, "disk passphrase changed")
	writeJSON(w, http.StatusOK, map[string]string{"note": "Changed. The server asks for the new passphrase at the next start."})
}
