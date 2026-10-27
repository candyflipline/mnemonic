package index

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/candyflipline/mnemonic/internal/schema"
)

var traceBudget = 64 << 20

type getTrace struct{}

func init() { register(getTrace{}) }

func (getTrace) Name() string { return "get_trace" }

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

func (getTrace) Call(ctx context.Context, db *DB, raw json.RawMessage) (any, string, error) {
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
	if n := db.sessionSpansSize(id.session); n > traceBudget && f.Readable(id.session) {
		return nil, "", argErrorf("trace too large for get_trace: %d MB of spans (limit %d MB); export it with: mnemonic export otlp --data-dir <data dir> --session %s",
			(n+1<<20-1)>>20, traceBudget>>20, a.SessionID)
	}
	spans := loadSpans(db, id.session, f)
	res := traceResult{SessionID: a.SessionID, Tree: []*traceSpan{}}
	res.Tree, res.Totals = buildTree(spans)
	if a.IncludeAnnotations == nil || *a.IncludeAnnotations {
		notes := loadAnnotations(db, id.session, f)
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

func loadSpans(db *DB, session string, f SessionFilter) []*traceSpan {
	if !f.Readable(session) {
		return nil
	}
	return spansToTrace(db.sessionSpans(session))
}

func spansToTrace(spans []schema.Span) []*traceSpan {
	ordered := spans[:0:0]
	ordered = append(ordered, spans...)
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if !a.Start.Equal(b.Start) {
			return a.Start.Before(b.Start)
		}
		if ra, rb := spanKindRank(string(a.Kind)), spanKindRank(string(b.Kind)); ra != rb {
			return ra < rb
		}
		return a.SpanID < b.SpanID
	})
	var out []*traceSpan
	for _, s := range ordered {
		out = append(out, &traceSpan{
			SpanID:   s.SpanID,
			parent:   s.ParentSpanID,
			Kind:     string(s.Kind),
			Name:     s.Name,
			Start:    formatRFC3339UTCMillis(s.Start.UnixMilli()),
			End:      formatRFC3339UTCMillis(s.End.UnixMilli()),
			Status:   string(s.Status),
			Model:    s.Model,
			Tokens:   traceTokens{s.InputTokens, s.OutputTokens, s.CacheReadTokens, s.CacheWriteTokens},
			Cost:     s.Cost,
			Children: []*traceSpan{},
		})
	}
	return out
}

func spanKindRank(kind string) int {
	switch schema.SpanKind(kind) {
	case schema.SpanInvokeAgent:
		return 0
	case schema.SpanChat:
		return 1
	case schema.SpanExecuteTool:
		return 2
	case schema.SpanTurn:
		return 3
	}
	return 4
}

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
		r := cycleMember(s, byID)
		p := byID[r.parent]
		kids[p] = slices.DeleteFunc(kids[p], func(c *traceSpan) bool { return c == r })
		roots = append(roots, r)
		walk(r)
	}
	return roots, rootTotals(len(spans), roots)
}

func cycleMember(from *traceSpan, byID map[string]*traceSpan) *traceSpan {
	seen := map[*traceSpan]bool{}
	r := from
	for !seen[r] {
		seen[r] = true
		r = byID[r.parent]
	}
	return r
}

// rootTotals sums only the roots: each root's tokens and cost already include
// everything beneath it.
func rootTotals(spans int, roots []*traceSpan) traceTotals {
	t := traceTotals{Spans: spans}
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
	return t
}

func loadAnnotations(db *DB, session string, f SessionFilter) []traceAnnotation {
	if !f.Readable(session) {
		return nil
	}
	return notesToTrace(db.sessionAnnotations(session), f)
}

func notesToTrace(notes []schema.Annotation, f SessionFilter) []traceAnnotation {
	kept := notes[:0:0]
	for _, n := range notes {
		if f.Author(n.Author) {
			kept = append(kept, n)
		}
	}
	sort.SliceStable(kept, func(i, j int) bool {
		if !kept[i].TS.Equal(kept[j].TS) {
			return kept[i].TS.Before(kept[j].TS)
		}
		return kept[i].ID < kept[j].ID
	})
	var out []traceAnnotation
	for _, n := range kept {
		out = append(out, traceAnnotation{
			ID:     n.ID,
			SpanID: n.Target.SpanID,
			Name:   n.Name,
			Value:  n.Value,
			Source: n.Source,
			Author: n.Author,
			TS:     formatRFC3339UTCMillis(n.TS.UnixMilli()),
		})
	}
	return out
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
