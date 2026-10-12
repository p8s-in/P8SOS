package users

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TOTP per RFC 6238: HMAC-SHA1, 30 s steps, 6 digits (what every
// authenticator app uses by default).

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret returns a random 160-bit secret in base32.
func NewTOTPSecret() string {
	b := make([]byte, 20)
	_, _ = rand.Read(b)
	return b32.EncodeToString(b)
}

// TOTPURI is the link authenticator apps understand (also shown as text to type in).
func TOTPURI(issuer, account, secret string) string {
	v := url.Values{}
	v.Set("secret", secret)
	v.Set("issuer", issuer)
	v.Set("algorithm", "SHA1")
	v.Set("digits", "6")
	v.Set("period", "30")
	return "otpauth://totp/" + url.PathEscape(issuer+":"+account) + "?" + v.Encode()
}

// hotp is RFC 4226.
func hotp(key []byte, counter uint64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	m := hmac.New(sha1.New, key)
	m.Write(msg[:])
	h := m.Sum(nil)
	off := h[len(h)-1] & 0x0f
	v := binary.BigEndian.Uint32(h[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", v%1_000_000)
}

// TOTPCode is the code for time t.
func TOTPCode(secret string, t time.Time) (string, error) {
	key, err := b32.DecodeString(strings.ToUpper(strings.TrimRight(secret, "=")))
	if err != nil {
		return "", err
	}
	return hotp(key, uint64(t.Unix()/30)), nil
}

// CheckTOTP accepts the code for the current step or one step either side
// (clock drift), but never a step at or before `last` (replay). It returns
// the matched step.
func CheckTOTP(secret, code string, now time.Time, last int64) (int64, bool) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != 6 || secret == "" {
		return 0, false
	}
	key, err := b32.DecodeString(strings.ToUpper(strings.TrimRight(secret, "=")))
	if err != nil {
		return 0, false
	}
	cur := now.Unix() / 30
	for _, step := range []int64{cur - 1, cur, cur + 1} {
		if step <= last {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(hotp(key, uint64(step))), []byte(code)) == 1 {
			return step, true
		}
	}
	return 0, false
}
