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
	//go:embed cloud.js
	cloudJS []byte
	//go:embed license.js
	licenseJS []byte
	//go:embed node.js
	nodeJS []byte
	//go:embed containers.js
	containersJS []byte
	//go:embed offsite.js
	offsiteJS []byte
	//go:embed pages.js
	pagesJS []byte
	//go:embed terminal.html
	terminalPage []byte
	//go:embed xterm.min.js
	xtermJS []byte
	//go:embed xterm-fit.min.js
	xtermFitJS []byte
	//go:embed xterm-LICENSE.txt
	xtermLicense []byte
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
	"/cloud.js":          {cloudJS, "text/javascript; charset=utf-8", "no-cache"},
	"/license.js":        {licenseJS, "text/javascript; charset=utf-8", "no-cache"},
	"/node.js":           {nodeJS, "text/javascript; charset=utf-8", "no-cache"},
	"/containers.js":     {containersJS, "text/javascript; charset=utf-8", "no-cache"},
	"/offsite.js":        {offsiteJS, "text/javascript; charset=utf-8", "no-cache"},
	"/pages.js":          {pagesJS, "text/javascript; charset=utf-8", "no-cache"},
	"/terminal":          {terminalPage, "text/html; charset=utf-8", "no-cache"},
	"/xterm.min.js":      {xtermJS, "text/javascript; charset=utf-8", "public, max-age=86400"},
	"/xterm-fit.min.js":  {xtermFitJS, "text/javascript; charset=utf-8", "public, max-age=86400"},
	"/xterm-LICENSE.txt": {xtermLicense, "text/plain; charset=utf-8", "public, max-age=86400"},
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
		// the console pages may be shown inside the dashboard (same origin only)
		frame := "'none'"
		if r.URL.Path == "/console" || r.URL.Path == "/terminal" {
			frame = "'self'"
			h.Set("X-Frame-Options", "SAMEORIGIN")
		} else {
			h.Set("X-Frame-Options", "DENY")
		}
		h.Set("Referrer-Policy", "no-referrer")
		// connect-src lists the WebSocket origin explicitly for older browsers
		// where 'self' doesn't cover ws:/wss:.
		ws := ""
		if reHost.MatchString(r.Host) {
			ws = " wss://" + r.Host + " ws://" + r.Host
		}
		// Razorpay Checkout (wallet top-ups) loads its script and opens its frame.
		const rzp = "https://checkout.razorpay.com"
		h.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self' 'unsafe-inline' "+rzp+"; style-src 'unsafe-inline'; img-src 'self' data: https:; "+
				"connect-src 'self'"+ws+" https://api.razorpay.com https://lumberjack.razorpay.com; "+
				"frame-src 'self' https://api.razorpay.com "+rzp+"; frame-ancestors "+frame+"; form-action 'self'")
		_, _ = w.Write(a.body)
	})
}
