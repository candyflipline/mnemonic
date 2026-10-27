package export

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
	_ "time/tzdata"
	"unicode/utf8"

	"github.com/candyflipline/mnemonic/internal/index"
)

// LinkView is one read link as the renderers see it: the sessions the caller
// already checked are readable, in the owner's order, read as of Cutoff.
// MarkdownURL is the whole link's .md URL. SessionURL gives the URL of
// session n (1-based) in format ext, "md" or "json".
type LinkView struct {
	Title, Note string
	SessionIDs  []string
	Cutoff      time.Time
	MarkdownURL string
	SessionURL  func(n int, ext string) string
}

const (
	maxArgsBytes   = 300
	maxResultBytes = 4096
	unstamped      = "2100-01-01T00:00:00.000Z"
	timeLayout     = "2006-01-02 15:04 MST"
)

// The server binary may run where no zoneinfo is installed, so the zone data
// ships in the binary through the time/tzdata import.
var pacific = func() *time.Location {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		panic(err)
	}
	return loc
}()

// stepCut keeps the steps stamped at or before until, an isoMillis string, so
// that string order is time order. The index stamps a row with no parseable
// time at unstamped, a year no cutoff reaches, so a step with that stamp or
// none is kept exactly when the step before it was.
type stepCut struct{ until string }

func (c *stepCut) filter(fn func(*Step) error) func(*Step) error {
	kept, n := false, 0
	return func(s *Step) error {
		if s.Timestamp != "" && s.Timestamp != unstamped {
			kept = s.Timestamp <= c.until
		}
		if !kept {
			return nil
		}
		n++
		s.StepID = n
		return fn(s)
	}
}

// linkSession is session n of a link, opened once so its page header and its
// body read the same plan.
type linkSession struct {
	n              int
	t              *trajectory
	project        string
	start          time.Time
	steps          int
	mdURL, jsonURL string
}

func openCut(db *index.DB, v LinkView, n int) (*trajectory, error) {
	if n < 1 || n > len(v.SessionIDs) {
		return nil, fmt.Errorf("export: %w: link has %d sessions, not %d", ErrSessionNotFound, len(v.SessionIDs), n)
	}
	t, err := newTrajectory(db, v.SessionIDs[n-1], nil)
	if err != nil {
		return nil, err
	}
	t.cut = &stepCut{until: isoMillis(v.Cutoff)}
	return t, nil
}

func openLinkSession(db *index.DB, v LinkView, n int) (*linkSession, error) {
	t, err := openCut(db, v, n)
	if err != nil {
		return nil, err
	}
	ls := &linkSession{n: n, t: t, mdURL: v.SessionURL(n, "md"), jsonURL: v.SessionURL(n, "json")}
	if cwd := t.src.FirstCwd; cwd != "" {
		if p := path.Base(cwd); p != "/" && p != "." {
			ls.project = p
		}
	}
	err = t.eachStep(func(s *Step) error {
		if ls.start.IsZero() && s.Timestamp != "" && s.Timestamp != unstamped {
			ls.start, _ = time.Parse("2006-01-02T15:04:05.000Z", s.Timestamp)
		}
		if !viewOf(s, "").empty() {
			ls.steps++
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ls, nil
}

func openLink(db *index.DB, v LinkView) ([]*linkSession, error) {
	out := make([]*linkSession, len(v.SessionIDs))
	for i := range out {
		ls, err := openLinkSession(db, v, i+1)
		if err != nil {
			return nil, err
		}
		out[i] = ls
	}
	return out, nil
}

func (ls *linkSession) startText() string {
	if ls.start.IsZero() {
		return "start unknown"
	}
	return ls.start.In(pacific).Format(timeLayout)
}

func (ls *linkSession) stepsText() string {
	if ls.steps == 1 {
		return "1 step"
	}
	return fmt.Sprintf("%d steps", ls.steps)
}

func (ls *linkSession) describe() string {
	parts := []string{ls.t.harness}
	if ls.project != "" {
		parts = append(parts, ls.project)
	}
	parts = append(parts, ls.startText(), ls.stepsText())
	return strings.Join(parts, ", ")
}

// stepView is what a step shows: the one shape the markdown and the HTML both
// print, so the two cannot drift.
type stepView struct {
	Role, Message string
	Calls         []callView
	Results       []resultView
}

type callView struct {
	Name, Args string
	Cut        bool
}

type resultView struct {
	Label, Body string
	Cut         bool
	Size        int
	URL         string
}

func (v stepView) empty() bool {
	return v.Message == "" && len(v.Calls) == 0 && len(v.Results) == 0
}

func viewOf(s *Step, jsonURL string) stepView {
	v := stepView{Message: s.Message}
	switch s.Source {
	case "user":
		v.Role = "User"
	case "agent":
		v.Role = "Assistant"
	default:
		v.Role = "System"
	}
	names := map[string]string{}
	for _, c := range s.ToolCalls {
		names[c.ToolCallID] = c.FunctionName
		args, cut := cutBytes(compactJSON(c.Arguments), maxArgsBytes)
		v.Calls = append(v.Calls, callView{Name: c.FunctionName, Args: args, Cut: cut})
	}
	if s.Observation != nil {
		for _, r := range s.Observation.Results {
			rv := resultView{Label: "result", Size: len(r.Content), URL: jsonURL}
			if name := names[r.SourceCallID]; name != "" {
				rv.Label = "result of " + name
			}
			rv.Body, rv.Cut = cutBytes(r.Content, maxResultBytes)
			v.Results = append(v.Results, rv)
		}
	}
	return v
}

func cutBytes(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}

func compactJSON(m map[string]any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return "{}"
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func fenced(body string) string {
	run, longest := 0, 0
	for _, r := range body {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", max(3, longest+1))
	return fence + "\n" + body + "\n" + fence + "\n"
}

func writeStepMarkdown(b *strings.Builder, v stepView) {
	fmt.Fprintf(b, "### %s\n\n", v.Role)
	if v.Message != "" {
		b.WriteString(v.Message + "\n\n")
	}
	for _, c := range v.Calls {
		sep := " "
		if c.Args == "" {
			sep = ""
		}
		suffix := ""
		if c.Cut {
			suffix = " [cut]"
		}
		fmt.Fprintf(b, "Tool call: %s%s%s%s\n\n", c.Name, sep, c.Args, suffix)
	}
	for _, r := range v.Results {
		fmt.Fprintf(b, "Tool %s:\n\n", r.Label)
		if r.Body == "" {
			b.WriteString("(empty)\n\n")
			continue
		}
		b.WriteString(fenced(r.Body) + "\n")
		if r.Cut {
			fmt.Fprintf(b, "Output cut at 4 KB of %d bytes. The full output is at %s\n\n", r.Size, r.URL)
		}
	}
}

func (ls *linkSession) writeMarkdown(bw *bufio.Writer, level string) error {
	if _, err := fmt.Fprintf(bw, "%s Session %d: %s\n\n", level, ls.n, ls.describe()); err != nil {
		return err
	}
	var b strings.Builder
	return ls.t.eachStep(func(s *Step) error {
		v := viewOf(s, ls.jsonURL)
		if v.empty() {
			return nil
		}
		b.Reset()
		writeStepMarkdown(&b, v)
		_, err := bw.WriteString(b.String())
		return err
	})
}

// WriteLinkMarkdown writes the whole link as markdown: title, note, a
// numbered list of the sessions with their .md URLs, then each session.
func WriteLinkMarkdown(w io.Writer, db *index.DB, v LinkView) error {
	sessions, err := openLink(db, v)
	if err != nil {
		return err
	}
	bw := bufio.NewWriter(w)
	fmt.Fprintf(bw, "# %s\n\n", linkTitle(v))
	if v.Note != "" {
		fmt.Fprintf(bw, "%s\n\n", v.Note)
	}
	bw.WriteString("Times are Pacific time (PDT or PST).\n\n")
	for _, ls := range sessions {
		fmt.Fprintf(bw, "%d. %s: %s\n", ls.n, ls.describe(), ls.mdURL)
	}
	bw.WriteString("\n")
	for _, ls := range sessions {
		if err := ls.writeMarkdown(bw, "##"); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// WriteSessionMarkdown writes session n (1-based) of the link as markdown.
// An n outside the link is ErrSessionNotFound.
func WriteSessionMarkdown(w io.Writer, db *index.DB, v LinkView, n int) error {
	ls, err := openLinkSession(db, v, n)
	if err != nil {
		return err
	}
	bw := bufio.NewWriter(w)
	if err := ls.writeMarkdown(bw, "#"); err != nil {
		return err
	}
	return bw.Flush()
}

// WriteSessionATIF writes session n of the link as the validated ATIF
// trajectory WriteATIF writes, with the steps stamped after the cutoff
// dropped and the rest renumbered from 1. A session with no step at or before
// the cutoff is refused as ErrInvalidATIF, as an empty trajectory is.
func WriteSessionATIF(w io.Writer, db *index.DB, v LinkView, n int) error {
	t, err := openCut(db, v, n)
	if err != nil {
		return err
	}
	return t.writeIndented(w)
}

func linkTitle(v LinkView) string {
	if v.Title == "" {
		return "Shared sessions"
	}
	return v.Title
}
