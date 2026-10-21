package main

import (
	"encoding/binary"
	"hash/fnv"
	"math"
	"slices"
	"sort"
	"strings"
)

func (st *Store) termAt(i int) (string, uint64, uint64) {
	b := st.termsOff[i*12:]
	so := binary.LittleEndian.Uint32(b)
	po := binary.LittleEndian.Uint64(b[4:])
	se := binary.LittleEndian.Uint32(b[12:])
	pe := binary.LittleEndian.Uint64(b[16:])
	return string(st.terms[so:se]), po, pe
}

func (st *Store) postingsOf(term string) []byte {
	i := sort.Search(st.nTerms, func(i int) bool {
		t, _, _ := st.termAt(i)
		return t >= term
	})
	if i >= st.nTerms {
		return nil
	}
	t, po, pe := st.termAt(i)
	if t != term {
		return nil
	}
	return st.postings[po:pe]
}

func searchFlags(r *Row) int {
	class, payload, actor := r.EventKind, r.PayloadType, strings.ToLower(r.ActorKind)
	inSet := func(v string, set ...string) bool { return slices.Contains(set, v) }
	message := class == "message" || (class == "event_msg" && inSet(payload, "user_message", "agent_message", "message", "text", "event_msg"))
	nonSystem := actor != "system"
	bits := []bool{
		actor == "user" && message,
		actor == "assistant" && message,
		nonSystem && (class == "reasoning" || inSet(payload, "agent_reasoning", "reasoning", "thinking")),
		nonSystem && (class == "tool_call" || inSet(payload, "tool_use", "function_call", "custom_tool_call", "web_search_call")),
		nonSystem && (class == "tool_result" || inSet(payload, "tool_result", "function_call_output", "custom_tool_call_output", "search_results_received")),
		inSet(class, "compacted_raw", "summary") || inSet(payload, "compacted", "summary"),
		actor == "system" || inSet(class, "system", "progress", "file_history_snapshot") || inSet(payload, "system", "progress", "file-history-snapshot", "file_history_snapshot"),
		class == "queue_operation" || inSet(payload, "task_started", "task_complete", "turn_aborted", "item_completed", "queue-operation"),
	}
	f := 0
	for i, b := range bits {
		if b {
			f |= 1 << i
		}
	}
	if r.SourceName != "codex-mcp" && !sqlInternalTool(r.ToolName) {
		f |= flagSearchable
	}
	return f
}

// sqlInternalTool is mcpInternalToolPredicate: DuckDB trim strips spaces only.
func sqlInternalTool(name string) bool {
	n := strings.ToLower(strings.Trim(name, " "))
	if slices.Contains(mcpInternalToolNames, n) {
		return true
	}
	parts := strings.Split(n, "__")
	return len(parts) == 3 && parts[0] == "mcp" && (parts[1] == legacyMcpServerName || parts[1] == mcpServerName) &&
		slices.Contains(mcpInternalToolNames, parts[2])
}

func textHash(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return h.Sum64()
}

func (db *DB) search(q searchQuery) (*searchResult, error) {
	terms := tokenizeQuery(q.query, bm25MaxQueryTerms)
	if len(terms) == 0 {
		return nil, argErrorf("query has no searchable terms (tokens shorter than 2 characters are excluded)")
	}
	limit := q.nHits + 1
	res := &searchResult{scopeExists: true}
	parts := db.parts
	if q.sessionOK {
		st, ok := db.part(q.sessionID)
		parts = nil
		res.scopeExists = false
		if ok {
			parts = []*Store{st}
		}
	}
	sorted := slices.Clone(terms)
	slices.Sort(sorted)
	df := make([]float64, len(sorted))
	for _, st := range db.parts {
		for i, t := range sorted {
			if p := st.postingsOf(t); p != nil {
				n, _ := binary.Uvarint(p)
				df[i] += float64(n)
			}
		}
	}
	docs := float64(db.scoredDocs)
	avg := 0.0
	if db.scoredDocs > 0 {
		avg = float64(db.totalLen) / float64(db.scoredDocs)
	}
	idf := make([]float64, len(sorted))
	for i := range sorted {
		idf[i] = math.Log(1.0 + ((math.Max(docs, df[i]) - df[i] + 0.5) / (df[i] + 0.5)))
	}
	norm := math.Max(avg, 1.0)
	type hit struct {
		st    *Store
		d     int
		score float64
	}
	var hits []hit
	for _, st := range parts {
		acc, exists := st.score(q, sorted, idf, norm)
		res.scopeExists = res.scopeExists || exists
		for d, s := range acc {
			hits = append(hits, hit{st, d, s})
		}
	}
	if len(hits) == 0 {
		return res, nil
	}
	scores := make([]float64, len(hits))
	for i, h := range hits {
		scores[i] = h.score
	}
	cut := 0.0
	if len(scores) > limit {
		sort.Float64s(scores)
		cut = scores[len(scores)-limit]
	} else {
		cut = slices.Min(scores)
	}
	var rows []*mcpRow
	for _, h := range hits {
		if h.score < cut {
			continue
		}
		x, ok, err := h.st.hitRow(h.d, h.score)
		if err != nil {
			return nil, err
		}
		if ok {
			rows = append(rows, x)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.rawScore != b.rawScore {
			return a.rawScore > b.rawScore
		}
		if a.eventTsMs != b.eventTsMs {
			return a.eventTsMs > b.eventTsMs
		}
		return a.eventUID < b.eventUID
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	sortMcpRows(rows)
	rows = dedupeMcpRows(rows, limit)
	res.truncated = len(rows) > q.nHits
	if res.truncated {
		rows = rows[:q.nHits]
	}
	res.hits = mapMcpRowsToHits(rows)
	return res, nil
}

// score sums BM25 over this part's postings for the docs q's filters and
// scope pass, and reports whether q's session or turn scope exists here.
func (st *Store) score(q searchQuery, terms []string, idf []float64, norm float64) (map[int]float64, bool) {
	exists := true
	lo, hi := 0, st.meta.Docs
	if q.sessionOK {
		var ok bool
		lo, hi, ok = st.sessionRange(q.sessionID)
		if !ok {
			lo, hi = 0, 0
		}
		if q.turnOK {
			found := false
			for d := lo; d < hi; d++ {
				if st.turnSeq(d) == q.turnSeq {
					found = true
					break
				}
			}
			exists = found
		} else {
			exists = hi > lo
		}
	}
	mask := uint16(eventTypeMask(q.eventTypes))
	pass := func(d int) bool {
		if d < lo || d >= hi {
			return false
		}
		f := st.flags(d)
		if f&flagSearchable == 0 || (len(q.eventTypes) > 0 && f&mask == 0) {
			return false
		}
		if q.turnOK && st.turnSeq(d) != q.turnSeq {
			return false
		}
		if q.harness != nil && st.harnessOf(d) != *q.harness {
			return false
		}
		if q.sourceName != nil && st.sourceNameOf(d) != *q.sourceName {
			return false
		}
		return true
	}
	acc := map[int]float64{}
	for i, t := range terms {
		p := st.postingsOf(t)
		if p == nil {
			continue
		}
		n, k := binary.Uvarint(p)
		p = p[k:]
		d := 0
		for j := uint64(0); j < n; j++ {
			gap, k1 := binary.Uvarint(p)
			tf, k2 := binary.Uvarint(p[k1:])
			p = p[k1+k2:]
			d += int(gap)
			if !pass(d) {
				continue
			}
			tff := float64(tf)
			dl := float64(st.docLen(d))
			acc[d] += idf[i] * ((tff * 1.2) / (tff + 1.2*(0.25+0.75*(dl/norm))))
		}
	}
	return acc, exists
}

func (st *Store) hitRow(d int, s float64) (*mcpRow, bool, error) {
	r, th, err := st.hot(d)
	if err != nil {
		return nil, false, err
	}
	si := st.sessionOf(d)
	sess := st.session(si)
	x := &mcpRow{eventUID: r.UID, sessionID: r.Session, doc: int64(d), rawScore: s, eventTsMs: r.TsMs, eventUnixMs: timeMs(r)}
	x.sourceName, x.harness, x.inferenceProvider = r.SourceName, r.Harness, r.InfProv
	x.eventClass, x.payloadType, x.actorRole, x.phase = r.EventKind, r.PayloadType, r.ActorKind, r.Phase
	x.sourceRef, x.textContent, x.textHash = r.SourceRef, r.TextPreview, th
	x.textPreview = truncateRunes(r.TextPreview, previewChars)
	x.eventOrder = int64(d - sess.start + 1)
	x.turnSeq = st.turnSeq(d)
	x.eventOrdinal = st.ordinal(d)
	t, ok := st.turn(si, x.turnSeq)
	if !ok {
		return nil, false, nil
	}
	x.turnEventCount, x.turnCompleted = t.count, t.completed
	if t.terminal >= 0 {
		tr, _, err := st.hot(t.terminal)
		if err != nil {
			return nil, false, err
		}
		x.turnTerminalUID = tr.UID
	}
	x.sessionStartedMs, x.sessionUpdatedMs, x.sessionCompleted = sess.first, sess.last, sess.completed
	return x, true, nil
}
