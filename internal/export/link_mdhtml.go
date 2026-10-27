package export

import (
	"html"
	"html/template"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// renderMarkdown is the only producer of template.HTML in the link pages.
// Every byte of src reaches the output through html.EscapeString or as an
// href whose scheme is http or https.
func renderMarkdown(src string) template.HTML {
	var b strings.Builder
	writeBlocks(&b, strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n"))
	return template.HTML(b.String())
}

var (
	headingLine = regexp.MustCompile(`^(#{1,6})\s+(.*?)\s*#*\s*$`)
	bulletLine  = regexp.MustCompile(`^\s*[-*+]\s+(.*)$`)
	orderedLine = regexp.MustCompile(`^\s*(\d{1,9})[.)]\s+(.*)$`)
	ruleLine    = regexp.MustCompile(`^(-{3,}|\*{3,}|_{3,})$`)
	tableRule   = regexp.MustCompile(`^\|?\s*:?-+:?\s*(\|\s*:?-+:?\s*)*\|?$`)
	fenceOpen   = regexp.MustCompile("^(`{3,}|~{3,})")
)

func writeBlocks(b *strings.Builder, lines []string) {
	for i := 0; i < len(lines); {
		t := strings.TrimSpace(lines[i])
		switch {
		case t == "":
			i++
		case fenceOpen.MatchString(t):
			i = writeFence(b, lines, i)
		case headingLine.MatchString(t):
			m := headingLine.FindStringSubmatch(t)
			n := strconv.Itoa(min(len(m[1])+2, 6))
			b.WriteString("<h" + n + ">" + inline(m[2]) + "</h" + n + ">\n")
			i++
		case ruleLine.MatchString(t):
			b.WriteString("<hr>\n")
			i++
		case strings.HasPrefix(t, ">"):
			var quote []string
			for ; i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), ">"); i++ {
				q := strings.TrimPrefix(strings.TrimSpace(lines[i]), ">")
				quote = append(quote, strings.TrimPrefix(q, " "))
			}
			b.WriteString("<blockquote>\n")
			writeBlocks(b, quote)
			b.WriteString("</blockquote>\n")
		case i+1 < len(lines) && strings.Contains(t, "|") && tableRule.MatchString(strings.TrimSpace(lines[i+1])):
			i = writeTable(b, lines, i)
		case bulletLine.MatchString(lines[i]):
			i = writeList(b, lines, i, bulletLine, "ul")
		case orderedLine.MatchString(lines[i]):
			i = writeList(b, lines, i, orderedLine, "ol")
		default:
			i = writeParagraph(b, lines, i)
		}
	}
}

func startsBlock(lines []string, i int) bool {
	t := strings.TrimSpace(lines[i])
	return t == "" || fenceOpen.MatchString(t) || headingLine.MatchString(t) || ruleLine.MatchString(t) ||
		strings.HasPrefix(t, ">") || bulletLine.MatchString(lines[i]) || orderedLine.MatchString(lines[i]) ||
		(i+1 < len(lines) && strings.Contains(t, "|") && tableRule.MatchString(strings.TrimSpace(lines[i+1])))
}

func writeFence(b *strings.Builder, lines []string, i int) int {
	t := strings.TrimSpace(lines[i])
	fence := fenceOpen.FindString(t)
	var body []string
	for i++; i < len(lines); i++ {
		c := strings.TrimSpace(lines[i])
		if strings.HasPrefix(c, fence) && strings.Trim(c, fence[:1]) == "" {
			i++
			break
		}
		body = append(body, lines[i])
	}
	b.WriteString("<pre><code>" + html.EscapeString(strings.Join(body, "\n")) + "</code></pre>\n")
	return i
}

func writeParagraph(b *strings.Builder, lines []string, i int) int {
	var parts []string
	for start := i; i < len(lines) && (i == start || !startsBlock(lines, i)); i++ {
		parts = append(parts, inline(strings.TrimSpace(lines[i])))
	}
	b.WriteString("<p>" + strings.Join(parts, "<br>\n") + "</p>\n")
	return i
}

func writeList(b *strings.Builder, lines []string, i int, item *regexp.Regexp, tag string) int {
	open := "<" + tag + ">"
	if m := orderedLine.FindStringSubmatch(lines[i]); tag == "ol" && m[1] != "1" {
		n, _ := strconv.Atoi(m[1])
		open = `<ol start="` + strconv.Itoa(n) + `">`
	}
	b.WriteString(open + "\n")
	var cur []string
	flush := func() {
		if cur != nil {
			b.WriteString("<li>" + strings.Join(cur, "<br>\n") + "</li>\n")
		}
		cur = nil
	}
	for i < len(lines) {
		l := lines[i]
		if m := item.FindStringSubmatch(l); m != nil {
			flush()
			cur = []string{inline(m[len(m)-1])}
			i++
			continue
		}
		t := strings.TrimSpace(l)
		if t == "" || startsBlock(lines, i) || !strings.HasPrefix(l, " ") && !strings.HasPrefix(l, "\t") {
			break
		}
		cur = append(cur, inline(t))
		i++
	}
	flush()
	b.WriteString("</" + tag + ">\n")
	return i
}

func tableCells(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	if strings.HasSuffix(line, "|") && !strings.HasSuffix(line, `\|`) {
		line = line[:len(line)-1]
	}
	var cells []string
	var cur strings.Builder
	for i := 0; i < len(line); i++ {
		switch {
		case line[i] == '\\' && i+1 < len(line) && line[i+1] == '|':
			cur.WriteByte('|')
			i++
		case line[i] == '|':
			cells = append(cells, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(line[i])
		}
	}
	return append(cells, strings.TrimSpace(cur.String()))
}

func writeTable(b *strings.Builder, lines []string, i int) int {
	b.WriteString("<div class=\"table\"><table>\n<tr>")
	for _, c := range tableCells(lines[i]) {
		b.WriteString("<th>" + inline(c) + "</th>")
	}
	b.WriteString("</tr>\n")
	for i += 2; i < len(lines) && strings.Contains(lines[i], "|") && strings.TrimSpace(lines[i]) != ""; i++ {
		b.WriteString("<tr>")
		for _, c := range tableCells(lines[i]) {
			b.WriteString("<td>" + inline(c) + "</td>")
		}
		b.WriteString("</tr>\n")
	}
	b.WriteString("</table></div>\n")
	return i
}

func safeHref(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", false
	}
	return html.EscapeString(raw), true
}

func link(href, label string) string {
	return `<a href="` + href + `" rel="noreferrer noopener">` + label + `</a>`
}

func inline(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '`':
			n := runOf(s[i:], '`')
			if end := strings.Index(s[i+n:], strings.Repeat("`", n)); end >= 0 {
				b.WriteString("<code>" + html.EscapeString(strings.TrimSpace(s[i+n:i+n+end])) + "</code>")
				i += 2*n + end
				continue
			}
			b.WriteString(s[i : i+n])
			i += n
			continue
		case c == '[':
			if label, target, n, ok := linkAt(s[i:]); ok {
				if href, ok := safeHref(target); ok {
					b.WriteString(link(href, inline(label)))
				} else {
					b.WriteString(html.EscapeString(s[i : i+n]))
				}
				i += n
				continue
			}
		case (c == '*' || c == '_') && i+1 < len(s) && s[i+1] == c:
			if end := strings.Index(s[i+2:], s[i:i+2]); end > 0 {
				b.WriteString("<strong>" + inline(s[i+2:i+2+end]) + "</strong>")
				i += 4 + end
				continue
			}
		case c == '*' || c == '_' && (i == 0 || !isWord(s[i-1])):
			if end := emphasisEnd(s[i+1:], c); end > 0 {
				b.WriteString("<em>" + inline(s[i+1:i+1+end]) + "</em>")
				i += 2 + end
				continue
			}
		case c == 'h' && (i == 0 || !isWord(s[i-1])) && (strings.HasPrefix(s[i:], "https://") || strings.HasPrefix(s[i:], "http://")):
			n := bareURLEnd(s[i:])
			if href, ok := safeHref(s[i : i+n]); ok {
				b.WriteString(link(href, href))
				i += n
				continue
			}
		}
		writeEscaped(&b, c)
		i++
	}
	return b.String()
}

func writeEscaped(b *strings.Builder, c byte) {
	switch c {
	case '<':
		b.WriteString("&lt;")
	case '>':
		b.WriteString("&gt;")
	case '&':
		b.WriteString("&amp;")
	case '"':
		b.WriteString("&#34;")
	case '\'':
		b.WriteString("&#39;")
	default:
		b.WriteByte(c)
	}
}

func runOf(s string, c byte) int {
	n := 0
	for n < len(s) && s[n] == c {
		n++
	}
	return n
}

func isWord(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func emphasisEnd(s string, c byte) int {
	if s == "" || s[0] == ' ' || s[0] == c {
		return -1
	}
	for j := 1; j < len(s); j++ {
		if s[j] == c && s[j-1] != ' ' && (c == '*' || j+1 == len(s) || !isWord(s[j+1])) {
			return j
		}
	}
	return -1
}

func linkAt(s string) (label, target string, n int, ok bool) {
	close := strings.Index(s, "](")
	if close < 0 || strings.ContainsAny(s[1:close], "[]") {
		return "", "", 0, false
	}
	end := strings.IndexByte(s[close+2:], ')')
	if end < 0 {
		return "", "", 0, false
	}
	target = strings.TrimSpace(s[close+2 : close+2+end])
	return s[1:close], target, close + 3 + end, true
}

func bareURLEnd(s string) int {
	n := strings.IndexAny(s, " \t\n<>\"`")
	if n < 0 {
		n = len(s)
	}
	for n > 0 && strings.ContainsRune(".,;:!?)'*_", rune(s[n-1])) {
		if s[n-1] == ')' && strings.Count(s[:n], "(") >= strings.Count(s[:n], ")") {
			break
		}
		n--
	}
	return n
}
