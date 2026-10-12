package tunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Cloudflare is a tiny client for the parts of the Cloudflare API P8S uses:
// zones, remotely-managed tunnels and DNS records.
type Cloudflare struct {
	Token string
	Base  string       // default https://api.cloudflare.com/client/v4
	HTTP  *http.Client // default: 30 s timeout
}

// APIError is an error Cloudflare reported.
type APIError struct {
	Status int
	Msg    string
}

func (e *APIError) Error() string { return fmt.Sprintf("cloudflare (HTTP %d): %s", e.Status, e.Msg) }

func (c *Cloudflare) do(ctx context.Context, method, path string, body, out any) error {
	base := c.Base
	if base == "" {
		base = "https://api.cloudflare.com/client/v4"
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("cloudflare: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	var env struct {
		Success bool                       `json:"success"`
		Errors  []struct{ Message string } `json:"errors"`
		Result  json.RawMessage            `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return &APIError{resp.StatusCode, "unexpected answer: " + strings.TrimSpace(string(raw[:min(len(raw), 200)]))}
	}
	if !env.Success || resp.StatusCode >= 300 {
		var msgs []string
		for _, e := range env.Errors {
			msgs = append(msgs, e.Message)
		}
		if len(msgs) == 0 {
			msgs = []string{http.StatusText(resp.StatusCode)}
		}
		return &APIError{resp.StatusCode, strings.Join(msgs, "; ")}
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

// Zone finds a domain's zone and the account it belongs to.
func (c *Cloudflare) Zone(ctx context.Context, domain string) (zoneID, accountID string, err error) {
	var zs []struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Account struct {
			ID string `json:"id"`
		} `json:"account"`
	}
	if err := c.do(ctx, "GET", "/zones?name="+url.QueryEscape(domain), nil, &zs); err != nil {
		return "", "", err
	}
	for _, z := range zs {
		if strings.EqualFold(z.Name, domain) {
			return z.ID, z.Account.ID, nil
		}
	}
	return "", "", fmt.Errorf("the domain %s is not in this Cloudflare account (or the API token can't see it)", domain)
}

// FindTunnel returns the id of a live tunnel with this name, or "".
func (c *Cloudflare) FindTunnel(ctx context.Context, account, name string) (string, error) {
	var ts []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	q := url.Values{"name": {name}, "is_deleted": {"false"}}
	if err := c.do(ctx, "GET", "/accounts/"+account+"/cfd_tunnel?"+q.Encode(), nil, &ts); err != nil {
		return "", err
	}
	for _, t := range ts {
		if t.Name == name {
			return t.ID, nil
		}
	}
	return "", nil
}

// CreateTunnel makes a remotely-managed tunnel (its routes are set via the API).
func (c *Cloudflare) CreateTunnel(ctx context.Context, account, name string) (string, error) {
	var t struct {
		ID string `json:"id"`
	}
	err := c.do(ctx, "POST", "/accounts/"+account+"/cfd_tunnel", map[string]string{"name": name, "config_src": "cloudflare"}, &t)
	return t.ID, err
}

// TunnelToken is what `cloudflared service install` needs.
func (c *Cloudflare) TunnelToken(ctx context.Context, account, tunnel string) (string, error) {
	var tok string
	err := c.do(ctx, "GET", "/accounts/"+account+"/cfd_tunnel/"+tunnel+"/token", nil, &tok)
	return tok, err
}

// Ingress is one public hostname -> local service.
type Ingress struct {
	Hostname      string         `json:"hostname,omitempty"`
	Service       string         `json:"service"`
	OriginRequest map[string]any `json:"originRequest,omitempty"`
}

// PutIngress replaces the tunnel's routes (a 404 catch-all is added last).
func (c *Cloudflare) PutIngress(ctx context.Context, account, tunnel string, rules []Ingress) error {
	all := append(append([]Ingress{}, rules...), Ingress{Service: "http_status:404"})
	return c.do(ctx, "PUT", "/accounts/"+account+"/cfd_tunnel/"+tunnel+"/configurations",
		map[string]any{"config": map[string]any{"ingress": all}}, nil)
}

type dnsRecord struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
}

func (c *Cloudflare) records(ctx context.Context, zone, host string) ([]dnsRecord, error) {
	var rs []dnsRecord
	err := c.do(ctx, "GET", "/zones/"+zone+"/dns_records?name="+url.QueryEscape(host), nil, &rs)
	return rs, err
}

// ErrNameTaken means a DNS record that isn't ours already uses the name.
var ErrNameTaken = errors.New("name already in use")

// EnsureCNAME points host at the tunnel. An existing record for something
// else is never overwritten.
func (c *Cloudflare) EnsureCNAME(ctx context.Context, zone, host, tunnel string) error {
	target := tunnel + ".cfargotunnel.com"
	rs, err := c.records(ctx, zone, host)
	if err != nil {
		return err
	}
	for _, r := range rs {
		if r.Type == "CNAME" && strings.EqualFold(r.Content, target) {
			return nil
		}
		return fmt.Errorf("%w: %s already has a DNS record (%s %s) in Cloudflare", ErrNameTaken, host, r.Type, r.Content)
	}
	return c.do(ctx, "POST", "/zones/"+zone+"/dns_records", map[string]any{
		"type": "CNAME", "name": host, "content": target, "proxied": true, "comment": "P8S OS tunnel",
	}, nil)
}

// DeleteCNAME removes host's record if it points at the tunnel.
func (c *Cloudflare) DeleteCNAME(ctx context.Context, zone, host, tunnel string) error {
	rs, err := c.records(ctx, zone, host)
	if err != nil {
		return err
	}
	for _, r := range rs {
		if r.Type == "CNAME" && strings.EqualFold(r.Content, tunnel+".cfargotunnel.com") {
			if err := c.do(ctx, "DELETE", "/zones/"+zone+"/dns_records/"+r.ID, nil, nil); err != nil {
				return err
			}
		}
	}
	return nil
}
