// Package web serves the P8S OS dashboard and the VM console page. Everything
// is embedded in the binary: no external assets, works offline.
package web

import (
	_ "embed"
	"net/http"
	"regexp"
)

var reHost = regexp.MustCompile(`^[A-Za-z0-9.\-\[\]:]{1,255}$`)

var (
	//go:embed index.html
	page []byte
	//go:embed console.html
	consolePage []byte
	//go:embed novnc.min.js
	novncJS []byte
	//go:embed novnc-LICENSE.txt
	novncLicense []byte
)

type asset struct {
	body  []byte
	ctype string
	cache string
}

var assets = map[string]asset{
	"/":                  {page, "text/html; charset=utf-8", "no-cache"},
	"/console":           {consolePage, "text/html; charset=utf-8", "no-cache"},
	"/novnc.min.js":      {novncJS, "text/javascript; charset=utf-8", "public, max-age=86400"},
	"/novnc-LICENSE.txt": {novncLicense, "text/plain; charset=utf-8", "public, max-age=86400"},
}

// Handler serves the embedded pages and 404s everything else.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a, ok := assets[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h := w.Header()
		h.Set("Content-Type", a.ctype)
		h.Set("Cache-Control", a.cache)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		// connect-src lists the WebSocket origin explicitly for older browsers
		// where 'self' doesn't cover ws:/wss:.
		ws := ""
		if reHost.MatchString(r.Host) {
			ws = " wss://" + r.Host + " ws://" + r.Host
		}
		h.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'unsafe-inline'; img-src 'self' data:; "+
				"connect-src 'self'"+ws+"; frame-ancestors 'none'; form-action 'self'")
		_, _ = w.Write(a.body)
	})
}
