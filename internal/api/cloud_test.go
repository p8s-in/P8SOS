package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"p8sos/internal/billing"
	"p8sos/internal/cloud"
	"p8sos/internal/ct"
	"p8sos/internal/mail"
	"p8sos/internal/support"
	"p8sos/internal/testfake"
	"p8sos/internal/vm"
)

func (o *ownedVMs) Stop(ctx context.Context, n string, force bool) (vm.VM, error) {
	v, err := o.Get(ctx, n)
	if err == nil {
		o.mu.Lock()
		v.State = "stopped"
		o.vms[n] = v
		o.mu.Unlock()
	}
	return v, err
}

type inbox struct {
	mu   sync.Mutex
	msgs map[string][]string // to -> bodies
}

func (b *inbox) last(to string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	l := b.msgs[to]
	if len(l) == 0 {
		return ""
	}
	return l[len(l)-1]
}

func waitMail(t *testing.T, b *inbox, to, contains string) string {
	t.Helper()
	for i := 0; i < 200; i++ {
		if m := b.last(to); strings.Contains(m, contains) {
			return m
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no mail to %s containing %q (last: %q)", to, contains, b.last(to))
	return ""
}

func hmacHex(secret, msg string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

func TestCloud(t *testing.T) {
	box := &inbox{msgs: map[string][]string{}}
	// fake Razorpay: orders, payments (authorized until captured)
	var rmu sync.Mutex
	orders := map[string]int64{}
	captured := map[string]bool{}
	rz := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rmu.Lock()
		defer rmu.Unlock()
		switch {
		case r.Method == "POST" && r.URL.Path == "/orders":
			var b struct {
				Amount int64 `json:"amount"`
			}
			json.NewDecoder(r.Body).Decode(&b)
			id := fmt.Sprintf("order_%d", len(orders)+1)
			orders[id] = b.Amount
			fmt.Fprintf(w, `{"id":%q}`, id)
		case strings.HasSuffix(r.URL.Path, "/capture"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/payments/"), "/capture")
			captured[id] = true
			fmt.Fprintf(w, `{"id":%q,"order_id":"order_1","amount":%d,"currency":"INR","status":"captured"}`, id, orders["order_1"])
		case strings.HasPrefix(r.URL.Path, "/payments/"):
			id := strings.TrimPrefix(r.URL.Path, "/payments/")
			st := "authorized"
			if captured[id] {
				st = "captured"
			}
			fmt.Fprintf(w, `{"id":%q,"order_id":"order_1","amount":%d,"currency":"INR","status":%q}`, id, orders["order_1"], st)
		}
	}))
	defer rz.Close()

	var cs *cloud.Store
	var bs *billing.Store
	now := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	m := newMultiUserCfg(t, func(s *Server) {
		dir := t.TempDir()
		cs = &cloud.Store{Path: filepath.Join(dir, "cloud.json")}
		bs = &billing.Store{Dir: dir, Now: func() time.Time { return now }}
		s.Cloud, s.Billing, s.Support = cs, bs, &support.Store{}
		s.Mail = &mail.Mailer{Dir: dir, Config: func() mail.Config {
			st := cs.Settings()
			return mail.Config{Host: st.SMTPHost, Port: st.SMTPPort, From: st.MailFrom}
		}, Send: func(c mail.Config, to string, msg []byte) error {
			box.mu.Lock()
			box.msgs[to] = append(box.msgs[to], string(msg))
			box.mu.Unlock()
			return nil
		}}
		s.Razorpay = func(st cloud.Settings) *billing.Razorpay {
			return &billing.Razorpay{KeyID: st.RazorpayKeyID, KeySecret: st.RazorpayKeySecret, Base: rz.URL}
		}
		run := &testfake.Runner{Out: map[string]string{
			"zfs list -H -t snapshot -o name,p8s:label": "tank/p8s/ct-templates/debian-13@base\tDebian 13\n",
			"lxc-info -n": "RUNNING\n",
		}}
		_ = os.MkdirAll(filepath.Join(dir, "ct", "ct1"), 0o755)
		s.CTs = &ct.Service{Run: run, Pool: "tank", MountDir: filepath.Join(dir, "ct"), LXCDir: filepath.Join(dir, "lxc"),
			MetaFile: filepath.Join(dir, "containers.json"), Bridge: "virbr0", Chown: func(string, int, int) error { return nil }}
	})

	// sign-up is closed until an admin exists and opens it
	m.req("POST", "/api/v1/signup", "", `{"user":"carol","email":"carol@example.com","password":"carol-password"}`, 403)
	m.req("POST", "/api/v1/users", secret, `{"name":"boss","role":"admin","password":"boss-password-1","email":"boss@example.com"}`, 201)
	boss := m.login("boss", "boss-password-1", "")
	st := m.req("GET", "/api/v1/cloud/settings", boss, "", 200)
	st["signup_open"] = true
	st["smtp_host"], st["mail_from"], st["public_url"] = "smtp.example.com", "P8S Cloud <noreply@example.com>", "https://panel.example.com"
	st["razorpay_key_id"], st["razorpay_key_secret"], st["razorpay_webhook_secret"] = "rzp_test_k", "keysecret", "whsecret"
	st["support_email"], st["seller_name"], st["seller_tax_id"] = "help@example.com", "P8S Technologies", "37AAAAA0000A1Z5"
	b, _ := json.Marshal(st)
	if got := m.req("PUT", "/api/v1/cloud/settings", boss, string(b), 200); got["razorpay_key_secret"] != cloud.Kept {
		t.Fatalf("secret not redacted: %v", got)
	}
	m.req("PUT", "/api/v1/cloud/settings", "", string(b), 401)
	if a := m.req("GET", "/api/v1/auth", "", "", 200); a["brand"] != "P8S Cloud" || a["signup"] != true {
		t.Fatalf("auth info %v", a)
	}

	// plans
	m.req("PUT", "/api/v1/plans/s1", boss, `{"name":"Starter","vcpus":1,"memory_mib":1024,"disk_gb":25,"price_monthly":43800}`, 200)
	if p := m.req("GET", "/api/v1/plans", boss, "", 200)["plans"].([]any)[0].(map[string]any); p["hourly"] != 60.0 {
		t.Fatalf("plan %v", p)
	}

	// sign-up with email confirmation
	v := m.req("POST", "/api/v1/signup", "", `{"user":"carol","email":"Carol@Example.com","password":"carol-password","full_name":"Carol D"}`, 202)
	if v["verify"] != true {
		t.Fatalf("signup %v", v)
	}
	m.req("POST", "/api/v1/signup", "", `{"user":"carol2","email":"carol@example.com","password":"carol-password"}`, 409)
	if e := m.req("POST", "/api/v1/login", "", `{"user":"carol","password":"carol-password"}`, 403); e["error"].(map[string]any)["code"] != "unverified" {
		t.Fatalf("unverified login %v", e)
	}
	msg := waitMail(t, box, "carol@example.com", "#verify=")
	code := regexp.MustCompile(`#verify=([0-9a-f]{48})`).FindStringSubmatch(msg)[1]
	m.req("POST", "/api/v1/verify", "", `{"code":"0000"}`, 400)
	carol := m.req("POST", "/api/v1/verify", "", fmt.Sprintf(`{"code":%q}`, code), 200)["token"].(string)
	m.req("POST", "/api/v1/verify", "", fmt.Sprintf(`{"code":%q}`, code), 400) // used
	waitMail(t, box, "carol@example.com", "Welcome")
	m.login("carol", "carol-password", "")

	// customers must pick a plan and need credit
	m.req("POST", "/api/v1/vms", carol, `{"name":"c1","template":"debian-13","vcpus":1}`, 400)
	if e := m.req("POST", "/api/v1/vms", carol, `{"name":"c1","template":"debian-13","plan":"s1"}`, 402); !strings.Contains(e["error"].(map[string]any)["message"].(string), "₹14.40") {
		t.Fatalf("needs a day of credit: %v", e)
	}

	// top up 500 + 18% GST through Razorpay
	o := m.req("POST", "/api/v1/billing/topup", carol, `{"amount":50000}`, 201)
	if o["amount"] != 59000.0 || o["key_id"] != "rzp_test_k" || o["tax"] != 9000.0 {
		t.Fatalf("order %v", o)
	}
	m.req("POST", "/api/v1/billing/topup", carol, `{"amount":100}`, 400) // below minimum
	oid := o["order_id"].(string)
	bad := fmt.Sprintf(`{"razorpay_order_id":%q,"razorpay_payment_id":"pay_1","razorpay_signature":"00"}`, oid)
	m.req("POST", "/api/v1/billing/topup/verify", carol, bad, 400)
	good := fmt.Sprintf(`{"razorpay_order_id":%q,"razorpay_payment_id":"pay_1","razorpay_signature":%q}`, oid, hmacHex("keysecret", oid+"|pay_1"))
	r := m.req("POST", "/api/v1/billing/topup/verify", carol, good, 200)
	inv := r["invoice"].(map[string]any)
	if r["balance"] != 50000.0 || inv["number"] != "P8S-2026-000001" || inv["total"] != 59000.0 || inv["buyer"].(map[string]any)["name"] != "Carol D" {
		t.Fatalf("paid %v", r)
	}
	m.req("POST", "/api/v1/billing/topup/verify", carol, good, 200) // again: no double credit
	if b := m.req("GET", "/api/v1/billing", carol, "", 200); b["balance"] != 50000.0 {
		t.Fatalf("balance after repeat %v", b)
	}
	waitMail(t, box, "carol@example.com", "P8S-2026-000001")
	// the webhook for the same payment changes nothing
	hook := fmt.Sprintf(`{"event":"payment.captured","payload":{"payment":{"entity":{"id":"pay_1","order_id":%q}}}}`, oid)
	req, _ := http.NewRequest("POST", m.srv.URL+"/api/v1/billing/razorpay/webhook", strings.NewReader(hook))
	req.Header.Set("X-Razorpay-Signature", hmacHex("whsecret", hook))
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 200 {
		t.Fatalf("webhook %v %v", resp, err)
	}
	req, _ = http.NewRequest("POST", m.srv.URL+"/api/v1/billing/razorpay/webhook", strings.NewReader(hook))
	req.Header.Set("X-Razorpay-Signature", "forged")
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 401 {
		t.Fatal("forged webhook accepted")
	}
	if b := m.req("GET", "/api/v1/billing", carol, "", 200); b["balance"] != 50000.0 {
		t.Fatalf("webhook double credit %v", b)
	}
	if l := m.req("GET", "/api/v1/billing/invoices", carol, "", 200)["invoices"].([]any); len(l) != 1 {
		t.Fatal("invoices")
	}
	m.req("GET", "/api/v1/billing/invoices/P8S-2026-000001", carol, "", 200)

	// server on a plan: billed from the first hour, owner emailed
	srv := m.req("POST", "/api/v1/vms", carol, `{"name":"c1","template":"debian-13","plan":"s1","vcpus":8}`, 201)
	if srv["vcpus"] != 1.0 || srv["owner"] != "carol" {
		t.Fatalf("plan size not applied: %v", srv)
	}
	waitMail(t, box, "carol@example.com", "c1 is ready")
	bil := m.req("GET", "/api/v1/billing", carol, "", 200)
	if bil["balance"] != 50000.0-60 || bil["hourly"] != 60.0 || len(bil["servers"].([]any)) != 1 {
		t.Fatalf("billing after create %v", bil)
	}
	m.req("PATCH", "/api/v1/vms/c1", carol, `{"memory_mib":2048}`, 400) // plan servers: no free resize

	// containers use the same plans and billing
	m.req("POST", "/api/v1/containers", carol, `{"name":"ct1","template":"debian-13","vcpus":1}`, 400) // must pick a plan
	cn := m.req("POST", "/api/v1/containers", carol, `{"name":"ct1","template":"debian-13","plan":"s1","vcpus":8}`, 201)
	if cn["vcpus"] != 1.0 || cn["owner"] != "carol" {
		t.Fatalf("plan size not applied to container: %v", cn)
	}
	if b := m.req("GET", "/api/v1/billing", carol, "", 200); len(b["servers"].([]any)) != 2 {
		t.Fatalf("container not billed: %v", b)
	}
	m.req("PATCH", "/api/v1/containers/ct1", carol, `{"memory_mib":2048}`, 400)
	m.req("DELETE", "/api/v1/containers/ct1", carol, "", 204)
	if b := m.req("GET", "/api/v1/billing", carol, "", 200); len(b["servers"].([]any)) != 1 {
		t.Fatalf("deleted container still billed: %v", b)
	}

	// admin: customers overview, manual payment with invoice
	cu := m.req("GET", "/api/v1/billing/customers", boss, "", 200)["customers"].([]any)
	if len(cu) != 1 || cu[0].(map[string]any)["servers"] != 1.0 {
		t.Fatalf("customers %v", cu)
	}
	m.req("GET", "/api/v1/billing/customers", carol, "", 403)
	m.req("POST", "/api/v1/billing/adjust", carol, `{"user":"carol","amount":1000000,"note":"x"}`, 403)

	// run the balance into the red: suspended after the grace period, servers stopped
	m.req("POST", "/api/v1/billing/adjust", boss, `{"user":"carol","amount":-49980,"note":"test debit"}`, 201)
	now = now.Add(2 * time.Hour)
	if err := m.server.BillingTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitMail(t, box, "carol@example.com", "balance is negative")
	now = now.Add(73 * time.Hour)
	m.server.BillingTick(context.Background())
	waitMail(t, box, "carol@example.com", "were stopped")
	if v := m.req("GET", "/api/v1/vms/c1", carol, "", 200); v["state"] != "stopped" {
		t.Fatalf("server not stopped: %v", v)
	}
	m.req("POST", "/api/v1/vms/c1/start", carol, "", 403)
	if b := m.req("GET", "/api/v1/billing", carol, "", 200); b["suspended"] != true {
		t.Fatal("not suspended")
	}
	// paying (bank transfer recorded by the admin) lifts it
	m.req("POST", "/api/v1/billing/adjust", boss, `{"user":"carol","amount":100000,"note":"NEFT 123","invoice":true}`, 201)
	m.server.BillingTick(context.Background())
	waitMail(t, box, "carol@example.com", "positive again")
	m.req("POST", "/api/v1/vms/c1/start", carol, "", 200)

	// support tickets
	tk := m.req("POST", "/api/v1/tickets", carol, `{"subject":"Can't connect","vm":"c1","text":"ssh times out"}`, 201)
	id := tk["id"].(string)
	waitMail(t, box, "help@example.com", "Can't connect")
	m.req("GET", "/api/v1/tickets/"+id, m.login("boss", "boss-password-1", ""), "", 200)
	m.req("POST", "/api/v1/tickets/"+id+"/reply", boss, `{"text":"Open port 22 in the firewall"}`, 200)
	waitMail(t, box, "carol@example.com", "Open port 22")
	if l := m.req("GET", "/api/v1/tickets", carol, "", 200)["tickets"].([]any); len(l) != 1 || l[0].(map[string]any)["status"] != "answered" {
		t.Fatalf("tickets %v", l)
	}
	m.req("POST", "/api/v1/users", secret, `{"name":"dave","password":"dave-password1"}`, 201)
	dave := m.login("dave", "dave-password1", "")
	m.req("GET", "/api/v1/tickets/"+id, dave, "", 404)

	// personal API tokens
	tok := m.req("POST", "/api/v1/me/tokens", carol, `{"name":"ci"}`, 201)
	key := tok["secret"].(string)
	if l := m.req("GET", "/api/v1/vms", key, "", 200)["vms"].([]any); len(l) != 1 {
		t.Fatalf("token sees %v", l)
	}
	m.req("POST", "/api/v1/me/password", key, `{"old_password":"carol-password","new_password":"x-new-password"}`, 400) // tokens can't change the account
	m.req("DELETE", "/api/v1/me/tokens/"+tok["token"].(map[string]any)["id"].(string), carol, "", 204)
	m.req("GET", "/api/v1/vms", key, "", 401)

	// password reset by email
	m.req("POST", "/api/v1/password/forgot", "", `{"email":"carol@example.com"}`, 204)
	m.req("POST", "/api/v1/password/forgot", "", `{"email":"nobody@example.com"}`, 204) // same answer
	msg = waitMail(t, box, "carol@example.com", "#reset=")
	rc := regexp.MustCompile(`#reset=([0-9a-f]{48})`).FindStringSubmatch(msg)[1]
	m.req("POST", "/api/v1/password/reset", "", fmt.Sprintf(`{"code":%q,"password":"carol-new-pass"}`, rc), 204)
	m.req("GET", "/api/v1/vms", carol, "", 401) // signed out everywhere
	m.login("carol", "carol-new-pass", "")

	// deleting the server stops billing
	carol = m.login("carol", "carol-new-pass", "")
	m.req("DELETE", "/api/v1/vms/c1", carol, "", 204)
	if b := m.req("GET", "/api/v1/billing", carol, "", 200); len(b["servers"].([]any)) != 0 {
		t.Fatal("still billed after delete")
	}
}
