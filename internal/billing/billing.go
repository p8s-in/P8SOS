// Package billing is the money side of P8S Cloud: a prepaid wallet per
// customer, hourly charges for servers on a plan (capped at the monthly
// price, like the big cloud providers), tax invoices for top-ups, and the
// rules for low and negative balances (warn, then stop servers after a grace
// period).
//
// Storage: ledger.jsonl and invoices.jsonl (append-only), billing.json (state).
// All amounts are integers in the currency's smallest unit (paise).
package billing

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Entry is one line in a customer's account.
type Entry struct {
	ID      string `json:"id"`
	Time    string `json:"time"`
	User    string `json:"user"`
	Kind    string `json:"kind"`   // topup, charge, adjust
	Amount  int64  `json:"amount"` // + credit, - debit
	VM      string `json:"vm,omitempty"`
	Plan    string `json:"plan,omitempty"`
	Hours   int    `json:"hours,omitempty"`
	Note    string `json:"note,omitempty"`
	Ref     string `json:"ref,omitempty"`     // payment id
	Invoice string `json:"invoice,omitempty"` // invoice number of a top-up
}

// Sub is a server being charged by the hour.
type Sub struct {
	VM           string    `json:"vm"`
	Owner        string    `json:"owner"`
	Plan         string    `json:"plan"`
	PlanName     string    `json:"plan_name"`
	Monthly      int64     `json:"monthly"`
	Hourly       int64     `json:"hourly"`
	Since        time.Time `json:"since"`
	PaidUntil    time.Time `json:"paid_until"` // charged up to here
	Month        string    `json:"month"`      // YYYY-MM of MonthCharged
	MonthCharged int64     `json:"month_charged"`
}

// Party is a seller or buyer on an invoice.
type Party struct {
	Name    string `json:"name"`
	Address string `json:"address,omitempty"`
	TaxID   string `json:"tax_id,omitempty"`
	Email   string `json:"email,omitempty"`
}

// Invoice is a tax invoice for a wallet top-up.
type Invoice struct {
	Number     string `json:"number"`
	Date       string `json:"date"`
	User       string `json:"user"`
	Seller     Party  `json:"seller"`
	Buyer      Party  `json:"buyer"`
	Item       string `json:"item"`
	Currency   string `json:"currency"`
	Subtotal   int64  `json:"subtotal"`
	TaxName    string `json:"tax_name"`
	TaxPercent int    `json:"tax_percent"`
	Tax        int64  `json:"tax"`
	Total      int64  `json:"total"`
	PaymentRef string `json:"payment_ref"`
	Method     string `json:"method"` // razorpay, manual
}

// Order is a top-up waiting for payment.
type Order struct {
	ID      string `json:"id"` // payment provider's order id
	User    string `json:"user"`
	Credit  int64  `json:"credit"` // goes into the wallet (before tax)
	Tax     int64  `json:"tax"`
	Total   int64  `json:"total"` // what the customer pays
	Created string `json:"created"`
	Paid    string `json:"paid,omitempty"`
	Payment string `json:"payment,omitempty"`
}

type state struct {
	Subs        map[string]*Sub      `json:"subs"`
	Orders      map[string]*Order    `json:"orders"`
	NextInvoice map[string]int       `json:"next_invoice"` // per year
	Overdue     map[string]time.Time `json:"overdue"`      // user -> balance negative since
	Suspended   map[string]bool      `json:"suspended"`
	Warned      map[string]time.Time `json:"warned"` // low-balance mail sent
}

// Event tells the caller to act (send mail, stop servers).
type Event struct {
	Kind    string // low, negative, suspend, resume
	User    string
	Balance int64
	Hours   int // low: about how many hours are left
}

var (
	ErrInvalid   = errors.New("invalid")
	ErrNotFound  = errors.New("not found")
	ErrFunds     = errors.New("not enough credit")
	ErrSuspended = errors.New("account suspended for an unpaid balance; top up to continue")
)

type Store struct {
	Dir string
	Now func() time.Time

	mu       sync.Mutex
	loaded   bool
	st       state
	balances map[string]int64
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Store) path(n string) string { return filepath.Join(s.Dir, n) }

func (s *Store) load() error {
	if s.loaded {
		return nil
	}
	s.st = state{Subs: map[string]*Sub{}, Orders: map[string]*Order{}, NextInvoice: map[string]int{},
		Overdue: map[string]time.Time{}, Suspended: map[string]bool{}, Warned: map[string]time.Time{}}
	if b, err := os.ReadFile(s.path("billing.json")); err == nil {
		if err := json.Unmarshal(b, &s.st); err != nil {
			return fmt.Errorf("billing.json: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, m := range []any{&s.st.Subs, &s.st.Orders, &s.st.NextInvoice, &s.st.Overdue, &s.st.Suspended, &s.st.Warned} {
		switch v := m.(type) {
		case *map[string]*Sub:
			if *v == nil {
				*v = map[string]*Sub{}
			}
		case *map[string]*Order:
			if *v == nil {
				*v = map[string]*Order{}
			}
		case *map[string]int:
			if *v == nil {
				*v = map[string]int{}
			}
		case *map[string]time.Time:
			if *v == nil {
				*v = map[string]time.Time{}
			}
		case *map[string]bool:
			if *v == nil {
				*v = map[string]bool{}
			}
		}
	}
	s.balances = map[string]int64{}
	err := readLines(s.path("ledger.jsonl"), func(b []byte) {
		var e Entry
		if json.Unmarshal(b, &e) == nil {
			s.balances[e.User] += e.Amount
		}
	})
	if err != nil {
		return err
	}
	s.loaded = true
	return nil
}

func readLines(path string, fn func([]byte)) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		fn(sc.Bytes())
	}
	return sc.Err()
}

func appendLine(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

func (s *Store) saveState() error {
	b, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o750); err != nil {
		return err
	}
	tmp := s.path("billing.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path("billing.json"))
}

func newID() string { b := make([]byte, 8); _, _ = rand.Read(b); return hex.EncodeToString(b) }

// post writes a ledger entry (caller holds mu).
func (s *Store) post(e Entry) (Entry, error) {
	if e.ID == "" {
		e.ID = newID()
	}
	if e.Time == "" {
		e.Time = s.now().Format(time.RFC3339)
	}
	if err := appendLine(s.path("ledger.jsonl"), e); err != nil {
		return Entry{}, err
	}
	s.balances[e.User] += e.Amount
	return e, nil
}

// Balance is the user's wallet.
func (s *Store) Balance(user string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.load() != nil {
		return 0
	}
	return s.balances[user]
}

// Balances returns every user's wallet.
func (s *Store) Balances() map[string]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int64{}
	if s.load() != nil {
		return out
	}
	for k, v := range s.balances {
		out[k] = v
	}
	return out
}

// Suspended reports whether the user's servers were stopped for non-payment.
func (s *Store) Suspended(user string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.load() != nil {
		return false
	}
	return s.st.Suspended[user]
}

// Entries returns the newest entries of a user ("" = everyone), newest first.
func (s *Store) Entries(user string, limit int) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var all []Entry
	err := readLines(s.path("ledger.jsonl"), func(b []byte) {
		var e Entry
		if json.Unmarshal(b, &e) == nil && (user == "" || e.User == user) {
			all = append(all, e)
		}
	})
	out := []Entry{}
	for i := len(all) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, all[i])
	}
	return out, err
}

// Adjust adds (or with a negative amount removes) credit by hand, e.g. a
// bank transfer, a refund or a goodwill credit.
func (s *Store) Adjust(user string, amount int64, note string) (Entry, error) {
	if amount == 0 || len(note) > 200 {
		return Entry{}, fmt.Errorf("%w: amount must not be 0; note up to 200 characters", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return Entry{}, err
	}
	return s.post(Entry{User: user, Kind: "adjust", Amount: amount, Note: note})
}

// --- subscriptions ---

// Subscribe starts charging for a server; the first hour is charged at once.
func (s *Store) Subscribe(vm, owner, planID, planName string, monthly, hourly int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return err
	}
	if _, ok := s.st.Subs[vm]; ok {
		return fmt.Errorf("%w: %s is already billed", ErrInvalid, vm)
	}
	now := s.now()
	sub := &Sub{VM: vm, Owner: owner, Plan: planID, PlanName: planName, Monthly: monthly, Hourly: hourly, Since: now, PaidUntil: now}
	s.st.Subs[vm] = sub
	if _, err := s.chargeHours(sub, now.Add(time.Hour)); err != nil {
		delete(s.st.Subs, vm)
		return err
	}
	return s.saveState()
}

// Unsubscribe stops charging (server deleted). Started hours are not refunded.
func (s *Store) Unsubscribe(vm string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return err
	}
	if _, ok := s.st.Subs[vm]; !ok {
		return nil
	}
	delete(s.st.Subs, vm)
	return s.saveState()
}

// Sub returns a server's billing, if it has any.
func (s *Store) Sub(vm string) (Sub, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.load() != nil {
		return Sub{}, false
	}
	x, ok := s.st.Subs[vm]
	if !ok {
		return Sub{}, false
	}
	return *x, true
}

// Subs lists billed servers of a user ("" = all).
func (s *Store) Subs(owner string) []Sub {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Sub{}
	if s.load() != nil {
		return out
	}
	for _, x := range s.st.Subs {
		if owner == "" || x.Owner == owner {
			out = append(out, *x)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].VM < out[j].VM })
	return out
}

// chargeHours charges sub up to `until` (whole hours), never more than the
// monthly price per calendar month. Caller holds mu.
func (s *Store) chargeHours(sub *Sub, until time.Time) (int, error) {
	hours := 0
	var amount int64
	for !sub.PaidUntil.Add(time.Hour).After(until) {
		month := sub.PaidUntil.Format("2006-01")
		if month != sub.Month {
			sub.Month, sub.MonthCharged = month, 0
		}
		h := sub.Hourly
		if sub.MonthCharged+h > sub.Monthly {
			h = max(sub.Monthly-sub.MonthCharged, 0)
		}
		sub.MonthCharged += h
		amount += h
		sub.PaidUntil = sub.PaidUntil.Add(time.Hour)
		hours++
	}
	if hours == 0 || amount == 0 {
		return hours, nil
	}
	_, err := s.post(Entry{User: sub.Owner, Kind: "charge", Amount: -amount, VM: sub.VM, Plan: sub.Plan, Hours: hours,
		Note: fmt.Sprintf("%s: %d h of %s", sub.VM, hours, sub.PlanName)})
	return hours, err
}

// Tick charges every server for the hours that started since the last tick
// and applies the balance rules. graceHours: how long a negative balance may
// last before servers are stopped. Returns what the caller must do.
func (s *Store) Tick(graceHours int) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return nil, err
	}
	now := s.now()
	var errs []error
	users := map[string]int64{} // user -> hourly burn
	for _, sub := range s.st.Subs {
		if _, err := s.chargeHours(sub, now.Add(time.Hour)); err != nil { // the hour that just began is paid in advance
			errs = append(errs, err)
		}
		users[sub.Owner] += sub.Hourly
	}
	for u := range s.st.Overdue {
		if _, ok := users[u]; !ok {
			users[u] = 0
		}
	}
	for u := range s.st.Suspended {
		if _, ok := users[u]; !ok {
			users[u] = 0
		}
	}
	var ev []Event
	names := make([]string, 0, len(users))
	for u := range users {
		names = append(names, u)
	}
	sort.Strings(names)
	for _, u := range names {
		burn, bal := users[u], s.balances[u]
		switch {
		case bal < 0:
			since, over := s.st.Overdue[u]
			if !over {
				s.st.Overdue[u] = now
				ev = append(ev, Event{Kind: "negative", User: u, Balance: bal})
			} else if !s.st.Suspended[u] && now.Sub(since) >= time.Duration(graceHours)*time.Hour {
				s.st.Suspended[u] = true
				ev = append(ev, Event{Kind: "suspend", User: u, Balance: bal})
			}
		default:
			if _, over := s.st.Overdue[u]; over {
				delete(s.st.Overdue, u)
			}
			if s.st.Suspended[u] {
				delete(s.st.Suspended, u)
				ev = append(ev, Event{Kind: "resume", User: u, Balance: bal})
			}
			if burn > 0 && bal < burn*72 && now.Sub(s.st.Warned[u]) > 24*time.Hour {
				s.st.Warned[u] = now
				ev = append(ev, Event{Kind: "low", User: u, Balance: bal, Hours: int(bal / burn)})
			}
		}
	}
	errs = append(errs, s.saveState())
	return ev, errors.Join(errs...)
}

// --- top-ups and invoices ---

// Tax returns the tax on an amount (rounded to the nearest unit).
func Tax(amount int64, percent int) int64 { return (amount*int64(percent) + 50) / 100 }

// NewOrder records a top-up waiting for payment (id = the payment provider's order id).
func (s *Store) NewOrder(id, user string, credit int64, taxPercent int) (Order, error) {
	if credit <= 0 {
		return Order{}, fmt.Errorf("%w: amount", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return Order{}, err
	}
	tax := Tax(credit, taxPercent)
	o := &Order{ID: id, User: user, Credit: credit, Tax: tax, Total: credit + tax, Created: s.now().Format(time.RFC3339)}
	s.st.Orders[id] = o
	return *o, s.saveState()
}

// Order returns a top-up order.
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

// Paid credits a paid order once (later calls with the same order return
// the first result) and writes its tax invoice.
func (s *Store) Paid(orderID, paymentID, method string, seller, buyer Party, currency, taxName string, taxPercent int, prefix string) (Invoice, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return Invoice{}, false, err
	}
	o, ok := s.st.Orders[orderID]
	if !ok {
		return Invoice{}, false, fmt.Errorf("%w: order %s", ErrNotFound, orderID)
	}
	if o.Paid != "" {
		inv, err := s.invoiceByRef(o.Payment)
		return inv, false, err
	}
	now := s.now()
	inv, err := s.newInvoice(now, o.User, seller, buyer, currency, o.Credit, taxName, taxPercent, o.Tax, paymentID, method, prefix)
	if err != nil {
		return Invoice{}, false, err
	}
	if _, err := s.post(Entry{User: o.User, Kind: "topup", Amount: o.Credit, Ref: paymentID, Invoice: inv.Number, Note: "wallet top-up"}); err != nil {
		return Invoice{}, false, err
	}
	o.Paid, o.Payment = now.Format(time.RFC3339), paymentID
	return inv, true, s.saveState()
}

// ManualTopup records money received outside the payment gateway (bank
// transfer, UPI) with a tax invoice, like an online top-up.
func (s *Store) ManualTopup(user string, credit int64, ref string, seller, buyer Party, currency, taxName string, taxPercent int, prefix string) (Invoice, error) {
	if credit <= 0 || len(ref) > 100 {
		return Invoice{}, fmt.Errorf("%w: amount must be positive; reference up to 100 characters", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return Invoice{}, err
	}
	tax := Tax(credit, taxPercent)
	inv, err := s.newInvoice(s.now(), user, seller, buyer, currency, credit, taxName, taxPercent, tax, ref, "manual", prefix)
	if err != nil {
		return Invoice{}, err
	}
	if _, err := s.post(Entry{User: user, Kind: "topup", Amount: credit, Ref: ref, Invoice: inv.Number, Note: "payment received"}); err != nil {
		return Invoice{}, err
	}
	return inv, s.saveState()
}

func (s *Store) newInvoice(now time.Time, user string, seller, buyer Party, currency string, sub int64, taxName string, taxPercent int, tax int64, ref, method, prefix string) (Invoice, error) {
	year := now.Format("2006")
	s.st.NextInvoice[year]++
	inv := Invoice{
		Number: fmt.Sprintf("%s-%s-%06d", prefix, year, s.st.NextInvoice[year]), Date: now.Format("2006-01-02"),
		User: user, Seller: seller, Buyer: buyer, Item: "Prepaid cloud server credit", Currency: currency,
		Subtotal: sub, TaxName: taxName, TaxPercent: taxPercent, Tax: tax, Total: sub + tax, PaymentRef: ref, Method: method,
	}
	if err := appendLine(s.path("invoices.jsonl"), inv); err != nil {
		s.st.NextInvoice[year]--
		return Invoice{}, err
	}
	return inv, nil
}

func (s *Store) invoiceByRef(ref string) (Invoice, error) {
	var found Invoice
	err := readLines(s.path("invoices.jsonl"), func(b []byte) {
		var inv Invoice
		if json.Unmarshal(b, &inv) == nil && inv.PaymentRef == ref {
			found = inv
		}
	})
	if found.Number == "" && err == nil {
		err = fmt.Errorf("%w: invoice for %s", ErrNotFound, ref)
	}
	return found, err
}

// Invoices lists a user's invoices ("" = all), newest first.
func (s *Store) Invoices(user string) ([]Invoice, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var all []Invoice
	err := readLines(s.path("invoices.jsonl"), func(b []byte) {
		var inv Invoice
		if json.Unmarshal(b, &inv) == nil && (user == "" || inv.User == user) {
			all = append(all, inv)
		}
	})
	out := []Invoice{}
	for i := len(all) - 1; i >= 0; i-- {
		out = append(out, all[i])
	}
	return out, err
}

// InvoiceByNumber returns one invoice.
func (s *Store) InvoiceByNumber(n string) (Invoice, error) {
	l, err := s.Invoices("")
	if err != nil {
		return Invoice{}, err
	}
	for _, inv := range l {
		if inv.Number == n {
			return inv, nil
		}
	}
	return Invoice{}, fmt.Errorf("%w: invoice %s", ErrNotFound, n)
}
