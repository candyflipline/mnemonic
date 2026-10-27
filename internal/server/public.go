package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/export"
	"github.com/candyflipline/mnemonic/internal/index"
	"github.com/candyflipline/mnemonic/internal/wire"
)

const (
	bodyNotFound = "not found\n"
	bodyGone     = "this link was revoked or has expired\n"
	maxJoinBytes = 4 << 10
)

// Public is the [share] listener's handler: read links under /s/, invites
// under /i/, nothing else. It is the only code that reads a token: it parses
// it, finds the link, and hands the renderers a view.
func (s *Server) Public() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /s/{path...}", s.serveRead)
	mux.HandleFunc("GET /i/{path...}", s.serveInvite)
	mux.HandleFunc("POST /i/{token}", s.serveJoin)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Robots-Tag", "noindex, nofollow")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
		mux.ServeHTTP(w, r)
	})
}

// readPath is one URL form of a read link: the whole link (n 0) as HTML or
// markdown, or session n as markdown or ATIF JSON.
type readPath struct {
	token string
	n     int
	ext   string
}

func parseReadPath(p string) (readPath, bool) {
	token, rest, nested := strings.Cut(p, "/")
	if !nested {
		if t, ok := strings.CutSuffix(token, ".md"); ok {
			return readPath{token: t, ext: "md"}, true
		}
		return readPath{token: token, ext: "html"}, true
	}
	num, ext, ok := strings.Cut(rest, ".")
	n, err := strconv.Atoi(num)
	if !ok || err != nil || n < 1 || strconv.Itoa(n) != num || (ext != "md" && ext != "json") {
		return readPath{}, false
	}
	return readPath{token: token, n: n, ext: ext}, true
}

func linkError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, index.ErrLinkGone):
		http.Error(w, bodyGone, http.StatusGone)
	case errors.Is(err, index.ErrLinkUnknown):
		http.Error(w, bodyNotFound, http.StatusNotFound)
	case errors.Is(err, index.ErrKeyMember):
		http.Error(w, err.Error()+"\n", http.StatusConflict)
	default:
		http.Error(w, "internal error\n", http.StatusInternalServerError)
	}
}

func (s *Server) linkView(token string, l index.ReadLink) export.LinkView {
	base := s.share.BaseURL + "/s/" + token
	return export.LinkView{
		Title: l.Title, Note: l.Note, SessionIDs: l.SessionIDs, Cutoff: l.Cutoff,
		MarkdownURL: base + ".md",
		SessionURL:  func(n int, ext string) string { return fmt.Sprintf("%s/%d.%s", base, n, ext) },
	}
}

func (s *Server) markdownSize(db *index.DB, token string, l index.ReadLink, n int) (int64, error) {
	var c export.Counted
	err := export.WriteSessionMarkdown(&c, db, s.linkView(token, l), n)
	return c.N, err
}

func (s *Server) serveRead(w http.ResponseWriter, r *http.Request) {
	p, ok := parseReadPath(r.PathValue("path"))
	h, valid := index.ParseToken(p.token)
	if !ok || !valid {
		http.Error(w, bodyNotFound, http.StatusNotFound)
		return
	}
	db, release := s.ix.Snapshot()
	defer release()
	l, err := s.ctl.OpenRead(db, h, time.Now())
	if err != nil {
		linkError(w, err)
		return
	}
	if p.n > len(l.SessionIDs) {
		http.Error(w, bodyNotFound, http.StatusNotFound)
		return
	}
	v := s.linkView(p.token, l)
	out := &export.Counted{W: w}
	switch {
	case p.n == 0 && p.ext == "html":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		err = export.WriteLinkHTML(out, db, v)
	case p.n == 0:
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		err = export.WriteLinkMarkdown(out, db, v)
	case p.ext == "md":
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		err = export.WriteSessionMarkdown(out, db, v, p.n)
	default:
		w.Header().Set("Content-Type", "application/json")
		err = export.WriteSessionATIF(out, db, v, p.n)
	}
	if err != nil {
		if out.N > 0 {
			panic(http.ErrAbortHandler)
		}
		w.Header().Del("Content-Type")
		http.Error(w, "internal error\n", http.StatusInternalServerError)
	}
}

var invitePage = template.Must(template.New("invite").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<title>Join {{.Server}} as {{.Author}}</title>
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:46rem;margin:3rem auto;padding:0 1rem}pre{background:#f4f4f4;padding:1rem;overflow-x:auto}</style>
</head><body>
<h1>Join the team on {{.Server}}</h1>
<p>This invite admits one machine, which pushes its agent sessions as <b>{{.Author}}</b> and reads the team's sessions. It works once, until {{.Expires}}.</p>
<p>On that machine, run:</p>
<pre>{{.Install}}
{{.Join}}</pre>
<p>An agent can read this page as markdown at <code>{{.MarkdownURL}}</code>.</p>
</body></html>
`))

const inviteMarkdown = `# Join the team on %s

This invite admits one machine, which pushes its agent sessions as %s and reads the team's sessions. It works once, until %s.

On that machine, run:

` + "```" + `
%s
%s
` + "```" + `
`

func (s *Server) serveInvite(w http.ResponseWriter, r *http.Request) {
	path := r.PathValue("path")
	token, md := strings.CutSuffix(path, ".md")
	h, ok := index.ParseToken(token)
	if !ok {
		http.Error(w, bodyNotFound, http.StatusNotFound)
		return
	}
	inv, err := s.ctl.OpenInvite(h, time.Now())
	if err != nil {
		linkError(w, err)
		return
	}
	url := s.share.BaseURL + "/i/" + token
	page := struct{ Server, Author, Expires, Install, Join, MarkdownURL string }{
		Server: s.share.QUICAddr, Author: inv.Author,
		Expires: index.PacificText(inv.Expires),
		Install: "CGO_ENABLED=0 go install github.com/candyflipline/mnemonic/cmd/mnemonic@latest",
		Join:    "mnemonic join " + url, MarkdownURL: url + ".md",
	}
	if md {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		fmt.Fprintf(w, inviteMarkdown, page.Server, page.Author, page.Expires, page.Install, page.Join)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	invitePage.Execute(w, page)
}

func (s *Server) serveJoin(w http.ResponseWriter, r *http.Request) {
	h, ok := index.ParseToken(r.PathValue("token"))
	if !ok {
		http.Error(w, bodyNotFound, http.StatusNotFound)
		return
	}
	var req wire.JoinRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, maxJoinBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "body must be {\"key\": \"<base64url ed25519 public key>\", \"machine\": \"<name>\"}\n", http.StatusBadRequest)
		return
	}
	if _, err := auth.ParseKey(req.Key); err != nil {
		http.Error(w, err.Error()+"\n", http.StatusBadRequest)
		return
	}
	if !wire.ValidSegment(req.Machine) {
		http.Error(w, "machine must be lowercase letters, digits, '.', '_' or '-'\n", http.StatusBadRequest)
		return
	}
	author, err := s.ctl.Join(s.st, h, req.Key, req.Machine, time.Now())
	if err != nil {
		linkError(w, err)
		return
	}
	var b bytes.Buffer
	json.NewEncoder(&b).Encode(wire.JoinAnswer{Server: s.share.QUICAddr, ServerKey: s.share.ServerKey, Author: author})
	w.Header().Set("Content-Type", "application/json")
	w.Write(b.Bytes())
}
