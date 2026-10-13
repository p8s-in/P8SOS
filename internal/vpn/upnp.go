package vpn

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// UPnP IGD: ask the home router for its internet address (to spot CGNAT)
// and, when the admin asks, forward the VPN port to this server.

// Router is the home router as UPnP sees it.
type Router struct {
	Found      bool   `json:"found"`
	Name       string `json:"name,omitempty"`
	ExternalIP string `json:"external_ip,omitempty"`
	CGNAT      bool   `json:"cgnat"` // the router itself has no public address
	control    string
	service    string
	localIP    string
}

// SSDPAddr is where routers answer discovery (tests replace it).
var SSDPAddr = "239.255.255.250:1900"

// FindRouter looks for a UPnP router on the local network (about 2 s).
func FindRouter(ctx context.Context) (Router, error) {
	c, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		return Router{}, err
	}
	defer c.Close()
	dst, err := net.ResolveUDPAddr("udp4", SSDPAddr)
	if err != nil {
		return Router{}, err
	}
	for _, st := range []string{"urn:schemas-upnp-org:device:InternetGatewayDevice:1", "urn:schemas-upnp-org:device:InternetGatewayDevice:2"} {
		msg := "M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nMAN: \"ssdp:discover\"\r\nMX: 2\r\nST: " + st + "\r\n\r\n"
		_, _ = c.WriteTo([]byte(msg), dst)
	}
	_ = c.SetReadDeadline(time.Now().Add(2500 * time.Millisecond))
	buf := make([]byte, 2048)
	for {
		n, _, err := c.ReadFrom(buf)
		if err != nil {
			return Router{}, nil // nobody answered: no UPnP
		}
		resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(buf[:n])), nil)
		if err != nil {
			continue
		}
		loc := resp.Header.Get("Location")
		if loc == "" {
			continue
		}
		r, err := describe(ctx, loc)
		if err != nil {
			continue
		}
		return r, nil
	}
}

type xmlService struct {
	Type    string `xml:"serviceType"`
	Control string `xml:"controlURL"`
}
type xmlDevice struct {
	Name     string       `xml:"friendlyName"`
	Services []xmlService `xml:"serviceList>service"`
	Devices  []xmlDevice  `xml:"deviceList>device"`
}

func (d xmlDevice) find() (xmlService, bool) {
	for _, s := range d.Services {
		if strings.Contains(s.Type, "WANIPConnection") || strings.Contains(s.Type, "WANPPPConnection") {
			return s, true
		}
	}
	for _, c := range d.Devices {
		if s, ok := c.find(); ok {
			return s, true
		}
	}
	return xmlService{}, false
}

func describe(ctx context.Context, loc string) (Router, error) {
	u, err := url.Parse(loc)
	if err != nil || u.Scheme != "http" {
		return Router{}, errors.New("bad location")
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsPrivate() && !ip.IsLoopback() { // only routers on our own network
		return Router{}, errors.New("not a local router")
	}
	cl := &http.Client{Timeout: 3 * time.Second}
	req, _ := http.NewRequestWithContext(ctx, "GET", loc, nil)
	resp, err := cl.Do(req)
	if err != nil {
		return Router{}, err
	}
	defer resp.Body.Close()
	var root struct {
		Device xmlDevice `xml:"device"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&root); err != nil {
		return Router{}, err
	}
	s, ok := root.Device.find()
	if !ok {
		return Router{}, errors.New("no WAN service")
	}
	ctl, err := u.Parse(s.Control)
	if err != nil {
		return Router{}, err
	}
	r := Router{Found: true, Name: root.Device.Name, control: ctl.String(), service: s.Type}
	// our address towards the router (for port mappings)
	if c, err := net.DialTimeout("udp4", u.Host, time.Second); err == nil {
		r.localIP = c.LocalAddr().(*net.UDPAddr).IP.String()
		c.Close()
	}
	out, err := r.soap(ctx, "GetExternalIPAddress", "")
	if err == nil {
		r.ExternalIP = tagValue(out, "NewExternalIPAddress")
		if ext := net.ParseIP(r.ExternalIP); ext != nil {
			_, cg, _ := net.ParseCIDR("100.64.0.0/10")
			r.CGNAT = ext.IsPrivate() || cg.Contains(ext)
		}
	}
	return r, nil
}

func tagValue(s, tag string) string {
	i := strings.Index(s, "<"+tag+">")
	j := strings.Index(s, "</"+tag+">")
	if i < 0 || j < i {
		return ""
	}
	return strings.TrimSpace(s[i+len(tag)+2 : j])
}

func (r Router) soap(ctx context.Context, action, args string) (string, error) {
	body := `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>` +
		`<u:` + action + ` xmlns:u="` + r.service + `">` + args + `</u:` + action + `></s:Body></s:Envelope>`
	req, _ := http.NewRequestWithContext(ctx, "POST", r.control, strings.NewReader(body))
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", `"`+r.service+`#`+action+`"`)
	resp, err := (&http.Client{Timeout: 4 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != 200 {
		return string(b), fmt.Errorf("router said HTTP %d %s", resp.StatusCode, tagValue(string(b), "errorDescription"))
	}
	return string(b), nil
}

// ForwardUDP asks the router to send UDP port to this server (0 lease = permanent).
func (r Router) ForwardUDP(ctx context.Context, port int, what string) error {
	if !r.Found || r.localIP == "" {
		return errors.New("no UPnP router found")
	}
	args := fmt.Sprintf(`<NewRemoteHost></NewRemoteHost><NewExternalPort>%d</NewExternalPort><NewProtocol>UDP</NewProtocol>`+
		`<NewInternalPort>%d</NewInternalPort><NewInternalClient>%s</NewInternalClient><NewEnabled>1</NewEnabled>`+
		`<NewPortMappingDescription>%s</NewPortMappingDescription><NewLeaseDuration>0</NewLeaseDuration>`, port, port, r.localIP, what)
	_, err := r.soap(ctx, "AddPortMapping", args)
	return err
}
