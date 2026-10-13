// Package netx dials out to the internet robustly from p8sd.
//
// p8sd is a static Go program, so it resolves names with Go's own DNS client.
// Some home routers answer the system resolver (what `getent` and curl use)
// but not Go's queries, and many networks hand out IPv6 addresses without a
// working IPv6 route. Dial therefore tries IPv4 first and, when Go's lookup
// fails, asks the system resolver (`getent ahosts`) and then public DNS.
package netx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// ErrNoRoute means the site can't be reached from this network.
var ErrNoRoute = errors.New("can't reach the site")

var (
	// Getent runs the system resolver; tests replace it.
	Getent = func(ctx context.Context, host string) ([]byte, error) {
		return exec.CommandContext(ctx, "getent", "ahosts", host).Output()
	}
	publicDNS = []string{"1.1.1.1:53", "8.8.8.8:53", "9.9.9.9:53"}
)

// SystemLookup asks the system resolver; IPv4 addresses first.
func SystemLookup(ctx context.Context, host string) ([]net.IP, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	out, err := Getent(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("system resolver: %w", err)
	}
	var v4, v6 []net.IP
	seen := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || seen[f[0]] {
			continue
		}
		seen[f[0]] = true
		if ip := net.ParseIP(f[0]); ip != nil {
			if ip.To4() != nil {
				v4 = append(v4, ip)
			} else {
				v6 = append(v6, ip)
			}
		}
	}
	if len(v4)+len(v6) == 0 {
		return nil, fmt.Errorf("system resolver: no address for %s", host)
	}
	return append(v4, v6...), nil
}

func publicLookup(ctx context.Context, host string) []net.IP {
	for _, srv := range publicDNS {
		r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 4 * time.Second}).DialContext(ctx, "udp", srv)
		}}
		lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		ips, err := r.LookupIP(lctx, "ip4", host)
		cancel()
		if err == nil && len(ips) > 0 {
			return ips
		}
	}
	return nil
}

// Dial connects to addr (host:port): IPv4 first, then whatever works.
func Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	c, err4 := d.DialContext(ctx, "tcp4", addr)
	if err4 == nil {
		return c, nil
	}
	host, port, _ := net.SplitHostPort(addr)
	var dnsErr *net.DNSError
	if errors.As(err4, &dnsErr) && net.ParseIP(host) == nil {
		ips, serr := SystemLookup(ctx, host)
		if serr != nil {
			ips = publicLookup(ctx, host)
		}
		var lastDial error
		for _, ip := range ips {
			if c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), port)); err == nil {
				return c, nil
			} else {
				lastDial = err
			}
		}
		if len(ips) > 0 {
			return nil, unreachable(host, lastDial)
		}
	}
	c, err := d.DialContext(ctx, network, addr)
	if err != nil {
		if strings.Contains(err.Error(), "network is unreachable") {
			return nil, unreachable(host, fmt.Errorf("%v (IPv4: %v)", err, err4))
		}
		return nil, fmt.Errorf("%v (IPv4: %v)", err, err4)
	}
	return c, nil
}

func unreachable(host string, err error) error {
	return fmt.Errorf("%w: %s doesn't answer from this network (%v)", ErrNoRoute, host, err)
}

// Transport is an HTTP transport using Dial (honours HTTPS_PROXY like the default).
func Transport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = Dial
	return t
}

// Client is an HTTP client with Transport and the given timeout (0 = none).
func Client(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: Transport()}
}
