package cloud

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSettingsAndPlans(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "cloud.json")}
	if st := s.Settings(); st.Brand != "P8S Cloud" || st.Currency != "INR" || st.TaxPercent != 18 {
		t.Fatalf("defaults %+v", st)
	}
	st := s.Settings()
	st.RazorpayKeyID, st.RazorpayKeySecret, st.SMTPPass = "rzp_test_abc", "secret1", "smtp-pw"
	st.PublicURL = "https://panel.example.com/"
	if _, err := s.SetSettings(st); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(s.Path); fi.Mode().Perm() != 0o600 {
		t.Fatal("cloud.json must be 0600")
	}
	r := s.Settings().Redacted()
	if r.RazorpayKeySecret != Kept || r.SMTPPass != Kept || r.RazorpayWebhookSecret != "" {
		t.Fatalf("redacted %+v", r)
	}
	r.Brand = "Acme Cloud"
	if _, err := s.SetSettings(r); err != nil { // secrets sent back as ******** stay
		t.Fatal(err)
	}
	s2 := &Store{Path: s.Path}
	if g := s2.Settings(); g.RazorpayKeySecret != "secret1" || g.SMTPPass != "smtp-pw" || g.Brand != "Acme Cloud" || g.PublicURL != "https://panel.example.com" {
		t.Fatalf("reloaded %+v", g)
	}
	for _, bad := range []func(*Settings){
		func(x *Settings) { x.Currency = "rupees" }, func(x *Settings) { x.TaxPercent = 99 },
		func(x *Settings) { x.Brand = "<script>" }, func(x *Settings) { x.RazorpayKeyID = "abc" },
		func(x *Settings) { x.PublicURL = "javascript:alert(1)" }, func(x *Settings) { x.MailFrom = "nope" },
	} {
		x := s2.Settings()
		bad(&x)
		if _, err := s2.SetSettings(x); !errors.Is(err, ErrInvalid) {
			t.Errorf("accepted bad settings %+v", x)
		}
	}

	if _, err := s2.PutPlan(Plan{ID: "S 1", Name: "x", VCPUs: 1, MemoryMiB: 1024, DiskGB: 25}); !errors.Is(err, ErrInvalid) {
		t.Fatal("bad plan id accepted")
	}
	if _, err := s2.PutPlan(Plan{ID: "s2", Name: "Standard", VCPUs: 2, MemoryMiB: 2048, DiskGB: 50, PriceMonthly: 79900, PublicIP: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.PutPlan(Plan{ID: "s1", Name: "Starter", VCPUs: 1, MemoryMiB: 1024, DiskGB: 25, PriceMonthly: 39900}); err != nil {
		t.Fatal(err)
	}
	if l := s2.Plans(); len(l) != 2 || l[0].ID != "s1" {
		t.Fatalf("plans %+v", l)
	}
	p, _ := s2.Plan("s1")
	if p.Hourly() != 55 { // 39900/730 = 54.66 -> 55 paise
		t.Fatalf("hourly %d", p.Hourly())
	}
	if err := s2.DeletePlan("s1"); err != nil || len(s2.Plans()) != 1 {
		t.Fatal("delete plan")
	}
}
