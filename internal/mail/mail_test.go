package mail

import (
	"errors"
	"strings"
	"testing"
)

func TestBuildAndQueue(t *testing.T) {
	msg, err := Build("P8S Cloud <noreply@p8s.in>", "alice@example.com", "Your server web1 is ready ✓", "line1\n.dot line")
	if err != nil {
		t.Fatal(err)
	}
	s := string(msg)
	for _, want := range []string{"From: \"P8S Cloud\" <noreply@p8s.in>\r\n", "To: <alice@example.com>\r\n", "Subject: =?utf-8?q?", "Message-ID: <", "@p8s.in>", "charset=utf-8", "\r\n\r\nline1\r\n.dot line\r\n"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
	if _, err := Build("x", "a@b.c", "s", ""); err == nil {
		t.Error("bad sender accepted")
	}
	if _, err := Build("a@b.c", "a@b.c", "two\r\nBcc: x@y.z", ""); err == nil {
		t.Error("header injection accepted")
	}

	dir := t.TempDir()
	var sent []string
	cfg := Config{}
	m := &Mailer{Dir: dir, Config: func() Config { return cfg }, Send: func(c Config, to string, msg []byte) error {
		if to == "fail@example.com" {
			return errors.New("550 mailbox unavailable")
		}
		sent = append(sent, to)
		return nil
	}}
	if err := m.SendNow("alice@example.com", "hi", "x", "test"); err == nil {
		t.Fatal("sent without SMTP settings")
	}
	cfg = Config{Host: "smtp.example.com", Port: 587, From: "noreply@example.com"}
	if err := m.SendNow("alice@example.com", "hi", "x", "test"); err != nil || len(sent) != 1 {
		t.Fatalf("send: %v %v", err, sent)
	}
	if err := m.SendNow("fail@example.com", "hi", "x", "test"); err == nil {
		t.Fatal("failure not reported")
	}
	r := m.Recent(10)
	if len(r) != 3 || r[0].Status != "failed" || r[1].Status != "sent" || r[2].Status != "skipped" {
		t.Fatalf("outbox %+v", r)
	}
}

func TestMessages(t *testing.T) {
	b := Brand{Name: "P8S Cloud", URL: "https://panel.p8s.in", Support: "help@p8s.in"}
	if m := Verify(b, "alice", "abc"); !strings.Contains(m.Body, "https://panel.p8s.in/#verify=abc") {
		t.Fatal(m.Body)
	}
	if m := ServerReady(b, "alice", "web1", "203.0.113.10", "root", "Starter"); !strings.Contains(m.Body, "ssh root@203.0.113.10") {
		t.Fatal(m.Body)
	}
	if got := Money(59000, "INR"); got != "₹590.00" {
		t.Fatal(got)
	}
	if got := Money(-1505, "USD"); got != "-$15.05" {
		t.Fatal(got)
	}
	if m := Suspended(b, "alice", -500, "INR", []string{"web1", "db1"}); !strings.Contains(m.Body, "web1, db1") || !strings.Contains(m.Body, "-₹5.00") {
		t.Fatal(m.Body)
	}
}
