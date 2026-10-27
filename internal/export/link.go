package export

import (
	"bufio"
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
// time at unstamped, a year no cutoff reaches. A step with that stamp or none
// is dropped, as the owner's count of messages at create drops it.
type stepCut struct{ until string }

func (c *stepCut) filter(fn func(*Step) error) func(*Step) error {
	n := 0
	return func(s *Step) error {
		if s.Timestamp == "" || s.Timestamp == unstamped || s.Timestamp > c.until {
			return nil
		}
		n++
		s.StepID = n
		return fn(s)
	}
}

type linkSession struct {
	n              int
	harness        string
	project        string
	start          time.Time
	th             *thread
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
	if n < 1 || n > len(v.SessionIDs) {
		return nil, fmt.Errorf("export: %w: link has %d sessions, not %d", ErrSessionNotFound, len(v.SessionIDs), n)
	}
	s, err := openSession(db, v.SessionIDs[n-1], nil)
	if err != nil {
		return nil, err
	}
	ls := &linkSession{n: n, harness: s.harness, mdURL: v.SessionURL(n, "md"), jsonURL: v.SessionURL(n, "json")}
	if cwd := s.src.FirstCwd; cwd != "" {
		if p := path.Base(cwd); p != "/" && p != "." {
			ls.project = p
		}
	}
	ls.th, ls.start, err = buildConversation(s.src, isoMillis(v.Cutoff), ls.jsonURL, n)
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

func pacificTime(t time.Time) string {
	if t.IsZero() {
		return "start unknown"
	}
	return t.In(pacific).Format(timeLayout)
}

func (ls *linkSession) prompts() int {
	n := 0
	for _, t := range ls.th.Turns {
		if t.Human() {
			n++
		}
	}
	return n
}

func (ls *linkSession) describe() string {
	parts := []string{ls.harness}
	if ls.project != "" {
		parts = append(parts, ls.project)
	}
	parts = append(parts, pacificTime(ls.start), plural(ls.prompts(), "prompt"))
	return strings.Join(parts, ", ")
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

func quoted(text string) string {
	return "> " + strings.ReplaceAll(text, "\n", "\n> ") + "\n\n"
}

func writeWorkLog(b *strings.Builder, steps []*step, indent string) {
	for _, s := range steps {
		switch s.Kind {
		case stepNote:
			fmt.Fprintf(b, "%s- Note: %s\n", indent, clip(s.Text, 200))
		case stepNotice:
			fmt.Fprintf(b, "%s- %s\n", indent, s.Text)
		case stepTool:
			c := s.Call
			line := c.Name
			if c.Summary != "" {
				line += ": " + c.Summary
			}
			if c.Sub == nil {
				fmt.Fprintf(b, "%s- %s\n", indent, line)
				continue
			}
			fmt.Fprintf(b, "%s- Subagent: %s (%s). Its report:\n\n", indent, c.Summary, c.Sub.StepsSummary())
			report := c.Sub.Report
			if report == "" {
				report = "(no report)"
			}
			b.WriteString(quoted(report))
		}
	}
}

func (ls *linkSession) writeMarkdown(bw *bufio.Writer, level string) error {
	if _, err := fmt.Fprintf(bw, "%s Session %d: %s\n\n", level, ls.n, ls.describe()); err != nil {
		return err
	}
	fmt.Fprintf(bw, "Tool outputs are left out. The full session, with every tool call and output, is at %s\n\n", ls.jsonURL)
	var b strings.Builder
	n := 0
	for _, t := range ls.th.Turns {
		b.Reset()
		switch {
		case t.Human():
			n++
			fmt.Fprintf(&b, "%s# Turn %d, %s\n\n**User:**\n\n%s\n\n", level, n, pacificTime(t.At), t.Prompt)
		case t.Background():
			fmt.Fprintf(&b, "%s# Background update, %s\n\n%s\n\n", level, pacificTime(t.At), t.Prompt)
		default:
			fmt.Fprintf(&b, "%s# Before the first prompt, %s\n\n", level, pacificTime(t.At))
		}
		for _, q := range t.Asks {
			fmt.Fprintf(&b, "**Asked:** %s\n\n**Answered:** %s\n\n", q.Question, q.Answer)
		}
		if len(t.Work) > 0 {
			fmt.Fprintf(&b, "**Work** (%s):\n\n", t.workStats())
			writeWorkLog(&b, t.Work, "")
			b.WriteString("\n")
		}
		if t.Answer != "" {
			b.WriteString("**Answer:**\n\n" + t.Answer + "\n\n")
		}
		if _, err := bw.WriteString(b.String()); err != nil {
			return err
		}
	}
	return nil
}

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
