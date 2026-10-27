package export

import (
	"bufio"
	"html/template"
	"io"
	"strings"

	"github.com/candyflipline/mnemonic/internal/index"
)

type pageHead struct {
	Title, Note, MarkdownURL string
	Sessions                 []sessionRow
}

type sessionRow struct {
	N                       int
	Harness, Project, Start string
	Steps                   string
	MD, JSON                string
}

func rowOf(ls *linkSession) sessionRow {
	return sessionRow{N: ls.n, Harness: ls.t.harness, Project: ls.project, Start: ls.startText(),
		Steps: ls.stepsText(), MD: ls.mdURL, JSON: ls.jsonURL}
}

var linkPage = template.Must(template.New("link").Parse(`
{{- define "head" -}}
<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<meta name="robots" content="noindex, nofollow">
<title>{{.Title}}</title>
<style>
:root{color-scheme:light dark;--bg:#fff;--fg:#1a1a1a;--mut:#666;--line:#d8d8d8;--code:#f3f3f3;--user:#3b73d6}
@media (prefers-color-scheme:dark){:root{--bg:#121212;--fg:#e6e6e6;--mut:#9a9a9a;--line:#333;--code:#1e1e1e;--user:#6c9bf0}}
body{margin:0;background:var(--bg);color:var(--fg);font:16px/1.5 system-ui,sans-serif}
main{max-width:52rem;margin:0 auto;padding:1rem}
a{color:var(--user)}
h1{font-size:1.5rem;margin:.5rem 0}
h2{font-size:1.15rem;margin:2rem 0 .5rem;overflow-wrap:anywhere}
h3{font-size:.8rem;margin:0 0 .25rem;text-transform:uppercase;letter-spacing:.05em;color:var(--mut)}
.agents{background:var(--code);padding:.5rem .75rem;border-radius:.4rem;overflow-wrap:anywhere}
.muted{color:var(--mut)}
.table{overflow-x:auto}
table{border-collapse:collapse;font-size:.9rem;width:100%}
th,td{padding:.3rem .6rem;border-bottom:1px solid var(--line);text-align:left;white-space:nowrap}
.step{border-left:3px solid var(--line);padding-left:.8rem;margin:1rem 0}
.step.user{border-color:var(--user)}
.msg{white-space:pre-wrap;overflow-wrap:anywhere}
details{margin:.4rem 0}
summary{cursor:pointer;color:var(--mut);overflow-wrap:anywhere}
pre{background:var(--code);padding:.6rem;border-radius:.4rem;overflow-x:auto;white-space:pre-wrap;overflow-wrap:anywhere;margin:.4rem 0}
</style>
</head>
<body>
<main>
<p class="agents">Agents: run <code>curl -s {{.MarkdownURL}} -o link.md</code> and read link.md.</p>
<h1>{{.Title}}</h1>
{{if .Note}}<p class="msg">{{.Note}}</p>
{{end}}<p class="muted">Times are Pacific time (PDT or PST).</p>
<div class="table"><table>
<tr><th>#</th><th>Harness</th><th>Project</th><th>Started</th><th>Steps</th><th>Formats</th></tr>
{{range .Sessions}}<tr><td><a href="#s{{.N}}">{{.N}}</a></td><td>{{.Harness}}</td><td>{{.Project}}</td><td>{{.Start}}</td><td>{{.Steps}}</td><td><a href="{{.MD}}">md</a> <a href="{{.JSON}}">json</a></td></tr>
{{end}}</table></div>
{{end -}}

{{- define "session" -}}
<h2 id="s{{.N}}">Session {{.N}}: {{.Harness}}{{if .Project}}, {{.Project}}{{end}}, {{.Start}}, {{.Steps}}</h2>
{{end -}}

{{- define "step" -}}
<div class="step {{.Class}}">
<h3>{{.Role}}</h3>
{{if .Message}}<div class="msg">{{.Message}}</div>
{{end}}{{range .Calls}}<details><summary>Tool call: {{.Name}}</summary><pre>{{.Args}}{{if .Cut}} [cut]{{end}}</pre></details>
{{end}}{{range .Results}}<details><summary>Tool {{.Label}}</summary>{{if .Body}}<pre>{{.Body}}</pre>{{else}}<p class="muted">(empty)</p>{{end}}{{if .Cut}}<p class="muted">Output cut at 4 KB of {{.Size}} bytes. The full output is at <a href="{{.URL}}">{{.URL}}</a></p>{{end}}</details>
{{end}}</div>
{{end -}}

{{- define "foot" -}}
</main>
</body>
</html>
{{end -}}
`))

type htmlStep struct {
	stepView
	Class string
}

func WriteLinkHTML(w io.Writer, db *index.DB, v LinkView) error {
	sessions, err := openLink(db, v)
	if err != nil {
		return err
	}
	head := pageHead{Title: linkTitle(v), Note: v.Note, MarkdownURL: v.MarkdownURL}
	for _, ls := range sessions {
		head.Sessions = append(head.Sessions, rowOf(ls))
	}
	bw := bufio.NewWriter(w)
	if err := linkPage.ExecuteTemplate(bw, "head", head); err != nil {
		return err
	}
	for i, ls := range sessions {
		if err := linkPage.ExecuteTemplate(bw, "session", head.Sessions[i]); err != nil {
			return err
		}
		err := ls.t.eachStep(func(s *Step) error {
			sv := viewOf(s, ls.jsonURL)
			if sv.empty() {
				return nil
			}
			return linkPage.ExecuteTemplate(bw, "step", htmlStep{sv, strings.ToLower(sv.Role)})
		})
		if err != nil {
			return err
		}
	}
	if err := linkPage.ExecuteTemplate(bw, "foot", nil); err != nil {
		return err
	}
	return bw.Flush()
}
