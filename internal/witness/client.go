package witness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Conf is how the cluster reaches its witness.
type Conf struct {
	URL     string `json:"url"`     // https://192.168.0.50:8007
	Token   string `json:"token"`   // shared secret
	Pin     string `json:"pin"`     // sha256 of its certificate (hex), printed by p8s-witness init
	Cluster string `json:"cluster"` // this cluster's id at the witness
}

// Client calls the witness, accepting only its pinned certificate.
type Client struct {
	Conf Conf
	HTTP *http.Client // tests; nil = pinned TLS client
}

// Fingerprint is the sha256 of a DER certificate as hex (what Pin holds).
func Fingerprint(der []byte) string {
	s := sha256.Sum256(der)
	return hex.EncodeToString(s[:])
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	pin := strings.ToLower(strings.ReplaceAll(c.Conf.Pin, ":", ""))
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, // the certificate is checked by its pin below instead
			VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
				if len(raw) == 0 || Fingerprint(raw[0]) != pin {
					return fmt.Errorf("the witness's certificate isn't the pinned one")
				}
				return nil
			},
		},
	}}
}

func (c *Client) do(ctx context.Context, method, path string, in any) (Reply, error) {
	var body *bytes.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	} else {
		body = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.Conf.URL, "/")+path, body)
	if err != nil {
		return Reply{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Conf.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http().Do(req)
	if err != nil {
		return Reply{}, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return Reply{}, fmt.Errorf("the witness refused the token")
	}
	var r Reply
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return Reply{}, fmt.Errorf("witness: %v (HTTP %d)", err, resp.StatusCode)
	}
	return r, nil
}

// Ping checks it answers (and tells how long ago it heard the master).
func (c *Client) Ping(ctx context.Context) (Reply, error) {
	return c.do(ctx, "GET", "/witness/ping?cluster="+c.Conf.Cluster, nil)
}

// Beat is the master's heartbeat.
func (c *Client) Beat(ctx context.Context, term int, master string) (Reply, error) {
	return c.do(ctx, "POST", "/witness/heartbeat", Heartbeat{Cluster: c.Conf.Cluster, Term: term, Master: master})
}

// Vote asks for its vote.
func (c *Client) Vote(ctx context.Context, term int, candidate string) (Reply, error) {
	return c.do(ctx, "POST", "/witness/vote", VoteRequest{Cluster: c.Conf.Cluster, Term: term, Candidate: candidate})
}
