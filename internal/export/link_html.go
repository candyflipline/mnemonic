package export

import (
	"bufio"
	"html/template"
	"io"

	"github.com/candyflipline/mnemonic/internal/index"
)

type pageHead struct {
	Title, Note, MarkdownURL string
	Sessions                 []sessionRow
}

type sessionRow struct {
	N                       int
	Harness, Project, Start string
	Prompts                 string
	MD, JSON                string
	Turns                   []*turn
}

func rowOf(ls *linkSession) sessionRow {
	return sessionRow{N: ls.n, Harness: ls.harness, Project: ls.project, Start: pacificTime(ls.start),
		Prompts: plural(ls.prompts(), "prompt"), MD: ls.mdURL, JSON: ls.jsonURL, Turns: ls.th.Turns}
}

var linkPage = template.Must(template.New("link").Funcs(template.FuncMap{
	"md":       renderMarkdown,
	"pacific":  pacificTime,
	"stepsSum": func(s []*step) string { return plural(countCalls(s), "tool call") },
}).Parse(`
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
:root{color-scheme:light dark;--bg:#fff;--fg:#1a1a1a;--mut:#6b6b6b;--line:#e2e2e2;--code:#f4f4f4;--acc:#3b73d6;--bub:#eef3fc;--bg2:#f7f7f7}
@media (prefers-color-scheme:dark){:root{--bg:#141414;--fg:#e6e6e6;--mut:#9a9a9a;--line:#2e2e2e;--code:#1f1f1f;--acc:#7aa5f2;--bub:#1d2636;--bg2:#1a1a1a}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);font:16px/1.55 system-ui,-apple-system,sans-serif}
main{max-width:48rem;margin:0 auto;padding:1rem}
a{color:var(--acc)}
h1{font-size:1.5rem;margin:.5rem 0}
h2{font-size:1.15rem;margin:2.5rem 0 .75rem;padding-bottom:.4rem;border-bottom:1px solid var(--line);overflow-wrap:anywhere}
.agents{background:var(--code);padding:.5rem .75rem;border-radius:.5rem;overflow-wrap:anywhere;font-size:.9rem}
.muted{color:var(--mut)}
.table{overflow-x:auto;margin:.5rem 0}
table{border-collapse:collapse;font-size:.9rem}
th,td{padding:.3rem .6rem;border-bottom:1px solid var(--line);text-align:left;vertical-align:top}
.sessions th,.sessions td{white-space:nowrap}
details{margin:.3rem 0}
summary{cursor:pointer;color:var(--mut);overflow-wrap:anywhere}
summary::marker{color:var(--mut)}
pre{background:var(--code);padding:.6rem;border-radius:.4rem;overflow-x:auto;white-space:pre-wrap;overflow-wrap:anywhere;margin:.4rem 0;font-size:.85rem}
code{background:var(--code);padding:0 .25em;border-radius:.25rem;font-size:.9em;overflow-wrap:anywhere}
pre code{background:none;padding:0}
blockquote{margin:.5rem 0;padding-left:.8rem;border-left:3px solid var(--line);color:var(--mut)}
.toc ol{margin:.4rem 0;padding-left:1.6rem}
.toc li{overflow-wrap:anywhere}
.turn{margin:1.75rem 0}
.user{margin-left:auto;max-width:85%;width:fit-content;background:var(--bub);padding:.6rem .9rem;border-radius:1rem 1rem .25rem 1rem;white-space:pre-wrap;overflow-wrap:anywhere}
.user.bg{background:none;border:1px dashed var(--line);color:var(--mut);font-size:.9rem;white-space:normal}
.label{display:block;font-size:.75rem;text-transform:uppercase;letter-spacing:.05em;color:var(--mut)}
.ask{margin:.6rem 0 .6rem 15%;border:1px solid var(--line);border-radius:.75rem;padding:.5rem .8rem;font-size:.95rem}
.ask b{display:block}
.work{margin:.6rem 0;font-size:.92rem}
.work>.steps,.sub>.steps{border-left:2px solid var(--line);padding-left:.8rem;margin:.3rem 0 .3rem .3rem}
.note{color:var(--mut)}
.note p{margin:.3rem 0}
.notice{color:var(--mut);font-style:italic;margin:.3rem 0}
.tool>summary b{color:var(--fg);font-weight:600}
.tool>.body{padding-left:.8rem}
.answer{overflow-wrap:anywhere}
.answer p,.answer li{margin:.4rem 0}
.answer h3,.answer h4,.answer h5,.answer h6{margin:1rem 0 .4rem;font-size:1rem}
.more{display:inline}
.more>summary{display:inline;color:var(--acc)}
</style>
</head>
<body>
<main>
<p class="agents">Agents: run <code>curl -s {{.MarkdownURL}} -o link.md</code> and read link.md.</p>
<h1>{{.Title}}</h1>
{{if .Note}}<p>{{.Note}}</p>
{{end}}<p class="muted">Times are Pacific time (PDT or PST).</p>
<div class="table"><table class="sessions">
<tr><th>#</th><th>Harness</th><th>Project</th><th>Started</th><th>Prompts</th><th>Formats</th></tr>
{{range .Sessions}}<tr><td><a href="#s{{.N}}">{{.N}}</a></td><td>{{.Harness}}</td><td>{{.Project}}</td><td>{{.Start}}</td><td>{{.Prompts}}</td><td><a href="{{.MD}}">md</a> <a href="{{.JSON}}">json</a></td></tr>
{{end}}</table></div>
{{end -}}

{{- define "session" -}}
<h2 id="s{{.N}}">Session {{.N}}: {{.Harness}}{{if .Project}}, {{.Project}}{{end}}, {{.Start}}, {{.Prompts}}</h2>
<details class="toc"><summary>Contents · {{.Prompts}}</summary><ol>
{{range .Turns}}{{if .Human}}<li><a href="#{{.ID}}">{{.Title}}</a></li>
{{end}}{{end}}</ol></details>
{{range .Turns}}{{template "turn" .}}{{end}}
{{- end -}}

{{- define "turn" -}}
<section class="turn" id="{{.ID}}">
{{if .Human}}<div class="user">{{.PromptHead}}{{if .PromptRest}}<details class="more"><summary>Show more</summary>{{.PromptRest}}</details>{{end}}</div>
{{else if .Background}}<div class="user bg"><span class="label">Background update · {{pacific .At}}</span>{{.Prompt}}{{if .Detail}}<details><summary>Notification</summary><pre>{{.Detail}}</pre></details>{{end}}</div>
{{end}}{{range .Asks}}<div class="ask"><span class="label">Asked</span>{{.Question}}<b>{{.Answer}}</b></div>
{{end}}{{if .Work}}<details class="work"><summary>{{.WorkSummary}}</summary><div class="steps">
{{template "steps" .Work}}</div></details>
{{end}}{{if .Answer}}<div class="answer">{{md .Answer}}</div>
{{end}}</section>
{{end -}}

{{- define "steps" -}}
{{range .}}{{if eq .Kind "note"}}<div class="note">{{md .Text}}</div>
{{else if eq .Kind "notice"}}{{if .Body}}<details class="notice"><summary>{{.Text}}</summary><pre>{{.Body}}</pre></details>
{{else}}<p class="notice">{{.Text}}</p>
{{end}}{{else}}{{template "tool" .Call}}{{end}}{{end}}
{{- end -}}

{{- define "tool" -}}
<details class="tool"><summary><b>{{if .Sub}}Subagent{{else}}{{.Name}}{{end}}</b> {{.Summary}}{{with .Sub}} · {{stepsSum .Steps}}{{end}}</summary><div class="body">
{{with .Sub}}{{if .Prompt}}<details><summary>Prompt</summary><pre>{{.Prompt}}</pre></details>
{{end}}{{if .Report}}<span class="label">Report</span><div class="answer">{{md .Report}}</div>
{{end}}{{if .Steps}}<details class="sub"><summary>Steps · {{stepsSum .Steps}}</summary><div class="steps">
{{template "steps" .Steps}}</div></details>
{{end}}{{else}}<span class="label">Arguments</span><pre>{{.Args}}{{if .ArgsCut}} [cut]{{end}}</pre>
<span class="label">Output</span>{{if .Output}}<pre>{{.Output}}</pre>{{else}}<p class="muted">(empty)</p>{{end}}
{{if .OutCut}}<p class="muted">Output cut at 4 KB of {{.OutSize}} bytes. The full output is at <a href="{{.URL}}">{{.URL}}</a></p>
{{end}}{{end}}</div></details>
{{end -}}

{{- define "foot" -}}
</main>
</body>
</html>
{{end -}}
`))

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
	for _, row := range head.Sessions {
		if err := linkPage.ExecuteTemplate(bw, "session", row); err != nil {
			return err
		}
	}
	if err := linkPage.ExecuteTemplate(bw, "foot", nil); err != nil {
		return err
	}
	return bw.Flush()
}
