package cluster

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Finding servers on the local network: the master broadcasts a question on
// UDP 51821, every P8S OS server answers with its name and whether it can
// join. Adding one sends it an invitation that its admin must accept (with a
// 6-digit number shown on both screens), so nothing joins just by being found.

const (
	DiscoverPort = 51821
	discoverAsk  = "P8S-DISCOVER/1 "
	inviteTTL    = 15 * time.Minute
)

// Found is a server that answered on the local network.
type Found struct {
	IP        string `json:"ip"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	Role      string `json:"role"` // "" = can join
	APIPort   string `json:"api_port"`
	CertSHA   string `json:"cert_sha"`
	CPUs      int    `json:"cpus"`
	MemMiB    int    `json:"mem_total_mib"`
	Available bool   `json:"available"`
	Why       string `json:"why,omitempty"` // why it can't join
}

type discoverReply struct {
	Nonce   string `json:"nonce"`
	Name    string `json:"name"`
	Version string `json:"version"`
	Role    string `json:"role"`
	APIPort string `json:"api_port"`
	CertSHA string `json:"cert_sha"`
	CPUs    int    `json:"cpus"`
	MemMiB  int    `json:"mem_total_mib"`
}

// Respond answers discovery questions (every server runs it).
func (m *Manager) Respond(ctx context.Context, version string) error {
	pc, err := net.ListenPacket("udp4", fmt.Sprintf(":%d", m.dport()))
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); pc.Close() }()
	buf := make([]byte, 512)
	var last time.Time
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		msg := string(buf[:n])
		if !strings.HasPrefix(msg, discoverAsk) || time.Since(last) < 200*time.Millisecond {
			continue // not ours, or answering too often
		}
		last = time.Now()
		nonce := strings.TrimSpace(strings.TrimPrefix(msg, discoverAsk))
		if len(nonce) > 64 {
			continue
		}
		pin, _ := CertSHA(m.CertFile)
		rep := discoverReply{Nonce: nonce, Name: m.SelfName(), Version: version, Role: m.Role(), APIPort: m.port(), CertSHA: pin}
		if m.LocalCapacity != nil {
			c := m.LocalCapacity(ctx)
			rep.CPUs, rep.MemMiB = c.CPUs, c.MemTotalMiB
		}
		b, _ := json.Marshal(rep)
		_, _ = pc.WriteTo(b, from)
	}
}

// broadcasts lists the broadcast address of every IPv4 network this server is on.
func broadcasts() []string {
	out := []string{"255.255.255.255"}
	ifs, _ := net.Interfaces()
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || ifc.Name == Iface || strings.HasPrefix(ifc.Name, "vnet") ||
			strings.HasPrefix(ifc.Name, "veth") || strings.HasPrefix(ifc.Name, "virbr") {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok || n.IP.To4() == nil {
				continue
			}
			ip := n.IP.To4()
			b := make(net.IP, 4)
			for i := range ip {
				b[i] = ip[i] | ^n.Mask[i]
			}
			out = append(out, b.String())
		}
	}
	return out
}

// Discover asks the local network which P8S OS servers are there (about 2 s).
func (m *Manager) Discover(ctx context.Context, version string, targets ...string) ([]Found, error) {
	pc, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		return nil, err
	}
	defer pc.Close()
	if len(targets) == 0 {
		targets = broadcasts()
	}
	nb := make([]byte, 8)
	_, _ = rand.Read(nb)
	nonce := hex.EncodeToString(nb)
	for _, t := range targets {
		addr, err := net.ResolveUDPAddr("udp4", net.JoinHostPort(t, fmt.Sprint(m.dport())))
		if err != nil {
			continue
		}
		_, _ = pc.WriteTo([]byte(discoverAsk+nonce), addr)
	}
	selfPin, _ := CertSHA(m.CertFile)
	members, names := map[string]bool{}, map[string]bool{m.SelfName(): true}
	for _, n := range m.Workers() {
		members[n.CertSHA] = true
		names[n.Name] = true
	}
	seen := map[string]Found{}
	deadline := time.Now().Add(2 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = pc.SetReadDeadline(deadline)
	buf := make([]byte, 2048)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			break
		}
		var r discoverReply
		if json.Unmarshal(buf[:n], &r) != nil || r.Nonce != nonce || r.CertSHA == selfPin || len(r.CertSHA) != 64 {
			continue
		}
		ip := from.(*net.UDPAddr).IP.String()
		f := Found{IP: ip, Name: r.Name, Version: r.Version, Role: r.Role, APIPort: r.APIPort, CertSHA: r.CertSHA, CPUs: r.CPUs, MemMiB: r.MemMiB}
		switch {
		case members[r.CertSHA]:
			f.Why = "already in this cluster"
		case r.Role != "":
			f.Why = "already in another cluster (as " + r.Role + ")"
		case names[r.Name]:
			f.Why = "has the same name as a server of this cluster; rename it first (on it: Server page → Rename)"
		case version != "" && r.Version != "" && r.Version != version:
			f.Why = "runs P8S OS " + r.Version + " (this server: " + version + "); update both first"
		default:
			f.Available = true
		}
		seen[r.CertSHA] = f
	}
	out := make([]Found, 0, len(seen))
	for _, f := range seen {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Available != out[j].Available {
			return out[i].Available
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// --- invitations (received by a server that may join) ---

// Invite is a pending request from a master to add this server.
type Invite struct {
	ID         string `json:"id"`
	MasterName string `json:"master_name"`
	MasterURL  string `json:"master_url"`
	From       string `json:"from"`    // the address it came from
	Confirm    string `json:"confirm"` // 6 digits, also shown on the master
	Expires    string `json:"expires"`
	code       string
}

type inviteBox struct {
	mu     sync.Mutex
	list   []*Invite
	last   time.Time
	loaded bool
}

// box returns the open invitations. They are kept in data_dir/cluster/invites.json
// (0600: they hold join codes) so a p8sd restart — a rename, an update — doesn't lose them.
// Callers lock b.mu; box itself loads the file once.
func (m *Manager) box() *inviteBox {
	b := &m.inv
	b.mu.Lock()
	if !b.loaded && m.Dir != "" {
		b.loaded = true
		var saved []savedInvite
		if raw, err := os.ReadFile(filepath.Join(m.Dir, "invites.json")); err == nil && json.Unmarshal(raw, &saved) == nil {
			for _, s := range saved {
				i := s.Invite
				i.code = s.Code
				b.list = append(b.list, &i)
			}
		}
	}
	b.mu.Unlock()
	return b
}

type savedInvite struct {
	Invite
	Code string `json:"code"`
}

// saveInvites writes the list (b.mu held).
func (m *Manager) saveInvites(b *inviteBox) {
	if m.Dir == "" {
		return
	}
	l := make([]savedInvite, 0, len(b.list))
	for _, i := range b.list {
		l = append(l, savedInvite{Invite: *i, Code: i.code})
	}
	raw, _ := json.Marshal(l)
	_ = os.MkdirAll(m.Dir, 0o700)
	p := filepath.Join(m.Dir, "invites.json")
	if os.WriteFile(p+".tmp", raw, 0o600) == nil {
		_ = os.Rename(p+".tmp", p)
	}
}

// ConfirmNumber is the 6-digit number both screens show for a join code.
func ConfirmNumber(code string) string {
	sum := sha256.Sum256([]byte("p8s-invite:" + code))
	return fmt.Sprintf("%06d", binary.BigEndian.Uint32(sum[:4])%1000000)
}

// ReceiveInvite keeps an invitation until the admin accepts or declines it.
func (m *Manager) ReceiveInvite(masterName, masterURL, code, from string) (Invite, error) {
	if m.Role() != "" {
		return Invite{}, fmt.Errorf("%w: this server is already in a cluster", ErrRole)
	}
	if !reNodeName.MatchString(masterName) || !strings.HasPrefix(masterURL, "https://") || len(masterURL) > 200 || len(code) < 40 || len(code) > 120 {
		return Invite{}, fmt.Errorf("%w: invitation", ErrInvalid)
	}
	b := m.box()
	b.mu.Lock()
	defer b.mu.Unlock()
	now := m.now()
	if now.Sub(b.last) < 2*time.Second {
		return Invite{}, fmt.Errorf("%w: too many invitations; try again in a moment", ErrInvalid)
	}
	b.last = now
	keep := b.list[:0]
	for _, i := range b.list {
		if t, _ := time.Parse(time.RFC3339, i.Expires); t.After(now) && i.MasterURL != masterURL {
			keep = append(keep, i)
		}
	}
	b.list = keep
	if len(b.list) >= 5 {
		return Invite{}, fmt.Errorf("%w: too many open invitations", ErrInvalid)
	}
	inv := &Invite{ID: randToken(5), MasterName: masterName, MasterURL: masterURL, From: from, Confirm: ConfirmNumber(code),
		Expires: now.Add(inviteTTL).UTC().Format(time.RFC3339), code: code}
	b.list = append(b.list, inv)
	m.saveInvites(b)
	if m.Log != nil {
		m.Log.Warn("a cluster master wants to add this server: accept in the dashboard (Server → Cluster) or with p8 cluster accept",
			"master", masterName, "from", from, "confirm", inv.Confirm)
	}
	return *inv, nil
}

// Invites lists the open invitations.
func (m *Manager) Invites() []Invite {
	b := m.box()
	b.mu.Lock()
	defer b.mu.Unlock()
	now, out := m.now(), []Invite{}
	for _, i := range b.list {
		if t, _ := time.Parse(time.RFC3339, i.Expires); t.After(now) {
			out = append(out, *i)
		}
	}
	return out
}

func (m *Manager) takeInvite(id string) (*Invite, error) {
	b := m.box()
	b.mu.Lock()
	defer b.mu.Unlock()
	for k, i := range b.list {
		if i.ID == id {
			b.list = append(b.list[:k], b.list[k+1:]...)
			m.saveInvites(b)
			if t, _ := time.Parse(time.RFC3339, i.Expires); !t.After(m.now()) {
				return nil, errors.New("the invitation has expired; add this server again on the master")
			}
			return i, nil
		}
	}
	return nil, fmt.Errorf("%w: invitation %s", ErrNotFound, id)
}

// AcceptInvite joins the master that sent invitation id.
func (m *Manager) AcceptInvite(ctx context.Context, id string) (RegisterReply, error) {
	i, err := m.takeInvite(id)
	if err != nil {
		return RegisterReply{}, err
	}
	return m.Join(ctx, i.MasterURL, i.code)
}

// DeclineInvite drops an invitation.
func (m *Manager) DeclineInvite(id string) error {
	_, err := m.takeInvite(id)
	return err
}

// LocalURL is this server's dashboard address as seen from ip (the address of
// the interface that reaches it).
func (m *Manager) LocalURL(ip string) (string, error) {
	c, err := net.Dial("udp4", net.JoinHostPort(ip, "9"))
	if err != nil {
		return "", err
	}
	defer c.Close()
	return "https://" + net.JoinHostPort(c.LocalAddr().(*net.UDPAddr).IP.String(), m.port()), nil
}

// SendInvite asks a found server to join; its admin accepts it there.
// It returns the number both screens show.
func (m *Manager) SendInvite(ctx context.Context, f Found) (confirm string, err error) {
	if net.ParseIP(f.IP) == nil || len(f.CertSHA) != 64 {
		return "", fmt.Errorf("%w: server", ErrInvalid)
	}
	masterURL, err := m.LocalURL(f.IP)
	if err != nil {
		return "", err
	}
	code, _, err := m.JoinCode(ctx, masterURL, "")
	if err != nil {
		return "", err
	}
	port := f.APIPort
	if port == "" {
		port = "8006"
	}
	target := Node{Name: f.Name, API: "https://" + net.JoinHostPort(f.IP, port), CertSHA: f.CertSHA}
	body := map[string]string{"master_name": m.SelfName(), "master_url": masterURL, "code": code}
	if err := m.call(ctx, target, "POST", "/cluster/invite", body, nil); err != nil {
		return "", err
	}
	return ConfirmNumber(code), nil
}

func (m *Manager) dport() int {
	if m.DiscoverPort != 0 {
		return m.DiscoverPort
	}
	return DiscoverPort
}
