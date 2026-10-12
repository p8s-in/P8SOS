// Package console lets a browser reach a VM's screen: short-lived one-time
// tickets, a minimal WebSocket server (RFC 6455, stdlib only) and a proxy from
// the WebSocket to the VM's VNC port on 127.0.0.1.
package console

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// --- tickets ---

// Tickets hands out one-time, short-lived console passes. Browsers can't send
// an Authorization header on a WebSocket, so the dashboard first asks for a
// ticket with its token, then opens the socket with the ticket.
type Tickets struct {
	TTL time.Duration // 0 = 30s
	Now func() time.Time

	mu sync.Mutex
	m  map[string]ticket
}

type ticket struct {
	vm      string
	expires time.Time
}

func (t *Tickets) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// Issue returns a new ticket for vm.
func (t *Tickets) Issue(vm string) (string, time.Duration) {
	ttl := t.TTL
	if ttl == 0 {
		ttl = 30 * time.Second
	}
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = map[string]ticket{}
	}
	now := t.now()
	for k, v := range t.m { // drop expired ones
		if now.After(v.expires) {
			delete(t.m, k)
		}
	}
	t.m[id] = ticket{vm: vm, expires: now.Add(ttl)}
	return id, ttl
}

// Redeem consumes a ticket. It works once, and only before it expires.
func (t *Tickets) Redeem(id string) (vm string, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	tk, ok := t.m[id]
	if !ok {
		return "", false
	}
	delete(t.m, id)
	if t.now().After(tk.expires) {
		return "", false
	}
	return tk.vm, true
}

// --- WebSocket ---

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

const (
	opCont   = 0x0
	opText   = 0x1
	opBinary = 0x2
	opClose  = 0x8
	opPing   = 0x9
	opPong   = 0xA
)

// maxFrame bounds a single client frame (noVNC sends small input events).
const maxFrame = 1 << 20

// Conn is a server-side WebSocket connection.
type Conn struct {
	c  net.Conn
	br *bufio.Reader
	wm sync.Mutex
}

// Upgrade performs the WebSocket handshake. If the client offers the "binary"
// subprotocol (noVNC does), it is selected.
func Upgrade(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	if !headerHas(r.Header, "Connection", "upgrade") || !headerHas(r.Header, "Upgrade", "websocket") {
		http.Error(w, "websocket upgrade required", http.StatusBadRequest)
		return nil, errors.New("not a websocket request")
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		w.Header().Set("Sec-WebSocket-Version", "13")
		http.Error(w, "unsupported websocket version", http.StatusUpgradeRequired)
		return nil, errors.New("bad websocket version")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if k, err := base64.StdEncoding.DecodeString(key); err != nil || len(k) != 16 {
		http.Error(w, "bad Sec-WebSocket-Key", http.StatusBadRequest)
		return nil, errors.New("bad websocket key")
	}
	proto := ""
	if headerHas(r.Header, "Sec-WebSocket-Protocol", "binary") {
		proto = "binary"
	}
	c, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, "websocket not supported here", http.StatusInternalServerError)
		return nil, err
	}
	_ = c.SetDeadline(time.Time{}) // the HTTP server's timeouts don't apply to a long-lived console
	sum := sha1.Sum([]byte(key + wsGUID))
	resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(sum[:]) + "\r\n"
	if proto != "" {
		resp += "Sec-WebSocket-Protocol: " + proto + "\r\n"
	}
	if _, err := io.WriteString(c, resp+"\r\n"); err != nil {
		c.Close()
		return nil, err
	}
	return &Conn{c: c, br: brw.Reader}, nil
}

func headerHas(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, p := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(p), token) {
				return true
			}
		}
	}
	return false
}

// ReadMessage returns the payload of the next data frame. Control frames are
// handled here: pings are answered, a close frame ends the stream with io.EOF.
func (c *Conn) ReadMessage() ([]byte, error) {
	for {
		fin, op, payload, err := c.readFrame()
		if err != nil {
			return nil, err
		}
		switch op {
		case opPing:
			if err := c.write(opPong, payload); err != nil {
				return nil, err
			}
		case opPong:
		case opClose:
			_ = c.write(opClose, nil)
			return nil, io.EOF
		case opText, opBinary, opCont:
			_ = fin // fragments are forwarded as they arrive; VNC is a byte stream
			return payload, nil
		default:
			return nil, fmt.Errorf("websocket: unknown opcode %d", op)
		}
	}
}

func (c *Conn) readFrame() (fin bool, op byte, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(c.br, h[:]); err != nil {
		return
	}
	fin, op = h[0]&0x80 != 0, h[0]&0x0f
	if h[1]&0x80 == 0 {
		return false, 0, nil, errors.New("websocket: client frame not masked")
	}
	n := uint64(h[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		if _, err = io.ReadFull(c.br, b[:]); err != nil {
			return
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err = io.ReadFull(c.br, b[:]); err != nil {
			return
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if n > maxFrame {
		return false, 0, nil, fmt.Errorf("websocket: frame of %d bytes is too large", n)
	}
	var mask [4]byte
	if _, err = io.ReadFull(c.br, mask[:]); err != nil {
		return
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return fin, op, payload, nil
}

// WriteBinary sends one binary message.
func (c *Conn) WriteBinary(p []byte) error { return c.write(opBinary, p) }

func (c *Conn) write(op byte, p []byte) error {
	hdr := make([]byte, 0, 10)
	hdr = append(hdr, 0x80|op)
	switch n := len(p); {
	case n < 126:
		hdr = append(hdr, byte(n))
	case n <= 0xffff:
		hdr = append(hdr, 126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	c.wm.Lock()
	defer c.wm.Unlock()
	if _, err := c.c.Write(hdr); err != nil {
		return err
	}
	_, err := c.c.Write(p)
	return err
}

// Close closes the connection.
func (c *Conn) Close() error { return c.c.Close() }

// Proxy copies bytes both ways between the WebSocket and a TCP stream until
// either side closes. It returns when both directions are done.
func Proxy(ws *Conn, tcp net.Conn) {
	var once sync.Once
	stop := func() { once.Do(func() { _ = ws.Close(); _ = tcp.Close() }) }
	done := make(chan struct{})
	go func() { // VM screen -> browser
		defer close(done)
		defer stop()
		buf := make([]byte, 32*1024)
		for {
			n, err := tcp.Read(buf)
			if n > 0 {
				if werr := ws.WriteBinary(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				_ = ws.write(opClose, nil)
				return
			}
		}
	}()
	for { // browser keyboard/mouse -> VM
		p, err := ws.ReadMessage()
		if err != nil {
			break
		}
		if _, err := tcp.Write(p); err != nil {
			break
		}
	}
	stop()
	<-done
}
