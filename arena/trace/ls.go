package trace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// lsRec is one session's list_sessions rollup, computed at build over the
// session's winners (internal/query/list_sql.go lsSummarizePage).
type lsRec struct {
	LastUs        int64  `json:"last_us"`
	TotalTurns    int    `json:"turns"`
	TotalEvents   int64  `json:"events"`
	Mode          string `json:"mode"`
	TermSeq       uint64 `json:"term_seq"`
	TermType      string `json:"term_type"`
	Harness       string `json:"harness"`
	Source        string `json:"source"`
	CodexPreview  string `json:"codex_preview"`
	ExplicitTitle string `json:"explicit_title"`
	MetaTitle     string `json:"meta_title"`
	MetaSummary   string `json:"meta_summary"`
	MdTitle       string `json:"md_title"`
	MdName        string `json:"md_name"`
	MdSummary     string `json:"md_summary"`
	Slug          string `json:"slug"`
}

// argMax keeps the value of the row latest by (event_ts, event_uid).
type argMax struct {
	ts  int64
	uid string
	v   string
	ok  bool
}

func (a *argMax) offer(r *Row, v string) {
	if !a.ok || r.TsMs > a.ts || (r.TsMs == a.ts && r.UID > a.uid) {
		*a = argMax{ts: r.TsMs, uid: r.UID, v: v, ok: true}
	}
}

func duckTrim(s string) string { return strings.Trim(s, " ") }

func duckSubstr(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := 0
	for k := range s {
		if i == n {
			return s[:k]
		}
		i++
	}
	return s
}

func lsAggregate(evs []built, turnSeqs []uint32, payload func(b built) string) lsRec {
	var rec lsRec
	var web, mcp, tool bool
	var term, harness, source argMax
	var explicit, metaTitle, metaSummary, mdTitle, mdName, mdSummary, slug argMax
	var preview *Row
	var previewV string
	for k, b := range evs {
		r := b.row
		rec.TotalEvents++
		rec.TotalTurns = max(rec.TotalTurns, int(turnSeqs[k]))
		if us := eventTimeUs(r); k == 0 || us > rec.LastUs {
			rec.LastUs = us
		}
		if r.PayloadType == "web_search_call" || r.PayloadType == "search_results_received" ||
			(r.PayloadType == "tool_use" && (r.ToolName == "WebSearch" || r.ToolName == "WebFetch")) {
			web = true
		}
		if r.SourceName == "codex-mcp" || sqlInternalTool(r.ToolName) {
			mcp = true
		}
		if r.EventKind == "tool_call" || r.EventKind == "tool_result" || r.PayloadType == "tool_use" {
			tool = true
		}
		if r.PayloadType == "task_complete" || r.PayloadType == "turn_aborted" {
			rec.TermSeq = max(rec.TermSeq, r.TurnIndex)
			term.offer(r, r.PayloadType)
		}
		if r.Harness != "" {
			harness.offer(r, r.Harness)
		}
		if r.SourceName != "" {
			source.offer(r, r.SourceName)
		}
		if r.Harness == "codex" && r.ActorKind == "user" && r.EventKind == "event_msg" && r.PayloadType == "user_message" {
			if v := duckTrim(duckSubstr(r.TextPreview, 320)); v != "" && (preview == nil || lsMinLess(r, preview)) {
				preview, previewV = r, v
			}
		}
		if r.Bits&bitMeta == 0 {
			continue
		}
		p := payload(b)
		title, name, summ := jsonTopString(p, "title"), jsonTopString(p, "name"), jsonTopString(p, "summary")
		meta := r.EventKind == "session_meta"
		if meta {
			if duckTrim(title) != "" || duckTrim(name) != "" {
				if v := firstNonEmpty(title, name); v != "" {
					explicit.offer(r, v)
				}
			}
			if v := firstNonEmpty(title, name, summ); v != "" {
				metaTitle.offer(r, v)
			}
			if v := firstNonEmpty(summ, title, name); v != "" {
				metaSummary.offer(r, v)
			}
			if name != "" {
				mdName.offer(r, name)
			}
			if summ != "" {
				mdSummary.offer(r, summ)
			}
			if s := jsonTopString(p, "slug"); s != "" {
				slug.offer(r, s)
			}
		}
		if title != "" {
			mdTitle.offer(r, title)
		}
	}
	switch {
	case web:
		rec.Mode = "web_search"
	case mcp:
		rec.Mode = "mcp_internal"
	case tool:
		rec.Mode = "tool_calling"
	default:
		rec.Mode = "chat"
	}
	rec.TermType, rec.Harness, rec.Source = term.v, harness.v, source.v
	rec.CodexPreview = previewV
	rec.ExplicitTitle, rec.MetaTitle, rec.MetaSummary = explicit.v, metaTitle.v, metaSummary.v
	rec.MdTitle, rec.MdName, rec.MdSummary, rec.Slug = mdTitle.v, mdName.v, mdSummary.v, slug.v
	return rec
}

// lsMinLess is the arg_min order (event_ts, source_name, source_file,
// source_generation, source_offset, source_line_no, event_uid).
func lsMinLess(a, b *Row) bool {
	switch {
	case a.TsMs != b.TsMs:
		return a.TsMs < b.TsMs
	case a.SourceName != b.SourceName:
		return a.SourceName < b.SourceName
	case a.SourceFile != b.SourceFile:
		return a.SourceFile < b.SourceFile
	case a.Gen != b.Gen:
		return a.Gen < b.Gen
	case a.Offset != b.Offset:
		return a.Offset < b.Offset
	case a.LineNo != b.LineNo:
		return a.LineNo < b.LineNo
	}
	return a.UID < b.UID
}

func writeLs(dir string, recs []lsRec) error {
	b, err := json.Marshal(recs)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "ls.json"), b, 0o644)
}

func (st *Part) lsRecs() ([]lsRec, error) {
	if st.ls != nil {
		return st.ls, nil
	}
	b, err := st.read("ls.json")
	if err != nil {
		return nil, err
	}
	var recs []lsRec
	if err := json.Unmarshal(b, &recs); err != nil {
		return nil, err
	}
	st.ls = recs
	return recs, nil
}

func (db *DB) lsSelectPage(a *lsArgs, cursor *lsCursor) ([]lsItem, error) {
	var recs []lsRec
	var infos []sessInfo
	for _, st := range db.parts {
		pr, err := st.lsRecs()
		if err != nil {
			return nil, err
		}
		recs = append(recs, pr...)
		for i := 0; i < st.nSessions; i++ {
			infos = append(infos, st.session(i))
		}
	}
	type cand struct {
		i    int
		info sessInfo
	}
	var cands []cand
	for i, s := range infos {
		r := recs[i]
		if duckTrim(s.id) == "" || s.last < a.StartUnixMs || s.first >= a.EndUnixMs {
			continue
		}
		if a.Mode != nil && r.Mode != *a.Mode {
			continue
		}
		if a.Harness != nil && r.Harness != *a.Harness {
			continue
		}
		if a.Source != nil && r.Source != *a.Source {
			continue
		}
		if cursor != nil {
			after := s.last > cursor.LastEventUnixMs || (s.last == cursor.LastEventUnixMs && s.id > cursor.SessionID)
			before := s.last < cursor.LastEventUnixMs || (s.last == cursor.LastEventUnixMs && s.id < cursor.SessionID)
			if (a.Sort == lsSortDesc && !before) || (a.Sort != lsSortDesc && !after) {
				continue
			}
		}
		cands = append(cands, cand{i, s})
	}
	desc := a.Sort == lsSortDesc
	less := func(x, y cand, us bool) bool {
		kx, ky := x.info.last, y.info.last
		if us {
			kx, ky = recs[x.i].LastUs, recs[y.i].LastUs
		}
		if kx != ky {
			return (kx < ky) != desc
		}
		return (x.info.id < y.info.id) != desc
	}
	sort.Slice(cands, func(i, j int) bool { return less(cands[i], cands[j], true) })
	if len(cands) > a.Limit+1 {
		cands = cands[:a.Limit+1]
	}
	sort.SliceStable(cands, func(i, j int) bool { return less(cands[i], cands[j], false) })
	out := make([]lsItem, 0, len(cands))
	for _, c := range cands {
		r, s := recs[c.i], c.info
		item := lsItem{SessionID: s.id, FirstEventUnixMs: s.first, LastEventUnixMs: s.last,
			TotalTurns: r.TotalTurns, TotalEvents: r.TotalEvents, Mode: r.Mode,
			Completed: int(r.TermSeq) == r.TotalTurns && r.TermType == "task_complete"}
		item.FirstEventTime, item.LastEventTime = qFormatMs(s.first), qFormatMs(s.last)
		title, summary := r.MetaTitle, r.MetaSummary
		if r.Source == "omp" {
			title = firstNonEmpty(r.MdTitle, r.MdName, r.MdSummary)
			summary = firstNonEmpty(r.MdSummary, r.MdTitle, r.MdName)
		}
		preview := ""
		if r.Harness == "codex" {
			preview = r.CodexPreview
		}
		item.Title = strPtr(title)
		item.Source, item.Harness = strPtr(r.Source), strPtr(r.Harness)
		item.SessionSlug, item.SessionSummary = strPtr(r.Slug), strPtr(summary)
		item.DisplayLabel = lsDisplayLabel(r.ExplicitTitle, preview, title, summary, r.Slug, r.Harness, lsParseMode(item.Mode), item.LastEventTime, item.TotalTurns)
		out = append(out, item)
	}
	return out, nil
}

func qFormatMs(ms int64) string { return qFormatDateTime(time.UnixMilli(ms)) }
