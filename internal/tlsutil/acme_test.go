package tlsutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writePair(t *testing.T, dir, base string, names ...string) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: names[0]},
		DNSNames: names, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(90 * 24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	_ = os.WriteFile(filepath.Join(dir, base+".crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	_ = os.WriteFile(filepath.Join(dir, base+".key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
}

func TestCertsBySNI(t *testing.T) {
	dir := t.TempDir()
	dc, dk, err := Ensure(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := &Certs{DefaultCert: dc, DefaultKey: dk, ACMECert: filepath.Join(dir, "acme.crt"), ACMEKey: filepath.Join(dir, "acme.key")}
	def, _ := c.GetCertificate(&tls.ClientHelloInfo{ServerName: "p8s.example.com"})
	if _, ok := c.Info(); ok || def == nil {
		t.Fatal("no acme cert yet: the default one")
	}
	writePair(t, dir, "acme", "p8s.example.com")
	if _, ok := c.Info(); !ok {
		t.Fatal("acme cert picked up")
	}
	got, _ := c.GetCertificate(&tls.ClientHelloInfo{ServerName: "P8S.example.com"})
	if got == def {
		t.Fatal("the domain gets the Let's Encrypt cert")
	}
	if ip, _ := c.GetCertificate(&tls.ClientHelloInfo{}); ip != def {
		t.Fatal("connections by IP (the cluster) keep the pinned cert")
	}
	if o, _ := c.GetCertificate(&tls.ClientHelloInfo{ServerName: "other.example.com"}); o != def {
		t.Fatal("other names: default")
	}
}
