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
