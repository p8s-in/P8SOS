package cluster

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// APIError is an error answer from another node's API.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string { return e.Message }

// call does one API request on node n as the cluster (admin rights there).
func (m *Manager) call(ctx context.Context, n Node, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(n.API, "/")+"/api/v1"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+n.Token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := m.client(n.CertSHA).Do(req)
	if err != nil {
		return fmt.Errorf("server %s (%s) can't be reached: %w", n.Name, n.ID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var e struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		_ = json.Unmarshal(b, &e)
		if e.Error.Message == "" {
			e.Error.Message = fmt.Sprintf("server %s answered HTTP %d", n.Name, resp.StatusCode)
		}
		return &APIError{Status: resp.StatusCode, Code: e.Error.Code, Message: e.Error.Message}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if w, ok := out.(*[]byte); ok {
		*w, err = io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Call does one API request on node n as the cluster (admin rights there).
func (m *Manager) Call(ctx context.Context, n Node, method, path string, in, out any) error {
	return m.call(ctx, n, method, path, in, out)
}

// DialVNC opens the raw VNC stream of a VM on node n (the worker bridges it
// to the VM's local VNC port after an HTTP Upgrade).
func (m *Manager) DialVNC(ctx context.Context, n Node, vm string) (net.Conn, error) {
	return m.dialUpgrade(ctx, n, "/api/v1/cluster/vnc/"+url.PathEscape(vm), "p8s-vnc")
}

// DialShell opens the root shell of node n (Server → Shell of a worker). The
// stream carries frames to the node (see ShellFrame) and raw output back.
func (m *Manager) DialShell(ctx context.Context, n Node, user string) (net.Conn, error) {
	return m.dialUpgrade(ctx, n, "/api/v1/cluster/shell?user="+url.QueryEscape(user), "p8s-shell")
}

func (m *Manager) dialUpgrade(ctx context.Context, n Node, path, proto string) (net.Conn, error) {
	u, err := url.Parse(n.API)
	if err != nil {
		return nil, err
	}
	d := &net.Dialer{Timeout: 10 * time.Second}
	raw, err := d.DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return nil, fmt.Errorf("server %s can't be reached: %w", n.Name, err)
	}
	conn := raw
	if u.Scheme == "https" {
		pin := n.CertSHA
		tc := tls.Client(raw, &tls.Config{InsecureSkipVerify: true, VerifyPeerCertificate: func(c [][]byte, _ [][]*x509.Certificate) error {
			if len(c) == 0 {
				return errors.New("no certificate")
			}
			sum := sha256.Sum256(c[0])
			if hex.EncodeToString(sum[:]) != pin {
				return errors.New("certificate doesn't match the cluster")
			}
			return nil
		}, NextProtos: []string{"http/1.1"}})
		if err := tc.HandshakeContext(ctx); err != nil {
			raw.Close()
			return nil, err
		}
		conn = tc
	}
	req, _ := http.NewRequest("GET", strings.TrimRight(n.API, "/")+path, nil)
	req.Header.Set("Authorization", "Bearer "+n.Token)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", proto)
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		msg := apiError(resp)
		conn.Close()
		return nil, fmt.Errorf("%s on %s: %s", proto, n.Name, msg)
	}
	return &bufConn{Conn: conn, r: br}, nil
}

// bufConn keeps bytes the response reader already buffered.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// Shell stream frames (master -> node): 1 byte kind, 2 bytes length, payload.
// Kind 0 = keystrokes, 1 = size as "cols rows".
const (
	ShellData   = 0
	ShellResize = 1
)

// WriteShellFrame writes one frame.
func WriteShellFrame(w io.Writer, kind byte, p []byte) error {
	for len(p) > 0 || kind == ShellResize {
		n := min(len(p), 65535)
		hdr := []byte{kind, byte(n >> 8), byte(n)}
		if _, err := w.Write(append(hdr, p[:n]...)); err != nil {
			return err
		}
		p = p[n:]
		if kind == ShellResize {
			break
		}
	}
	return nil
}

// ReadShellFrame reads one frame.
func ReadShellFrame(r io.Reader) (kind byte, p []byte, err error) {
	hdr := make([]byte, 3)
	if _, err = io.ReadFull(r, hdr); err != nil {
		return 0, nil, err
	}
	p = make([]byte, int(hdr[1])<<8|int(hdr[2]))
	_, err = io.ReadFull(r, p)
	return hdr[0], p, err
}

// stream does a request whose body or answer is a long data stream (VM
// moves): no overall timeout, a long wait for the answer after the upload.
func (m *Manager) stream(ctx context.Context, n Node, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(n.API, "/")+"/api/v1"+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+n.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	c := *m.client(n.CertSHA)
	c.Timeout = 0
	if t, ok := c.Transport.(*http.Transport); ok && m.HTTP == nil {
		t = t.Clone()
		t.ResponseHeaderTimeout = 10 * time.Minute
		c.Transport = t
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("server %s (%s) can't be reached: %w", n.Name, n.ID, err)
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, &APIError{Status: resp.StatusCode, Message: apiError(resp)}
	}
	return resp, nil
}
