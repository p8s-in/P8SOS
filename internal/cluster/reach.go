package cluster

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"p8sos/internal/netx"
	"strings"
	"time"
)

// PublicIP asks a public service which address this network has on the
// internet (to fill in the "other network" form).
func PublicIP(ctx context.Context) (string, error) {
	c := netx.Client(6 * time.Second)
	var last error
	for _, u := range []string{"https://api.ipify.org", "https://ifconfig.me/ip", "https://icanhazip.com"} {
		req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
		resp, err := c.Do(req)
		if err != nil {
			last = err
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 100))
		resp.Body.Close()
		ip := strings.TrimSpace(string(b))
		if net.ParseIP(ip) != nil {
			return ip, nil
		}
	}
	return "", fmt.Errorf("couldn't find this network's public address: %v", last)
}

// CheckTCP tries to connect to host:port (e.g. the master's public address
// from inside; many routers allow that "hairpin", some don't).
func CheckTCP(ctx context.Context, host string, port int) error {
	d := net.Dialer{Timeout: 5 * time.Second}
	c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, fmt.Sprint(port)))
	if err != nil {
		return err
	}
	return c.Close()
}

// PrivateAddr reports whether host is a LAN/CGNAT address (not reachable from the internet).
func PrivateAddr(host string) bool {
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return false
	}
	ip := ips[0]
	_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || cgnat.Contains(ip)
}
