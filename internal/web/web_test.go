package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServesPageWithSecurityHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "P8S <b>OS</b>") {
		t.Fatalf("code %d", rec.Code)
	}
	for _, h := range []string{"Content-Security-Policy", "X-Frame-Options", "X-Content-Type-Options"} {
		if rec.Header().Get(h) == "" {
			t.Errorf("missing %s", h)
		}
	}
	rec = httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/secret", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("other paths: %d", rec.Code)
	}
}

func TestServesConsoleAndNoVNC(t *testing.T) {
	for path, want := range map[string]string{
		"/console":           "new RFB(",
		"/novnc.min.js":      "window.RFB",
		"/novnc-LICENSE.txt": "MPL",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", path, nil)
		req.Host = "192.168.0.106:8006"
		Handler().ServeHTTP(rec, req)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s: %d, missing %q", path, rec.Code, want)
		}
		if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "wss://192.168.0.106:8006") {
			t.Errorf("%s: csp %q", path, csp)
		}
	}
}
