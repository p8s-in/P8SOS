package tlsutil

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"strings"
	"sync"
	"time"
)

// Certs serves the right certificate for each connection: a Let's Encrypt
// certificate (files written by scripts/acme.sh, renewed by certbot) for its
// domain names, and the server's own certificate for everything else — the
// cluster servers connect by IP and pin that one, so it never changes under
// them. New files are picked up within a minute, without a restart.
type Certs struct {
	DefaultCert, DefaultKey string // the self-signed (or given) pair
	ACMECert, ACMEKey       string // Let's Encrypt pair; may not exist

	mu      sync.Mutex
	def     *tls.Certificate
	acme    *tls.Certificate
	names   []string
	checked time.Time
	mtime   time.Time
}

func (c *Certs) refresh() {
	if time.Since(c.checked) < time.Minute && c.def != nil {
		return
	}
	c.checked = time.Now()
	if c.def == nil {
		if k, err := tls.LoadX509KeyPair(c.DefaultCert, c.DefaultKey); err == nil {
			c.def = &k
		}
	}
	st, err := os.Stat(c.ACMECert)
	if err != nil {
		c.acme, c.names = nil, nil
		return
	}
	if st.ModTime().Equal(c.mtime) && c.acme != nil {
		return
	}
	k, err := tls.LoadX509KeyPair(c.ACMECert, c.ACMEKey)
	if err != nil {
		return // half-written: keep the old one, try again later
	}
	leaf, err := x509.ParseCertificate(k.Certificate[0])
	if err != nil {
		return
	}
	k.Leaf = leaf
	c.acme, c.names, c.mtime = &k, leaf.DNSNames, st.ModTime()
}

// GetCertificate is for tls.Config.
func (c *Certs) GetCertificate(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refresh()
	if c.acme != nil && h.ServerName != "" {
		sn := strings.ToLower(h.ServerName)
		for _, n := range c.names {
			if n == sn || (strings.HasPrefix(n, "*.") && strings.HasSuffix(sn, n[1:]) && strings.Count(sn, ".") == strings.Count(n, ".")) {
				return c.acme, nil
			}
		}
	}
	if c.def == nil {
		k, err := tls.LoadX509KeyPair(c.DefaultCert, c.DefaultKey)
		if err != nil {
			return nil, err
		}
		c.def = &k
	}
	return c.def, nil
}

// ACMEInfo is what the dashboard shows about the Let's Encrypt certificate.
type ACMEInfo struct {
	Names    []string `json:"names"`
	NotAfter string   `json:"not_after"`
	Issuer   string   `json:"issuer"`
}

// Info reads the Let's Encrypt certificate (ok=false: none).
func (c *Certs) Info() (ACMEInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checked = time.Time{}
	c.refresh()
	if c.acme == nil || c.acme.Leaf == nil {
		return ACMEInfo{}, false
	}
	return ACMEInfo{Names: c.names, NotAfter: c.acme.Leaf.NotAfter.UTC().Format(time.RFC3339), Issuer: c.acme.Leaf.Issuer.CommonName}, true
}
