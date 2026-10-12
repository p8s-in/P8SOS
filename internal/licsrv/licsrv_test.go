package licsrv

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"p8sos/internal/billing"
	"p8sos/internal/license"
	"p8sos/internal/mail"
)

const (
	s1 = "P8S-AAAA-0000-0000-0001"
	s2 = "P8S-AAAA-0000-0000-0002"
)

// fakeRZP is enough of Razorpay's API: orders and payments.
type fakeRZP struct {
	mu       sync.Mutex
	orders   map[string]int64
	payments map[string]billing.Payment
}

func (f *fakeRZP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == "POST" && r.URL.Path == "/orders":
		var in struct{ Amount int64 }
		_ = json.NewDecoder(r.Body).Decode(&in)
		id := "order_" + string(rune('A'+len(f.orders)))
		f.orders[id] = in.Amount
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/payments/"):
		_ = json.NewEncoder(w).Encode(f.payments[strings.TrimPrefix(r.URL.Path, "/payments/")])
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/capture"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/payments/"), "/capture")
		p := f.payments[id]
		p.Status = "captured"
		f.payments[id] = p
		_ = json.NewEncoder(w).Encode(p)
	default:
		http.NotFound(w, r)
	}
}

func sig(secret, msg string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

func post(t *testing.T, url, body string, want int) map[string]any {
	t.Helper()
	r, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != want {
		t.Fatalf("POST %s: %d %s (want %d)", url, r.StatusCode, b, want)
	}
	var v map[string]any
	_ = json.Unmarshal(b, &v)
	return v
}

func TestShop(t *testing.T) {
	dir := t.TempDir()
	pub, err := GenerateKey(filepath.Join(dir, "signing.key"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := GenerateKey(filepath.Join(dir, "signing.key")); err == nil {
		t.Fatal("keygen overwrote a key")
	}
	key, err := LoadKey(filepath.Join(dir, "signing.key"))
	if err != nil || PublicKey(key) != pub {
		t.Fatal("key round trip", err)
	}
	trusted := license.ParsePublicKeys("# x\n" + pub + "\n")

	now := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	st := &Store{Dir: filepath.Join(dir, "data"), Key: key, Now: func() time.Time { return now }}
	cfg, _ := LoadConfig(filepath.Join(dir, "none.json"))
	cfg.RazorpayKeyID, cfg.RazorpayKeySecret, cfg.RazorpayWebhookSecret = "rzp_test", "sekret", "whsec"
	cfg.Seller.Name, cfg.Support = "P8S", "help@p8s.example"
	fr := &fakeRZP{orders: map[string]int64{}, payments: map[string]billing.Payment{}}
	rz := httptest.NewServer(fr)
	defer rz.Close()
	var mu sync.Mutex
	sent := map[string]string{}
	m := &mail.Mailer{Dir: dir, Config: func() mail.Config { return mail.Config{Host: "x", Port: 25, From: "a@b.c"} },
		Send: func(_ mail.Config, to string, msg []byte) error {
			mu.Lock()
			sent[to] = string(msg)
			mu.Unlock()
			return nil
		}}
	ts := httptest.NewServer((&Server{Cfg: cfg, Store: st, Mail: m, Razorpay: &billing.Razorpay{KeyID: "rzp_test", KeySecret: "sekret", Base: rz.URL}}).Handler())
	defer ts.Close()

	// shop page and prices
	r, _ := http.Get(ts.URL + "/")
	if b, _ := io.ReadAll(r.Body); r.StatusCode != 200 || !bytes.Contains(b, []byte("Subscriptions")) || r.Header.Get("Content-Security-Policy") == "" {
		t.Fatal("shop page")
	}
	r, _ = http.Get(ts.URL + "/api/shop")
	var info map[string]any
	_ = json.NewDecoder(r.Body).Decode(&info)
	if len(info["tiers"].([]any)) != 4 || info["payments"] != true {
		t.Fatalf("shop info %v", info)
	}

	// bad orders
	post(t, ts.URL+"/api/order", `{"tier":"hosting","servers":"nope","name":"A","email":"a@x.com"}`, 400)
	post(t, ts.URL+"/api/order", `{"tier":"internal","servers":"`+s1+`","name":"A","email":"a@x.com"}`, 400)
	post(t, ts.URL+"/api/order", `{"tier":"home","servers":"`+s1+`","name":"A","email":"a@x.com"}`, 400)
	post(t, ts.URL+"/api/order", `{"tier":"hosting","servers":"`+s1+`","name":"A","email":"bad"}`, 400)

	// buy Hosting for two servers: 2 x 9999 + 18% GST
	o := post(t, ts.URL+"/api/order", `{"tier":"hosting","servers":"`+s1+`\n`+strings.ToLower(s2)+`","name":"Ravi","company":"Acme","gstin":"37abcde1234f1z5","email":"Ravi@Acme.in"}`, 200)
	if o["amount"].(float64) != 2*999900*118/100 {
		t.Fatalf("amount %v", o["amount"])
	}
	oid := o["order_id"].(string)
	fr.payments["pay_1"] = billing.Payment{ID: "pay_1", OrderID: oid, Amount: int64(o["amount"].(float64)), Currency: "INR", Status: "authorized"}
	post(t, ts.URL+"/api/paid", `{"razorpay_order_id":"`+oid+`","razorpay_payment_id":"pay_1","razorpay_signature":"forged"}`, 400)
	d := post(t, ts.URL+"/api/paid", `{"razorpay_order_id":"`+oid+`","razorpay_payment_id":"pay_1","razorpay_signature":"`+sig("sekret", oid+"|pay_1")+`"}`, 200)
	k := d["key"].(string)
	l, err := license.Parse(trusted, k)
	if err != nil || l.Tier != "hosting" || !l.Covers(s1) || !l.Covers(s2) || l.Expires != "2027-10-11" || l.Email != "ravi@acme.in" {
		t.Fatalf("licence %v %+v", err, l)
	}
	inv := d["invoice"].(map[string]any)
	if inv["number"] != "P8SL-2026-000001" || inv["total"].(float64) != o["amount"].(float64) {
		t.Fatalf("invoice %v", inv)
	}
	// paying twice (browser + webhook) issues once
	again := post(t, ts.URL+"/api/paid", `{"razorpay_order_id":"`+oid+`","razorpay_payment_id":"pay_1","razorpay_signature":"`+sig("sekret", oid+"|pay_1")+`"}`, 200)
	if again["key"] != k {
		t.Fatal("second fulfil changed the key")
	}
	if ls, _ := st.List(); len(ls) != 1 {
		t.Fatal("licences", len(ls))
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		msg := sent["ravi@acme.in"]
		mu.Unlock()
		if strings.Contains(msg, l.ID) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no key email")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// a payment for a different amount is refused
	o2 := post(t, ts.URL+"/api/order", `{"tier":"business","servers":"`+s1+`","name":"Eve","email":"eve@x.com"}`, 200)
	oid2 := o2["order_id"].(string)
	fr.payments["pay_2"] = billing.Payment{ID: "pay_2", OrderID: oid2, Amount: 100, Currency: "INR", Status: "captured"}
	post(t, ts.URL+"/api/paid", `{"razorpay_order_id":"`+oid2+`","razorpay_payment_id":"pay_2","razorpay_signature":"`+sig("sekret", oid2+"|pay_2")+`"}`, 400)

	// p8sd's check
	c := post(t, ts.URL+"/api/v1/check", `{"id":"`+l.ID+`","server":"`+s2+`"}`, 200)
	if c["status"] != "active" || c["key"] != k {
		t.Fatalf("check %v", c)
	}
	if c := post(t, ts.URL+"/api/v1/check", `{"id":"`+l.ID+`","server":"P8S-FFFF-0000-0000-0001"}`, 200); c["status"] != "unknown" {
		t.Fatalf("other server %v", c)
	}

	// renewal by webhook: wrong email refused, then a renewal moving up to Partner
	post(t, ts.URL+"/api/order", `{"tier":"hosting","renew":"`+l.ID+`","name":"X","email":"x@x.com"}`, 404)
	post(t, ts.URL+"/api/order", `{"tier":"business","renew":"`+l.ID+`","name":"Ravi","email":"ravi@acme.in"}`, 400)
	o3 := post(t, ts.URL+"/api/order", `{"tier":"partner","renew":"`+l.ID+`","name":"Ravi","email":"ravi@acme.in"}`, 200)
	if o3["amount"].(float64) != 2499900*118/100 {
		t.Fatalf("partner amount %v", o3["amount"])
	}
	oid3 := o3["order_id"].(string)
	fr.payments["pay_3"] = billing.Payment{ID: "pay_3", OrderID: oid3, Amount: int64(o3["amount"].(float64)), Currency: "INR", Status: "captured"}
	body := `{"event":"payment.captured","payload":{"payment":{"entity":{"id":"pay_3","order_id":"` + oid3 + `"}}}}`
	req, _ := http.NewRequest("POST", ts.URL+"/api/razorpay/webhook", strings.NewReader(body))
	req.Header.Set("X-Razorpay-Signature", sig("whsec", body))
	if r, _ := http.DefaultClient.Do(req); r.StatusCode != 200 {
		t.Fatal("webhook", r.StatusCode)
	}
	rec, _ := st.Get(l.ID)
	if rec.License.Tier != "partner" || rec.License.Expires != "2028-10-11" {
		t.Fatalf("renewed %+v", rec.License)
	}

	// p8sd picks the renewal up through its check
	lm := &license.Manager{Path: filepath.Join(dir, "p8sd", "license.json"), ServerID: s1, Keys: trusted, CheckURL: ts.URL + "/api/v1/check",
		Now: func() time.Time { return now }}
	if _, err := lm.Install(k); err != nil {
		t.Fatal(err)
	}
	if err := lm.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := lm.Status(); s.Tier != "partner" || s.License.Expires != "2028-10-11" {
		t.Fatalf("p8sd after renewal %+v", s)
	}

	// admin: revoke, rebind, extend, issue
	if _, err := st.Revoke(l.ID, "refunded", true); err != nil {
		t.Fatal(err)
	}
	_ = lm.Check(context.Background())
	if s := lm.Status(); s.State != "revoked" || s.Tier != "home" {
		t.Fatalf("revoked %+v", s)
	}
	r2, err := st.Rebind(l.ID, []string{"P8S-BBBB-0000-0000-0009"})
	if err != nil || r2.License.Covers(s1) {
		t.Fatal(err)
	}
	r3, err := st.Issue("internal", []string{s1}, "P8S", "", 120, "own server")
	if err != nil || r3.License.Expires != "2036-10-11" {
		t.Fatal(err, r3.License.Expires)
	}
	r4, _ := st.Extend(r3.License.ID, 12)
	if r4.License.Expires != "2037-10-11" {
		t.Fatal(r4.License.Expires)
	}
	if _, err := license.Parse(trusted, r4.Key); err != nil {
		t.Fatal(err)
	}
}
