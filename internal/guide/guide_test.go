package guide

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	md := "# Title\n\nSome **bold** and `a|b` and *it* <script>x</script>.\n\n" +
		"## Two words\n\n| A | B |\n|---|---|\n| `p8 start\\|stop` | [link](x.html) |\n\n" +
		"- one\n- two\n\n1. first\n2. second\n\n```\nsudo zpool replace <old>\n```\n\n> **Note** quote\n\n" +
		"![pic](ha.svg)\n\nSee https://example.com/a. and [bad](javascript:alert(1)) and \\~1 GB\n"
	h, heads := Render(md)
	for _, want := range []string{
		`<h1 id="title">`, `<strong>bold</strong>`, `<code>a|b</code>`, `<em>it</em>`, `&lt;script&gt;`,
		`<h2 id="two-words">`, `<td><code>p8 start|stop</code></td>`, `<a href="x.html">link</a>`,
		"<ul>\n<li>one</li>\n<li>two</li>\n</ul>", "<ol>\n<li>first</li>", `<pre><code>sudo zpool replace &lt;old&gt;</code></pre>`,
		`<blockquote><p><strong>Note</strong> quote</p></blockquote>`, `<img src="ha.svg" alt="pic"`,
		`<a href="https://example.com/a">https://example.com/a</a>.`, `<a href="#">bad</a>`, "~1 GB",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("missing %q in\n%s", want, h)
		}
	}
	if strings.Contains(h, "<script>") || strings.Contains(h, "javascript:") {
		t.Fatalf("unsafe output:\n%s", h)
	}
	if len(heads) != 2 || heads[1].ID != "two-words" || heads[1].Text != "Two words" {
		t.Fatalf("headings %+v", heads)
	}
}

func TestBuiltinAndHandler(t *testing.T) {
	s, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Pages) < 16 || s.Pages[0].Name != "what-is" || s.Pages[0].Summary == "" {
		t.Fatalf("pages: %d %+v", len(s.Pages), s.Pages[0].Name)
	}
	if _, ok := s.Assets["ha-failover.svg"]; !ok {
		t.Fatal("picture missing")
	}
	for _, p := range s.Pages {
		if strings.Contains(p.Body, "Summary:") {
			t.Errorf("%s: summary line left in the body", p.Name)
		}
	}
	h := Handler()
	for path, code := range map[string]int{"/docs/": 200, "/docs/vms": 200, "/docs/07-vms": 200, "/docs/vms.html": 200,
		"/docs/ha-failover.svg": 200, "/docs/nope": 404, "/docs": 301} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != code {
			t.Errorf("%s: %d, want %d", path, w.Code, code)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/docs/cluster-ha", nil))
	if b := w.Body.String(); !strings.Contains(b, `href="/docs/storage"`) || !strings.Contains(b, `src="ha-failover.svg"`) || !strings.Contains(b, "&larr; Dashboard") {
		t.Fatal("page links wrong")
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/docs/", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", w.Code)
	}
}

func TestWriteSite(t *testing.T) {
	s, _ := Builtin()
	dir := t.TempDir()
	if err := s.WriteSite(dir); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil || !strings.Contains(string(b), `href="vms.html"`) || strings.Contains(string(b), "Dashboard") {
		t.Fatalf("index: %v", err)
	}
	for _, f := range []string{"vms.html", "commands.html", "ha-failover.svg"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatal(err)
		}
	}
}
