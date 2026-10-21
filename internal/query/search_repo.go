package query

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
)

type searchQuery struct {
	query      string
	nHits      int
	sessionID  string
	sessionOK  bool
	turnSeq    uint32
	turnOK     bool
	eventTypes []string
	harness    *string
	sourceName *string
}

type searchHit struct {
	rank                   int
	eventUID               string
	sessionID              string
	eventType              string
	eventUnixMs            int64
	turnSeq                uint32
	turnOrdinal            uint32
	eventOrder             int64
	eventOrdinal           uint32
	turnEventCount         int64
	turnCompleted          bool
	turnTerminalEventUID   string
	sessionStartedAtUnixMs int64
	sessionUpdatedAtUnixMs int64
	sessionTitle           string
	sessionSummary         string
	sessionSlug            string
	sessionCompleted       bool
	sourceName             string
	harness                string
	inferenceProvider      string
	snippet                string
	snippetTruncated       bool
	score                  float64
	rawScore               float64
}

type searchResult struct {
	hits        []searchHit
	truncated   bool
	scopeExists bool
}

func searchMcpEvents(ctx context.Context, db *sql.DB, q searchQuery) (*searchResult, error) {
	terms := tokenizeQuery(q.query, bm25MaxQueryTerms)
	if len(terms) == 0 {
		return nil, argErrorf("query has no searchable terms (tokens shorter than 2 characters are excluded)")
	}

	fetchLimit := q.nHits + 1
	rows, err := searchMcpCandidates(ctx, db, q, terms, fetchLimit)
	if err != nil {
		return nil, err
	}
	scopeExists, err := scopeExistsFor(ctx, db, q)
	if err != nil {
		return nil, err
	}
	res := &searchResult{scopeExists: scopeExists}
	if len(rows) > 0 {
		if rows, err = hydrateMcpCandidates(ctx, db, rows, fetchLimit); err != nil {
			return nil, err
		}
		sortMcpRows(rows)
		rows = dedupeMcpRows(rows, fetchLimit)
	}
	res.truncated = len(rows) > q.nHits
	if res.truncated {
		rows = rows[:q.nHits]
	}
	res.hits = mapMcpRowsToHits(rows)
	return res, nil
}

type mcpRow struct {
	eventUID    string
	sessionID   string
	doc         int64
	rawScore    float64
	eventTsMs   int64
	eventUnixMs int64
	mcpDetail
}

type mcpDetail struct {
	sourceName        string
	harness           string
	inferenceProvider string
	endpointKind      string
	eventClass        string
	payloadType       string
	actorRole         string
	name              string
	phase             string
	sourceRef         string
	callID            string
	itemID            string
	model             string
	textPreview       string
	textContent       string
	textHash          uint64
	eventOrder        int64
	turnSeq           uint32
	eventOrdinal      uint32
	turnEventCount    int64
	turnCompleted     bool
	turnTerminalUID   string
	sessionStartedMs  int64
	sessionUpdatedMs  int64
	sessionCompleted  bool
}

const flagSearchable = 1 << 8

func SearchFlagsSQL() string {
	parts := make([]string, 0, len(searchableEventTypes)+1)
	for i, t := range searchableEventTypes {
		parts = append(parts, fmt.Sprintf("CASE WHEN coalesce(%s, false) THEN %d ELSE 0 END",
			singleMcpEventTypeClause("event_kind", "payload_type", "actor_kind", t), 1<<i))
	}
	parts = append(parts, fmt.Sprintf("CASE WHEN coalesce(source_name != 'codex-mcp' AND NOT %s, false) THEN %d ELSE 0 END",
		mcpInternalToolPredicate("tool_name"), flagSearchable))
	return "cast(" + strings.Join(parts, " + ") + " AS USMALLINT)"
}

func eventTypeMask(types []string) int {
	mask := 0
	for i, t := range searchableEventTypes {
		if slices.Contains(types, t) {
			mask |= 1 << i
		}
	}
	return mask
}

func searchMcpCandidates(ctx context.Context, db *sql.DB, q searchQuery, terms []string, limit int) ([]*mcpRow, error) {
	filters := []string{"(p.flags & " + strconv.Itoa(flagSearchable) + ") <> 0"}
	if len(q.eventTypes) > 0 {
		filters = append(filters, "(p.flags & "+strconv.Itoa(eventTypeMask(q.eventTypes))+") <> 0")
	}
	var scope []string
	if q.sessionOK {
		scope = append(scope, "session_id = "+sqlQuote(q.sessionID))
	}
	if q.turnOK {
		scope = append(scope, "turn_seq = "+strconv.Itoa(int(q.turnSeq)))
	}
	if q.harness != nil {
		scope = append(scope, "harness = "+sqlQuote(*q.harness))
	}
	if q.sourceName != nil {
		scope = append(scope, "source_name = "+sqlQuote(*q.sourceName))
	}
	if len(scope) > 0 {
		filters = append(filters, "p.doc IN (SELECT doc FROM events WHERE "+strings.Join(scope, " AND ")+")")
	}

	sorted := slices.Clone(terms)
	slices.Sort(sorted)
	perTerm := make([]string, len(sorted))
	total := make([]string, len(sorted))
	for i, t := range sorted {
		perTerm[i] = fmt.Sprintf("sum(x) FILTER (WHERE term = %s) AS t%d", sqlQuote(t), i)
		total[i] = fmt.Sprintf("coalesce(t%d, 0)", i)
	}

	sqlStr := `WITH p AS (SELECT term, doc, tf, doc_len, flags FROM postings WHERE term IN (` + strings.Join(quoteAll(terms), ", ") + `)),
s AS (SELECT term, count(*) AS df FROM p GROUP BY term),
k AS (SELECT cast(docs AS DOUBLE) AS docs,
             cast(coalesce(total_len / nullif(docs, 0), 0) AS DOUBLE) AS avg_doc_len FROM corpus),
x AS (
  SELECT p.doc, p.term,
    ln(1.0 + ((greatest(k.docs, cast(s.df AS DOUBLE))
        - cast(s.df AS DOUBLE) + 0.5) / (cast(s.df AS DOUBLE) + 0.5)))
      * ((cast(p.tf AS DOUBLE) * 1.200000)
        / (cast(p.tf AS DOUBLE) + 1.200000 * (1.0 - 0.750000
          + 0.750000 * (cast(p.doc_len AS DOUBLE)
            / greatest(k.avg_doc_len, 1.0))))) AS x
  FROM p JOIN s ON s.term = p.term CROSS JOIN k
  WHERE ` + strings.Join(filters, "\n    AND ") + `),
c AS MATERIALIZED (
  SELECT doc, ` + strings.Join(total, " + ") + ` AS raw_score
  FROM (SELECT doc, ` + strings.Join(perTerm, ", ") + ` FROM x GROUP BY doc)
  WHERE ` + strings.Join(total, " + ") + ` >= 0.000000)
SELECT doc, raw_score FROM c
WHERE raw_score >= (SELECT coalesce(min(raw_score), 0) FROM (SELECT raw_score FROM c ORDER BY raw_score DESC LIMIT ` + strconv.Itoa(limit) + `))`

	rows, err := db.QueryContext(ctx, sqlStr)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*mcpRow
	for rows.Next() {
		var r mcpRow
		if err := rows.Scan(&r.doc, &r.rawScore); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

func scopeExistsFor(ctx context.Context, db *sql.DB, q searchQuery) (bool, error) {
	if !q.sessionOK {
		return true, nil
	}
	s := "SELECT count(*) FROM events WHERE session_id = " + sqlQuote(q.sessionID)
	if q.turnOK {
		s += " AND turn_seq = " + strconv.Itoa(int(q.turnSeq))
	}
	var n int64
	if err := db.QueryRowContext(ctx, s).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// hydrateMcpCandidates: session title/slug/summary stay empty, matching
// upstream.
func hydrateMcpCandidates(ctx context.Context, db *sql.DB, rows []*mcpRow, limit int) ([]*mcpRow, error) {
	byDoc := make(map[int64]*mcpRow, len(rows))
	docs := make([]string, len(rows))
	for i, r := range rows {
		byDoc[r.doc] = r
		docs[i] = strconv.FormatInt(r.doc, 10)
	}
	rs, err := db.QueryContext(ctx, `WITH e AS MATERIALIZED (
  SELECT doc, event_uid, session_id,
    coalesce(epoch_ms(event_ts), 0) AS ts_ms, epoch_ms(event_time) AS time_ms,
    event_order, turn_seq, event_ordinal,
    event_kind, payload_type, actor_kind, tool_name, coalesce(nullif(tool_phase, ''), op_status) AS phase,
    tool_call_id, item_id, source_ref, text_preview, text_hash,
    source_name, harness, inference_provider, endpoint_kind, model
  FROM events WHERE rowid IN (SELECT rowid FROM events WHERE doc IN (`+strings.Join(docs, ", ")+`)))
SELECT e.*, t.event_count, t.completed, t.terminal_uid,
  epoch_ms(se.first_event_time), epoch_ms(se.last_event_time), se.completed
FROM e
JOIN turns t ON t.session_id = e.session_id AND t.turn_seq = e.turn_seq
JOIN sessions se ON se.session_id = e.session_id`)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []*mcpRow
	for rs.Next() {
		var doc int64
		var x mcpRow
		r := &x.mcpDetail
		var turnCompleted, sessionCompleted int
		var turnTerminal any
		if err := rs.Scan(&doc, &x.eventUID, &x.sessionID, &x.eventTsMs, &x.eventUnixMs,
			&r.eventOrder, &r.turnSeq, &r.eventOrdinal,
			&r.eventClass, &r.payloadType, &r.actorRole, &r.name, &r.phase,
			&r.callID, &r.itemID, &r.sourceRef, &r.textContent, &r.textHash,
			&r.sourceName, &r.harness, &r.inferenceProvider, &r.endpointKind, &r.model,
			&r.turnEventCount, &turnCompleted, &turnTerminal,
			&r.sessionStartedMs, &r.sessionUpdatedMs, &sessionCompleted); err != nil {
			return nil, err
		}
		r.turnCompleted = turnCompleted != 0
		r.sessionCompleted = sessionCompleted != 0
		r.turnTerminalUID = jsonAsString(turnTerminal)
		r.textPreview = truncateRunes(r.textContent, previewChars)
		x.doc, x.rawScore = doc, byDoc[doc].rawScore
		out = append(out, &x)
	}
	if err := rs.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.rawScore != b.rawScore {
			return a.rawScore > b.rawScore
		}
		if a.eventTsMs != b.eventTsMs {
			return a.eventTsMs > b.eventTsMs
		}
		return a.eventUID < b.eventUID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func sortMcpRows(rows []*mcpRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.rawScore != b.rawScore {
			return a.rawScore > b.rawScore
		}
		if a.eventUnixMs != b.eventUnixMs {
			return a.eventUnixMs > b.eventUnixMs
		}
		return a.eventUID < b.eventUID
	})
}

func dedupeMcpRows(rows []*mcpRow, limit int) []*mcpRow {
	var out []*mcpRow
	for _, row := range rows {
		dup := false
		for _, existing := range out {
			if mcpRowsAreEquivalent(existing, row) {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		out = append(out, row)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// mcpRowsAreEquivalent: same session, turn, event type and content. The
// codex-mirror extension (adjacent agent_message/message rows within 10 ms)
// rides on top, as upstream.
func mcpRowsAreEquivalent(a, b *mcpRow) bool {
	sameContent := a.textHash == b.textHash && a.textContent == b.textContent
	sameCoords := a.sessionID == b.sessionID && a.turnSeq == b.turnSeq &&
		eventTypeFor(a.eventClass, a.payloadType, a.actorRole) == eventTypeFor(b.eventClass, b.payloadType, b.actorRole) &&
		sameContent
	if !sameCoords {
		return false
	}
	return a.eventUnixMs == b.eventUnixMs || codexFinalAnswerMirrors(a, b)
}

const codexMirrorMaxDeltaMs = 10

func codexFinalAnswerMirrors(a, b *mcpRow) bool {
	pair := func(x, y *mcpRow) bool {
		return (x.eventClass == "message" && x.payloadType == "message" &&
			y.eventClass == "event_msg" && y.payloadType == "agent_message") ||
			(x.eventClass == "event_msg" && x.payloadType == "agent_message" &&
				y.eventClass == "message" && y.payloadType == "message")
	}
	isFinalAnswer := func(r *mcpRow) bool { return r.phase == "final_answer" }
	aRef, aOK := parseMcpSourceRef(a.sourceRef)
	bRef, bOK := parseMcpSourceRef(b.sourceRef)
	if !aOK || !bOK {
		return false
	}
	delta := a.eventUnixMs - b.eventUnixMs
	if delta < 0 {
		delta = -delta
	}
	orderDelta := a.eventOrder - b.eventOrder
	if orderDelta < 0 {
		orderDelta = -orderDelta
	}
	lineDelta := aRef.line - bRef.line
	if aRef.line < bRef.line {
		lineDelta = bRef.line - aRef.line
	}
	return a.harness == "codex" && b.harness == "codex" &&
		a.sourceName != "" && a.sourceName == b.sourceName &&
		pair(a, b) && isFinalAnswer(a) && isFinalAnswer(b) &&
		aRef.file == bRef.file && aRef.generation == bRef.generation &&
		lineDelta == 1 && orderDelta == 1 && delta <= codexMirrorMaxDeltaMs
}

type sourceRefParts struct {
	file       string
	generation uint64
	line       uint64
}

func parseMcpSourceRef(s string) (sourceRefParts, bool) {
	fileAndGen, line, ok1 := strings.Cut(s, ":")
	var parts sourceRefParts
	file, gen, ok2 := func() (string, string, bool) {
		i := strings.LastIndex(fileAndGen, ":")
		if i < 0 {
			return "", "", false
		}
		return fileAndGen[:i], fileAndGen[i+1:], true
	}()
	if !ok1 || !ok2 || file == "" || !allDigits(line) || !allDigits(gen) {
		return parts, false
	}
	parts.file = file
	parts.generation = parseUint(gen)
	parts.line = parseUint(line)
	return parts, true
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func parseUint(s string) uint64 {
	var n uint64
	for _, c := range s {
		n = n*10 + uint64(c-'0')
	}
	return n
}

func mapMcpRowsToHits(rows []*mcpRow) []searchHit {
	maxRaw := 0.0
	for _, r := range rows {
		if r.rawScore > maxRaw {
			maxRaw = r.rawScore
		}
	}
	hits := make([]searchHit, 0, len(rows))
	for i, r := range rows {
		score := 0.0
		if maxRaw > 0 {
			score = r.rawScore / maxRaw
			if score < 0 {
				score = 0
			}
			if score > 1 {
				score = 1
			}
		}
		snippet := r.textPreview
		snippetTruncated := len([]rune(r.textContent)) > len([]rune(r.textPreview)) && len([]rune(r.textPreview)) > 0
		if r.textPreview == "" {
			snippet = r.textContent
			snippetTruncated = false
		}
		hits = append(hits, searchHit{
			rank:                   i + 1,
			eventUID:               r.eventUID,
			sessionID:              r.sessionID,
			eventType:              eventTypeFor(r.eventClass, r.payloadType, r.actorRole),
			eventUnixMs:            r.eventUnixMs,
			turnSeq:                r.turnSeq,
			turnOrdinal:            r.turnSeq,
			eventOrder:             r.eventOrder,
			eventOrdinal:           r.eventOrdinal,
			turnEventCount:         r.turnEventCount,
			turnCompleted:          r.turnCompleted,
			turnTerminalEventUID:   r.turnTerminalUID,
			sessionStartedAtUnixMs: r.sessionStartedMs,
			sessionUpdatedAtUnixMs: r.sessionUpdatedMs,
			sessionCompleted:       r.sessionCompleted,
			sourceName:             r.sourceName,
			harness:                r.harness,
			inferenceProvider:      r.inferenceProvider,
			snippet:                snippet,
			snippetTruncated:       snippetTruncated,
			score:                  score,
			rawScore:               r.rawScore,
		})
	}
	return hits
}

func quoteAll(values []string) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = sqlQuote(v)
	}
	return out
}
