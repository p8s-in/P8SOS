package licsrv

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"p8sos/internal/billing"
	"p8sos/internal/license"
	"p8sos/internal/mail"
	"p8sos/internal/users"
)

// Config is /etc/p8s-licensed.json.
type Config struct {
	Listen     string        `json:"listen"`      // default 127.0.0.1:8090 (put it behind Cloudflare Tunnel or a proxy)
	DataDir    string        `json:"data_dir"`    // default /var/lib/p8s-licensed
	SigningKey string        `json:"signing_key"` // default data_dir/signing.key
	Brand      string        `json:"brand"`       // default "P8S OS"
	PublicURL  string        `json:"public_url"`  // e.g. https://licence.example.com
	Support    string        `json:"support_email"`
	Seller     billing.Party `json:"seller"` // on invoices: name, address, GSTIN, email
	Currency   string        `json:"currency"`
	TaxName    string        `json:"tax_name"`
	TaxPercent int           `json:"tax_percent"`
	Prefix     string        `json:"invoice_prefix"`
	// Prices per year in paise: per server (business, hosting) or per pack (partner).
	Prices map[string]int64 `json:"prices"`

	RazorpayKeyID         string `json:"razorpay_key_id"`
	RazorpayKeySecret     string `json:"razorpay_key_secret"`
	RazorpayWebhookSecret string `json:"razorpay_webhook_secret"`

	SMTPHost string `json:"smtp_host"`
	SMTPPort int    `json:"smtp_port"`
	SMTPUser string `json:"smtp_user"`
	SMTPPass string `json:"smtp_pass"`
	MailFrom string `json:"mail_from"`
}

// LoadConfig reads the config (a missing file = defaults).
func LoadConfig(path string) (*Config, error) {
	c := &Config{}
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if err := json.Unmarshal(b, c); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	c.defaults()
	return c, nil
}

func (c *Config) defaults() {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8090"
	}
	if c.DataDir == "" {
		c.DataDir = "/var/lib/p8s-licensed"
	}
	if c.SigningKey == "" {
		c.SigningKey = c.DataDir + "/signing.key"
	}
	if c.Brand == "" {
		c.Brand = "P8S OS"
	}
	if c.Currency == "" {
		c.Currency = "INR"
	}
	if c.TaxName == "" {
		c.TaxName = "GST"
	}
	if c.TaxPercent == 0 {
		c.TaxPercent = 18
	}
	if c.Prefix == "" {
		c.Prefix = "P8SL"
	}
	if c.SMTPPort == 0 {
		c.SMTPPort = 587
	}
	if c.Prices == nil {
		c.Prices = map[string]int64{}
	}
	for _, t := range license.Tiers() {
		if _, ok := c.Prices[t.ID]; !ok && t.Price > 0 {
			c.Prices[t.ID] = t.Price
		}
	}
}

// Price is what a purchase costs before tax.
func (c *Config) Price(tier string, servers int) (int64, error) {
	t, ok := license.Tier(tier)
	p := c.Prices[tier]
	if !ok || p <= 0 || tier == license.Internal {
		return 0, fmt.Errorf("%w: %q is not for sale", ErrInvalid, tier)
	}
	if t.Servers > 0 { // a pack
		if servers > t.Servers {
			return 0, fmt.Errorf("%w: %s covers up to %d servers", ErrInvalid, t.Name, t.Servers)
		}
		return p, nil
	}
	if servers > 50 {
		return 0, fmt.Errorf("%w: up to 50 servers per purchase; contact us for more", ErrInvalid)
	}
	return p * int64(servers), nil
}

//go:embed shop.html
var shopFS embed.FS

// Server is the licence server's HTTP side.
type Server struct {
	Cfg      *Config
	Store    *Store
	Mail     *mail.Mailer
	Razorpay *billing.Razorpay // nil = from Cfg
	Log      *slog.Logger

	mu   sync.Mutex
	hits map[string][]time.Time
}

func (s *Server) rzp() *billing.Razorpay {
	if s.Razorpay != nil {
		return s.Razorpay
	}
	return &billing.Razorpay{KeyID: s.Cfg.RazorpayKeyID, KeySecret: s.Cfg.RazorpayKeySecret}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.shop)
	mux.HandleFunc("GET /api/shop", s.shopInfo)
	mux.HandleFunc("POST /api/order", s.order)
	mux.HandleFunc("POST /api/paid", s.paid)
	mux.HandleFunc("POST /api/razorpay/webhook", s.webhook)
	mux.HandleFunc("POST /api/v1/check", s.check)
	mux.HandleFunc("POST /api/v1/updates", s.updates)
	mux.HandleFunc("GET /api/v1/updates/download", s.download)
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]string{"message": msg}})
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalid), errors.Is(err, users.ErrInvalid):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrSubscription):
		writeErr(w, http.StatusForbidden, err.Error())
	default:
		if s.Log != nil {
			s.Log.Error("licence server", "err", err)
		}
		writeErr(w, http.StatusInternalServerError, "something went wrong on our side; please try again or contact support")
	}
}

// clientIP: the visitor (Cloudflare's header only when the proxy is local).
func clientIP(r *http.Request) string {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if cf := r.Header.Get("CF-Connecting-IP"); cf != "" {
			return cf
		}
	}
	return host
}

// limit allows n requests per IP per window for a bucket.
func (s *Server) limit(r *http.Request, bucket string, n int, window time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hits == nil {
		s.hits = map[string][]time.Time{}
	}
	k := bucket + ":" + clientIP(r)
	now := time.Now()
	l := s.hits[k][:0]
	for _, t := range s.hits[k] {
		if now.Sub(t) < window {
			l = append(l, t)
		}
	}
	if len(l) >= n {
		s.hits[k] = l
		return false
	}
	s.hits[k] = append(l, now)
	if len(s.hits) > 100000 { // don't grow forever
		s.hits = map[string][]time.Time{}
	}
	return true
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return false
	}
	return true
}

func (s *Server) shop(w http.ResponseWriter, r *http.Request) {
	b, _ := shopFS.ReadFile("shop.html")
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline' https://checkout.razorpay.com; "+
		"style-src 'self' 'unsafe-inline'; frame-src https://api.razorpay.com https://checkout.razorpay.com; "+
		"connect-src 'self' https://api.razorpay.com https://lumberjack.razorpay.com; img-src 'self' data: https:")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	_, _ = w.Write(b)
}

func (s *Server) shopInfo(w http.ResponseWriter, r *http.Request) {
	type tier struct {
		license.TierInfo
		Price int64 `json:"price"`
	}
	out := []tier{}
	for _, t := range license.Tiers() {
		p := s.Cfg.Prices[t.ID]
		if t.ID == license.Home {
			p = 0
		}
		out = append(out, tier{t, p})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"brand": s.Cfg.Brand, "currency": s.Cfg.Currency, "tax_name": s.Cfg.TaxName, "tax_percent": s.Cfg.TaxPercent,
		"tiers": out, "payments": s.Cfg.RazorpayKeyID != "" && s.Cfg.RazorpayKeySecret != "", "support": s.Cfg.Support,
	})
}

func (s *Server) order(w http.ResponseWriter, r *http.Request) {
	if !s.limit(r, "order", 20, time.Hour) {
		writeErr(w, http.StatusTooManyRequests, "too many orders from here; try again later")
		return
	}
	var in struct {
		Tier    string `json:"tier"`
		Servers string `json:"servers"`
		Renew   string `json:"renew"`
		Name    string `json:"name"`
		Email   string `json:"email"`
		Company string `json:"company"`
		GSTIN   string `json:"gstin"`
		Address string `json:"address"`
	}
	if !decode(w, r, &in) {
		return
	}
	if s.Cfg.RazorpayKeyID == "" || s.Cfg.RazorpayKeySecret == "" {
		writeErr(w, http.StatusServiceUnavailable, "online payment is not set up yet; please contact us")
		return
	}
	email, err := users.NormEmail(in.Email)
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, f := range []string{in.Name, in.Company, in.GSTIN, in.Address} {
		if len(f) > 300 || strings.ContainsAny(f, "<>") {
			writeErr(w, http.StatusBadRequest, "name, company, GSTIN and address: up to 300 characters, no < or >")
			return
		}
	}
	if strings.TrimSpace(in.Name) == "" {
		writeErr(w, http.StatusBadRequest, "your name is needed for the invoice")
		return
	}
	o := Order{Tier: in.Tier}
	if in.Renew != "" {
		rec, err := s.Store.Get(in.Renew)
		if err != nil || !strings.EqualFold(rec.License.Email, email) {
			writeErr(w, http.StatusNotFound, "no licence with that ID for this email address")
			return
		}
		if rec.Revoked {
			writeErr(w, http.StatusBadRequest, "this licence was cancelled; please contact us")
			return
		}
		rank := map[string]int{license.Business: 1, license.Hosting: 2, license.Partner: 3}
		if rank[in.Tier] < rank[rec.License.Tier] {
			writeErr(w, http.StatusBadRequest, "a renewal can stay on the same tier or move up")
			return
		}
		o.Renew, o.Servers = rec.License.ID, rec.License.Servers
	} else if o.Servers, err = NormServers([]string{in.Servers}); err != nil {
		s.fail(w, err)
		return
	}
	sub, err := s.Cfg.Price(in.Tier, len(o.Servers))
	if err != nil {
		s.fail(w, err)
		return
	}
	buyer := billing.Party{Name: strings.TrimSpace(in.Name), Email: email, Address: strings.TrimSpace(in.Address), TaxID: strings.ToUpper(strings.TrimSpace(in.GSTIN))}
	if c := strings.TrimSpace(in.Company); c != "" {
		buyer.Name = c + " (" + buyer.Name + ")"
	}
	o.Buyer, o.Subtotal = buyer, sub
	o.Tax = billing.Tax(sub, s.Cfg.TaxPercent)
	o.Total = sub + o.Tax
	id, err := s.rzp().CreateOrder(r.Context(), o.Total, s.Cfg.Currency, "p8s-licence", map[string]string{"tier": o.Tier, "email": email})
	if err != nil {
		if s.Log != nil {
			s.Log.Error("razorpay order", "err", err)
		}
		writeErr(w, http.StatusBadGateway, "the payment service is not answering; please try again")
		return
	}
	o.ID = id
	if o, err = s.Store.NewOrder(o); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"order_id": o.ID, "amount": o.Total, "subtotal": o.Subtotal, "tax": o.Tax,
		"currency": s.Cfg.Currency, "key_id": s.Cfg.RazorpayKeyID, "name": s.Cfg.Brand, "email": email})
}

// settle checks a payment with Razorpay and fulfils its order.
func (s *Server) settle(ctx context.Context, orderID, paymentID string) (Record, billing.Invoice, error) {
	o, err := s.Store.Order(orderID)
	if err != nil {
		return Record{}, billing.Invoice{}, err
	}
	if o.Paid == "" {
		p, err := s.rzp().FetchPayment(ctx, paymentID)
		if err != nil {
			return Record{}, billing.Invoice{}, err
		}
		if p.OrderID != orderID || p.Amount != o.Total || !strings.EqualFold(p.Currency, s.Cfg.Currency) {
			return Record{}, billing.Invoice{}, fmt.Errorf("%w: payment does not match the order", ErrInvalid)
		}
		if p.Status == "authorized" {
			if p, err = s.rzp().Capture(ctx, paymentID, o.Total, s.Cfg.Currency); err != nil {
				return Record{}, billing.Invoice{}, err
			}
		}
		if p.Status != "captured" {
			return Record{}, billing.Invoice{}, fmt.Errorf("%w: payment is %s", ErrInvalid, p.Status)
		}
	}
	rec, inv, fresh, err := s.Store.Fulfil(orderID, paymentID, InvoiceInfo{Seller: s.Cfg.Seller, Currency: s.Cfg.Currency,
		TaxName: s.Cfg.TaxName, TaxPercent: s.Cfg.TaxPercent, Prefix: s.Cfg.Prefix})
	if err == nil && fresh && s.Mail != nil {
		subj, body := s.keyMail(rec, inv, o.Renew != "")
		s.Mail.Queue(rec.License.Email, subj, body, "licence")
	}
	return rec, inv, err
}

func money(cur string, v int64) string {
	sym := cur + " "
	if cur == "INR" {
		sym = "₹"
	}
	return fmt.Sprintf("%s%d.%02d", sym, v/100, v%100)
}

func (s *Server) keyMail(rec Record, inv billing.Invoice, renewal bool) (string, string) {
	t, _ := license.Tier(rec.License.Tier)
	var b strings.Builder
	subj := fmt.Sprintf("Your %s %s subscription (%s)", s.Cfg.Brand, t.Name, rec.License.ID)
	fmt.Fprintf(&b, "Hello %s,\n\nThank you for your purchase.", inv.Buyer.Name)
	if renewal {
		fmt.Fprintf(&b, " Licence %s is renewed until %s. Servers pick up the renewal by themselves within a day, or paste the key below.\n\n", rec.License.ID, rec.License.Expires)
	} else {
		fmt.Fprintf(&b, "\n\n")
	}
	fmt.Fprintf(&b, "Licence:  %s\nTier:     %s\nServers:  %s\nValid to: %s\n\n", rec.License.ID, t.Name, strings.Join(rec.License.Servers, ", "), rec.License.Expires)
	fmt.Fprintf(&b, "Your licence key (paste it in the dashboard: Subscription > Enter key, or run: p8 license set <key>):\n\n%s\n\n", rec.Key)
	fmt.Fprintf(&b, "Tax invoice %s, %s\n  %s\n  Amount: %s\n  %s %d%%: %s\n  Total paid: %s\n  Payment: %s\n",
		inv.Number, inv.Date, inv.Item, money(inv.Currency, inv.Subtotal), inv.TaxName, inv.TaxPercent, money(inv.Currency, inv.Tax), money(inv.Currency, inv.Total), inv.PaymentRef)
	if inv.Seller.Name != "" {
		fmt.Fprintf(&b, "  Seller: %s, %s", inv.Seller.Name, inv.Seller.Address)
		if inv.Seller.TaxID != "" {
			fmt.Fprintf(&b, ", GSTIN %s", inv.Seller.TaxID)
		}
		b.WriteString("\n")
	}
	if inv.Buyer.TaxID != "" {
		fmt.Fprintf(&b, "  Buyer GSTIN: %s\n", inv.Buyer.TaxID)
	}
	if s.Cfg.Support != "" {
		fmt.Fprintf(&b, "\nQuestions: %s\n", s.Cfg.Support)
	}
	return subj, b.String()
}

func (s *Server) paid(w http.ResponseWriter, r *http.Request) {
	var in struct {
		OrderID   string `json:"razorpay_order_id"`
		PaymentID string `json:"razorpay_payment_id"`
		Signature string `json:"razorpay_signature"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !billing.CheckoutSignature(in.OrderID, in.PaymentID, in.Signature, s.Cfg.RazorpayKeySecret) {
		writeErr(w, http.StatusBadRequest, "the payment could not be verified")
		return
	}
	rec, inv, err := s.settle(r.Context(), in.OrderID, in.PaymentID)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"license": rec.License, "key": rec.Key, "invoice": inv})
}

func (s *Server) webhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return
	}
	if !billing.WebhookSignature(body, r.Header.Get("X-Razorpay-Signature"), s.Cfg.RazorpayWebhookSecret) {
		writeErr(w, http.StatusUnauthorized, "bad signature")
		return
	}
	var ev struct {
		Event   string `json:"event"`
		Payload struct {
			Payment struct {
				Entity struct {
					ID      string `json:"id"`
					OrderID string `json:"order_id"`
				} `json:"entity"`
			} `json:"payment"`
		} `json:"payload"`
	}
	if json.Unmarshal(body, &ev) == nil && (ev.Event == "payment.captured" || ev.Event == "order.paid") {
		e := ev.Payload.Payment.Entity
		if _, err := s.Store.Order(e.OrderID); err == nil {
			if _, _, err := s.settle(r.Context(), e.OrderID, e.ID); err != nil && s.Log != nil {
				s.Log.Error("razorpay webhook", "order", e.OrderID, "err", err)
			}
		}
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) check(w http.ResponseWriter, r *http.Request) {
	if !s.limit(r, "check", 120, time.Hour) {
		writeErr(w, http.StatusTooManyRequests, "too many checks")
		return
	}
	var in struct {
		ID     string `json:"id"`
		Server string `json:"server"`
	}
	if !decode(w, r, &in) {
		return
	}
	writeJSON(w, http.StatusOK, s.Store.Check(in.ID, in.Server))
}

// updates tells a server the newest release on its channel.
func (s *Server) updates(w http.ResponseWriter, r *http.Request) {
	if !s.limit(r, "updates", 120, time.Hour) {
		writeErr(w, http.StatusTooManyRequests, "too many update checks")
		return
	}
	var in struct {
		Channel string `json:"channel"`
		License string `json:"license"`
		Server  string `json:"server"`
		Current string `json:"current"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := s.Store.MayUpdate(in.Channel, in.License, in.Server); err != nil {
		s.fail(w, err)
		return
	}
	rel, ok := s.Store.Latest(in.Channel)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"channel": in.Channel})
		return
	}
	q := url.Values{"version": {rel.Version}, "channel": {in.Channel}, "license": {in.License}, "server": {in.Server}}
	writeJSON(w, http.StatusOK, map[string]any{"channel": in.Channel, "release": rel.Manifest, "signature": rel.Signature,
		"download": "/api/v1/updates/download?" + q.Encode()})
}

func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	if !s.limit(r, "download", 30, time.Hour) {
		writeErr(w, http.StatusTooManyRequests, "too many downloads")
		return
	}
	q := r.URL.Query()
	ch := q.Get("channel")
	if err := s.Store.MayUpdate(ch, q.Get("license"), q.Get("server")); err != nil {
		s.fail(w, err)
		return
	}
	rel, ok := s.Store.releaseOn(q.Get("version"), ch)
	if !ok {
		writeErr(w, http.StatusNotFound, "no such release on this channel")
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="p8sos-`+rel.Version+`.tar.gz"`)
	http.ServeFile(w, r, s.Store.ReleaseFile(rel.Version))
}
