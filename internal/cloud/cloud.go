// Package cloud holds the "P8S Cloud" settings an admin sets in the
// dashboard: the brand customers see, self sign-up, the sizes (plans) with
// prices, tax and seller details for invoices, payment and email settings.
// One JSON file (0600: it holds the payment and SMTP secrets).
package cloud

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Plan is a server size customers can pick, with its monthly price.
type Plan struct {
	ID        string `json:"id"`   // e.g. "s1"
	Name      string `json:"name"` // e.g. "Starter"
	VCPUs     int    `json:"vcpus"`
	MemoryMiB int    `json:"memory_mib"`
	DiskGB    int    `json:"disk_gb"`
	// PriceMonthly in the smallest currency unit (paise for INR), before tax.
	PriceMonthly int64 `json:"price_monthly"`
	PublicIP     bool  `json:"public_ip"` // comes with a public IPv4 from the pool below
	Hidden       bool  `json:"hidden"`    // not offered to new servers (existing ones keep it)
	// For: what the plan sizes: "" = VMs and containers, "vm" = VMs only,
	// "ct" = containers only (they share the host's kernel, so they are often cheaper).
	For string `json:"for,omitempty"`
}

// Plan kinds.
const (
	ForVM = "vm"
	ForCT = "ct"
)

// Fits: the plan can size a guest of kind (ForVM or ForCT).
func (p Plan) Fits(kind string) bool { return p.For == "" || p.For == kind }

// Hourly is what one hour costs: monthly / 730, rounded up.
func (p Plan) Hourly() int64 { return (p.PriceMonthly + 729) / 730 }

// Settings is everything the admin configures for selling.
type Settings struct {
	Brand      string `json:"brand"`       // shown to customers, e.g. "P8S Cloud"
	PublicURL  string `json:"public_url"`  // where customers open the panel (links in emails)
	SignupOpen bool   `json:"signup_open"` // anyone may create an account
	Currency   string `json:"currency"`    // ISO code, e.g. INR
	TaxName    string `json:"tax_name"`    // e.g. GST
	TaxPercent int    `json:"tax_percent"` // e.g. 18
	// Seller details printed on invoices.
	SellerName    string `json:"seller_name"`
	SellerAddress string `json:"seller_address"`
	SellerTaxID   string `json:"seller_tax_id"` // GSTIN
	SupportEmail  string `json:"support_email"`
	InvoicePrefix string `json:"invoice_prefix"` // e.g. P8S
	// Billing rules.
	MinTopup   int64 `json:"min_topup"`   // smallest wallet top-up (smallest unit), before tax
	GraceHours int   `json:"grace_hours"` // after the balance goes negative, servers are stopped after this
	// Pool for plans with a public IP; NAT network when empty.
	PublicPool string `json:"public_pool"`
	// Quota of a self-signed-up customer (a safety cap; billing limits real use).
	SignupQuota struct {
		VMs       int `json:"vms"`
		VCPUs     int `json:"vcpus"`
		MemoryMiB int `json:"memory_mib"`
		DiskGB    int `json:"disk_gb"`
		Sites     int `json:"sites"`
		PublicIPs int `json:"public_ips"`
	} `json:"signup_quota"`
	// Payments (Razorpay).
	RazorpayKeyID         string `json:"razorpay_key_id"`
	RazorpayKeySecret     string `json:"razorpay_key_secret,omitempty"`
	RazorpayWebhookSecret string `json:"razorpay_webhook_secret,omitempty"`
	// Email (SMTP).
	SMTPHost string `json:"smtp_host"`
	SMTPPort int    `json:"smtp_port"` // 587 STARTTLS, 465 TLS
	SMTPUser string `json:"smtp_user"`
	SMTPPass string `json:"smtp_pass,omitempty"`
	MailFrom string `json:"mail_from"` // e.g. "P8S Cloud <noreply@p8s.in>"
}

// Defaults are used until the admin saves settings.
func Defaults() Settings {
	s := Settings{Brand: "P8S Cloud", Currency: "INR", TaxName: "GST", TaxPercent: 18, InvoicePrefix: "P8S",
		MinTopup: 10000, GraceHours: 72, SMTPPort: 587}
	s.SignupQuota.VMs, s.SignupQuota.VCPUs, s.SignupQuota.MemoryMiB, s.SignupQuota.DiskGB = 3, 8, 16384, 300
	s.SignupQuota.Sites, s.SignupQuota.PublicIPs = 3, 3
	return s
}

// Secrets shown to the dashboard are replaced by this; sending it back keeps the stored value.
const Kept = "********"

// Redacted hides secrets (the dashboard shows whether they are set).
func (s Settings) Redacted() Settings {
	for _, p := range []*string{&s.RazorpayKeySecret, &s.RazorpayWebhookSecret, &s.SMTPPass} {
		if *p != "" {
			*p = Kept
		}
	}
	return s
}

// Payments reports whether online payment is set up.
func (s Settings) Payments() bool { return s.RazorpayKeyID != "" && s.RazorpayKeySecret != "" }

// Email reports whether email can be sent.
func (s Settings) Email() bool { return s.SMTPHost != "" && s.MailFrom != "" }

var (
	ErrInvalid  = errors.New("invalid")
	ErrNotFound = errors.New("not found")

	rePlanID   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,23}$`)
	reCurrency = regexp.MustCompile(`^[A-Z]{3}$`)
	reHost     = regexp.MustCompile(`^[A-Za-z0-9.-]{1,253}$`)
	reText     = regexp.MustCompile(`^[^<>\x00-\x08\x0b\x0c\x0e-\x1f]*$`)
	rePrefix   = regexp.MustCompile(`^[A-Z0-9-]{1,10}$`)
	reKey      = regexp.MustCompile(`^[A-Za-z0-9_]{0,64}$`)
)

func text(field, v string, max int) error {
	if len(v) > max || !reText.MatchString(v) {
		return fmt.Errorf("%w: %s: up to %d characters, no < >", ErrInvalid, field, max)
	}
	return nil
}

// Validate checks settings (secrets equal to Kept are filled in by the store).
func (s Settings) Validate() error {
	var errs []error
	add := func(e error) {
		if e != nil {
			errs = append(errs, e)
		}
	}
	add(text("brand", s.Brand, 40))
	if strings.TrimSpace(s.Brand) == "" {
		add(fmt.Errorf("%w: brand name is empty", ErrInvalid))
	}
	if s.PublicURL != "" {
		u, err := url.Parse(s.PublicURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			add(fmt.Errorf("%w: public URL must look like https://panel.example.com", ErrInvalid))
		}
	}
	if !reCurrency.MatchString(s.Currency) {
		add(fmt.Errorf("%w: currency must be a 3-letter code like INR", ErrInvalid))
	}
	add(text("tax name", s.TaxName, 20))
	if s.TaxPercent < 0 || s.TaxPercent > 50 {
		add(fmt.Errorf("%w: tax must be 0-50%%", ErrInvalid))
	}
	add(text("seller name", s.SellerName, 100))
	add(text("seller address", s.SellerAddress, 400))
	add(text("tax id", s.SellerTaxID, 30))
	if s.SupportEmail != "" {
		if _, err := mail.ParseAddress(s.SupportEmail); err != nil {
			add(fmt.Errorf("%w: support email", ErrInvalid))
		}
	}
	if !rePrefix.MatchString(s.InvoicePrefix) {
		add(fmt.Errorf("%w: invoice prefix: up to 10 capital letters, digits, -", ErrInvalid))
	}
	if s.MinTopup < 100 || s.GraceHours < 1 || s.GraceHours > 24*60 {
		add(fmt.Errorf("%w: minimum top-up must be at least 1.00 and grace 1-1440 hours", ErrInvalid))
	}
	add(text("public pool", s.PublicPool, 32))
	q := s.SignupQuota
	if q.VMs < 0 || q.VCPUs < 0 || q.MemoryMiB < 0 || q.DiskGB < 0 || q.Sites < 0 || q.PublicIPs < 0 {
		add(fmt.Errorf("%w: sign-up quota can't be negative", ErrInvalid))
	}
	if !reKey.MatchString(s.RazorpayKeyID) || (s.RazorpayKeyID != "" && !strings.HasPrefix(s.RazorpayKeyID, "rzp_")) {
		add(fmt.Errorf("%w: Razorpay key id looks like rzp_live_… or rzp_test_…", ErrInvalid))
	}
	if s.SMTPHost != "" && !reHost.MatchString(s.SMTPHost) {
		add(fmt.Errorf("%w: SMTP server name", ErrInvalid))
	}
	if s.SMTPPort < 0 || s.SMTPPort > 65535 {
		add(fmt.Errorf("%w: SMTP port", ErrInvalid))
	}
	if s.MailFrom != "" {
		if _, err := mail.ParseAddress(s.MailFrom); err != nil {
			add(fmt.Errorf("%w: sender like \"P8S Cloud <noreply@example.com>\"", ErrInvalid))
		}
	}
	add(text("SMTP user", s.SMTPUser, 200))
	return errors.Join(errs...)
}

// minPlanMem: containers need less RAM than VMs.
func minPlanMem(kind string) int {
	if kind == ForCT {
		return 128
	}
	return 256
}

// ValidatePlan checks a plan.
func ValidatePlan(p Plan) (Plan, error) {
	p.ID = strings.ToLower(strings.TrimSpace(p.ID))
	p.Name = strings.TrimSpace(p.Name)
	switch {
	case !rePlanID.MatchString(p.ID):
		return p, fmt.Errorf("%w: plan id: lowercase letters, digits, - (e.g. s1)", ErrInvalid)
	case p.Name == "" || text("name", p.Name, 40) != nil:
		return p, fmt.Errorf("%w: plan name (up to 40 characters)", ErrInvalid)
	case p.For != "" && p.For != ForVM && p.For != ForCT:
		return p, fmt.Errorf(`%w: plan for: "" (VMs and containers), "vm" or "ct"`, ErrInvalid)
	case p.VCPUs < 1 || p.VCPUs > 128 || p.MemoryMiB < minPlanMem(p.For) || p.MemoryMiB > 1<<20 || p.DiskGB < 1 || p.DiskGB > 100000:
		return p, fmt.Errorf("%w: plan size (CPUs 1-128, RAM ≥ %d MB, disk ≥ 1 GB)", ErrInvalid, minPlanMem(p.For))
	case p.PriceMonthly < 0 || p.PriceMonthly > 1e10:
		return p, fmt.Errorf("%w: price", ErrInvalid)
	}
	return p, nil
}

type file struct {
	Settings Settings `json:"settings"`
	Plans    []Plan   `json:"plans"`
}

// Store keeps settings and plans.
type Store struct {
	Path string // e.g. /var/lib/p8s/cloud.json; empty = memory only

	mu   sync.Mutex
	f    *file
	read bool
}

func (s *Store) load() (*file, error) {
	if s.read {
		return s.f, nil
	}
	f := &file{Settings: Defaults(), Plans: []Plan{}}
	if s.Path != "" {
		b, err := os.ReadFile(s.Path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if err == nil {
			if err := json.Unmarshal(b, f); err != nil {
				return nil, fmt.Errorf("%s: %w", s.Path, err)
			}
		}
	}
	s.f, s.read = f, true
	return f, nil
}

func (s *Store) save(f *file) error {
	sort.Slice(f.Plans, func(i, j int) bool { return f.Plans[i].PriceMonthly < f.Plans[j].PriceMonthly })
	if s.Path == "" {
		s.f = f
		return nil
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o750); err != nil {
		return err
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.Path); err != nil {
		return err
	}
	s.f = f
	return nil
}

// Settings returns the current settings (with secrets).
func (s *Store) Settings() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return Defaults()
	}
	return f.Settings
}

// SetSettings saves new settings; secrets sent back as Kept keep their value.
func (s *Store) SetSettings(n Settings) (Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return Settings{}, err
	}
	old := f.Settings
	keep := func(p *string, o string) {
		if *p == Kept {
			*p = o
		}
	}
	keep(&n.RazorpayKeySecret, old.RazorpayKeySecret)
	keep(&n.RazorpayWebhookSecret, old.RazorpayWebhookSecret)
	keep(&n.SMTPPass, old.SMTPPass)
	n.Brand, n.Currency, n.InvoicePrefix = strings.TrimSpace(n.Brand), strings.ToUpper(strings.TrimSpace(n.Currency)), strings.ToUpper(strings.TrimSpace(n.InvoicePrefix))
	n.PublicURL = strings.TrimRight(strings.TrimSpace(n.PublicURL), "/")
	if err := n.Validate(); err != nil {
		return Settings{}, err
	}
	nf := *f
	nf.Settings = n
	nf.Plans = append([]Plan(nil), f.Plans...)
	if err := s.save(&nf); err != nil {
		return Settings{}, err
	}
	return n, nil
}

// Plans lists the plans, cheapest first.
func (s *Store) Plans() []Plan {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return []Plan{}
	}
	return append([]Plan{}, f.Plans...)
}

// Plan returns one plan.
func (s *Store) Plan(id string) (Plan, error) {
	for _, p := range s.Plans() {
		if p.ID == id {
			return p, nil
		}
	}
	return Plan{}, fmt.Errorf("%w: plan %q", ErrNotFound, id)
}

// PutPlan adds or replaces a plan (servers already on it keep their price).
func (s *Store) PutPlan(p Plan) (Plan, error) {
	p, err := ValidatePlan(p)
	if err != nil {
		return p, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return p, err
	}
	nf := *f
	nf.Plans = []Plan{}
	for _, o := range f.Plans {
		if o.ID != p.ID {
			nf.Plans = append(nf.Plans, o)
		}
	}
	nf.Plans = append(nf.Plans, p)
	return p, s.save(&nf)
}

// DeletePlan removes a plan (servers on it keep running at their price).
func (s *Store) DeletePlan(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return err
	}
	nf := *f
	nf.Plans = []Plan{}
	found := false
	for _, o := range f.Plans {
		if o.ID == id {
			found = true
			continue
		}
		nf.Plans = append(nf.Plans, o)
	}
	if !found {
		return fmt.Errorf("%w: plan %q", ErrNotFound, id)
	}
	return s.save(&nf)
}
