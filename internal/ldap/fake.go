package ldap

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"strings"
	"time"
)

// FakePerson is someone in a FakeDirectory.
type FakePerson struct {
	DN, Password, UID, Mail, Name, UUID string
	MemberOf                            []string
}

// FakeDirectory is a small LDAPS server for tests (and trying the settings
// page without a real directory): one service account and some people.
type FakeDirectory struct {
	BindDN, BindPassword string
	People               []FakePerson
	ln                   net.Listener
}

// Start listens on 127.0.0.1 and returns its ldaps:// address.
func (d *FakeDirectory) Start() (string, error) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fake-directory"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return "", err
	}
	cfg := &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	d.ln, err = tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		return "", err
	}
	go func() {
		for {
			c, err := d.ln.Accept()
			if err != nil {
				return
			}
			go d.serve(c)
		}
	}()
	return "ldaps://" + d.ln.Addr().String(), nil
}

// Close stops the server.
func (d *FakeDirectory) Close() { _ = d.ln.Close() }

func (d *FakeDirectory) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	for {
		p, err := readPacket(r)
		if err != nil || len(p.children) < 2 {
			return
		}
		id, op := p.children[0].int(), p.children[1]
		reply := func(o *packet) { _, _ = c.Write(seq(intVal(id), o).bytes()) }
		res := func(n byte, code int64) *packet { return app(n, true, enumVal(code), str(""), str("")) }
		switch op.tag {
		case classApplication | constructed | 0:
			dn, pw := op.children[1].str(), op.children[2].str()
			ok := dn == d.BindDN && pw == d.BindPassword && pw != ""
			for _, f := range d.People {
				ok = ok || strings.EqualFold(dn, f.DN) && pw == f.Password && pw != ""
			}
			reply(res(1, map[bool]int64{true: 0, false: 49}[ok]))
		case classApplication | constructed | 3:
			f := op.children[6].bytes()
			for _, f2 := range d.People {
				if f2.UID != "" && bytes.Contains(f, []byte(f2.UID)) && !bytes.Contains(f, []byte("member")) {
					var groups []*packet
					for _, g := range f2.MemberOf {
						groups = append(groups, str(g))
					}
					reply(app(4, true, str(f2.DN), seq(
						seq(str("mail"), &packet{tag: tagSet, children: []*packet{str(f2.Mail)}}),
						seq(str("displayName"), &packet{tag: tagSet, children: []*packet{str(f2.Name)}}),
						seq(str("uid"), &packet{tag: tagSet, children: []*packet{str(f2.UID)}}),
						seq(str("entryUUID"), &packet{tag: tagSet, children: []*packet{str(f2.UUID)}}),
						seq(str("memberOf"), &packet{tag: tagSet, children: groups}))))
				}
			}
			reply(res(5, 0))
		case classApplication | 2:
			return
		}
	}
}
