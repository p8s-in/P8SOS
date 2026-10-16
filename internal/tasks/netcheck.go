package tasks

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// NetReport is netcheck.sh's answer, read into parts (Datacenter → Network).
type NetReport struct {
	Sections []NetSection `json:"sections"`
	Problems int          `json:"problems"`
	Summary  string       `json:"summary"`
	At       string       `json:"at"`
}

// NetSection is one "==> …" part of the check.
type NetSection struct {
	Title string    `json:"title"`
	Items []NetItem `json:"items"`
}

// NetItem is one line: State "ok", "bad" or "info", with its hints.
type NetItem struct {
	State string   `json:"state"`
	Text  string   `json:"text"`
	Hints []string `json:"hints,omitempty"`
}

// ParseNetCheck reads netcheck.sh's output.
func ParseNetCheck(out string) NetReport {
	var r NetReport
	var cur *NetSection
	var last *NetItem
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "==> "):
			r.Sections = append(r.Sections, NetSection{Title: strings.TrimPrefix(t, "==> ")})
			cur, last = &r.Sections[len(r.Sections)-1], nil
		case cur != nil && (strings.HasPrefix(t, "✔ ") || strings.HasPrefix(t, "✘ ") || strings.HasPrefix(t, "· ")):
			st := map[string]string{"✔": "ok", "✘": "bad", "·": "info"}[strings.Fields(t)[0]]
			_, text, _ := strings.Cut(t, " ")
			cur.Items = append(cur.Items, NetItem{State: st, Text: text})
			last = &cur.Items[len(cur.Items)-1]
			if st == "bad" {
				r.Problems++
			}
		case t == "":
			if cur != nil && last != nil {
				last = nil
			}
		case strings.HasPrefix(line, "    ") && last != nil:
			last.Hints = append(last.Hints, t)
		case cur == nil || last == nil && !strings.HasPrefix(line, " "):
			if !strings.HasPrefix(line, " ") {
				r.Summary = t // the closing sentence
			}
		}
	}
	return r
}

var netMu sync.Mutex
var netLast struct {
	at  time.Time
	rep NetReport
}

// NetCheck runs netcheck.sh (check only; at most once every 20 s).
func (m *Manager) NetCheck(ctx context.Context) (NetReport, error) {
	netMu.Lock()
	defer netMu.Unlock()
	if !netLast.at.IsZero() && m.now().Sub(netLast.at) < 20*time.Second {
		return netLast.rep, nil
	}
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	out, err := m.Run.Run(cctx, "/bin/bash", filepath.Join(m.Scripts, "netcheck.sh"))
	if len(out) == 0 && err != nil {
		return NetReport{}, err
	}
	r := ParseNetCheck(string(out)) // exit 1 = problems found
	r.At = m.now().UTC().Format(time.RFC3339)
	netLast.at, netLast.rep = m.now(), r
	return r, nil
}
