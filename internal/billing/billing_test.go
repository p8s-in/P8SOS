package billing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHourlyChargesAndMonthlyCap(t *testing.T) {
	now := time.Date(2026, 10, 30, 22, 10, 0, 0, time.UTC)
	s := &Store{Dir: t.TempDir(), Now: func() time.Time { return now }}
	if _, err := s.Adjust("alice", 100000, "welcome credit"); err != nil {
		t.Fatal(err)
	}
	// 399.00/month -> 55 paise/hour
	if err := s.Subscribe("web1", "alice", "s1", "Starter", 39900, 55); err != nil {
		t.Fatal(err)
	}
	if b := s.Balance("alice"); b != 100000-55 {
		t.Fatalf("first hour: balance %d", b)
	}
	if err := s.Subscribe("web1", "alice", "s1", "Starter", 39900, 55); err == nil {
		t.Fatal("double subscription")
	}
	now = now.Add(150 * time.Minute) // 00:40 next day: hours starting 23:10 and 00:10 are due
	if _, err := s.Tick(72); err != nil {
		t.Fatal(err)
	}
	if b := s.Balance("alice"); b != 100000-3*55 {
		t.Fatalf("after 3 hours: %d", b)
	}
	now = now.Add(5 * time.Minute)
	s.Tick(72)
	if b := s.Balance("alice"); b != 100000-3*55 {
		t.Fatalf("same hour charged twice: %d", b)
	}
	// a whole month never costs more than the monthly price
	s2 := &Store{Dir: t.TempDir(), Now: func() time.Time { return time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC) }}
	s2.Adjust("bob", 1000000, "")
	s2.Subscribe("db1", "bob", "s1", "Starter", 39900, 55)
	end := time.Date(2026, 12, 31, 22, 30, 0, 0, time.UTC) // 744 h x 0.55 = 409.20 > 399.00
	s2.Now = func() time.Time { return end }
	s2.Tick(72)
	if spent := 1000000 - s2.Balance("bob"); spent != 39900 {
		t.Fatalf("December cost %d, want the monthly price 39900", spent)
	}
	// unsubscribe stops charges; reload from disk keeps balance and subs
	s2.Unsubscribe("db1")
	s2.Now = func() time.Time { return end.Add(48 * time.Hour) }
	s2.Tick(72)
	if spent := 1000000 - s2.Balance("bob"); spent != 39900 {
		t.Fatalf("charged after unsubscribe: %d", spent)
	}
	s3 := &Store{Dir: s.Dir, Now: s.Now}
	if s3.Balance("alice") != s.Balance("alice") || len(s3.Subs("alice")) != 1 {
		t.Fatal("state not persisted")
	}
	if e, _ := s3.Entries("alice", 10); len(e) != 3 || e[0].Kind != "charge" || e[len(e)-1].Kind != "adjust" {
		t.Fatalf("entries %+v", e)
	}
}

func TestLowNegativeSuspendResume(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	s := &Store{Dir: t.TempDir(), Now: func() time.Time { return now }}
	s.Adjust("alice", 1000, "")                           // 10.00
	s.Subscribe("web1", "alice", "s2", "Std", 73000, 100) // 1.00/h
	ev, _ := s.Tick(24)
	if len(ev) != 1 || ev[0].Kind != "low" || ev[0].Hours != 9 {
		t.Fatalf("low-balance warning: %+v", ev)
	}
	if ev, _ := s.Tick(24); len(ev) != 0 {
		t.Fatalf("warned twice in a day: %+v", ev)
	}
	now = now.Add(10 * time.Hour)
	ev, _ = s.Tick(24)
	if len(ev) != 1 || ev[0].Kind != "negative" {
		t.Fatalf("negative: %+v (balance %d)", ev, s.Balance("alice"))
	}
	now = now.Add(23 * time.Hour)
	if ev, _ := s.Tick(24); len(ev) != 0 || s.Suspended("alice") {
		t.Fatalf("suspended inside the grace period: %+v", ev)
	}
	now = now.Add(2 * time.Hour)
	ev, _ = s.Tick(24)
	if len(ev) != 1 || ev[0].Kind != "suspend" || !s.Suspended("alice") {
		t.Fatalf("suspend: %+v", ev)
	}
	// paying the debt resumes
	s.Adjust("alice", 10000, "bank transfer")
	ev, _ = s.Tick(24)
	if len(ev) == 0 || ev[0].Kind != "resume" || s.Suspended("alice") {
		t.Fatalf("resume: %+v", ev)
	}
}

func TestOrdersInvoicesAndRazorpay(t *testing.T) {
	now := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	s := &Store{Dir: t.TempDir(), Now: func() time.Time { return now }}
	o, err := s.NewOrder("order_1", "alice", 50000, 18)
	if err != nil || o.Tax != 9000 || o.Total != 59000 {
		t.Fatalf("order %+v %v", o, err)
	}
	seller := Party{Name: "P8S", TaxID: "37ABCDE1234F1Z5"}
	inv, fresh, err := s.Paid("order_1", "pay_1", "razorpay", seller, Party{Name: "Alice"}, "INR", "GST", 18, "P8S")
	if err != nil || !fresh || inv.Number != "P8S-2026-000001" || inv.Total != 59000 || inv.Subtotal != 50000 {
		t.Fatalf("invoice %+v %v", inv, err)
	}
	if s.Balance("alice") != 50000 {
		t.Fatalf("credited %d", s.Balance("alice"))
	}
	inv2, fresh, err := s.Paid("order_1", "pay_1", "razorpay", seller, Party{}, "INR", "GST", 18, "P8S")
	if err != nil || fresh || inv2.Number != inv.Number || s.Balance("alice") != 50000 {
		t.Fatalf("paid twice: %+v %v balance %d", inv2, err, s.Balance("alice"))
	}
	m, err := s.ManualTopup("bob", 10000, "UPI 1234", seller, Party{Name: "Bob"}, "INR", "GST", 18, "P8S")
	if err != nil || m.Number != "P8S-2026-000002" || m.Tax != 1800 {
		t.Fatalf("manual %+v %v", m, err)
	}
	if l, _ := s.Invoices("alice"); len(l) != 1 {
		t.Fatal("alice's invoices")
	}
	if _, err := s.InvoiceByNumber("P8S-2026-000002"); err != nil {
		t.Fatal(err)
	}

	// signatures (Razorpay's documented scheme)
	sig := mac("secret", "order_1|pay_1")
	if !CheckoutSignature("order_1", "pay_1", sig, "secret") || CheckoutSignature("order_1", "pay_2", sig, "secret") || CheckoutSignature("order_1", "pay_1", sig, "") {
		t.Fatal("checkout signature")
	}
	body := []byte(`{"event":"payment.captured"}`)
	if !WebhookSignature(body, mac("wh", string(body)), "wh") || WebhookSignature(body, mac("wh", "x"), "wh") {
		t.Fatal("webhook signature")
	}

	// client against a fake API
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, _ := r.BasicAuth()
		if u != "rzp_test_k" || p != "sec" {
			w.WriteHeader(401)
			w.Write([]byte(`{"error":{"description":"Authentication failed"}}`))
			return
		}
		switch {
		case r.Method == "POST" && r.URL.Path == "/orders":
			var b map[string]any
			json.NewDecoder(r.Body).Decode(&b)
			if b["amount"] != 59000.0 || b["currency"] != "INR" {
				w.WriteHeader(400)
				return
			}
			w.Write([]byte(`{"id":"order_X","status":"created"}`))
		case r.URL.Path == "/payments/pay_X":
			w.Write([]byte(`{"id":"pay_X","order_id":"order_X","amount":59000,"currency":"INR","status":"authorized"}`))
		case r.URL.Path == "/payments/pay_X/capture":
			w.Write([]byte(`{"id":"pay_X","order_id":"order_X","amount":59000,"currency":"INR","status":"captured"}`))
		}
	}))
	defer srv.Close()
	rz := &Razorpay{KeyID: "rzp_test_k", KeySecret: "sec", Base: srv.URL}
	id, err := rz.CreateOrder(context.Background(), 59000, "INR", "r1", nil)
	if err != nil || id != "order_X" {
		t.Fatalf("order %q %v", id, err)
	}
	p, _ := rz.FetchPayment(context.Background(), "pay_X")
	if p.Status != "authorized" {
		t.Fatalf("payment %+v", p)
	}
	if p, _ = rz.Capture(context.Background(), "pay_X", 59000, "INR"); p.Status != "captured" {
		t.Fatalf("capture %+v", p)
	}
	bad := &Razorpay{KeyID: "rzp_test_k", KeySecret: "wrong", Base: srv.URL}
	if _, err := bad.CreateOrder(context.Background(), 1, "INR", "", nil); err == nil || !strings.Contains(err.Error(), "Authentication failed") {
		t.Fatalf("bad key: %v", err)
	}
}
