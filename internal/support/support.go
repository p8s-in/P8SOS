// Package support keeps customer support requests (tickets): a subject and a
// conversation between the customer and the provider's admins.
package support

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Message struct {
	From  string `json:"from"`  // user name
	Staff bool   `json:"staff"` // written by an admin
	Time  string `json:"time"`
	Text  string `json:"text"`
}

type Ticket struct {
	ID       string    `json:"id"`
	User     string    `json:"user"`
	Subject  string    `json:"subject"`
	VM       string    `json:"vm,omitempty"`
	Status   string    `json:"status"` // open (waiting for staff), answered (waiting for customer), closed
	Created  string    `json:"created"`
	Updated  string    `json:"updated"`
	Messages []Message `json:"messages"`
}

var (
	ErrInvalid  = errors.New("invalid")
	ErrNotFound = errors.New("not found")
)

type Store struct {
	Path string
	Now  func() time.Time

	mu sync.Mutex
	m  map[string]*Ticket
}

func (s *Store) now() string {
	t := time.Now()
	if s.Now != nil {
		t = s.Now()
	}
	return t.UTC().Format(time.RFC3339)
}

func (s *Store) load() error {
	if s.m != nil {
		return nil
	}
	s.m = map[string]*Ticket{}
	if s.Path == "" {
		return nil
	}
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var l []*Ticket
	if err := json.Unmarshal(b, &l); err != nil {
		return err
	}
	for _, t := range l {
		s.m[t.ID] = t
	}
	return nil
}

func (s *Store) save() error {
	if s.Path == "" {
		return nil
	}
	l := make([]*Ticket, 0, len(s.m))
	for _, t := range s.m {
		l = append(l, t)
	}
	sort.Slice(l, func(i, j int) bool { return l[i].Created < l[j].Created })
	b, err := json.MarshalIndent(l, "", "  ")
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
	return os.Rename(tmp, s.Path)
}

func checkText(subject, text string) error {
	if subject != "" && (len(subject) > 120 || strings.ContainsAny(subject, "\r\n")) {
		return fmt.Errorf("%w: subject: one line, up to 120 characters", ErrInvalid)
	}
	if strings.TrimSpace(text) == "" || len(text) > 10000 {
		return fmt.Errorf("%w: message: 1-10000 characters", ErrInvalid)
	}
	return nil
}

// Open starts a ticket.
func (s *Store) Open(user, subject, vm, text string) (Ticket, error) {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return Ticket{}, fmt.Errorf("%w: subject is empty", ErrInvalid)
	}
	if err := checkText(subject, text); err != nil {
		return Ticket{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return Ticket{}, err
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	now := s.now()
	t := &Ticket{ID: "T" + strings.ToUpper(hex.EncodeToString(b)), User: user, Subject: subject, VM: vm, Status: "open",
		Created: now, Updated: now, Messages: []Message{{From: user, Time: now, Text: strings.TrimSpace(text)}}}
	s.m[t.ID] = t
	return *t, s.save()
}

// Reply adds a message; staff replies mark it answered, customer replies reopen it.
func (s *Store) Reply(id, from string, staff bool, text string) (Ticket, error) {
	if err := checkText("", text); err != nil {
		return Ticket{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return Ticket{}, err
	}
	t, ok := s.m[id]
	if !ok {
		return Ticket{}, fmt.Errorf("%w: ticket %s", ErrNotFound, id)
	}
	now := s.now()
	t.Messages = append(t.Messages, Message{From: from, Staff: staff, Time: now, Text: strings.TrimSpace(text)})
	t.Updated = now
	if staff {
		t.Status = "answered"
	} else {
		t.Status = "open"
	}
	return *t, s.save()
}

// Close closes a ticket (a later reply reopens it).
func (s *Store) Close(id string) (Ticket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return Ticket{}, err
	}
	t, ok := s.m[id]
	if !ok {
		return Ticket{}, fmt.Errorf("%w: ticket %s", ErrNotFound, id)
	}
	t.Status, t.Updated = "closed", s.now()
	return *t, s.save()
}

// Get returns one ticket.
func (s *Store) Get(id string) (Ticket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return Ticket{}, err
	}
	t, ok := s.m[id]
	if !ok {
		return Ticket{}, fmt.Errorf("%w: ticket %s", ErrNotFound, id)
	}
	return *t, nil
}

// List returns tickets of a user ("" = all), most recently updated first,
// without the messages.
func (s *Store) List(user string) ([]Ticket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Ticket{}
	if err := s.load(); err != nil {
		return out, err
	}
	for _, t := range s.m {
		if user == "" || t.User == user {
			c := *t
			c.Messages = nil
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated > out[j].Updated })
	return out, nil
}
