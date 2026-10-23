package index

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/candyflipline/mnemonic/internal/ql"
	"github.com/candyflipline/mnemonic/internal/schema"
)

// qlSource implements ql.RowSource over the engine: it enumerates the rows of
// one relation that the caller may read (scope.go) inside from..to, the rows
// the old tool's temp views held (internal/query/sqltool.go sqlScopeDDL):
// events by event_ts, spans by start_time, sessions overlapping the range,
// turns and annotations of the sessions in scope. A session the caller may
// not list never reaches the evaluator, which no clause can change.
type qlSource struct {
	db       *DB
	f        SessionFilter
	from, to time.Time

	// spansOf and annotationsOf are the part sections; fields so a scratch
	// test can inject readers while lane U5S is not merged.
	spansOf       func(sessionID string) []schema.Span
	annotationsOf func(sessionID string) []schema.Annotation
}

func newQLSource(ctx context.Context, db *DB, from, to time.Time) (*qlSource, error) {
	f, err := Sessions(ctx, db)
	if err != nil {
		return nil, err
	}
	s := &qlSource{db: db, f: f, from: from, to: to, spansOf: db.sessionSpans, annotationsOf: db.sessionAnnotations}
	return s, nil
}

func (s *qlSource) Scan(ctx context.Context, req ql.ScanRequest, fn func(ql.Row) error) error {
	switch req.Relation {
	case "events":
		return s.scanEvents(req, fn)
	case "sessions":
		return s.scanSessions(req, fn)
	case "turns":
		return s.scanTurns(req, fn)
	case "spans":
		return s.scanSpans(req, fn)
	case "annotations":
		return s.scanAnnotations(req, fn)
	}
	return fmt.Errorf("ql: the source does not serve relation %s", req.Relation)
}

// lazyRow answers Get through one compiled cell per requested column, so the
// evaluator decides what a row costs (contract 3): a heavy column is decoded
// only when a predicate or an output asks for it.
type lazyRow struct {
	cells []func() (ql.Value, error)
}

func (r lazyRow) Get(i int) (ql.Value, error) { return r.cells[i]() }

// str and opt render a column: str for NOT NULL columns (the engine keeps ""
// for absent), null for the ones the old view could hold NULL.
func str(s string) (ql.Value, error)  { return ql.StringValue(s), nil }
func opt(s string) (ql.Value, error)  { return strPtrValue(s), nil }
func num(n int64) (ql.Value, error)   { return ql.IntValue(n), nil }
func flt(f float64) (ql.Value, error) { return ql.FloatValue(f), nil }

func strPtrValue(s string) ql.Value {
	if s == "" {
		return ql.Null()
	}
	return ql.StringValue(s)
}

func msValue(ms int64) (ql.Value, error) {
	if ms == 0 {
		return ql.Null(), nil
	}
	return ql.TimeValue(time.UnixMilli(ms)), nil
}

func usValue(us int64) (ql.Value, error) { return ql.TimeValue(time.UnixMicro(us)), nil }

// ---------------------------------------------------------------- events

// scanEvents walks every part's docs in stored order. req.Match narrows the
// walk through each part's postings when every hint word is a term the
// tokenizer could have (lowercase ASCII runs of 2..64); a shorter or
// non-ASCII word is no term, so narrowing there could skip a row whose words
// hold it, and the scan stays full (Run re-checks either way).
func (s *qlSource) scanEvents(req ql.ScanRequest, fn func(ql.Row) error) error {
	cells, err := eventCells(req.Columns)
	if err != nil {
		return err
	}
	lo, hi := s.from.UnixMilli(), s.to.UnixMilli()
	for _, st := range s.db.parts {
		cand, narrowed := s.candidates(st, req.Match)
		n := len(st.docs) / docWidth
		for d := 0; d < n; d++ {
			if narrowed && !cand[d] {
				continue
			}
			if !s.f.Listed(st.session(st.sessionOf(d)).id) {
				continue
			}
			r, err := st.row(d)
			if err != nil {
				return err
			}
			if r.TsMs < lo || r.TsMs >= hi {
				continue
			}
			if err := fn(eventRow{st: st, r: r, d: d, cells: cells}); err != nil {
				return err
			}
		}
	}
	return nil
}

type eventRow struct {
	st    *Part
	r     *Row
	d     int
	cells []func(*Part, *Row, int) (ql.Value, error)
}

func (e eventRow) Get(i int) (ql.Value, error) { return e.cells[i](e.st, e.r, e.d) }

func eventCells(cols []ql.Column) ([]func(*Part, *Row, int) (ql.Value, error), error) {
	out := make([]func(*Part, *Row, int) (ql.Value, error), len(cols))
	for i, c := range cols {
		g, ok := eventColumn[c.Name]
		if !ok {
			return nil, fmt.Errorf("ql: the source does not serve events column %s", c.Name)
		}
		out[i] = g
	}
	return out, nil
}

// eventColumn answers one events column from the light row, the doc section
// or the member source. model, request_id, trace_id, latency_ms and the
// token counts ride the light row since the sections lane extended it: the
// parse stamps them the way the old events table was filled, so the column is
// the parse's value, not a payload re-derivation. An absent value reads "", 0
// or [], never null — the old columns were NOT NULL (pinned against the
// oracle on the arena fixture).
var eventColumn = map[string]func(*Part, *Row, int) (ql.Value, error){
	"event_uid":              func(_ *Part, r *Row, _ int) (ql.Value, error) { return str(r.UID) },
	"session_id":             func(_ *Part, r *Row, _ int) (ql.Value, error) { return str(r.Session) },
	"source_name":            func(_ *Part, r *Row, _ int) (ql.Value, error) { return str(r.SourceName) },
	"harness":                func(_ *Part, r *Row, _ int) (ql.Value, error) { return str(r.Harness) },
	"inference_provider":     func(_ *Part, r *Row, _ int) (ql.Value, error) { return str(r.InfProv) },
	"source_file":            func(_ *Part, r *Row, _ int) (ql.Value, error) { return str(r.SourceFile) },
	"source_generation":      func(_ *Part, r *Row, _ int) (ql.Value, error) { return num(int64(r.Gen)) },
	"source_line_no":         func(_ *Part, r *Row, _ int) (ql.Value, error) { return num(int64(r.LineNo)) },
	"source_offset":          func(_ *Part, r *Row, _ int) (ql.Value, error) { return num(int64(r.Offset)) },
	"source_ref":             func(_ *Part, r *Row, _ int) (ql.Value, error) { return str(r.SourceRef) },
	"event_ts":               func(_ *Part, r *Row, _ int) (ql.Value, error) { return msValue(r.TsMs) },
	"event_kind":             func(_ *Part, r *Row, _ int) (ql.Value, error) { return str(r.EventKind) },
	"actor_kind":             func(_ *Part, r *Row, _ int) (ql.Value, error) { return str(r.ActorKind) },
	"payload_type":           func(_ *Part, r *Row, _ int) (ql.Value, error) { return str(r.PayloadType) },
	"turn_index":             func(_ *Part, r *Row, _ int) (ql.Value, error) { return num(int64(r.TurnIndex)) },
	"item_id":                func(_ *Part, r *Row, _ int) (ql.Value, error) { return str(r.ItemID) },
	"tool_call_id":           func(_ *Part, r *Row, _ int) (ql.Value, error) { return str(r.ToolCallID) },
	"tool_name":              func(_ *Part, r *Row, _ int) (ql.Value, error) { return str(r.ToolName) },
	"tool_phase":             func(_ *Part, r *Row, _ int) (ql.Value, error) { return str(r.ToolPhase) },
	"endpoint_kind":          func(_ *Part, r *Row, _ int) (ql.Value, error) { return str(r.EndpointKind) },
	"text_content":           func(_ *Part, r *Row, _ int) (ql.Value, error) { return str(r.Text) },
	"text_preview":           func(_ *Part, r *Row, _ int) (ql.Value, error) { return str(r.TextPreview) },
	"project_id":             func(_ *Part, r *Row, _ int) (ql.Value, error) { return str(r.ProjectID) },
	"repo_rel_path":          func(_ *Part, r *Row, _ int) (ql.Value, error) { return str(r.RepoRelPath) },
	"worktree_root":          func(_ *Part, r *Row, _ int) (ql.Value, error) { return str(r.WorktreeRoot) },
	"cwd":                    func(_ *Part, r *Row, _ int) (ql.Value, error) { return str(r.Cwd) },
	"event_version":          func(_ *Part, r *Row, _ int) (ql.Value, error) { return num(int64(r.Version)) },
	"author":                 authorColumn,
	"machine_id":             memberColumn(func(m member) string { return m.Machine }),
	"tool_io_input_preview":  func(_ *Part, r *Row, _ int) (ql.Value, error) { return str(r.TioInPrev) },
	"tool_io_output_preview": func(_ *Part, r *Row, _ int) (ql.Value, error) { return str(r.TioOutPrev) },
	"tool_io_project_id":     func(_ *Part, r *Row, _ int) (ql.Value, error) { return str(r.TioProj) },
	"tool_io_repo_rel_path":  func(_ *Part, r *Row, _ int) (ql.Value, error) { return str(r.TioRel) },
	"tool_io_worktree_root":  func(_ *Part, r *Row, _ int) (ql.Value, error) { return str(r.TioWT) },
	"event_time":             func(_ *Part, r *Row, _ int) (ql.Value, error) { return usValue(eventTimeUs(r)) },
	"event_order": func(st *Part, _ *Row, d int) (ql.Value, error) {
		return num(int64(d - st.session(st.sessionOf(d)).start + 1))
	},
	"turn_seq":            func(st *Part, _ *Row, d int) (ql.Value, error) { return num(int64(st.turnSeq(d))) },
	"event_ordinal":       func(st *Part, _ *Row, d int) (ql.Value, error) { return num(int64(st.ordinal(d))) },
	"payload_json":        heavyColumn(0),
	"token_usage_json":    heavyColumn(1),
	"model":               func(_ *Part, r *Row, _ int) (ql.Value, error) { return str(r.Model) },
	"request_id":          func(_ *Part, r *Row, _ int) (ql.Value, error) { return str(r.RequestID) },
	"trace_id":            func(_ *Part, r *Row, _ int) (ql.Value, error) { return str(r.TraceID) },
	"latency_ms":          func(_ *Part, r *Row, _ int) (ql.Value, error) { return num(int64(r.LatencyMs)) },
	"input_tokens":        func(_ *Part, r *Row, _ int) (ql.Value, error) { return num(int64(r.InputTokens)) },
	"output_tokens":       func(_ *Part, r *Row, _ int) (ql.Value, error) { return num(int64(r.OutputTokens)) },
	"content_types":       contentTypesCell,
	"token_usage_buckets": func(_ *Part, r *Row, _ int) (ql.Value, error) { return uintMap(r.Buckets) },
	"token_usage_native_units": func(_ *Part, r *Row, _ int) (ql.Value, error) {
		return floatMap(r.Native)
	},
}

func authorColumn(st *Part, _ *Row, d int) (ql.Value, error) {
	return str(st.meta.Members[st.member(d)].Author)
}

func memberColumn(get func(member) string) func(*Part, *Row, int) (ql.Value, error) {
	return func(st *Part, _ *Row, d int) (ql.Value, error) { return str(get(st.meta.Members[st.member(d)])) }
}

// heavyColumn is the heavy record's field i: 0 payload_json, 1
// token_usage_json.
func heavyColumn(i int) func(*Part, *Row, int) (ql.Value, error) {
	return func(st *Part, r *Row, _ int) (ql.Value, error) {
		p, t, err := st.heavyOf(r)
		if err != nil {
			return ql.Null(), err
		}
		return str([]string{p, t}[i])
	}
}

// lookupPath is the first key present in m, dotted keys descending objects.
func lookupPath(m map[string]json.RawMessage, keys []string) (json.RawMessage, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			return v, true
		}
	}
	for _, k := range keys {
		dot := strings.IndexByte(k, '.')
		if dot < 0 {
			continue
		}
		if v, ok := m[k[:dot]]; ok {
			var next map[string]json.RawMessage
			if json.Unmarshal(v, &next) == nil {
				if v, ok := lookupPath(next, []string{k[dot+1:]}); ok {
					return v, true
				}
			}
		}
	}
	return nil, false
}

// contentTypesCell is events.content_types, which the parsers filled at
// parse time two ways: shared.ExtractContentTypes over a message content
// array — message.content (claude-code scalar messages) or the item's own
// content (codex messages, whose heavy record is the item) — or the fixed
// stamp [payload_type] on claude-code block rows (text, tool_use,
// tool_result). The engine's light row does not carry the slice and row.go
// is not mine, so the column is re-derived from the row and the heavy
// record. Thinking blocks and every harness whose parse stamped another way
// (cursor kinds) read [] here where the old column held null; a difference
// listed, not hidden.
func contentTypesCell(st *Part, r *Row, _ int) (ql.Value, error) {
	if r.Harness == "claude-code" && r.PayloadType != "" &&
		(r.EventKind == "message" || r.EventKind == "tool_call" || r.EventKind == "tool_result") {
		if r.PayloadType != "message" {
			return ql.StringsValue([]string{r.PayloadType}), nil
		}
	}
	p, _, err := st.heavyOf(r)
	if err != nil {
		return ql.StringsValue(nil), err
	}
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(p), &m) != nil {
		return ql.StringsValue(nil), nil
	}
	bases := []string{"message"}
	if r.Harness == "codex" {
		bases = []string{"content"}
	}
	for _, base := range bases {
		v, ok := m[base]
		if !ok {
			continue
		}
		return extractContentTypes(v), nil
	}
	return ql.StringsValue(nil), nil
}

// extractContentTypes is shared.ExtractContentTypes over a content array:
// the distinct sorted non-empty type of every item.
func extractContentTypes(content json.RawMessage) ql.Value {
	var items []json.RawMessage
	if json.Unmarshal(content, &items) != nil {
		return ql.StringsValue(nil)
	}
	seen := map[string]bool{}
	var out []string
	for _, item := range items {
		var o map[string]json.RawMessage
		if json.Unmarshal(item, &o) != nil {
			continue
		}
		var t string
		if v, ok := o["type"]; ok && json.Unmarshal(v, &t) == nil && t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return ql.StringsValue(out)
}

func uintMap(s string) (ql.Value, error) {
	if s == "" {
		return ql.Null(), nil
	}
	var m map[string]uint64
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return ql.Null(), err
	}
	out := make(map[string]int64, len(m))
	for k, v := range m {
		out[k] = int64(v)
	}
	return ql.IntMapValue(out), nil
}

func floatMap(s string) (ql.Value, error) {
	if s == "" {
		return ql.Null(), nil
	}
	var m map[string]float64
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return ql.Null(), err
	}
	return ql.FloatMapValue(m), nil
}

// candidates intersects every hint's postings per part. It is a superset of
// the docs whose text_content holds every hint word, so skipping the rest
// keeps Run's re-check correct.
func (s *qlSource) candidates(st *Part, hints []ql.MatchHint) (map[int]bool, bool) {
	var cand map[int]bool
	for _, h := range hints {
		if h.Column != "text_content" {
			return nil, false
		}
		for _, w := range h.Words {
			if !isTerm(w) {
				return nil, false
			}
			docs, ok := st.docsOfTerm(w)
			if !ok {
				return map[int]bool{}, true
			}
			if cand == nil {
				cand = docs
				continue
			}
			for d := range cand {
				if !docs[d] {
					delete(cand, d)
				}
			}
		}
	}
	return cand, cand != nil
}

func isTerm(w string) bool {
	if len(w) < 2 || len(w) > 64 {
		return false
	}
	for i := 0; i < len(w); i++ {
		c := w[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

// docsOfTerm decodes a term's postings into its doc set; ok is false when the
// part holds no such term.
func (st *Part) docsOfTerm(term string) (map[int]bool, bool) {
	p := st.postingsOf(term)
	if p == nil {
		return nil, false
	}
	n, k := binary.Uvarint(p)
	if k <= 0 {
		return nil, false
	}
	p = p[k:]
	out := make(map[int]bool, n)
	d := 0
	for range n {
		gap, k1 := binary.Uvarint(p)
		_, k2 := binary.Uvarint(p[k1:])
		if k1 <= 0 || k2 <= 0 {
			return nil, false
		}
		d += int(gap)
		out[d] = true
		p = p[k1+k2:]
	}
	return out, true
}

// member is a doc's member source index (the doc record's src field).
func (st *Part) member(d int) int { return int(binary.LittleEndian.Uint16(st.docRec(d)[10:])) }

// -------------------------------------------------- sessions, turns

// eachSession calls fn for every session in the part sections the caller may
// list. overlap narrows to the sessions the old sessions view held.
func (s *qlSource) eachSession(overlap bool, fn func(st *Part, i int, info sessInfo) error) error {
	lo, hi := s.from.UnixMilli(), s.to.UnixMilli()
	for _, st := range s.db.parts {
		for i := 0; i < st.nSessions; i++ {
			info := st.session(i)
			if !s.f.Listed(info.id) {
				continue
			}
			if overlap && !(info.first < hi && info.last >= lo) {
				continue
			}
			if err := fn(st, i, info); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *qlSource) scanSessions(req ql.ScanRequest, fn func(ql.Row) error) error {
	cells, err := sessionCells(req.Columns)
	if err != nil {
		return err
	}
	return s.eachSession(true, func(st *Part, i int, info sessInfo) error {
		recs, err := st.lsRecs()
		if err != nil {
			return err
		}
		return fn(lazyRow{cells: bindCells(info, recs[i], cells)})
	})
}

func bindCells(info sessInfo, r lsRec, gs []func(sessInfo, lsRec) (ql.Value, error)) []func() (ql.Value, error) {
	out := make([]func() (ql.Value, error), len(gs))
	for i, g := range gs {
		v, err := g(info, r)
		out[i] = func() (ql.Value, error) { return v, err }
	}
	return out
}

var sessionColumn = map[string]func(sessInfo, lsRec) (ql.Value, error){
	"session_id":       func(i sessInfo, _ lsRec) (ql.Value, error) { return str(i.id) },
	"first_event_time": func(i sessInfo, _ lsRec) (ql.Value, error) { return msValue(i.first) },
	"last_event_time":  func(i sessInfo, _ lsRec) (ql.Value, error) { return msValue(i.last) },
	"total_turns":      func(_ sessInfo, r lsRec) (ql.Value, error) { return num(int64(r.TotalTurns)) },
	"total_events":     func(_ sessInfo, r lsRec) (ql.Value, error) { return num(r.TotalEvents) },
	// the sessions section's completed is max(payload_type = 'task_complete')
	// over the session, the old sessions table's derivation.
	"completed": func(i sessInfo, _ lsRec) (ql.Value, error) {
		c := 0
		if i.completed {
			c = 1
		}
		return num(int64(c))
	},
}

func sessionCells(cols []ql.Column) ([]func(sessInfo, lsRec) (ql.Value, error), error) {
	out := make([]func(sessInfo, lsRec) (ql.Value, error), len(cols))
	for i, c := range cols {
		g, ok := sessionColumn[c.Name]
		if !ok {
			return nil, fmt.Errorf("ql: the source does not serve sessions column %s", c.Name)
		}
		out[i] = g
	}
	return out, nil
}

// scanTurns walks the part's turns section; a turn's session must be in
// scope, which eachSession(overlap) answers by session index.
func (s *qlSource) scanTurns(req ql.ScanRequest, fn func(ql.Row) error) error {
	cells, err := turnCells(req.Columns)
	if err != nil {
		return err
	}
	return s.eachSession(true, func(st *Part, i int, info sessInfo) error {
		for t := 0; t < st.nTurns; t++ {
			b := st.turns[t*turnWidth:]
			if int(binary.LittleEndian.Uint32(b)) != i {
				continue
			}
			row := turnRow{st: st, b: b, cells: cells}
			if err := fn(row); err != nil {
				return err
			}
		}
		return nil
	})
}

type turnRow struct {
	st    *Part
	b     []byte
	cells []func(*Part, []byte) (ql.Value, error)
}

func (t turnRow) Get(i int) (ql.Value, error) { return t.cells[i](t.st, t.b) }

var turnColumn = map[string]func(*Part, []byte) (ql.Value, error){
	"session_id": func(st *Part, b []byte) (ql.Value, error) {
		return str(st.session(int(binary.LittleEndian.Uint32(b))).id)
	},
	"turn_seq": func(_ *Part, b []byte) (ql.Value, error) { return num(int64(binary.LittleEndian.Uint32(b[4:]))) },
	"event_count": func(_ *Part, b []byte) (ql.Value, error) {
		return num(int64(binary.LittleEndian.Uint32(b[8:])))
	},
	"completed": func(_ *Part, b []byte) (ql.Value, error) {
		return num(int64(binary.LittleEndian.Uint32(b[12:])))
	},
	"terminal_uid": func(st *Part, b []byte) (ql.Value, error) {
		doc := int(int64(binary.LittleEndian.Uint64(b[16:])))
		if doc < 0 {
			return ql.Null(), nil
		}
		r, err := st.row(doc)
		if err != nil {
			return ql.Null(), err
		}
		return str(r.UID)
	},
}

func turnCells(cols []ql.Column) ([]func(*Part, []byte) (ql.Value, error), error) {
	out := make([]func(*Part, []byte) (ql.Value, error), len(cols))
	for i, c := range cols {
		g, ok := turnColumn[c.Name]
		if !ok {
			return nil, fmt.Errorf("ql: the source does not serve turns column %s", c.Name)
		}
		out[i] = g
	}
	return out, nil
}

// -------------------------------------------------- spans, annotations

func (s *qlSource) scanSpans(req ql.ScanRequest, fn func(ql.Row) error) error {
	cells, err := spanCells(req.Columns)
	if err != nil {
		return err
	}
	return s.eachSession(false, func(_ *Part, _ int, info sessInfo) error {
		for _, sp := range s.spansOf(info.id) {
			if sp.Start.Before(s.from) || !sp.Start.Before(s.to) {
				continue
			}
			if err := fn(spanRow{sp: sp, cells: cells}); err != nil {
				return err
			}
		}
		return nil
	})
}

type spanRow struct {
	sp    schema.Span
	cells []func(schema.Span) (ql.Value, error)
}

func (r spanRow) Get(i int) (ql.Value, error) { return r.cells[i](r.sp) }

var spanColumn = map[string]func(schema.Span) (ql.Value, error){
	"session_id":        func(sp schema.Span) (ql.Value, error) { return str(sp.SessionID) },
	"span_id":           func(sp schema.Span) (ql.Value, error) { return str(sp.SpanID) },
	"parent_span_id":    func(sp schema.Span) (ql.Value, error) { return str(sp.ParentSpanID) },
	"kind":              func(sp schema.Span) (ql.Value, error) { return str(string(sp.Kind)) },
	"name":              func(sp schema.Span) (ql.Value, error) { return str(sp.Name) },
	"start_time":        func(sp schema.Span) (ql.Value, error) { return ql.TimeValue(sp.Start), nil },
	"end_time":          func(sp schema.Span) (ql.Value, error) { return ql.TimeValue(sp.End), nil },
	"status":            func(sp schema.Span) (ql.Value, error) { return str(string(sp.Status)) },
	"model":             func(sp schema.Span) (ql.Value, error) { return str(sp.Model) },
	"input_tokens":      func(sp schema.Span) (ql.Value, error) { return num(int64(sp.InputTokens)) },
	"output_tokens":     func(sp schema.Span) (ql.Value, error) { return num(int64(sp.OutputTokens)) },
	"cache_read_tokens": func(sp schema.Span) (ql.Value, error) { return num(int64(sp.CacheReadTokens)) },
	"cache_write_tokens": func(sp schema.Span) (ql.Value, error) {
		return num(int64(sp.CacheWriteTokens))
	},
	"cost": func(sp schema.Span) (ql.Value, error) {
		if sp.Cost == nil {
			return ql.Null(), nil
		}
		return flt(*sp.Cost)
	},
	"first_event_uid": func(sp schema.Span) (ql.Value, error) { return str(sp.FirstEventUID) },
	"last_event_uid":  func(sp schema.Span) (ql.Value, error) { return str(sp.LastEventUID) },
}

func spanCells(cols []ql.Column) ([]func(schema.Span) (ql.Value, error), error) {
	out := make([]func(schema.Span) (ql.Value, error), len(cols))
	for i, c := range cols {
		g, ok := spanColumn[c.Name]
		if !ok {
			return nil, fmt.Errorf("ql: the source does not serve spans column %s", c.Name)
		}
		out[i] = g
	}
	return out, nil
}

func (s *qlSource) scanAnnotations(req ql.ScanRequest, fn func(ql.Row) error) error {
	cells, err := annotationCells(req.Columns)
	if err != nil {
		return err
	}
	return s.eachSession(false, func(_ *Part, _ int, info sessInfo) error {
		for _, a := range s.annotationsOf(info.id) {
			if a.TS.Before(s.from) || !a.TS.Before(s.to) {
				continue
			}
			if !s.f.Author(a.Author) {
				continue
			}
			if err := fn(annotationRow{a: a, cells: cells}); err != nil {
				return err
			}
		}
		return nil
	})
}

type annotationRow struct {
	a     schema.Annotation
	cells []func(schema.Annotation) (ql.Value, error)
}

func (r annotationRow) Get(i int) (ql.Value, error) { return r.cells[i](r.a) }

var annotationColumn = map[string]func(schema.Annotation) (ql.Value, error){
	"annotation_id":     func(a schema.Annotation) (ql.Value, error) { return str(a.ID) },
	"target_session_id": func(a schema.Annotation) (ql.Value, error) { return str(a.Target.SessionID) },
	"target_span_id":    func(a schema.Annotation) (ql.Value, error) { return opt(a.Target.SpanID) },
	"name":              func(a schema.Annotation) (ql.Value, error) { return str(a.Name) },
	"value_number": func(a schema.Annotation) (ql.Value, error) {
		if n, ok := a.Value.Number(); ok {
			return flt(n)
		}
		return ql.Null(), nil
	},
	"value_label": func(a schema.Annotation) (ql.Value, error) {
		if s, ok := a.Value.Label(); ok {
			return str(s)
		}
		return ql.Null(), nil
	},
	"value_text": func(a schema.Annotation) (ql.Value, error) {
		if s, ok := a.Value.Text(); ok {
			return str(s)
		}
		return ql.Null(), nil
	},
	"source": func(a schema.Annotation) (ql.Value, error) { return str(string(a.Source)) },
	"author": func(a schema.Annotation) (ql.Value, error) { return str(a.Author) },
	"ts":     func(a schema.Annotation) (ql.Value, error) { return ql.TimeValue(a.TS), nil },
}

func annotationCells(cols []ql.Column) ([]func(schema.Annotation) (ql.Value, error), error) {
	out := make([]func(schema.Annotation) (ql.Value, error), len(cols))
	for i, c := range cols {
		g, ok := annotationColumn[c.Name]
		if !ok {
			return nil, fmt.Errorf("ql: the source does not serve annotations column %s", c.Name)
		}
		out[i] = g
	}
	return out, nil
}
