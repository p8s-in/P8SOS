package console

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTickets(t *testing.T) {
	now := time.Unix(1000, 0)
	tk := &Tickets{Now: func() time.Time { return now }}
	id, ttl := tk.Issue("web1")
	if len(id) != 48 || ttl != 30*time.Second {
		t.Fatalf("ticket %q ttl %v", id, ttl)
	}
	if vm, ok := tk.Redeem(id); !ok || vm != "web1" {
		t.Fatal("first redeem should work")
	}
	if _, ok := tk.Redeem(id); ok {
		t.Fatal("ticket must be single-use")
	}
	id, _ = tk.Issue("web1")
	now = now.Add(31 * time.Second)
	if _, ok := tk.Redeem(id); ok {
		t.Fatal("expired ticket accepted")
	}
	if _, ok := tk.Redeem("nope"); ok {
		t.Fatal("unknown ticket accepted")
	}
}

// echoServer stands in for a VM's VNC port: it greets, then echoes.
func echoServer(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = c.Write([]byte("RFB 003.008\n"))
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return l.Addr().String()
}

type client struct {
	c  net.Conn
	br *bufio.Reader
}

func dial(t *testing.T, srvURL string, protocols string) (*client, *http.Response) {
	t.Helper()
	c, err := net.Dial("tcp", strings.TrimPrefix(srvURL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	req := "GET /ws HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: keep-alive, Upgrade\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + key + "\r\n"
	if protocols != "" {
		req += "Sec-WebSocket-Protocol: " + protocols + "\r\n"
	}
	_, _ = io.WriteString(c, req+"\r\n")
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha1.Sum([]byte(key + wsGUID))
	if resp.StatusCode == 101 && resp.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(sum[:]) {
		t.Fatal("bad accept header")
	}
	return &client{c, br}, resp
}

func (c *client) send(op byte, p []byte) {
	mask := []byte{1, 2, 3, 4}
	h := []byte{0x80 | op}
	switch {
	case len(p) < 126:
		h = append(h, 0x80|byte(len(p)))
	case len(p) <= 0xffff:
		h = append(h, 0x80|126, byte(len(p)>>8), byte(len(p)))
	default:
		h = append(h, 0x80|127)
		h = binary.BigEndian.AppendUint64(h, uint64(len(p)))
	}
	h = append(h, mask...)
	m := make([]byte, len(p))
	for i := range p {
		m[i] = p[i] ^ mask[i%4]
	}
	_, _ = c.c.Write(append(h, m...))
}

func (c *client) recv(t *testing.T) (byte, []byte) {
	t.Helper()
	_ = c.c.SetReadDeadline(time.Now().Add(3 * time.Second))
	var h [2]byte
	if _, err := io.ReadFull(c.br, h[:]); err != nil {
		t.Fatal(err)
	}
	if h[1]&0x80 != 0 {
		t.Fatal("server frames must not be masked")
	}
	n := int(h[1] & 0x7f)
	if n == 126 {
		var b [2]byte
		_, _ = io.ReadFull(c.br, b[:])
		n = int(binary.BigEndian.Uint16(b[:]))
	} else if n == 127 {
		var b [8]byte
		_, _ = io.ReadFull(c.br, b[:])
		n = int(binary.BigEndian.Uint64(b[:]))
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(c.br, p); err != nil {
		t.Fatal(err)
	}
	return h[0] & 0x0f, p
}

// recvBytes reads binary frames until n bytes have arrived.
func (c *client) recvBytes(t *testing.T, n int) []byte {
	var out []byte
	for len(out) < n {
		op, p := c.recv(t)
		if op != opBinary {
			t.Fatalf("got opcode %d, want binary", op)
		}
		out = append(out, p...)
	}
	return out
}

func TestProxy(t *testing.T) {
	vnc := echoServer(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := Upgrade(w, r)
		if err != nil {
			return
		}
		tcp, err := net.Dial("tcp", vnc)
		if err != nil {
			ws.Close()
			return
		}
		Proxy(ws, tcp)
	}))
	defer srv.Close()

	c, resp := dial(t, srv.URL, "binary, base64")
	if resp.StatusCode != 101 || resp.Header.Get("Sec-WebSocket-Protocol") != "binary" {
		t.Fatalf("handshake: %d proto=%q", resp.StatusCode, resp.Header.Get("Sec-WebSocket-Protocol"))
	}
	if got := c.recvBytes(t, 12); string(got) != "RFB 003.008\n" {
		t.Fatalf("greeting %q", got)
	}
	// small, medium (16-bit length) and large (64-bit length) frames round-trip
	for _, size := range []int{5, 300, 70000} {
		msg := bytes.Repeat([]byte{byte(size)}, size)
		c.send(opBinary, msg)
		if got := c.recvBytes(t, size); !bytes.Equal(got, msg) {
			t.Fatalf("size %d: echo mismatch", size)
		}
	}
	c.send(opPing, []byte("hi"))
	if op, p := c.recv(t); op != opPong || string(p) != "hi" {
		t.Fatalf("ping -> %d %q", op, p)
	}
	c.send(opClose, nil)
	if op, _ := c.recv(t); op != opClose {
		t.Fatalf("close -> %d", op)
	}
}

func TestUpgradeRejectsPlainRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ws, err := Upgrade(w, r); err == nil {
			ws.Close()
		}
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("plain GET: %d", resp.StatusCode)
	}
}

func TestUnmaskedFrameRejected(t *testing.T) {
	vnc := echoServer(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := Upgrade(w, r)
		if err != nil {
			return
		}
		tcp, _ := net.Dial("tcp", vnc)
		Proxy(ws, tcp)
	}))
	defer srv.Close()
	c, _ := dial(t, srv.URL, "")
	c.recvBytes(t, 12)
	_, _ = c.c.Write([]byte{0x82, 0x01, 'x'}) // no mask bit
	_ = c.c.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		if _, err := c.br.ReadByte(); err != nil {
			return // server hung up: good
		}
	}
}
