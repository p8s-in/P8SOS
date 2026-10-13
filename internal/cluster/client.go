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

// DialVNC opens the raw VNC stream of a VM on node n (the worker bridges it
// to the VM's local VNC port after an HTTP Upgrade).
func (m *Manager) DialVNC(ctx context.Context, n Node, vm string) (net.Conn, error) {
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
	req, _ := http.NewRequest("GET", strings.TrimRight(n.API, "/")+"/api/v1/cluster/vnc/"+url.PathEscape(vm), nil)
	req.Header.Set("Authorization", "Bearer "+n.Token)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "p8s-vnc")
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
		return nil, fmt.Errorf("console on %s: %s", n.Name, msg)
	}
	return &bufConn{Conn: conn, r: br}, nil
}

// bufConn keeps bytes the response reader already buffered.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }
