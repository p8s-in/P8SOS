// Package audit records who changed what: one JSON line per action.
//
// The log is kept for RetainDays (default 180, what India's CERT-In asks
// for): the current file is rotated every day and at 10 MB into
// <path>.<YYYYMMDD-HHMMSS>, and rotated files older than the retention are
// deleted. Every entry carries the hash of the one before it (a chain), so a
// changed or deleted line shows up in Verify.
package audit

import (
	"bufio"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Entry struct {
	Time   string `json:"time"`
	User   string `json:"user"`
	IP     string `json:"ip"`
	Action string `json:"action"` // e.g. "POST /api/v1/vms/web1/start"
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
	Prev   string `json:"prev,omitempty"` // hash of the entry before
	Hash   string `json:"hash,omitempty"` // sha256(prev + "\n" + this entry without hash)
}

type Log struct {
	Path       string // empty = keep nothing
	MaxSize    int64  // rotate above this; 0 = 10 MB
	RetainDays int    // delete rotated files older than this; 0 = 180
	Now        func() time.Time

	mu     sync.Mutex
	last   string // hash of the newest entry
	loaded bool
	day    string // day of the current file's first entry
	pruned string // day we last deleted old files
}

func (l *Log) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l *Log) retain() int {
	if l.RetainDays <= 0 {
		return 180
	}
	return l.RetainDays
}

// hashOf is the chain hash of e (its Hash field is ignored).
func hashOf(e Entry) string {
	e.Hash = ""
	b, _ := json.Marshal(e)
	sum := sha256.Sum256(append([]byte(e.Prev+"\n"), b...))
	return hex.EncodeToString(sum[:])
}

// files lists the log files, oldest first (rotated ones, then the current).
func (l *Log) files() []string {
	old, _ := filepath.Glob(l.Path + ".*")
	var out []string
	for _, f := range old {
		if !strings.HasSuffix(f, ".tmp") {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return rotKey(out[i], l.Path) < rotKey(out[j], l.Path) })
	return append(out, l.Path)
}

// rotKey orders rotated files: the old ".1" first, then by date.
func rotKey(f, base string) string {
	s := strings.TrimPrefix(f, base+".")
	if s == "1" {
		return "0"
	}
	return s
}

// lastLine reads the newest entry of the current file (to continue the chain).
func (l *Log) loadLocked() {
	if l.loaded {
		return
	}
	l.loaded = true
	fs := l.files()
	for i := len(fs) - 1; i >= 0; i-- {
		var last Entry
		var first Entry
		n := 0
		_ = scan(fs[i], func(e Entry) bool {
			if n == 0 {
				first = e
			}
			n++
			last = e
			return true
		})
		if n > 0 {
			l.last = last.Hash
			if fs[i] == l.Path {
				l.day = dayOf(first.Time)
			}
			return
		}
	}
}

func dayOf(t string) string {
	if len(t) >= 10 {
		return t[:10]
	}
	return ""
}

// Write appends one entry (Time is filled in when empty).
func (l *Log) Write(e Entry) error {
	if l == nil || l.Path == "" {
		return nil
	}
	if e.Time == "" {
		e.Time = l.now().UTC().Format(time.RFC3339)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.loadLocked()
	max := l.MaxSize
	if max == 0 {
		max = 10 << 20
	}
	today := dayOf(e.Time)
	if st, err := os.Stat(l.Path); err == nil && (st.Size() > max || l.day != "" && l.day != today) {
		_ = os.Rename(l.Path, l.Path+"."+l.now().UTC().Format("20060102-150405"))
		l.day = ""
	}
	if l.pruned != today {
		l.pruned = today
		l.pruneLocked()
	}
	e.Prev, e.Hash = l.last, ""
	e.Hash = hashOf(e)
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.Path), 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(l.Path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = f.Write(append(b, '\n')); err != nil {
		return err
	}
	l.last = e.Hash
	if l.day == "" {
		l.day = today
	}
	return nil
}

// pruneLocked deletes rotated files older than the retention.
func (l *Log) pruneLocked() {
	cut := l.now().Add(-time.Duration(l.retain()) * 24 * time.Hour)
	for _, f := range l.files() {
		if f == l.Path {
			continue
		}
		if st, err := os.Stat(f); err == nil && st.ModTime().Before(cut) {
			_ = os.Remove(f)
		}
	}
}

func scan(path string, fn func(Entry) bool) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			if !fn(e) {
				return nil
			}
		}
	}
	return sc.Err()
}

// Recent returns up to n newest entries, newest first; user != "" keeps only that user's.
func (l *Log) Recent(n int, user string) ([]Entry, error) {
	out := []Entry{}
	if l == nil || l.Path == "" {
		return out, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	fs := l.files()
	for i := len(fs) - 1; i >= 0 && len(out) < n; i-- {
		var part []Entry
		if err := scan(fs[i], func(e Entry) bool {
			if user == "" || e.User == user {
				part = append(part, e)
			}
			return true
		}); err != nil {
			return nil, err
		}
		for j := len(part) - 1; j >= 0 && len(out) < n; j-- {
			out = append(out, part[j])
		}
	}
	return out, nil
}

// Between calls fn for every entry from..to (oldest first; zero = open end).
func (l *Log) Between(from, to time.Time, fn func(Entry)) error {
	if l == nil || l.Path == "" {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, f := range l.files() {
		if err := scan(f, func(e Entry) bool {
			t, err := time.Parse(time.RFC3339, e.Time)
			if err == nil && (from.IsZero() || !t.Before(from)) && (to.IsZero() || t.Before(to)) {
				fn(e)
			}
			return true
		}); err != nil {
			return err
		}
	}
	return nil
}

// WriteCSV writes entries from..to as CSV (for auditors / CERT-In requests).
func (l *Log) WriteCSV(w io.Writer, from, to time.Time) error {
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"time", "user", "ip", "action", "status", "detail", "hash"})
	err := l.Between(from, to, func(e Entry) {
		_ = cw.Write([]string{e.Time, e.User, e.IP, e.Action, fmt.Sprint(e.Status), e.Detail, e.Hash})
	})
	cw.Flush()
	if err != nil {
		return err
	}
	return cw.Error()
}

// Check is the result of Verify.
type Check struct {
	OK      bool   `json:"ok"`
	Entries int    `json:"entries"`
	Oldest  string `json:"oldest,omitempty"`
	Problem string `json:"problem,omitempty"`
}

// Verify walks the hash chain. Entries written before the chain existed
// (no hash) are skipped; the first kept entry may point at a deleted one.
func (l *Log) Verify() (Check, error) {
	c := Check{OK: true}
	if l == nil || l.Path == "" {
		return c, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	prev, started := "", false
	for _, f := range l.files() {
		line := 0
		if err := scan(f, func(e Entry) bool {
			line++
			if e.Hash == "" {
				return true
			}
			c.Entries++
			if c.Oldest == "" {
				c.Oldest = e.Time
			}
			switch {
			case hashOf(e) != e.Hash:
				c.OK, c.Problem = false, fmt.Sprintf("%s line %d (%s): the entry was changed", filepath.Base(f), line, e.Time)
			case started && e.Prev != prev:
				c.OK, c.Problem = false, fmt.Sprintf("%s line %d (%s): an entry before it is missing", filepath.Base(f), line, e.Time)
			}
			prev, started = e.Hash, true
			return c.OK
		}); err != nil {
			return c, err
		}
		if !c.OK {
			break
		}
	}
	return c, nil
}
