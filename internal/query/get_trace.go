package query

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/candyflipline/mnemonic/internal/schema"
)

type getTrace struct{}

func (getTrace) Name() string { return "get_trace" }

func init() { register(getTrace{}) }

type traceArgs struct {
	SessionID          string `json:"session_id"`
	IncludeAnnotations *bool  `json:"include_annotations"`
}

type traceTokens struct {
	Input      uint64 `json:"input"`
	Output     uint64 `json:"output"`
	CacheRead  uint64 `json:"cache_read"`
	CacheWrite uint64 `json:"cache_write"`
}

type traceAnnotation struct {
	ID     string                  `json:"id"`
	SpanID string                  `json:"span_id,omitempty"`
	Name   string                  `json:"name"`
	Value  schema.Value            `json:"value"`
	Source schema.AnnotationSource `json:"source"`
	Author string                  `json:"author"`
	TS     string                  `json:"ts"`
}

type traceSpan struct {
	SpanID      string            `json:"span_id"`
	Kind        string            `json:"kind"`
	Name        string            `json:"name"`
	Start       string            `json:"start"`
	End         string            `json:"end"`
	Status      string            `json:"status"`
	Model       string            `json:"model,omitempty"`
	Tokens      traceTokens       `json:"tokens"`
	Cost        *float64          `json:"cost"`
	Annotations []traceAnnotation `json:"annotations,omitempty"`
	Children    []*traceSpan      `json:"children"`
	parent      string
}

type traceTotals struct {
	Spans  int         `json:"spans"`
	Tokens traceTokens `json:"tokens"`
	Cost   *float64    `json:"cost"`
}

type traceResult struct {
	SessionID   string            `json:"session_id"`
	Totals      traceTotals       `json:"totals"`
	Annotations []traceAnnotation `json:"annotations,omitempty"`
	Tree        []*traceSpan      `json:"tree"`
}

func (getTrace) Call(ctx context.Context, db *sql.DB, raw json.RawMessage) (any, string, error) {
	var a traceArgs
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return nil, "", argErrorf("invalid arguments: %s", err)
	}
	id, err := parseMcpID(a.SessionID)
	if err != nil {
		return nil, "", err
	}
	if id.kind != kindSession {
		return nil, "", argErrorf("session_id must be a session id")
	}
	if err := requireTarget(ctx, db, id.session, ""); err != nil {
		return nil, "", err
	}
	f, err := Sessions(ctx, db)
	if err != nil {
		return nil, "", err
	}
	spans, err := loadSpans(ctx, db, id.session, f)
	if err != nil {
		return nil, "", err
	}
	res := traceResult{SessionID: a.SessionID, Tree: []*traceSpan{}}
	res.Tree, res.Totals = buildTree(spans)
	if a.IncludeAnnotations == nil || *a.IncludeAnnotations {
		notes, err := loadAnnotations(ctx, db, id.session, f)
		if err != nil {
			return nil, "", err
		}
		bySpan := map[string]*traceSpan{}
		for _, s := range spans {
			bySpan[s.SpanID] = s
		}
		for _, n := range notes {
			if n.SpanID == "" {
				res.Annotations = append(res.Annotations, n)
			} else if s, ok := bySpan[n.SpanID]; ok {
				s.Annotations = append(s.Annotations, n)
			}
		}
	}
	return res, renderTrace(res), nil
}

func loadSpans(ctx context.Context, db *sql.DB, session string, f SessionFilter) ([]*traceSpan, error) {
	scope := ""
	if p := f.Opened("session_id"); p != "" {
		scope = " AND " + p
	}
	rows, err := db.QueryContext(ctx, `SELECT span_id, parent_span_id, kind, name, epoch_ms(start_time), epoch_ms(end_time),
  status, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost
FROM spans WHERE session_id = `+sqlQuote(session)+scope+` ORDER BY start_time, kind, span_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*traceSpan
	for rows.Next() {
		var s traceSpan
		var startMs, endMs int64
		var cost sql.NullFloat64
		if err := rows.Scan(&s.SpanID, &s.parent, &s.Kind, &s.Name, &startMs, &endMs, &s.Status, &s.Model,
			&s.Tokens.Input, &s.Tokens.Output, &s.Tokens.CacheRead, &s.Tokens.CacheWrite, &cost); err != nil {
			return nil, err
		}
		s.Start, s.End = formatRFC3339UTCMillis(startMs), formatRFC3339UTCMillis(endMs)
		if cost.Valid {
			s.Cost = &cost.Float64
		}
		s.Children = []*traceSpan{}
		out = append(out, &s)
	}
	return out, rows.Err()
}

// buildTree nests spans under their parents; a span whose parent is not in
// the session is a root. A span the roots do not reach sits on a parent cycle
// (or hangs off one): following parent links from the first such span in order
// until one repeats finds a cycle member, which becomes an extra root with its
// parent link cut, and so on until every span is in the tree once. Roots
// carry the sums of what is beneath them, so the totals are the roots' sums.
func buildTree(spans []*traceSpan) ([]*traceSpan, traceTotals) {
	byID := make(map[string]*traceSpan, len(spans))
	for _, s := range spans {
		byID[s.SpanID] = s
	}
	roots := []*traceSpan{}
	kids := make(map[*traceSpan][]*traceSpan, len(spans))
	for _, s := range spans {
		if p, ok := byID[s.parent]; ok && s.parent != "" && p != s {
			kids[p] = append(kids[p], s)
		} else {
			roots = append(roots, s)
		}
	}
	reached := make(map[*traceSpan]bool, len(spans))
	var walk func(*traceSpan)
	walk = func(s *traceSpan) {
		if reached[s] {
			return
		}
		reached[s] = true
		s.Children = append(s.Children, kids[s]...)
		for _, c := range kids[s] {
			walk(c)
		}
	}
	for _, r := range roots {
		walk(r)
	}
	for _, s := range spans {
		if reached[s] {
			continue
		}
		seen := map[*traceSpan]bool{}
		r := s
		for !seen[r] {
			seen[r] = true
			r = byID[r.parent]
		}
		p := byID[r.parent]
		kids[p] = slices.DeleteFunc(kids[p], func(c *traceSpan) bool { return c == r })
		roots = append(roots, r)
		walk(r)
	}
	t := traceTotals{Spans: len(spans)}
	for _, r := range roots {
		t.Tokens.Input += r.Tokens.Input
		t.Tokens.Output += r.Tokens.Output
		t.Tokens.CacheRead += r.Tokens.CacheRead
		t.Tokens.CacheWrite += r.Tokens.CacheWrite
		if r.Cost != nil {
			sum := *r.Cost
			if t.Cost != nil {
				sum += *t.Cost
			}
			t.Cost = &sum
		}
	}
	return roots, t
}

func loadAnnotations(ctx context.Context, db *sql.DB, session string, f SessionFilter) ([]traceAnnotation, error) {
	scope := ""
	if p := and(f.Opened("target_session_id"), f.Authors("author")); p != "" {
		scope = " AND " + p
	}
	rows, err := db.QueryContext(ctx, `SELECT annotation_id, coalesce(target_span_id, ''), name, value_number, value_label, value_text,
  source, author, epoch_ms(ts)
FROM annotations WHERE target_session_id = `+sqlQuote(session)+scope+` ORDER BY ts, annotation_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []traceAnnotation
	for rows.Next() {
		var n traceAnnotation
		var num sql.NullFloat64
		var label, text sql.NullString
		var ms int64
		if err := rows.Scan(&n.ID, &n.SpanID, &n.Name, &num, &label, &text, &n.Source, &n.Author, &ms); err != nil {
			return nil, err
		}
		switch {
		case num.Valid:
			n.Value = schema.Number(num.Float64)
		case label.Valid:
			n.Value = schema.Label(label.String)
		default:
			n.Value = schema.Text(text.String)
		}
		n.TS = formatRFC3339UTCMillis(ms)
		out = append(out, n)
	}
	return out, rows.Err()
}

func renderTrace(r traceResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Trace %s: %d span(s), tokens in %d / out %d / cache read %d / cache write %d",
		r.SessionID, r.Totals.Spans, r.Totals.Tokens.Input, r.Totals.Tokens.Output, r.Totals.Tokens.CacheRead, r.Totals.Tokens.CacheWrite)
	if r.Totals.Cost != nil {
		fmt.Fprintf(&b, ", cost $%g", *r.Totals.Cost)
	}
	b.WriteString(".\n")
	for _, n := range r.Annotations {
		writeNote(&b, 0, n)
	}
	for _, s := range r.Tree {
		writeSpan(&b, 0, s)
	}
	return strings.TrimRight(b.String(), "\n")
}

func writeSpan(b *strings.Builder, depth int, s *traceSpan) {
	fmt.Fprintf(b, "%s%s %s [%s] %s", strings.Repeat("  ", depth), s.Kind, s.Name, s.Status, s.Start)
	if s.Model != "" {
		fmt.Fprintf(b, " model=%s", s.Model)
	}
	fmt.Fprintf(b, " tokens in %d / out %d", s.Tokens.Input, s.Tokens.Output)
	if s.Cost != nil {
		fmt.Fprintf(b, " cost $%g", *s.Cost)
	}
	fmt.Fprintf(b, " span=%s\n", s.SpanID)
	for _, n := range s.Annotations {
		writeNote(b, depth+1, n)
	}
	for _, c := range s.Children {
		writeSpan(b, depth+1, c)
	}
}

func writeNote(b *strings.Builder, depth int, n traceAnnotation) {
	fmt.Fprintf(b, "%s@ %s=%s (%s, %s)\n", strings.Repeat("  ", depth), n.Name, valueText(n.Value), n.Source, n.Author)
}
