// Package licsrv is the P8S licence server: it issues signed P8S OS
// subscription keys, sells them online (Razorpay) with GST invoices,
// renews and cancels them, and answers the daily check from p8sd.
package licsrv

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"p8sos/internal/billing"
	"p8sos/internal/license"
)

// Record is an issued licence.
type Record struct {
	License license.License `json:"license"`
	Key     string          `json:"key"`
	Revoked bool            `json:"revoked,omitempty"`
	Note    string          `json:"note,omitempty"`
	Created string          `json:"created"`
	Updated string          `json:"updated"`
	Orders  []string        `json:"orders,omitempty"`
}

// Order is a purchase waiting for (or done with) payment.
type Order struct {
	ID       string        `json:"id"` // Razorpay order id
	Tier     string        `json:"tier"`
	Servers  []string      `json:"servers"`
	Renew    string        `json:"renew,omitempty"` // licence id being renewed
	Buyer    billing.Party `json:"buyer"`
	Subtotal int64         `json:"subtotal"`
	Tax      int64         `json:"tax"`
	Total    int64         `json:"total"`
	Created  string        `json:"created"`
	Paid     string        `json:"paid,omitempty"`
	Payment  string        `json:"payment,omitempty"`
	License  string        `json:"license,omitempty"`
	Invoice  string        `json:"invoice,omitempty"`
}

type state struct {
	Licenses    map[string]*Record `json:"licenses"`
	Orders      map[string]*Order  `json:"orders"`
	NextInvoice map[string]int     `json:"next_invoice"`
	Releases    []*Release         `json:"releases"`
}

var (
	ErrInvalid  = errors.New("invalid")
	ErrNotFound = errors.New("not found")
	// ErrSubscription: the server's subscription doesn't allow this.
	ErrSubscription = errors.New("subscription needed")
)

// Store keeps state.json and invoices.jsonl in Dir; Key signs licences.
type Store struct {
	Dir string
	Key ed25519.PrivateKey
	Now func() time.Time

	mu     sync.Mutex
	loaded bool
	st     state
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Store) load() error {
	if s.loaded {
		return nil
	}
	s.st = state{Licenses: map[string]*Record{}, Orders: map[string]*Order{}, NextInvoice: map[string]int{}}
	b, err := os.ReadFile(filepath.Join(s.Dir, "state.json"))
	if err == nil {
		if err := json.Unmarshal(b, &s.st); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if s.st.Licenses == nil {
		s.st.Licenses = map[string]*Record{}
	}
	if s.st.Orders == nil {
		s.st.Orders = map[string]*Order{}
	}
	if s.st.NextInvoice == nil {
		s.st.NextInvoice = map[string]int{}
	}
	s.loaded = true
	return nil
}

func (s *Store) save() error {
	b, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	p := filepath.Join(s.Dir, "state.json")
	if err := os.WriteFile(p+".tmp", b, 0o600); err != nil {
		return err
	}
	return os.Rename(p+".tmp", p)
}

const idChars = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func newLicenseID() string {
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	out := []byte{'L'}
	for _, c := range b {
		out = append(out, idChars[int(c)%len(idChars)])
	}
	return string(out)
}

// NormServers cleans a list of server IDs.
func NormServers(in []string) ([]string, error) {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\r' || r == '\t' }) {
			f = strings.ToUpper(strings.TrimSpace(f))
			if !license.ValidServerID(f) {
				return nil, fmt.Errorf("%w: %q is not a server ID (P8S-XXXX-XXXX-XXXX-XXXX; run \"p8sd server-id\" or see Subscription in the dashboard)", ErrInvalid, f)
			}
			if !seen[f] {
				seen[f] = true
				out = append(out, f)
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: at least one server ID", ErrInvalid)
	}
	if len(out) > 100 {
		return nil, fmt.Errorf("%w: up to 100 servers", ErrInvalid)
	}
	return out, nil
}

func (s *Store) sign(r *Record) error {
	k, err := license.Sign(s.Key, r.License)
	if err != nil {
		return err
	}
	r.Key, r.Updated = k, s.now().Format(time.RFC3339)
	return nil
}

// Issue creates a licence directly (CLI, internal servers).
func (s *Store) Issue(tier string, servers []string, customer, email string, months int, note string) (Record, error) {
	if months < 1 || months > 120 {
		return Record{}, fmt.Errorf("%w: months 1-120", ErrInvalid)
	}
	sv, err := NormServers(servers)
	if err != nil {
		return Record{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return Record{}, err
	}
	now := s.now()
	r := &Record{License: license.License{ID: newLicenseID(), Customer: customer, Email: email, Tier: tier, Servers: sv,
		Issued: now.Format("2006-01-02"), Expires: now.AddDate(0, months, -1).Format("2006-01-02")},
		Created: now.Format(time.RFC3339), Note: note}
	if err := s.sign(r); err != nil {
		return Record{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	s.st.Licenses[r.License.ID] = r
	return *r, s.save()
}

// Get returns one licence.
func (s *Store) Get(id string) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return Record{}, err
	}
	r, ok := s.st.Licenses[strings.ToUpper(strings.TrimSpace(id))]
	if !ok {
		return Record{}, fmt.Errorf("%w: licence %s", ErrNotFound, id)
	}
	return *r, nil
}

// List returns all licences, newest first.
func (s *Store) List() ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return nil, err
	}
	out := []Record{}
	for _, r := range s.st.Licenses {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created > out[j].Created })
	return out, nil
}

// Revoke cancels a licence (p8sd drops it at its next daily check).
func (s *Store) Revoke(id, note string, revoked bool) (Record, error) {
	return s.change(id, func(r *Record) error { r.Revoked, r.Note = revoked, note; return nil })
}

// Rebind moves a licence to other servers (e.g. after a reinstall) and re-signs it.
func (s *Store) Rebind(id string, servers []string) (Record, error) {
	sv, err := NormServers(servers)
	if err != nil {
		return Record{}, err
	}
	return s.change(id, func(r *Record) error { r.License.Servers = sv; return s.sign(r) })
}

// Extend adds months to a licence (from its expiry, or today if it already ran out).
func (s *Store) Extend(id string, months int) (Record, error) {
	return s.change(id, func(r *Record) error { s.extend(r, months); return s.sign(r) })
}

func (s *Store) extend(r *Record, months int) {
	from := s.now()
	if t, err := time.Parse("2006-01-02", r.License.Expires); err == nil && t.AddDate(0, 0, 1).After(from) {
		from = t.AddDate(0, 0, 1)
	}
	r.License.Expires = from.AddDate(0, months, -1).Format("2006-01-02")
}

func (s *Store) change(id string, fn func(*Record) error) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return Record{}, err
	}
	r, ok := s.st.Licenses[strings.ToUpper(strings.TrimSpace(id))]
	if !ok {
		return Record{}, fmt.Errorf("%w: licence %s", ErrNotFound, id)
	}
	c := *r
	c.License.Servers = append([]string{}, r.License.Servers...)
	if err := fn(&c); err != nil {
		return Record{}, err
	}
	c.Updated = s.now().Format(time.RFC3339)
	*r = c
	return c, s.save()
}

// CheckResult is the answer to p8sd's daily check.
type CheckResult struct {
	Status  string `json:"status"` // active, revoked, unknown
	Message string `json:"message,omitempty"`
	Key     string `json:"key,omitempty"` // the current key (renewals arrive this way)
	Expires string `json:"expires,omitempty"`
}

// Check answers p8sd.
func (s *Store) Check(id, server string) CheckResult {
	r, err := s.Get(id)
	if err != nil || !r.License.Covers(server) {
		return CheckResult{Status: "unknown"}
	}
	if r.Revoked {
		return CheckResult{Status: "revoked", Message: r.Note}
	}
	return CheckResult{Status: "active", Key: r.Key, Expires: r.License.Expires}
}

// --- orders ---

// NewOrder records a purchase waiting for payment.
func (s *Store) NewOrder(o Order) (Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return Order{}, err
	}
	o.Created = s.now().Format(time.RFC3339)
	s.st.Orders[o.ID] = &o
	return o, s.save()
}

// Order returns one order.
func (s *Store) Order(id string) (Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return Order{}, err
	}
	o, ok := s.st.Orders[id]
	if !ok {
		return Order{}, fmt.Errorf("%w: order %s", ErrNotFound, id)
	}
	return *o, nil
}

// InvoiceInfo is the seller side of invoices.
type InvoiceInfo struct {
	Seller     billing.Party
	Currency   string
	TaxName    string
	TaxPercent int
	Prefix     string
}

// Fulfil issues (or renews) the licence of a paid order and writes its
// invoice, once; later calls return the first result. fresh = this call did it.
func (s *Store) Fulfil(orderID, paymentID string, inv InvoiceInfo) (Record, billing.Invoice, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return Record{}, billing.Invoice{}, false, err
	}
	o, ok := s.st.Orders[orderID]
	if !ok {
		return Record{}, billing.Invoice{}, false, fmt.Errorf("%w: order %s", ErrNotFound, orderID)
	}
	if o.Paid != "" {
		r := s.st.Licenses[o.License]
		in, _ := s.invoice(o.Invoice)
		if r == nil {
			return Record{}, in, false, fmt.Errorf("%w: licence of order %s", ErrNotFound, orderID)
		}
		return *r, in, false, nil
	}
	now := s.now()
	var r *Record
	if o.Renew != "" {
		old, ok := s.st.Licenses[o.Renew]
		if !ok {
			return Record{}, billing.Invoice{}, false, fmt.Errorf("%w: licence %s", ErrNotFound, o.Renew)
		}
		c := *old
		c.License.Tier = o.Tier // renewals may move up a tier
		s.extend(&c, 12)
		r = &c
	} else {
		r = &Record{License: license.License{ID: newLicenseID(), Customer: firstNonEmpty(o.Buyer.Name), Email: o.Buyer.Email,
			Tier: o.Tier, Servers: o.Servers, Issued: now.Format("2006-01-02"), Expires: now.AddDate(1, 0, -1).Format("2006-01-02")},
			Created: now.Format(time.RFC3339)}
	}
	if err := s.sign(r); err != nil {
		return Record{}, billing.Invoice{}, false, err
	}
	r.Orders = append(append([]string{}, r.Orders...), o.ID)

	year := now.Format("2006")
	s.st.NextInvoice[year]++
	t, _ := license.Tier(o.Tier)
	item := fmt.Sprintf("P8S OS %s subscription, 1 year (%s to %s), licence %s, %d server(s)",
		t.Name, now.Format("2006-01-02"), r.License.Expires, r.License.ID, len(r.License.Servers))
	in := billing.Invoice{Number: fmt.Sprintf("%s-%s-%06d", inv.Prefix, year, s.st.NextInvoice[year]), Date: now.Format("2006-01-02"),
		User: r.License.ID, Seller: inv.Seller, Buyer: o.Buyer, Item: item, Currency: inv.Currency,
		Subtotal: o.Subtotal, TaxName: inv.TaxName, TaxPercent: inv.TaxPercent, Tax: o.Tax, Total: o.Total, PaymentRef: paymentID, Method: "razorpay"}
	if err := appendLine(filepath.Join(s.Dir, "invoices.jsonl"), in); err != nil {
		s.st.NextInvoice[year]--
		return Record{}, billing.Invoice{}, false, err
	}
	s.st.Licenses[r.License.ID] = r
	o.Paid, o.Payment, o.License, o.Invoice = now.Format(time.RFC3339), paymentID, r.License.ID, in.Number
	return *r, in, true, s.save()
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func appendLine(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

func (s *Store) invoice(number string) (billing.Invoice, error) {
	b, err := os.ReadFile(filepath.Join(s.Dir, "invoices.jsonl"))
	if err != nil {
		return billing.Invoice{}, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		var in billing.Invoice
		if json.Unmarshal([]byte(line), &in) == nil && in.Number == number {
			return in, nil
		}
	}
	return billing.Invoice{}, fmt.Errorf("%w: invoice %s", ErrNotFound, number)
}

// Invoices returns all invoices, newest first.
func (s *Store) Invoices() ([]billing.Invoice, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(filepath.Join(s.Dir, "invoices.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return []billing.Invoice{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []billing.Invoice{}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var in billing.Invoice
		if json.Unmarshal([]byte(lines[i]), &in) == nil {
			out = append(out, in)
		}
	}
	return out, nil
}

// --- signing keys ---

// GenerateKey writes a new private key file (0600) and returns the public key (base64).
func GenerateKey(path string) (string, error) {
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("%s exists; refusing to overwrite a signing key", path)
	}
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(priv.Seed())+"\n"), 0o600); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(pub), nil
}

// LoadKey reads a private key file.
func LoadKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("signing key: %w (make one with: p8s-licensed keygen)", err)
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("signing key %s: not a P8S signing key", path)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// PublicKey returns a private key's public half (base64), for pubkeys.txt.
func PublicKey(k ed25519.PrivateKey) string {
	return base64.StdEncoding.EncodeToString(k.Public().(ed25519.PublicKey))
}
