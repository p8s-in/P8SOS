// Package audit records who changed what: one JSON line per action in a log
// file that is rotated at 10 MB (one old file kept).
package audit

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
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
}

type Log struct {
	Path    string // empty = keep nothing
	MaxSize int64  // rotate above this; 0 = 10 MB

	mu sync.Mutex
}

// Write appends one entry (Time is filled in when empty).
func (l *Log) Write(e Entry) error {
	if l == nil || l.Path == "" {
		return nil
	}
	if e.Time == "" {
		e.Time = time.Now().UTC().Format(time.RFC3339)
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	max := l.MaxSize
	if max == 0 {
		max = 10 << 20
	}
	if st, err := os.Stat(l.Path); err == nil && st.Size() > max {
		_ = os.Rename(l.Path, l.Path+".1")
	}
	if err := os.MkdirAll(filepath.Dir(l.Path), 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(l.Path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// Recent returns up to n newest entries, newest first; user != "" keeps only that user's.
func (l *Log) Recent(n int, user string) ([]Entry, error) {
	out := []Entry{}
	if l == nil || l.Path == "" {
		return out, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var all []Entry
	for _, p := range []string{l.Path + ".1", l.Path} {
		f, err := os.Open(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			var e Entry
			if json.Unmarshal(sc.Bytes(), &e) == nil && (user == "" || e.User == user) {
				all = append(all, e)
			}
		}
		f.Close()
	}
	for i := len(all) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, all[i])
	}
	return out, nil
}
