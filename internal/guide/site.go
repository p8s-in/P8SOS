// Package guide turns the Administrator Guide (docs/guide/*.md) into web pages:
// served by p8sd at /docs/ (Help in the dashboard, works offline) and written
// out as a standalone website by cmd/p8s-docs.
package guide

import (
	"fmt"
	"html"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"p8sos/docs"
)

// Page is one chapter.
type Page struct {
	Slug     string // "07-vms" → URL /docs/vms
	Name     string // "vms"
	Title    string // "7. Virtual machines"
	Body     string // HTML
	Headings []Heading
	Summary  string // the first sentence (contents page)
}

// Site is the whole guide.
type Site struct {
	Pages  []Page
	Assets map[string][]byte // pictures by file name
}

// Load reads and renders the guide from fsys (a directory holding NN-name.md files).
func Load(fsys fs.FS) (*Site, error) {
	ents, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	s := &Site{Assets: map[string][]byte{}}
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() {
			continue
		}
		b, err := fs.ReadFile(fsys, n)
		if err != nil {
			return nil, err
		}
		switch path.Ext(n) {
		case ".md":
			base := strings.TrimSuffix(n, ".md")
			name := base
			if i := strings.IndexByte(base, '-'); i > 0 {
				name = base[i+1:]
			}
			src, sum := string(b), ""
			// "Summary: ..." right after the title: shown on the contents page
			if l := strings.SplitN(src, "\n", 3); len(l) == 3 && strings.HasPrefix(l[1], "Summary:") {
				sum = strings.TrimSpace(strings.TrimPrefix(l[1], "Summary:"))
				src = l[0] + "\n" + l[2]
			}
			body, heads := Render(src)
			if sum == "" {
				sum = summary(body)
			}
			title := name
			if len(heads) > 0 && heads[0].Level == 1 {
				title = heads[0].Text
				heads = heads[1:]
			}
			s.Pages = append(s.Pages, Page{Slug: base, Name: name, Title: title, Body: body, Headings: heads, Summary: sum})
		case ".svg", ".png":
			s.Assets[n] = b
		}
	}
	if len(s.Pages) == 0 {
		return nil, fmt.Errorf("no guide pages")
	}
	sort.Slice(s.Pages, func(i, j int) bool { return s.Pages[i].Slug < s.Pages[j].Slug })
	return s, nil
}

var (
	once    sync.Once
	builtin *Site
	loadErr error
)

// Builtin is the guide embedded in the binary.
func Builtin() (*Site, error) {
	once.Do(func() {
		sub, err := fs.Sub(docs.Guide, "guide")
		if err != nil {
			loadErr = err
			return
		}
		builtin, loadErr = Load(sub)
	})
	return builtin, loadErr
}

// link is how pages link to each other: "/docs/vms" in p8sd, "vms.html" on the static site.
type link func(name string) string

// HTML is one page as a whole HTML document; i = -1 is the contents page.
func (s *Site) HTML(i int, ln link, home string) string {
	var b strings.Builder
	title := "P8S OS Administrator Guide"
	if i >= 0 {
		title = s.Pages[i].Title + " — " + title
	}
	fmt.Fprintf(&b, `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>%s</title><style>%s</style></head><body>
<header><button id="menu" aria-label="Chapters" onclick="document.body.classList.toggle('nav')">&#9776;</button>
<a class="brand" href="%s">P8S OS <span>Administrator Guide</span></a>`, html.EscapeString(title), css, ln(""))
	if home != "" {
		fmt.Fprintf(&b, `<a class="back" href="%s">&larr; Dashboard</a>`, home)
	}
	b.WriteString(`<input id="q" type="search" placeholder="Search the guide" aria-label="Search the guide"></header>
<div class="wrap"><nav><ol>`)
	for j, p := range s.Pages {
		cur := ""
		if j == i {
			cur = ` aria-current="page"`
		}
		fmt.Fprintf(&b, `<li><a href="%s"%s>%s</a></li>`, ln(p.Name), cur, html.EscapeString(p.Title))
	}
	b.WriteString(`</ol></nav><main><div id="results" hidden></div><div id="main">`)
	if i < 0 {
		b.WriteString(`<h1>P8S OS Administrator Guide</h1><p class="lead">How to plan, install and run P8S OS: virtual machines, containers, storage, backups, clusters and high availability, with step-by-step recipes for common setups.</p><div class="cards">`)
		for _, p := range s.Pages {
			fmt.Fprintf(&b, `<a class="card" href="%s"><strong>%s</strong><span>%s</span></a>`, ln(p.Name), html.EscapeString(p.Title), html.EscapeString(p.Summary))
		}
		b.WriteString(`</div>`)
	} else {
		p := s.Pages[i]
		body := p.Body
		if k := strings.Index(body, "</h1>\n"); k >= 0 && p.Summary != "" {
			body = body[:k+6] + `<p class="lead">` + html.EscapeString(p.Summary) + "</p>\n" + body[k+6:]
		}
		b.WriteString(`<article>` + body + `</article><div class="pager">`)
		if i > 0 {
			fmt.Fprintf(&b, `<a href="%s">&larr; %s</a>`, ln(s.Pages[i-1].Name), html.EscapeString(s.Pages[i-1].Title))
		} else {
			b.WriteString(`<span></span>`)
		}
		if i+1 < len(s.Pages) {
			fmt.Fprintf(&b, `<a class="next" href="%s">%s &rarr;</a>`, ln(s.Pages[i+1].Name), html.EscapeString(s.Pages[i+1].Title))
		}
		b.WriteString(`</div>`)
	}
	b.WriteString(`</div></main>`)
	if i >= 0 && len(s.Pages[i].Headings) > 1 {
		b.WriteString(`<aside><p>On this page</p><ul>`)
		for _, h := range s.Pages[i].Headings {
			if h.Level <= 3 {
				fmt.Fprintf(&b, `<li class="l%d"><a href="#%s">%s</a></li>`, h.Level, h.ID, html.EscapeString(h.Text))
			}
		}
		b.WriteString(`</ul></aside>`)
	}
	b.WriteString(`</div><script>` + s.searchJS(ln) + `</script></body></html>`)
	return b.String()
}

// searchJS: a small search over every page's text, all in the page (no server).
func (s *Site) searchJS(ln link) string {
	var idx strings.Builder
	idx.WriteString("[")
	for i, p := range s.Pages {
		if i > 0 {
			idx.WriteString(",")
		}
		for j, h := range append([]Heading{{ID: "", Text: p.Title}}, p.Headings...) {
			if j > 0 {
				idx.WriteString(",")
			}
			u := ln(p.Name)
			if h.ID != "" {
				u += "#" + h.ID
			}
			fmt.Fprintf(&idx, "[%q,%q,%q]", h.Text, p.Title, u)
		}
		fmt.Fprintf(&idx, ",[%q,%q,%q]", strings.ToLower(textOf(p.Body)), "\x00"+p.Title, ln(p.Name))
	}
	idx.WriteString("]")
	return `(function(){var I=` + strings.ReplaceAll(idx.String(), "</", `<\/`) + `,q=document.getElementById('q'),r=document.getElementById('results'),m=document.getElementById('main');
function esc(s){return s.replace(/[&<>"]/g,function(c){return{'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]})}
q.addEventListener('input',function(){var t=q.value.trim().toLowerCase();if(t.length<2){r.hidden=true;m.hidden=false;return}
var h=[],full=[];I.forEach(function(e){if(e[1].charAt(0)==='\u0000'){if(e[0].indexOf(t)>=0)full.push(e)}else if(e[0].toLowerCase().indexOf(t)>=0)h.push(e)});
var out='<h2>Results for “'+esc(q.value)+'”</h2><ul>';h.forEach(function(e){out+='<li><a href="'+e[2]+'">'+esc(e[0])+'</a> <span>'+esc(e[1])+'</span></li>'});
full.forEach(function(e){if(!h.some(function(x){return x[2].split('#')[0]===e[2]})){var i=e[0].indexOf(t),s=e[0].slice(Math.max(0,i-60),i+80);out+='<li><a href="'+e[2]+'">'+esc(e[1].slice(1))+'</a> <span>…'+esc(s)+'…</span></li>'}});
if(!h.length&&!full.length)out+='<li>Nothing found.</li>';r.innerHTML=out+'</ul>';r.hidden=false;m.hidden=true});
q.addEventListener('keydown',function(e){if(e.key==='Escape'){q.value='';q.dispatchEvent(new Event('input'))}})})();`
}

// summary is the first sentence of a page's first paragraph.
func summary(body string) string {
	i := strings.Index(body, "<p>")
	if i < 0 {
		return ""
	}
	j := strings.Index(body[i:], "</p>")
	if j < 0 {
		return ""
	}
	t := textOf(body[i : i+j])
	if k := strings.Index(t, ". "); k > 0 {
		t = t[:k+1]
	}
	if r := []rune(t); len(r) > 160 {
		t = string(r[:157]) + "…"
	}
	return t
}

// textOf strips tags (good enough for searching).
func textOf(h string) string {
	var b strings.Builder
	in := false
	for _, r := range h {
		switch {
		case r == '<':
			in = true
		case r == '>':
			in = false
			b.WriteByte(' ')
		case !in:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(html.UnescapeString(b.String())), " ")
}

// Handler serves the guide below /docs/.
func Handler() http.Handler {
	ln := func(n string) string {
		if n == "" {
			return "/docs/"
		}
		return "/docs/" + n
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s, err := Builtin()
		if err != nil {
			http.Error(w, "the guide is missing from this build", http.StatusInternalServerError)
			return
		}
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'self'")
		p := strings.TrimPrefix(r.URL.Path, "/docs")
		if p == "" {
			http.Redirect(w, r, "/docs/", http.StatusMovedPermanently)
			return
		}
		name := strings.TrimSuffix(strings.TrimPrefix(p, "/"), ".html")
		if b, ok := s.Assets[name]; ok {
			h.Set("Content-Type", "image/svg+xml")
			if strings.HasSuffix(name, ".png") {
				h.Set("Content-Type", "image/png")
			}
			h.Set("Cache-Control", "public, max-age=86400")
			_, _ = w.Write(b)
			return
		}
		i := -2
		if name == "" || name == "index" {
			i = -1
		}
		for j, pg := range s.Pages {
			if pg.Name == name || pg.Slug == name {
				i = j
			}
		}
		if i == -2 {
			http.NotFound(w, r)
			return
		}
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Cache-Control", "no-cache")
		_, _ = w.Write([]byte(s.HTML(i, ln, "/")))
	})
}

// WriteSite writes the guide as a standalone website into dir:
// index.html, one NAME.html per chapter and the pictures. Open index.html
// from disk or put the folder on any web host.
func (s *Site) WriteSite(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	ln := func(n string) string {
		if n == "" {
			return "index.html"
		}
		return n + ".html"
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(s.HTML(-1, ln, "")), 0o644); err != nil {
		return err
	}
	for i, p := range s.Pages {
		if err := os.WriteFile(filepath.Join(dir, p.Name+".html"), []byte(s.HTML(i, ln, "")), 0o644); err != nil {
			return err
		}
	}
	for n, b := range s.Assets {
		if err := os.WriteFile(filepath.Join(dir, n), b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

const css = `:root{--bg:#fff;--fg:#1d2430;--mute:#5b6574;--line:#e3e7ec;--soft:#f5f7f9;--accent:#014578;--code:#f1f4f7;color-scheme:light dark}
@media (prefers-color-scheme:dark){:root{--bg:#12161c;--fg:#e6e9ee;--mute:#a7b0bc;--line:#2a313b;--soft:#1a2028;--accent:#6cb4ee;--code:#1d242d}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);font:16px/1.6 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif}
a{color:var(--accent)}header{position:sticky;top:0;z-index:3;display:flex;gap:16px;align-items:center;padding:10px 20px;background:var(--bg);border-bottom:1px solid var(--line)}
.brand{white-space:nowrap;font-weight:700;text-decoration:none;color:var(--fg)}.brand span{font-weight:400;color:var(--mute)}.back{font-size:14px}
#q{margin-left:auto;width:min(320px,45vw);padding:7px 10px;border:1px solid var(--line);border-radius:8px;background:var(--soft);color:var(--fg);font:inherit;font-size:14px}
#menu{display:none;background:none;border:0;color:var(--fg);font-size:20px;cursor:pointer}
.wrap{display:grid;grid-template-columns:250px minmax(0,1fr) 220px;max-width:1400px;margin:0 auto}
nav{position:sticky;top:57px;height:calc(100vh - 57px);overflow:auto;padding:20px 12px;border-right:1px solid var(--line);font-size:14px}
nav ol{list-style:none;margin:0;padding:0}nav a{display:block;padding:5px 10px;border-radius:6px;color:var(--fg);text-decoration:none}
nav a:hover{background:var(--soft)}nav a[aria-current]{background:var(--soft);color:var(--accent);font-weight:600}
main{padding:24px 40px 64px;min-width:0}article{max-width:860px}
aside{position:sticky;top:57px;height:calc(100vh - 57px);overflow:auto;padding:24px 12px;font-size:13px}aside p{margin:0 0 6px;font-weight:600;color:var(--mute)}
aside ul{list-style:none;margin:0;padding:0}aside li{padding:3px 0}aside .l3{padding-left:12px}aside a{color:var(--mute);text-decoration:none}aside a:hover{color:var(--accent)}
h1{font-size:30px;line-height:1.25;margin:8px 0 16px}h2{font-size:22px;margin:36px 0 10px;padding-top:8px;border-top:1px solid var(--line)}h3{font-size:18px;margin:26px 0 8px}
h1,h2,h3{scroll-margin-top:70px}.anchor{margin-left:8px;color:var(--line);text-decoration:none;font-weight:400}h1:hover .anchor,h2:hover .anchor,h3:hover .anchor{color:var(--mute)}
code{font:13.5px/1.5 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;background:var(--code);padding:1px 5px;border-radius:4px}
pre{background:var(--code);padding:14px 16px;border-radius:8px;overflow:auto;border:1px solid var(--line)}pre code{padding:0;background:none}
.table{overflow-x:auto;margin:14px 0}table{border-collapse:collapse;width:100%;font-size:14.5px}th,td{text-align:left;vertical-align:top;padding:8px 10px;border-bottom:1px solid var(--line)}
th{background:var(--soft);font-weight:600}blockquote{margin:16px 0;padding:10px 16px;border-left:4px solid var(--accent);background:var(--soft);border-radius:0 8px 8px 0}blockquote p{margin:0}
figure{margin:20px 0}figure img{width:100%;height:auto;border:1px solid var(--line);border-radius:8px;background:var(--bg)}
.lead{font-size:18px;color:var(--mute);max-width:760px}.cards{display:grid;grid-template-columns:repeat(auto-fill,minmax(240px,1fr));gap:14px;margin-top:24px}
.card{display:flex;flex-direction:column;gap:6px;padding:16px;border:1px solid var(--line);border-radius:10px;text-decoration:none;color:var(--fg)}.card:hover{border-color:var(--accent)}.card span{font-size:13px;color:var(--mute)}
.pager{display:flex;justify-content:space-between;gap:16px;max-width:860px;margin-top:48px;padding-top:16px;border-top:1px solid var(--line)}.pager .next{margin-left:auto;text-align:right}
#results{max-width:860px}#results ul{list-style:none;padding:0}#results li{padding:8px 0;border-bottom:1px solid var(--line)}#results span{display:block;font-size:13px;color:var(--mute)}
@media (max-width:1100px){.wrap{grid-template-columns:230px minmax(0,1fr)}aside{display:none}}
@media (max-width:760px){#menu{display:block}.wrap{display:block}nav{display:none;position:fixed;top:53px;left:0;right:0;bottom:0;height:auto;z-index:2;background:var(--bg)}
body.nav nav{display:block}main{padding:16px}td,th{min-width:110px}.brand span{display:none}#q{width:auto;flex:1}}
@media print{header,nav,aside,.pager{display:none}.wrap{display:block}main{padding:0}}`
