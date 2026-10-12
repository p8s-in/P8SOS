// Package mail sends the emails a cloud provider sends: confirm your
// address, welcome, server ready, password reset, payment received, low
// balance, servers stopped, support replies. Messages are queued and sent in
// the background over SMTP (STARTTLS on 587, TLS on 465); every message is
// kept in outbox.jsonl so the admin can see what went out (or why not).
package mail

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config is how to reach the mail server.
type Config struct {
	Host string
	Port int
	User string
	Pass string
	From string // "P8S Cloud <noreply@example.com>"
}

func (c Config) ok() bool { return c.Host != "" && c.From != "" }

// Record is one message in the outbox log.
type Record struct {
	ID      string `json:"id"`
	Time    string `json:"time"`
	To      string `json:"to"`
	Subject string `json:"subject"`
	Kind    string `json:"kind"`
	Status  string `json:"status"` // sent, failed, skipped (no SMTP set up)
	Error   string `json:"error,omitempty"`
}

// Mailer queues and sends.
type Mailer struct {
	Dir    string                             // outbox.jsonl lives here
	Config func() Config                      // read at send time (settings can change)
	Send   func(Config, string, []byte) error // nil = SMTP (tests replace it)

	mu   sync.Mutex
	q    chan job
	once sync.Once
}

type job struct {
	to, subject, body, kind string
}

// Queue sends a plain-text message in the background.
func (m *Mailer) Queue(to, subject, body, kind string) {
	if m == nil || to == "" {
		return
	}
	m.once.Do(func() {
		m.q = make(chan job, 500)
		go m.worker()
	})
	select {
	case m.q <- job{to, subject, body, kind}:
	default:
		m.log(Record{To: to, Subject: subject, Kind: kind, Status: "failed", Error: "queue full"})
	}
}

// SendNow sends synchronously (the admin's "send test email").
func (m *Mailer) SendNow(to, subject, body, kind string) error {
	return m.deliver(job{to, subject, body, kind})
}

func (m *Mailer) worker() {
	for j := range m.q {
		for try := 0; try < 3; try++ {
			if err := m.deliver(j); err == nil || errors.Is(err, errNoSMTP) {
				break
			}
			time.Sleep(time.Duration(try+1) * 20 * time.Second)
		}
	}
}

var errNoSMTP = errors.New("email is not set up (Settings → Email)")

func (m *Mailer) deliver(j job) error {
	c := Config{}
	if m.Config != nil {
		c = m.Config()
	}
	r := Record{To: j.to, Subject: j.subject, Kind: j.kind}
	if !c.ok() {
		r.Status, r.Error = "skipped", errNoSMTP.Error()
		m.log(r)
		return errNoSMTP
	}
	msg, err := Build(c.From, j.to, j.subject, j.body)
	if err == nil {
		send := m.Send
		if send == nil {
			send = smtpSend
		}
		err = send(c, j.to, msg)
	}
	if err != nil {
		r.Status, r.Error = "failed", err.Error()
	} else {
		r.Status = "sent"
	}
	m.log(r)
	return err
}

func (m *Mailer) log(r Record) {
	if m.Dir == "" {
		return
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	r.ID, r.Time = hex.EncodeToString(b), time.Now().UTC().Format(time.RFC3339)
	line, _ := json.Marshal(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	_ = os.MkdirAll(m.Dir, 0o750)
	p := filepath.Join(m.Dir, "outbox.jsonl")
	if st, err := os.Stat(p); err == nil && st.Size() > 5<<20 {
		_ = os.Rename(p, p+".1")
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

// Recent returns the newest outbox records.
func (m *Mailer) Recent(n int) []Record {
	out := []Record{}
	if m == nil || m.Dir == "" {
		return out
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	f, err := os.Open(filepath.Join(m.Dir, "outbox.jsonl"))
	if err != nil {
		return out
	}
	defer f.Close()
	var all []Record
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r Record
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			all = append(all, r)
		}
	}
	for i := len(all) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, all[i])
	}
	return out
}

// Build makes an RFC 5322 message (UTF-8 plain text).
func Build(from, to, subject, body string) ([]byte, error) {
	f, err := mail.ParseAddress(from)
	if err != nil {
		return nil, fmt.Errorf("sender %q: %w", from, err)
	}
	t, err := mail.ParseAddress(to)
	if err != nil {
		return nil, fmt.Errorf("recipient %q: %w", to, err)
	}
	if strings.ContainsAny(subject, "\r\n") {
		return nil, errors.New("subject must be one line")
	}
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	domain := f.Address[strings.LastIndex(f.Address, "@")+1:]
	var sb strings.Builder
	fmt.Fprintf(&sb, "From: %s\r\n", f.String())
	fmt.Fprintf(&sb, "To: %s\r\n", t.String())
	fmt.Fprintf(&sb, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", subject))
	fmt.Fprintf(&sb, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&sb, "Message-ID: <%s@%s>\r\n", hex.EncodeToString(b), domain)
	sb.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n")
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		sb.WriteString(line + "\r\n") // net/smtp's DATA writer does the dot-stuffing
	}
	return []byte(sb.String()), nil
}

// smtpSend delivers over SMTP: implicit TLS on 465, otherwise STARTTLS when
// offered (required when a password is used).
func smtpSend(c Config, to string, msg []byte) error {
	from, _ := mail.ParseAddress(c.From)
	rcpt, err := mail.ParseAddress(to)
	if err != nil {
		return err
	}
	addr := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var conn net.Conn
	d := &net.Dialer{}
	tcfg := &tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12}
	if c.Port == 465 {
		conn, err = (&tls.Dialer{NetDialer: d, Config: tcfg}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
	}
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
	cl, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer cl.Close()
	if c.Port != 465 {
		if ok, _ := cl.Extension("STARTTLS"); ok {
			if err := cl.StartTLS(tcfg); err != nil {
				return err
			}
		} else if c.Pass != "" {
			return errors.New("the mail server doesn't offer STARTTLS; refusing to send the password in clear text")
		}
	}
	if c.User != "" {
		if err := cl.Auth(smtp.PlainAuth("", c.User, c.Pass, c.Host)); err != nil {
			return err
		}
	}
	if err := cl.Mail(from.Address); err != nil {
		return err
	}
	if err := cl.Rcpt(rcpt.Address); err != nil {
		return err
	}
	w, err := cl.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return cl.Quit()
}
