package query

// The search engine: BM25 candidates over all_postings/corpus/events plus
// hydration through trace, ported from upstream search.rs
// build_search_mcp_events_sql / build_search_mcp_event_details_sql /
// map_search_mcp_rows_to_hits / dedupe_mcp_search_rows. The df is counted
// from the query terms' postings and avgdl comes from corpus.

import (
	"context"
	"database/sql"
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

// searchMcpEvents ports search.rs search_mcp_events_impl minus the result
// cache (each cache file is one immutable snapshot; serve reopens per query).
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
		if err := hydrateMcpCandidates(ctx, db, rows); err != nil {
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
	matchedTerm int64
	eventUnixMs int64

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

// searchMcpCandidates ports build_search_mcp_events_sql: one ranked pass over
// the query terms' postings, scored with their df (postings per term across
// the whole corpus) and corpus.avg_doc_len, then filtered by the MCP
// source/event-type clauses on events.
func searchMcpCandidates(ctx context.Context, db *sql.DB, q searchQuery, terms []string, limit int) ([]*mcpRow, error) {
	termsSQL := "(" + strings.Join(quoteAll(terms), ", ") + ")"
	var filters []string
	filters = append(filters,
		"e.source_name != 'codex-mcp'",
		"NOT "+mcpToolNamesPredicate("e.tool_name"),
	)
	// events carries the raw lake column names; the trace renames
	// (event_kind->event_class, actor_kind->actor_role, tool_name->name)
	// exist only in the view, so the filter clauses bind the raw names.
	if len(q.eventTypes) > 0 {
		filters = append(filters,
			mcpEventTypeFilterClause("e.event_kind", "e.payload_type", "e.actor_kind", q.eventTypes))
	}
	if q.sessionOK {
		filters = append(filters, "e.session_id = "+sqlQuote(q.sessionID))
	}
	if q.harness != nil {
		filters = append(filters, "e.harness = "+sqlQuote(*q.harness))
	}
	if q.sourceName != nil {
		filters = append(filters, "e.source_name = "+sqlQuote(*q.sourceName))
	}
	if q.turnOK {
		// Upstream derives the turn's events from the navigation window; the
		// trace view carries the same turn_seq already.
		filters = append(filters, "e.event_uid IN (SELECT event_uid FROM trace WHERE session_id = "+
			sqlQuote(q.sessionID)+" AND turn_seq = "+sqlQuoteInt(int(q.turnSeq))+")")
	}

	sqlStr := `WITH p AS (SELECT term, doc, tf, doc_len FROM all_postings WHERE term IN ` + termsSQL + `),
s AS (SELECT term, count(*) AS df FROM p GROUP BY term)
SELECT event_uid, session_id, doc, raw_score, matched_terms, event_unix_ms FROM (
  SELECT e.event_uid AS event_uid,
    max(e.session_id) AS session_id,
    max(e.doc) AS doc,
    sum(ln(1.0 + ((greatest(cast((SELECT docs FROM corpus) AS DOUBLE), cast(s.df AS DOUBLE))
        - cast(s.df AS DOUBLE) + 0.5) / (cast(s.df AS DOUBLE) + 0.5)))
      * ((cast(p.tf AS DOUBLE) * 1.200000)
        / (cast(p.tf AS DOUBLE) + 1.200000 * (1.0 - 0.750000
          + 0.750000 * (cast(p.doc_len AS DOUBLE)
            / greatest((SELECT avg_doc_len FROM corpus), 1.0)))))) AS raw_score,
    count(DISTINCT p.term) AS matched_terms,
    coalesce(max(epoch_ms(e.event_ts)), 0) AS event_unix_ms
  FROM p
  JOIN s ON s.term = p.term
  JOIN events e ON e.doc = p.doc
  WHERE ` + strings.Join(filters, "\n    AND ") + `
  GROUP BY e.event_uid
  HAVING matched_terms >= ` + sqlQuoteInt(minShouldMatch) +
		` AND raw_score >= 0.000000
  ORDER BY raw_score DESC, event_unix_ms DESC, event_uid ASC
  LIMIT ` + sqlQuoteInt(limit) + `
) ORDER BY raw_score DESC, event_unix_ms DESC, event_uid ASC`

	rows, err := db.QueryContext(ctx, sqlStr)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*mcpRow
	for rows.Next() {
		var r mcpRow
		if err := rows.Scan(&r.eventUID, &r.sessionID, &r.doc, &r.rawScore, &r.matchedTerm,
			&r.eventUnixMs); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// scopeExistsFor ports the upstream scope_exists expression: only session and
// turn scopes can be missing; an unscoped search always has scope.
func scopeExistsFor(ctx context.Context, db *sql.DB, q searchQuery) (bool, error) {
	if !q.sessionOK {
		return true, nil
	}
	s := "SELECT count(*) FROM trace WHERE session_id = " + sqlQuote(q.sessionID)
	if q.turnOK {
		s += " AND turn_seq = " + sqlQuoteInt(int(q.turnSeq))
	}
	var n int64
	if err := db.QueryRowContext(ctx, s).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// hydrateMcpCandidates ports build_search_mcp_event_details_sql: navigation
// ordinals and turn/session totals for the candidates' turns, hydration from
// events. Session title/slug/summary stay empty, matching upstream.
func hydrateMcpCandidates(ctx context.Context, db *sql.DB, rows []*mcpRow) error {
	byUID := make(map[string]*mcpRow, len(rows))
	seen := map[string]bool{}
	var cand, tr []string
	for _, r := range rows {
		byUID[r.eventUID] = r
		cand = append(cand, `SELECT event_uid, session_id, source_ref, text_content,
  source_name, harness, inference_provider, endpoint_kind, model,
  event_kind AS event_class, payload_type, actor_kind AS actor_role, tool_name AS name,
  coalesce(nullif(tool_phase, ''), op_status) AS phase, tool_call_id AS call_id, item_id
FROM events WHERE doc = `+strconv.FormatInt(r.doc, 10))
		if !seen[r.sessionID] {
			seen[r.sessionID] = true
			tr = append(tr, "SELECT session_id, event_uid, turn_seq, event_order, event_time, payload_type FROM trace WHERE session_id = "+sqlQuote(r.sessionID))
		}
	}

	q2 := `WITH cand AS (` + strings.Join(cand, "\nUNION ALL ") + `),
tr AS (` + strings.Join(tr, "\nUNION ALL ") + `),
scope AS (
  SELECT DISTINCT t.session_id, t.turn_seq
  FROM tr t JOIN cand c ON t.session_id = c.session_id AND t.event_uid = c.event_uid
),
turn_rows AS (
  SELECT t.* FROM tr t
  JOIN scope s ON t.session_id = s.session_id AND t.turn_seq = s.turn_seq
),
turn_totals AS (
  SELECT session_id, turn_seq, count(*) AS turn_event_count,
         max(CASE WHEN payload_type = 'task_complete' THEN 1 ELSE 0 END) AS turn_completed
  FROM turn_rows GROUP BY session_id, turn_seq
),
turn_terminal AS (
  SELECT session_id, turn_seq, event_uid AS turn_terminal_event_uid FROM (
    SELECT session_id, turn_seq, event_uid,
           row_number() OVER (PARTITION BY session_id, turn_seq ORDER BY event_order DESC) AS rn
    FROM turn_rows
    WHERE payload_type IN ('task_complete', 'turn_aborted')
  ) WHERE rn = 1
),
sess AS (
  SELECT session_id, epoch_ms(min(event_time)) AS session_started_at_unix_ms,
         epoch_ms(max(event_time)) AS session_updated_at_unix_ms,
         max(CASE WHEN payload_type = 'task_complete' THEN 1 ELSE 0 END) AS session_completed
  FROM tr
  WHERE session_id IN (SELECT session_id FROM scope)
  GROUP BY session_id
),
ord AS (
  SELECT session_id, event_uid, event_order, event_time, turn_seq,
         row_number() OVER (PARTITION BY session_id, turn_seq ORDER BY event_order) AS event_ordinal
  FROM turn_rows
)
SELECT
  c.event_uid, c.session_id, o.event_order, o.turn_seq, o.event_ordinal,
  epoch_ms(o.event_time) AS event_unix_ms,
  c.event_class, c.payload_type, c.actor_role, c.name, c.phase, c.call_id, c.item_id,
  c.source_ref, c.text_content,
  c.source_name, c.harness, c.inference_provider, c.endpoint_kind, c.model,
  tt.turn_event_count, tt.turn_completed, tu.turn_terminal_event_uid,
  ss.session_started_at_unix_ms, ss.session_updated_at_unix_ms, ss.session_completed
FROM cand c
JOIN ord o ON o.session_id = c.session_id AND o.event_uid = c.event_uid
JOIN turn_totals tt ON tt.session_id = o.session_id AND tt.turn_seq = o.turn_seq
LEFT JOIN turn_terminal tu ON tu.session_id = o.session_id AND tu.turn_seq = o.turn_seq
JOIN sess ss ON ss.session_id = o.session_id`
	rs, err := db.QueryContext(ctx, q2)
	if err != nil {
		return err
	}
	defer rs.Close()
	for rs.Next() {
		var r mcpRow
		var turnCompleted, sessionCompleted int
		var turnTerminal any
		if err := rs.Scan(&r.eventUID, &r.sessionID, &r.eventOrder, &r.turnSeq, &r.eventOrdinal,
			&r.eventUnixMs,
			&r.eventClass, &r.payloadType, &r.actorRole, &r.name, &r.phase, &r.callID, &r.itemID,
			&r.sourceRef, &r.textContent,
			&r.sourceName, &r.harness, &r.inferenceProvider, &r.endpointKind, &r.model,
			&r.turnEventCount, &turnCompleted, &turnTerminal,
			&r.sessionStartedMs, &r.sessionUpdatedMs, &sessionCompleted); err != nil {
			return err
		}
		r.turnCompleted = turnCompleted != 0
		r.sessionCompleted = sessionCompleted != 0
		r.turnTerminalUID = jsonAsString(turnTerminal)
		// Upstream hydrates text_preview as leftUTF8(text_content, preview).
		r.textPreview = truncateRunes(r.textContent, previewChars)
		if existing, ok := byUID[r.eventUID]; ok {
			// The candidate pass supplied the ranking inputs; the hydration
			// pass must not wipe them.
			rawScore, matched := existing.rawScore, existing.matchedTerm
			*existing = r
			existing.rawScore, existing.matchedTerm = rawScore, matched
		}
	}
	return rs.Err()
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// sortMcpRows ports sort_search_mcp_event_rows.
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

// dedupeMcpRows ports dedupe_mcp_search_rows with mcp_search_rows_are_equivalent.
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
	sameContent := a.textContent == b.textContent
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

// parseMcpSourceRef splits "file:generation:line".
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

// mapMcpRowsToHits ports map_search_mcp_rows_to_hits: score is the row's share
// of the best raw score.
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
