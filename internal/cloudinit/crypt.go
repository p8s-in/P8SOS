package cloudinit

import (
	"crypto/rand"
	"crypto/sha512"
	"fmt"
	"strings"
)

// SHA-512 crypt ("$6$", Ulrich Drepper's spec), the hash format /etc/shadow
// uses. The VM's password is only ever written to its seed ISO in this form.

const itoa64 = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// HashPassword returns a $6$ hash with a random 16-character salt.
func HashPassword(password string) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	salt := make([]byte, 16)
	for i := range b {
		salt[i] = itoa64[int(b[i])%64]
	}
	return sha512Crypt(password, string(salt), 5000)
}

func sha512Crypt(password, salt string, rounds int) string {
	if len(salt) > 16 {
		salt = salt[:16]
	}
	p, s := []byte(password), []byte(salt)

	alt := sha512.Sum512(append(append(append([]byte{}, p...), s...), p...))

	a := sha512.New()
	a.Write(p)
	a.Write(s)
	i := len(p)
	for ; i > 64; i -= 64 {
		a.Write(alt[:])
	}
	a.Write(alt[:i])
	for i = len(p); i > 0; i >>= 1 {
		if i&1 != 0 {
			a.Write(alt[:])
		} else {
			a.Write(p)
		}
	}
	c := a.Sum(nil)

	dp := sha512.New()
	for range p {
		dp.Write(p)
	}
	pseq := repeatTo(dp.Sum(nil), len(p))

	ds := sha512.New()
	for range 16 + int(c[0]) {
		ds.Write(s)
	}
	sseq := repeatTo(ds.Sum(nil), len(s))

	for r := 0; r < rounds; r++ {
		h := sha512.New()
		if r&1 != 0 {
			h.Write(pseq)
		} else {
			h.Write(c)
		}
		if r%3 != 0 {
			h.Write(sseq)
		}
		if r%7 != 0 {
			h.Write(pseq)
		}
		if r&1 != 0 {
			h.Write(c)
		} else {
			h.Write(pseq)
		}
		c = h.Sum(nil)
	}

	var out strings.Builder
	out.WriteString("$6$")
	if rounds != 5000 {
		fmt.Fprintf(&out, "rounds=%d$", rounds)
	}
	out.WriteString(salt)
	out.WriteByte('$')
	order := [][3]int{{0, 21, 42}, {22, 43, 1}, {44, 2, 23}, {3, 24, 45}, {25, 46, 4}, {47, 5, 26}, {6, 27, 48},
		{28, 49, 7}, {50, 8, 29}, {9, 30, 51}, {31, 52, 10}, {53, 11, 32}, {12, 33, 54}, {34, 55, 13},
		{56, 14, 35}, {15, 36, 57}, {37, 58, 16}, {59, 17, 38}, {18, 39, 60}, {40, 61, 19}, {62, 20, 41}}
	for _, g := range order {
		b64(&out, uint(c[g[0]])<<16|uint(c[g[1]])<<8|uint(c[g[2]]), 4)
	}
	b64(&out, uint(c[63]), 2)
	return out.String()
}

func repeatTo(d []byte, n int) []byte {
	out := make([]byte, 0, n)
	for len(out)+len(d) <= n {
		out = append(out, d...)
	}
	return append(out, d[:n-len(out)]...)
}

func b64(sb *strings.Builder, w uint, n int) {
	for ; n > 0; n-- {
		sb.WriteByte(itoa64[w&0x3f])
		w >>= 6
	}
}
