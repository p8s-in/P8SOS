package guide

import (
	"fmt"
	"html"
	"regexp"
	"strings"
)

// A small Markdown renderer for the guide: headings, paragraphs, lists
// (one level), tables, fenced code, quotes, images, links, **bold**,
// *italic* and `code`. Everything is HTML-escaped; raw HTML is not allowed.

// Heading is a heading of a page (for the "On this page" list).
type Heading struct {
	Level int
	ID    string
	Text  string
}

var (
	reHeading  = regexp.MustCompile(`^(#{1,4})\s+(.*)$`)
	reOrdered  = regexp.MustCompile(`^\d+\.\s+`)
	reTableSep = regexp.MustCompile(`^\|?\s*:?-{3,}:?\s*(\|\s*:?-{3,}:?\s*)*\|?\s*$`)
	reImage    = regexp.MustCompile(`^!\[([^\]]*)\]\(([^)\s]+)\)$`)
	reSlug     = regexp.MustCompile(`[^a-z0-9]+`)
)

// Render turns Markdown into HTML and lists its headings.
func Render(md string) (string, []Heading) {
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	var b strings.Builder
	var heads []Heading
	ids := map[string]int{}
	for i := 0; i < len(lines); {
		l := lines[i]
		t := strings.TrimSpace(l)
		switch {
		case t == "":
			i++
		case strings.HasPrefix(t, "```"):
			lang := strings.TrimSpace(strings.TrimPrefix(t, "```"))
			i++
			var code []string
			for i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
				code = append(code, lines[i])
				i++
			}
			i++ // closing fence
			cls := ""
			if lang != "" && reSlug.ReplaceAllString(strings.ToLower(lang), "") == strings.ToLower(lang) {
				cls = ` class="lang-` + lang + `"`
			}
			fmt.Fprintf(&b, "<pre><code%s>%s</code></pre>\n", cls, html.EscapeString(strings.Join(code, "\n")))
		case reHeading.MatchString(t):
			m := reHeading.FindStringSubmatch(t)
			lv := len(m[1])
			id := slug(plain(m[2]))
			if n := ids[id]; n > 0 {
				id = fmt.Sprintf("%s-%d", id, n+1)
			}
			ids[slug(plain(m[2]))]++
			heads = append(heads, Heading{Level: lv, ID: id, Text: plain(m[2])})
			fmt.Fprintf(&b, "<h%d id=\"%s\">%s<a class=\"anchor\" href=\"#%s\" aria-label=\"Link to this section\">#</a></h%d>\n", lv, id, inline(m[2]), id, lv)
			i++
		case strings.HasPrefix(t, ">"):
			var q []string
			for i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), ">") {
				q = append(q, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[i]), ">")))
				i++
			}
			fmt.Fprintf(&b, "<blockquote><p>%s</p></blockquote>\n", inline(strings.Join(q, " ")))
		case strings.HasPrefix(t, "|") && i+1 < len(lines) && reTableSep.MatchString(strings.TrimSpace(lines[i+1])):
			head := cells(t)
			i += 2
			b.WriteString("<div class=\"table\"><table>\n<thead><tr>")
			for _, c := range head {
				fmt.Fprintf(&b, "<th>%s</th>", inline(c))
			}
			b.WriteString("</tr></thead>\n<tbody>\n")
			for i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "|") {
				b.WriteString("<tr>")
				for _, c := range cells(strings.TrimSpace(lines[i])) {
					fmt.Fprintf(&b, "<td>%s</td>", inline(c))
				}
				b.WriteString("</tr>\n")
				i++
			}
			b.WriteString("</tbody></table></div>\n")
		case strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ") || reOrdered.MatchString(t):
			ordered := reOrdered.MatchString(t)
			tag := "ul"
			if ordered {
				tag = "ol"
			}
			fmt.Fprintf(&b, "<%s>\n", tag)
			for i < len(lines) {
				t = strings.TrimSpace(lines[i])
				var item string
				switch {
				case ordered && reOrdered.MatchString(t):
					item = reOrdered.ReplaceAllString(t, "")
				case !ordered && (strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ")):
					item = t[2:]
				default:
					item = ""
				}
				if item == "" {
					break
				}
				i++
				// continuation lines (indented, not a new item)
				for i < len(lines) && strings.HasPrefix(lines[i], "  ") && strings.TrimSpace(lines[i]) != "" &&
					!reOrdered.MatchString(strings.TrimSpace(lines[i])) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "- ") {
					item += " " + strings.TrimSpace(lines[i])
					i++
				}
				fmt.Fprintf(&b, "<li>%s</li>\n", inline(item))
			}
			fmt.Fprintf(&b, "</%s>\n", tag)
		case reImage.MatchString(t):
			m := reImage.FindStringSubmatch(t)
			fmt.Fprintf(&b, "<figure><img src=\"%s\" alt=\"%s\" loading=\"lazy\"></figure>\n", html.EscapeString(safeURL(m[2])), html.EscapeString(m[1]))
			i++
		default:
			var p []string
			for i < len(lines) {
				t = strings.TrimSpace(lines[i])
				if t == "" || strings.HasPrefix(t, "```") || reHeading.MatchString(t) || strings.HasPrefix(t, ">") ||
					strings.HasPrefix(t, "|") || strings.HasPrefix(t, "- ") || reOrdered.MatchString(t) {
					break
				}
				p = append(p, t)
				i++
			}
			if len(p) == 0 { // a line no rule took: show it as text
				p = []string{t}
				i++
			}
			fmt.Fprintf(&b, "<p>%s</p>\n", inline(strings.Join(p, " ")))
		}
	}
	return b.String(), heads
}

// cells splits a table row on | (not \|).
func cells(row string) []string {
	row = strings.TrimSpace(row)
	row = strings.TrimPrefix(row, "|")
	if strings.HasSuffix(row, "|") && !strings.HasSuffix(row, `\|`) {
		row = row[:len(row)-1]
	}
	var out []string
	var cur strings.Builder
	for i := 0; i < len(row); i++ {
		if row[i] == '\\' && i+1 < len(row) && row[i+1] == '|' {
			cur.WriteByte('|')
			i++
			continue
		}
		if row[i] == '|' {
			out = append(out, strings.TrimSpace(cur.String()))
			cur.Reset()
			continue
		}
		cur.WriteByte(row[i])
	}
	return append(out, strings.TrimSpace(cur.String()))
}

func slug(s string) string {
	s = strings.Trim(reSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if s == "" {
		s = "section"
	}
	return s
}

// plain is the text of inline Markdown without its marks.
func plain(s string) string {
	s = strings.NewReplacer("**", "", "`", "").Replace(s)
	return unescape(s)
}

const punct = "\\`*_{}[]()#+-.!<>|~\""

func unescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && strings.IndexByte(punct, s[i+1]) >= 0 {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func safeURL(u string) string {
	l := strings.ToLower(u)
	if strings.HasPrefix(l, "javascript:") || strings.HasPrefix(l, "data:") || strings.HasPrefix(l, "vbscript:") {
		return "#"
	}
	return u
}

var reAuto = regexp.MustCompile(`https?://[^\s<>()]+[^\s<>().,;:!?*]`)

// inline renders code spans, links, bold, italic and bare https:// links.
func inline(s string) string {
	var b strings.Builder
	for len(s) > 0 {
		switch {
		case s[0] == '\\' && len(s) > 1 && strings.IndexByte(punct, s[1]) >= 0:
			b.WriteString(html.EscapeString(s[1:2]))
			s = s[2:]
		case s[0] == '`':
			end := strings.IndexByte(s[1:], '`')
			if end < 0 {
				b.WriteString("`")
				s = s[1:]
				continue
			}
			b.WriteString("<code>" + html.EscapeString(s[1:1+end]) + "</code>")
			s = s[end+2:]
		case strings.HasPrefix(s, "**"):
			end := strings.Index(s[2:], "**")
			if end < 0 {
				b.WriteString("**")
				s = s[2:]
				continue
			}
			b.WriteString("<strong>" + inline(s[2:2+end]) + "</strong>")
			s = s[end+4:]
		case s[0] == '*' && len(s) > 1 && s[1] != ' ':
			end := strings.IndexByte(s[1:], '*')
			if end < 0 {
				b.WriteString("*")
				s = s[1:]
				continue
			}
			b.WriteString("<em>" + inline(s[1:1+end]) + "</em>")
			s = s[end+2:]
		case s[0] == '[':
			close := strings.Index(s, "](")
			end := -1
			if close > 0 {
				end = strings.IndexByte(s[close:], ')')
			}
			if close < 0 || end < 0 {
				b.WriteString("[")
				s = s[1:]
				continue
			}
			text, url := s[1:close], s[close+2:close+end]
			fmt.Fprintf(&b, "<a href=\"%s\">%s</a>", html.EscapeString(safeURL(url)), inline(text))
			s = s[close+end+1:]
		case strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://"):
			if m := reAuto.FindString(s); m != "" && strings.HasPrefix(s, m) && !strings.ContainsAny(m, "<\\") {
				fmt.Fprintf(&b, "<a href=\"%s\">%s</a>", html.EscapeString(m), html.EscapeString(m))
				s = s[len(m):]
				continue
			}
			b.WriteString(html.EscapeString(s[:1]))
			s = s[1:]
		default:
			// copy up to the next special character
			n := strings.IndexAny(s[1:], "\\`*[h")
			if n < 0 {
				n = len(s) - 1
			}
			b.WriteString(html.EscapeString(s[:n+1]))
			s = s[n+1:]
		}
	}
	return b.String()
}
