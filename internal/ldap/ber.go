package ldap

import (
	"bufio"
	"errors"
	"fmt"
	"io"
)

// A tiny BER (ASN.1) encoder/decoder: just what LDAP v3 needs.

const (
	classUniversal   = 0x00
	classApplication = 0x40
	classContext     = 0x80
	constructed      = 0x20

	tagBoolean     = 0x01
	tagInteger     = 0x02
	tagOctetString = 0x04
	tagEnumerated  = 0x0a
	tagSequence    = 0x10 | constructed
	tagSet         = 0x11 | constructed
)

type packet struct {
	tag      byte
	value    []byte // primitive
	children []*packet
}

func (p *packet) constructed() bool { return p.tag&constructed != 0 }

func seq(children ...*packet) *packet { return &packet{tag: tagSequence, children: children} }
func app(n byte, cons bool, children ...*packet) *packet {
	t := classApplication | n
	if cons {
		t |= constructed
	}
	return &packet{tag: t, children: children}
}
func str(s string) *packet { return &packet{tag: tagOctetString, value: []byte(s)} }
func ctxStr(n byte, s string) *packet {
	return &packet{tag: classContext | n, value: []byte(s)}
}
func boolean(b bool) *packet {
	v := byte(0)
	if b {
		v = 0xff
	}
	return &packet{tag: tagBoolean, value: []byte{v}}
}
func integer(tag byte, n int64) *packet {
	var b []byte
	for {
		b = append([]byte{byte(n)}, b...)
		n >>= 8
		if (n == 0 && b[0]&0x80 == 0) || (n == -1 && b[0]&0x80 != 0) {
			break
		}
	}
	return &packet{tag: tag, value: b}
}
func intVal(n int64) *packet  { return integer(tagInteger, n) }
func enumVal(n int64) *packet { return integer(tagEnumerated, n) }

func (p *packet) bytes() []byte {
	var body []byte
	if p.constructed() {
		for _, c := range p.children {
			body = append(body, c.bytes()...)
		}
	} else {
		body = p.value
	}
	out := []byte{p.tag}
	n := len(body)
	switch {
	case n < 0x80:
		out = append(out, byte(n))
	case n < 0x100:
		out = append(out, 0x81, byte(n))
	case n < 0x10000:
		out = append(out, 0x82, byte(n>>8), byte(n))
	default:
		out = append(out, 0x84, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}
	return append(out, body...)
}

func (p *packet) int() int64 {
	var n int64
	for i, b := range p.value {
		if i == 0 && b&0x80 != 0 {
			n = -1
		}
		n = n<<8 | int64(b)
	}
	return n
}

func (p *packet) str() string { return string(p.value) }

const maxPacket = 16 << 20

// readPacket reads one BER element.
func readPacket(r *bufio.Reader) (*packet, error) {
	tag, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	l, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	n := int(l)
	if l&0x80 != 0 {
		k := int(l & 0x7f)
		if k == 0 || k > 4 {
			return nil, errors.New("ldap: bad length")
		}
		n = 0
		for i := 0; i < k; i++ {
			b, err := r.ReadByte()
			if err != nil {
				return nil, err
			}
			n = n<<8 | int(b)
		}
	}
	if n > maxPacket {
		return nil, fmt.Errorf("ldap: answer too big (%d bytes)", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return parse(tag, body)
}

func parse(tag byte, body []byte) (*packet, error) {
	p := &packet{tag: tag}
	if tag&constructed == 0 {
		p.value = body
		return p, nil
	}
	for len(body) > 0 {
		if len(body) < 2 {
			return nil, errors.New("ldap: truncated")
		}
		t, l := body[0], int(body[1])
		off := 2
		if l&0x80 != 0 {
			k := l & 0x7f
			if k == 0 || k > 4 || len(body) < 2+k {
				return nil, errors.New("ldap: bad length")
			}
			l = 0
			for i := 0; i < k; i++ {
				l = l<<8 | int(body[2+i])
			}
			off += k
		}
		if l < 0 || off+l > len(body) {
			return nil, errors.New("ldap: truncated")
		}
		c, err := parse(t, body[off:off+l])
		if err != nil {
			return nil, err
		}
		p.children = append(p.children, c)
		body = body[off+l:]
	}
	return p, nil
}
