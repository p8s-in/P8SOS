package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"p8sos/internal/billing"
	"p8sos/internal/cloud"
	"p8sos/internal/license"
	"p8sos/internal/mail"
	"p8sos/internal/support"
	"p8sos/internal/users"
	"p8sos/internal/vm"
)

// P8S Cloud: what a hosting customer sees — sign-up, priced plans, a prepaid
// wallet with online payment and invoices, emails, support tickets and
// personal API tokens.

func (s *Server) cloudOn(w http.ResponseWriter) bool {
	if s.Cloud != nil && s.Billing != nil {
		return true
	}
	writeErr(w, http.StatusNotFound, "not_found", "P8S Cloud features are not available on this host")
	return false
}

func (s *Server) brand() mail.Brand {
	if s.Cloud == nil {
		return mail.Brand{Name: "P8S OS"}
	}
	st := s.Cloud.Settings()
	if !s.License.Allows(license.FeatBrand) {
		st.Brand = cloud.Defaults().Brand
	}
	return mail.Brand{Name: st.Brand, URL: st.PublicURL, Support: st.SupportEmail}
}

// notify emails a user if they have an address.
func (s *Server) notify(user string, m mail.Message, kind string) {
	if s.Mail == nil || s.Users == nil {
		return
	}
	if u, err := s.Users.Get(user); err == nil && u.Email != "" {
		s.Mail.Queue(u.Email, m.Subject, m.Body, kind)
	}
}

func (s *Server) cloudErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, cloud.ErrInvalid), errors.Is(err, billing.ErrInvalid), errors.Is(err, support.ErrInvalid):
		writeErr(w, http.StatusBadRequest, "invalid", err.Error())
	case errors.Is(err, cloud.ErrNotFound), errors.Is(err, billing.ErrNotFound), errors.Is(err, support.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, billing.ErrSuspended):
		writeErr(w, http.StatusForbidden, "suspended", err.Error())
	case errors.Is(err, billing.ErrFunds):
		writeErr(w, http.StatusPaymentRequired, "funds", err.Error())
	default:
		s.fail(w, r, err)
	}
}

// --- plans on server creation ---

// applyPlan fills a create request from its plan. Customers must pick a plan
// once plans exist (that's how they are billed). Returns the plan (nil = none).
func (s *Server) applyPlan(w http.ResponseWriter, r *http.Request, req *vm.CreateRequest) (*cloud.Plan, bool) {
	if s.Cloud == nil || s.Billing == nil {
		if req.Plan != "" {
			writeErr(w, http.StatusBadRequest, "invalid", "plans are not available on this host")
			return nil, false
		}
		return nil, true
	}
	p := who(r)
	if req.Plan == "" {
		if !p.Admin && len(s.Cloud.Plans()) > 0 {
			writeErr(w, http.StatusBadRequest, "invalid", "choose a plan (size) for the server")
			return nil, false
		}
		return nil, true
	}
	plan, err := s.Cloud.Plan(req.Plan)
	if err != nil || (plan.Hidden && !p.Admin) {
		writeErr(w, http.StatusNotFound, "not_found", fmt.Sprintf("no plan %q", req.Plan))
		return nil, false
	}
	req.VCPUs, req.MemoryMiB, req.DiskGB = plan.VCPUs, plan.MemoryMiB, plan.DiskGB
	if plan.PublicIP {
		pool := s.Cloud.Settings().PublicPool
		if pool == "" || s.Pools == nil {
			writeErr(w, http.StatusConflict, "conflict", "this plan includes a public IP, but no IP pool is set for plans (admin: Settings → Cloud)")
			return nil, false
		}
		if v, err := s.Pools.Get(pool); err != nil || v.Free == 0 {
			writeErr(w, http.StatusConflict, "conflict", "no public IP addresses are free right now; please try a plan without one or contact support")
			return nil, false
		}
		req.IPPool, req.Network = pool, ""
	} else if req.IPPool != "" && !p.Admin {
		writeErr(w, http.StatusBadRequest, "invalid", "this plan has no public IP; choose a plan that includes one")
		return nil, false
	}
	return &plan, true
}

// canAfford: a customer needs credit for a day of the plan, and an active account.
func (s *Server) canAfford(w http.ResponseWriter, r *http.Request, user string, plan *cloud.Plan) bool {
	if s.Billing == nil || plan == nil {
		return true
	}
	if s.Billing.Suspended(user) {
		s.cloudErr(w, r, billing.ErrSuspended)
		return false
	}
	need := plan.Hourly() * 24
	if bal := s.Billing.Balance(user); bal < need {
		cur := s.Cloud.Settings().Currency
		s.cloudErr(w, r, fmt.Errorf("%w: this plan needs at least %s in your balance (one day); you have %s — add credit under Billing",
			billing.ErrFunds, mail.Money(need, cur), mail.Money(bal, cur)))
		return false
	}
	return true
}

// afterCreate starts billing and tells the owner their server is ready.
func (s *Server) afterCreate(r *http.Request, v vm.VM, plan *cloud.Plan) {
	if plan != nil && s.Billing != nil && v.Owner != "" {
		if err := s.Billing.Subscribe(v.Name, v.Owner, plan.ID, plan.Name, plan.PriceMonthly, plan.Hourly()); err != nil && s.Log != nil {
			s.Log.Error("start billing", "vm", v.Name, "err", err)
		}
	}
	if v.Owner != "" && s.Mail != nil {
		name := "custom size"
		if plan != nil {
			name = plan.Name
		}
		login := v.User
		if login == "" {
			login = "root"
		}
		s.notify(v.Owner, mail.ServerReady(s.brand(), v.Owner, v.Name, v.IP, login, name), "server-ready")
	}
}

// suspended stops customers whose servers were stopped for non-payment from starting them.
func (s *Server) suspended(w http.ResponseWriter, r *http.Request) bool {
	p := who(r)
	if p.Admin || s.Billing == nil || !s.Billing.Suspended(p.Name) {
		return false
	}
	s.cloudErr(w, r, billing.ErrSuspended)
	return true
}

// --- public: sign-up, email links ---

func (s *Server) signup(w http.ResponseWriter, r *http.Request) {
	if !s.usersOn(w) || !s.cloudOn(w) {
		return
	}
	if !s.License.Allows(license.FeatSelling) {
		writeErr(w, http.StatusForbidden, "closed", "sign-up is closed; ask the provider for an account")
		return
	}
	if s.userLimitReached(w) {
		return
	}
	st := s.Cloud.Settings()
	if !st.SignupOpen || s.Users.Count() == 0 {
		writeErr(w, http.StatusForbidden, "closed", "sign-up is closed; ask the provider for an account")
		return
	}
	var body struct {
		User     string `json:"user"`
		Email    string `json:"email"`
		Password string `json:"password"`
		FullName string `json:"full_name"`
	}
	if !decode(w, r, &body) {
		return
	}
	key := "su:" + clientIP(r)
	if wait, blocked := s.limiter().Blocked(key); blocked {
		writeErr(w, http.StatusTooManyRequests, "too_many_attempts", fmt.Sprintf("too many sign-ups from here; try again in %d minutes", int(wait.Minutes())+1))
		return
	}
	email, err := users.NormEmail(body.Email)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if s.Users.EmailTaken(email, "") {
		writeErr(w, http.StatusConflict, "exists", "an account with this email exists; sign in or reset the password")
		return
	}
	if len(body.FullName) > 100 || strings.ContainsAny(body.FullName, "<>\r\n") {
		writeErr(w, http.StatusBadRequest, "invalid", "name: up to 100 characters")
		return
	}
	q := users.Quota{VMs: st.SignupQuota.VMs, VCPUs: st.SignupQuota.VCPUs, MemoryMiB: st.SignupQuota.MemoryMiB,
		DiskGB: st.SignupQuota.DiskGB, Sites: st.SignupQuota.Sites, PublicIPs: st.SignupQuota.PublicIPs}
	name := strings.ToLower(strings.TrimSpace(body.User))
	if _, err := s.Users.Add(name, users.Customer, body.Password, q); err != nil {
		s.fail(w, r, err)
		return
	}
	s.limiter().Fail(key)   // counts sign-ups per address
	verified := !st.Email() // without email there's nothing to confirm
	_, _ = s.Users.Update(name, func(u *users.User) error {
		u.Email, u.Verified, u.FullName = email, verified, strings.TrimSpace(body.FullName)
		return nil
	})
	s.audit(r, name, http.StatusCreated, "signed up")
	if verified {
		tok, ttl := s.Sessions.New(name)
		s.notify(name, mail.Welcome(s.brand(), name), "welcome")
		writeJSON(w, http.StatusCreated, map[string]any{"token": tok, "idle_timeout": int(ttl.Seconds())})
		return
	}
	code, err := s.Users.NewCode(name, "verify", 48*time.Hour)
	if err == nil {
		m := mail.Verify(s.brand(), name, code)
		s.Mail.Queue(email, m.Subject, m.Body, "verify")
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"verify": true, "email": email})
}

func (s *Server) verifyEmail(w http.ResponseWriter, r *http.Request) {
	if !s.usersOn(w) {
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &body) {
		return
	}
	u, err := s.Users.UseCode("verify", body.Code)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_code", err.Error())
		return
	}
	_, _ = s.Users.Update(u.Name, func(x *users.User) error { x.Verified = true; return nil })
	s.notify(u.Name, mail.Welcome(s.brand(), u.Name), "welcome")
	s.audit(r, u.Name, http.StatusOK, "email confirmed")
	tok, ttl := s.Sessions.New(u.Name)
	writeJSON(w, http.StatusOK, map[string]any{"token": tok, "idle_timeout": int(ttl.Seconds())})
}

// resendVerify always answers 204 (doesn't tell whether the account exists).
func (s *Server) resendVerify(w http.ResponseWriter, r *http.Request) {
	var body struct {
		User string `json:"user"`
	}
	if !decode(w, r, &body) {
		return
	}
	key := "rv:" + clientIP(r)
	if _, blocked := s.limiter().Blocked(key); !blocked && s.Users != nil && s.Mail != nil {
		s.limiter().Fail(key)
		name := strings.ToLower(strings.TrimSpace(body.User))
		if u, err := s.Users.Get(name); err == nil && u.Email != "" && !u.Verified {
			if code, err := s.Users.NewCode(u.Name, "verify", 48*time.Hour); err == nil {
				m := mail.Verify(s.brand(), u.Name, code)
				s.Mail.Queue(u.Email, m.Subject, m.Body, "verify")
			}
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// forgotPassword always answers 204 (no account enumeration).
func (s *Server) forgotPassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email string `json:"email"`
	}
	if !decode(w, r, &body) {
		return
	}
	key := "fp:" + clientIP(r)
	if _, blocked := s.limiter().Blocked(key); !blocked && s.Users != nil && s.Mail != nil {
		s.limiter().Fail(key)
		if u, err := s.Users.ByEmail(body.Email); err == nil && !u.Disabled {
			if code, err := s.Users.NewCode(u.Name, "reset", time.Hour); err == nil {
				m := mail.Reset(s.brand(), u.Name, code)
				s.Mail.Queue(u.Email, m.Subject, m.Body, "reset")
				s.audit(r, u.Name, http.StatusOK, "password reset requested")
			}
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) resetPassword(w http.ResponseWriter, r *http.Request) {
	if !s.usersOn(w) {
		return
	}
	var body struct {
		Code     string `json:"code"`
		Password string `json:"password"`
	}
	if !decode(w, r, &body) {
		return
	}
	if err := users.CheckPassword(body.Password); err != nil {
		s.fail(w, r, err)
		return
	}
	u, err := s.Users.UseCode("reset", body.Code)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_code", err.Error())
		return
	}
	if err := s.Users.SetPassword(u.Name, body.Password); err != nil {
		s.fail(w, r, err)
		return
	}
	// the link proves the mailbox: confirm the address too
	_, _ = s.Users.Update(u.Name, func(x *users.User) error { x.Verified = true; return nil })
	s.Sessions.EndUser(u.Name, "")
	s.audit(r, u.Name, http.StatusOK, "password reset")
	w.WriteHeader(http.StatusNoContent)
}

// --- profile and personal API tokens ---

func (s *Server) setProfile(w http.ResponseWriter, r *http.Request) {
	if !s.accountOnly(w, r) {
		return
	}
	var body struct {
		Email    *string `json:"email"`
		FullName string  `json:"full_name"`
		Phone    string  `json:"phone"`
		Company  string  `json:"company"`
		GSTIN    string  `json:"gstin"`
		Address  string  `json:"address"`
	}
	if !decode(w, r, &body) {
		return
	}
	for _, f := range []string{body.FullName, body.Phone, body.Company, body.GSTIN} {
		if len(f) > 100 || strings.ContainsAny(f, "<>\r\n") {
			writeErr(w, http.StatusBadRequest, "invalid", "name, phone, company, GSTIN: one line, up to 100 characters")
			return
		}
	}
	if len(body.Address) > 400 || strings.ContainsAny(body.Address, "<>") {
		writeErr(w, http.StatusBadRequest, "invalid", "address: up to 400 characters")
		return
	}
	p := who(r)
	newEmail := ""
	if body.Email != nil {
		cur, _ := s.Users.Get(p.Name)
		e := ""
		if strings.TrimSpace(*body.Email) != "" {
			var err error
			if e, err = users.NormEmail(*body.Email); err != nil {
				s.fail(w, r, err)
				return
			}
		}
		if e != cur.Email {
			if e != "" && s.Users.EmailTaken(e, p.Name) {
				writeErr(w, http.StatusConflict, "exists", "another account uses that email")
				return
			}
			newEmail = e
			if e == "" {
				newEmail = "-" // remove
			}
		}
	}
	mailOn := s.Cloud != nil && s.Cloud.Settings().Email()
	u, err := s.Users.Update(p.Name, func(x *users.User) error {
		x.FullName, x.Phone, x.Company, x.GSTIN, x.Address = strings.TrimSpace(body.FullName), strings.TrimSpace(body.Phone),
			strings.TrimSpace(body.Company), strings.ToUpper(strings.TrimSpace(body.GSTIN)), strings.TrimSpace(body.Address)
		switch newEmail {
		case "":
		case "-":
			x.Email, x.Verified = "", false
		default:
			x.Email, x.Verified = newEmail, !mailOn || p.Admin
		}
		return nil
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if newEmail != "" && newEmail != "-" && !u.Verified {
		if code, err := s.Users.NewCode(p.Name, "verify", 48*time.Hour); err == nil {
			m := mail.Verify(s.brand(), p.Name, code)
			s.Mail.Queue(newEmail, m.Subject, m.Body, "verify")
		}
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) {
	if !s.accountOnly(w, r) {
		return
	}
	u, err := s.Users.Get(who(r).Name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := []users.APIToken{}
	for _, t := range u.Tokens {
		t.SHA256 = ""
		out = append(out, t)
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
}

func (s *Server) newToken(w http.ResponseWriter, r *http.Request) {
	if !s.accountOnly(w, r) {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &body) {
		return
	}
	t, secret, err := s.Users.NewToken(who(r).Name, body.Name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	t.SHA256 = ""
	writeJSON(w, http.StatusCreated, map[string]any{"token": t, "secret": secret})
}

func (s *Server) deleteToken(w http.ResponseWriter, r *http.Request) {
	if !s.accountOnly(w, r) {
		return
	}
	if err := s.Users.DeleteToken(who(r).Name, r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- plans and settings ---

type planView struct {
	cloud.Plan
	Hourly int64 `json:"hourly"`
}

func (s *Server) listPlans(w http.ResponseWriter, r *http.Request) {
	out := []planView{}
	if s.Cloud != nil {
		admin := who(r).Admin
		for _, p := range s.Cloud.Plans() {
			if admin || !p.Hidden {
				out = append(out, planView{p, p.Hourly()})
			}
		}
	}
	cur := "INR"
	if s.Cloud != nil {
		cur = s.Cloud.Settings().Currency
	}
	writeJSON(w, http.StatusOK, map[string]any{"plans": out, "currency": cur})
}

func (s *Server) putPlan(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.cloudOn(w) || !s.needs(w, license.FeatSelling, "Selling priced plans") {
		return
	}
	var p cloud.Plan
	if !decode(w, r, &p) {
		return
	}
	p.ID = r.PathValue("id")
	p, err := s.Cloud.PutPlan(p)
	if err != nil {
		s.cloudErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, planView{p, p.Hourly()})
}

func (s *Server) deletePlan(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.cloudOn(w) {
		return
	}
	if err := s.Cloud.DeletePlan(r.PathValue("id")); err != nil {
		s.cloudErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getCloudSettings(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.cloudOn(w) {
		return
	}
	writeJSON(w, http.StatusOK, s.Cloud.Settings().Redacted())
}

func (s *Server) putCloudSettings(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.cloudOn(w) {
		return
	}
	var st cloud.Settings
	if !decode(w, r, &st) {
		return
	}
	st, err := s.Cloud.SetSettings(st)
	if err != nil {
		s.cloudErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st.Redacted())
}

func (s *Server) testEmail(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.cloudOn(w) || s.Mail == nil {
		return
	}
	var body struct {
		To string `json:"to"`
	}
	if !decode(w, r, &body) {
		return
	}
	to, err := users.NormEmail(body.To)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	b := s.brand()
	if err := s.Mail.SendNow(to, "Test email from "+b.Name, "It works: "+b.Name+" can send email.\n", "test"); err != nil {
		writeErr(w, http.StatusBadGateway, "mail", "sending failed: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) outbox(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"emails": s.Mail.Recent(100)})
}

// --- billing ---

// billingUser is whose billing the request is about (admins may pass ?user=).
func (s *Server) billingUser(r *http.Request) string {
	p := who(r)
	if u := r.URL.Query().Get("user"); u != "" && p.Admin {
		return u
	}
	return p.Name
}

func (s *Server) getBilling(w http.ResponseWriter, r *http.Request) {
	if !s.cloudOn(w) {
		return
	}
	u := s.billingUser(r)
	st := s.Cloud.Settings()
	subs := s.Billing.Subs(u)
	var burn int64
	for _, x := range subs {
		burn += x.Hourly
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user": u, "balance": s.Billing.Balance(u), "currency": st.Currency, "suspended": s.Billing.Suspended(u),
		"hourly": burn, "servers": subs, "payments": st.Payments(), "min_topup": st.MinTopup,
		"tax_name": st.TaxName, "tax_percent": st.TaxPercent, "grace_hours": st.GraceHours,
	})
}

func (s *Server) billingEntries(w http.ResponseWriter, r *http.Request) {
	if !s.cloudOn(w) {
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if n <= 0 || n > 1000 {
		n = 200
	}
	l, err := s.Billing.Entries(s.billingUser(r), n)
	if err != nil {
		s.cloudErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": l})
}

func (s *Server) razorpay(st cloud.Settings) *billing.Razorpay {
	if s.Razorpay != nil {
		return s.Razorpay(st)
	}
	return &billing.Razorpay{KeyID: st.RazorpayKeyID, KeySecret: st.RazorpayKeySecret}
}

// topup starts an online payment: a Razorpay order the browser pays with Checkout.
func (s *Server) topup(w http.ResponseWriter, r *http.Request) {
	if !s.cloudOn(w) || !s.accountOnly(w, r) {
		return
	}
	var body struct {
		Amount int64 `json:"amount"` // credit wanted, smallest unit, before tax
	}
	if !decode(w, r, &body) {
		return
	}
	st := s.Cloud.Settings()
	if !st.Payments() {
		writeErr(w, http.StatusConflict, "conflict", "online payment isn't set up yet; contact support to add credit")
		return
	}
	if body.Amount < st.MinTopup || body.Amount > 100000000 {
		writeErr(w, http.StatusBadRequest, "invalid", "the smallest top-up is "+mail.Money(st.MinTopup, st.Currency))
		return
	}
	p := who(r)
	total := body.Amount + billing.Tax(body.Amount, st.TaxPercent)
	id, err := s.razorpay(st).CreateOrder(r.Context(), total, st.Currency, "p8s-"+p.Name+"-"+strconv.FormatInt(time.Now().Unix(), 36),
		map[string]string{"user": p.Name})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	o, err := s.Billing.NewOrder(id, p.Name, body.Amount, st.TaxPercent)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	u, _ := s.Users.Get(p.Name)
	writeJSON(w, http.StatusCreated, map[string]any{
		"order_id": o.ID, "key_id": st.RazorpayKeyID, "amount": o.Total, "credit": o.Credit, "tax": o.Tax,
		"currency": st.Currency, "name": st.Brand, "prefill": map[string]string{"name": u.FullName, "email": u.Email, "contact": u.Phone},
	})
}

func (s *Server) parties(user string) (seller, buyer billing.Party) {
	st := s.Cloud.Settings()
	seller = billing.Party{Name: st.SellerName, Address: st.SellerAddress, TaxID: st.SellerTaxID, Email: st.SupportEmail}
	if seller.Name == "" {
		seller.Name = st.Brand
	}
	buyer = billing.Party{Name: user}
	if u, err := s.Users.Get(user); err == nil {
		buyer = billing.Party{Name: firstNonEmpty(u.Company, u.FullName, u.Name), Address: u.Address, TaxID: u.GSTIN, Email: u.Email}
	}
	return
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// settle credits a paid order once (from the browser or the webhook).
func (s *Server) settle(ctx context.Context, orderID, paymentID string) (billing.Invoice, error) {
	st := s.Cloud.Settings()
	o, err := s.Billing.Order(orderID)
	if err != nil {
		return billing.Invoice{}, err
	}
	rz := s.razorpay(st)
	pay, err := rz.FetchPayment(ctx, paymentID)
	if err != nil {
		return billing.Invoice{}, err
	}
	if pay.OrderID != orderID || pay.Amount != o.Total || pay.Currency != st.Currency {
		return billing.Invoice{}, fmt.Errorf("%w: payment %s doesn't match order %s", billing.ErrInvalid, paymentID, orderID)
	}
	if pay.Status == "authorized" {
		if pay, err = rz.Capture(ctx, paymentID, o.Total, st.Currency); err != nil {
			return billing.Invoice{}, err
		}
	}
	if pay.Status != "captured" {
		return billing.Invoice{}, fmt.Errorf("%w: payment is %s, not captured", billing.ErrInvalid, pay.Status)
	}
	seller, buyer := s.parties(o.User)
	inv, fresh, err := s.Billing.Paid(orderID, paymentID, "razorpay", seller, buyer, st.Currency, st.TaxName, st.TaxPercent, st.InvoicePrefix)
	if err == nil && fresh {
		s.notify(o.User, mail.PaymentReceived(s.brand(), o.User, inv.Number, inv.Subtotal, inv.Total, inv.Currency), "payment")
	}
	return inv, err
}

func (s *Server) topupVerify(w http.ResponseWriter, r *http.Request) {
	if !s.cloudOn(w) {
		return
	}
	var body struct {
		OrderID   string `json:"razorpay_order_id"`
		PaymentID string `json:"razorpay_payment_id"`
		Signature string `json:"razorpay_signature"`
	}
	if !decode(w, r, &body) {
		return
	}
	st := s.Cloud.Settings()
	o, err := s.Billing.Order(body.OrderID)
	if err != nil || o.User != who(r).Name {
		writeErr(w, http.StatusNotFound, "not_found", "no such payment")
		return
	}
	if !billing.CheckoutSignature(o.ID, body.PaymentID, body.Signature, st.RazorpayKeySecret) {
		writeErr(w, http.StatusBadRequest, "invalid", "the payment signature is wrong")
		return
	}
	inv, err := s.settle(r.Context(), o.ID, body.PaymentID)
	if err != nil {
		s.cloudErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"invoice": inv, "balance": s.Billing.Balance(o.User)})
}

// razorpayWebhook confirms payments server-to-server (if the browser closed early).
func (s *Server) razorpayWebhook(w http.ResponseWriter, r *http.Request) {
	if s.Cloud == nil || s.Billing == nil {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return
	}
	st := s.Cloud.Settings()
	if !billing.WebhookSignature(body, r.Header.Get("X-Razorpay-Signature"), st.RazorpayWebhookSecret) {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "bad signature")
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
	if err := decodeBytes(body, &ev); err == nil && (ev.Event == "payment.captured" || ev.Event == "order.paid") {
		e := ev.Payload.Payment.Entity
		if _, err := s.Billing.Order(e.OrderID); err == nil {
			if _, err := s.settle(r.Context(), e.OrderID, e.ID); err != nil && s.Log != nil {
				s.Log.Error("razorpay webhook", "order", e.OrderID, "err", err)
			}
		}
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) listInvoices(w http.ResponseWriter, r *http.Request) {
	if !s.cloudOn(w) {
		return
	}
	user := s.billingUser(r)
	if who(r).Admin && r.URL.Query().Get("user") == "" && r.URL.Query().Get("all") != "" {
		user = ""
	}
	l, err := s.Billing.Invoices(user)
	if err != nil {
		s.cloudErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"invoices": l})
}

func (s *Server) getInvoice(w http.ResponseWriter, r *http.Request) {
	if !s.cloudOn(w) {
		return
	}
	inv, err := s.Billing.InvoiceByNumber(r.PathValue("number"))
	if err == nil && !who(r).Admin && inv.User != who(r).Name {
		err = billing.ErrNotFound
	}
	if err != nil {
		s.cloudErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, inv)
}

// adjust: admin credit/debit; with "invoice" it's a received payment (bank/UPI) with a tax invoice.
func (s *Server) adjust(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.cloudOn(w) {
		return
	}
	var body struct {
		User    string `json:"user"`
		Amount  int64  `json:"amount"`
		Note    string `json:"note"`
		Invoice bool   `json:"invoice"`
	}
	if !decode(w, r, &body) {
		return
	}
	if _, err := s.Users.Get(body.User); err != nil {
		s.fail(w, r, err)
		return
	}
	if body.Invoice {
		st := s.Cloud.Settings()
		seller, buyer := s.parties(body.User)
		inv, err := s.Billing.ManualTopup(body.User, body.Amount, body.Note, seller, buyer, st.Currency, st.TaxName, st.TaxPercent, st.InvoicePrefix)
		if err != nil {
			s.cloudErr(w, r, err)
			return
		}
		s.notify(body.User, mail.PaymentReceived(s.brand(), body.User, inv.Number, inv.Subtotal, inv.Total, inv.Currency), "payment")
		writeJSON(w, http.StatusCreated, map[string]any{"invoice": inv, "balance": s.Billing.Balance(body.User)})
		return
	}
	e, err := s.Billing.Adjust(body.User, body.Amount, body.Note)
	if err != nil {
		s.cloudErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"entry": e, "balance": s.Billing.Balance(body.User)})
}

// customers: admin overview of balances and spending.
func (s *Server) customers(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) || !s.cloudOn(w) {
		return
	}
	list, err := s.Users.List()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	bal := s.Billing.Balances()
	burn := map[string]int64{}
	count := map[string]int{}
	for _, x := range s.Billing.Subs("") {
		burn[x.Owner] += x.Hourly
		count[x.Owner]++
	}
	type row struct {
		Name      string `json:"name"`
		Email     string `json:"email"`
		Balance   int64  `json:"balance"`
		Hourly    int64  `json:"hourly"`
		Servers   int    `json:"servers"`
		Suspended bool   `json:"suspended"`
	}
	out := []row{}
	for _, u := range list {
		if u.Role != users.Customer {
			continue
		}
		out = append(out, row{u.Name, u.Email, bal[u.Name], burn[u.Name], count[u.Name], s.Billing.Suspended(u.Name)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"customers": out, "currency": s.Cloud.Settings().Currency})
}

// BillingTick charges servers and enforces balance rules (p8sd calls it every few minutes).
func (s *Server) BillingTick(ctx context.Context) error {
	if s.Billing == nil || s.Cloud == nil {
		return nil
	}
	st := s.Cloud.Settings()
	ev, err := s.Billing.Tick(st.GraceHours)
	b := s.brand()
	for _, e := range ev {
		switch e.Kind {
		case "low":
			s.notify(e.User, mail.LowBalance(b, e.User, e.Balance, st.Currency, e.Hours), "low-balance")
		case "negative":
			s.notify(e.User, mail.Negative(b, e.User, e.Balance, st.Currency, st.GraceHours), "negative")
		case "resume":
			s.notify(e.User, mail.Resumed(b, e.User), "resumed")
		case "suspend":
			var stopped []string
			if all, lerr := s.VMs.List(ctx); lerr == nil {
				for _, v := range all {
					if v.Owner == e.User && v.State == "running" {
						if _, serr := s.VMs.Stop(ctx, v.Name, false); serr == nil {
							stopped = append(stopped, v.Name)
						}
					}
				}
			}
			if s.CTs != nil {
				if all, lerr := s.CTs.List(ctx); lerr == nil {
					for _, c := range all {
						if c.Owner == e.User && c.State == "running" {
							if _, serr := s.CTs.Stop(ctx, c.Name, false); serr == nil {
								stopped = append(stopped, c.Name)
							}
						}
					}
				}
			}
			if s.Log != nil {
				s.Log.Warn("customer suspended for unpaid balance", "user", e.User, "balance", e.Balance, "stopped", stopped)
			}
			_ = s.Audit.Write(auditEntry(e.User, fmt.Sprintf("suspended (balance %s); stopped %s", mail.Money(e.Balance, st.Currency), strings.Join(stopped, ", "))))
			s.notify(e.User, mail.Suspended(b, e.User, e.Balance, st.Currency, stopped), "suspended")
		}
	}
	return err
}

// --- support tickets ---

func (s *Server) supportOn(w http.ResponseWriter) bool {
	if s.Support != nil {
		return true
	}
	writeErr(w, http.StatusNotFound, "not_found", "support tickets are not available on this host")
	return false
}

func (s *Server) listTickets(w http.ResponseWriter, r *http.Request) {
	if !s.supportOn(w) {
		return
	}
	user := who(r).Name
	if who(r).Admin {
		user = r.URL.Query().Get("user")
	}
	l, err := s.Support.List(user)
	if err != nil {
		s.cloudErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tickets": l})
}

func (s *Server) ticketFor(w http.ResponseWriter, r *http.Request) (support.Ticket, bool) {
	t, err := s.Support.Get(r.PathValue("id"))
	if err == nil && !who(r).Admin && t.User != who(r).Name {
		err = support.ErrNotFound
	}
	if err != nil {
		s.cloudErr(w, r, err)
		return support.Ticket{}, false
	}
	return t, true
}

func (s *Server) openTicket(w http.ResponseWriter, r *http.Request) {
	if !s.supportOn(w) {
		return
	}
	var body struct {
		Subject string `json:"subject"`
		VM      string `json:"vm"`
		Text    string `json:"text"`
	}
	if !decode(w, r, &body) {
		return
	}
	t, err := s.Support.Open(who(r).Name, body.Subject, body.VM, body.Text)
	if err != nil {
		s.cloudErr(w, r, err)
		return
	}
	if s.Mail != nil && s.Cloud != nil {
		if to := s.Cloud.Settings().SupportEmail; to != "" {
			m := mail.TicketNew(s.brand(), t.User, t.Subject, body.Text)
			s.Mail.Queue(to, m.Subject, m.Body, "ticket")
		}
	}
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) getTicket(w http.ResponseWriter, r *http.Request) {
	if !s.supportOn(w) {
		return
	}
	if t, ok := s.ticketFor(w, r); ok {
		writeJSON(w, http.StatusOK, t)
	}
}

func (s *Server) replyTicket(w http.ResponseWriter, r *http.Request) {
	if !s.supportOn(w) {
		return
	}
	t, ok := s.ticketFor(w, r)
	if !ok {
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if !decode(w, r, &body) {
		return
	}
	p := who(r)
	staff := p.Admin && t.User != p.Name
	t, err := s.Support.Reply(t.ID, p.Name, staff, body.Text)
	if err != nil {
		s.cloudErr(w, r, err)
		return
	}
	if staff {
		s.notify(t.User, mail.TicketReply(s.brand(), t.User, t.Subject, s.brand().Name+" support", body.Text), "ticket")
	} else if s.Mail != nil && s.Cloud != nil {
		if to := s.Cloud.Settings().SupportEmail; to != "" {
			m := mail.TicketNew(s.brand(), t.User, t.Subject, body.Text)
			s.Mail.Queue(to, m.Subject, m.Body, "ticket")
		}
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) closeTicket(w http.ResponseWriter, r *http.Request) {
	if !s.supportOn(w) {
		return
	}
	t, ok := s.ticketFor(w, r)
	if !ok {
		return
	}
	t, err := s.Support.Close(t.ID)
	if err != nil {
		s.cloudErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}
