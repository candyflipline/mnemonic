package query

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"time"
)

func lsModeAggregate() string {
	return "CASE\n" +
		"  WHEN count(*) FILTER (WHERE payload_type = 'web_search_call'\n" +
		"    OR payload_type = 'search_results_received'\n" +
		"    OR (payload_type = 'tool_use' AND tool_name IN ('WebSearch', 'WebFetch'))) > 0 THEN 'web_search'\n" +
		"  WHEN count(*) FILTER (WHERE source_name = 'codex-mcp' OR " + mcpInternalToolPredicate("tool_name") + ") > 0 THEN 'mcp_internal'\n" +
		"  WHEN count(*) FILTER (WHERE event_kind IN ('tool_call', 'tool_result') OR payload_type = 'tool_use') > 0 THEN 'tool_calling'\n" +
		"  ELSE 'chat' END"
}

// lsArgMax orders a trusted arg expression by (event_ts, event_uid), skipping
// NULL args the way ClickHouse argMax does.
func lsArgMax(arg string) string {
	return "arg_max(" + arg + ", " + lsArgMaxOrder + ") " +
		"FILTER (WHERE " + arg + " IS NOT NULL)"
}

const lsArgMaxOrder = "struct_pack(a := event_ts, b := event_uid)"

func lsArgMaxIf(arg, pred string) string {
	// One FILTER clause: DuckDB rejects two on the same aggregate.
	return "arg_max(" + arg + ", " + lsArgMaxOrder + ") FILTER (WHERE " + pred + " AND " + arg + " IS NOT NULL)"
}

func lsArgMinIf(arg, pred string) string {
	orderBy := "struct_pack(a := event_ts, b := source_name, c := source_file, d := source_generation, e := source_offset, f := source_line_no, g := event_uid)"
	return "arg_min(" + arg + ", " + orderBy + ") FILTER (WHERE " + pred + " AND " + arg + " IS NOT NULL)"
}

func lsSelectPage(ctx context.Context, db *sql.DB, a *lsArgs, cursor *lsCursor) ([]lsItem, error) {
	columns := []string{"session_id"}
	aggregates := ""
	having := []string{
		"epoch_ms(last_event_time) >= " + strconv.FormatInt(a.StartUnixMs, 10),
		"epoch_ms(first_event_time) < " + strconv.FormatInt(a.EndUnixMs, 10),
	}
	if a.Mode != nil {
		columns = append(columns, "event_kind", "payload_type", "tool_name", "source_name")
		aggregates += ",\n    " + lsModeAggregate() + " AS session_mode"
		having = append(having, "session_mode = "+sqlQuote(*a.Mode))
	}
	if a.Harness != nil || a.Source != nil {
		columns = append(columns, "event_ts", "event_uid")
	}
	if a.Harness != nil {
		columns = append(columns, "harness")
		aggregates += ",\n    " + lsArgMax("nullif(harness, '')") + " AS latest_harness"
		having = append(having, "latest_harness = "+sqlQuote(*a.Harness))
	}
	if a.Source != nil {
		if !contains(columns, "source_name") {
			columns = append(columns, "source_name")
		}
		aggregates += ",\n    " + lsArgMax("nullif(source_name, '')") + " AS latest_source_name"
		having = append(having, "latest_source_name = "+sqlQuote(*a.Source))
	}
	cmp := ">"
	if a.Sort == lsSortDesc {
		cmp = "<"
	}
	if cursor != nil {
		ms := strconv.FormatInt(cursor.LastEventUnixMs, 10)
		having = append(having, "(epoch_ms(last_event_time) "+cmp+" "+ms+
			" OR (epoch_ms(last_event_time) = "+ms+
			" AND session_id "+cmp+" "+sqlQuote(cursor.SessionID)+"))")
	}

	from := "sessions"
	if aggregates != "" {
		from = "sessions JOIN (\n" +
			"  SELECT session_id" + aggregates + "\n" +
			"  FROM (SELECT " + strings.Join(columns, ", ") + " FROM events WHERE trim(session_id) <> '') AS e\n" +
			"  GROUP BY session_id\n" +
			") AS p USING (session_id)"
	}
	q := "SELECT session_id, first_event_time, last_event_time\nFROM " + from +
		"\nWHERE trim(session_id) <> ''\n  AND " + strings.Join(having, "\n  AND ") +
		"\nORDER BY last_event_time " + dirSQL(a.Sort) + ", session_id " + dirSQL(a.Sort) +
		"\nLIMIT " + strconv.Itoa(a.Limit+1)

	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		var first, last time.Time
		if err := rows.Scan(&id, &first, &last); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return lsSummarizePage(ctx, db, ids, a.Sort)
}

func dirSQL(sort lsSort) string {
	if sort == lsSortAsc {
		return "ASC"
	}
	return "DESC"
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func lsSummarizePage(ctx context.Context, db *sql.DB, ids []string, sort lsSort) ([]lsItem, error) {
	idsSQL := qQuoteJoin(ids)
	modeAgg := lsModeAggregate()
	q := "WITH\n" +
		"page_sessions AS (\n" +
		"  SELECT session_id, first_event_time, last_event_time,\n" +
		"    epoch_ms(first_event_time) AS first_event_unix_ms,\n" +
		"    epoch_ms(last_event_time) AS last_event_unix_ms,\n" +
		"    total_turns, total_events\n" +
		"  FROM sessions WHERE session_id IN (" + idsSQL + ")\n" +
		"),\n" +
		"event_rollups AS (\n" +
		"  SELECT session_id,\n" +
		"    " + modeAgg + " AS mode,\n" +
		"    coalesce(max(turn_index) FILTER (WHERE payload_type IN ('task_complete', 'turn_aborted')), 0) AS latest_terminal_turn_seq,\n" +
		"    coalesce(" + lsArgMaxIf("payload_type", "payload_type IN ('task_complete', 'turn_aborted')") + ", '') AS latest_terminal_payload_type,\n" +
		"    coalesce(" + lsArgMax("nullif(harness, '')") + ", '') AS latest_harness,\n" +
		"    coalesce(" + lsArgMax("nullif(source_name, '')") + ", '') AS latest_source_name,\n" +
		"    coalesce(" + lsArgMinIf("nullif(trim(substring(text_preview, 1, 320)), '')",
		"harness = 'codex' AND actor_kind = 'user' AND event_kind = 'event_msg' AND payload_type = 'user_message'") + ", '') AS codex_user_message_preview\n" +
		"  FROM (\n" +
		"    SELECT session_id, event_ts, event_uid, event_kind, actor_kind, payload_type, tool_name,\n" +
		"      source_name, source_file, source_generation, source_offset, source_line_no,\n" +
		"      harness, turn_index, text_preview\n" +
		"    FROM events WHERE session_id IN (" + idsSQL + ")\n" +
		"  ) AS e\n" +
		"  GROUP BY session_id\n" +
		"),\n" +
		lsMetadataRollups(idsSQL) + ",\n" +
		"candidate_sessions AS (\n" +
		"  SELECT\n" +
		"    w.session_id AS session_id,\n" +
		"    w.first_event_time AS first_event_time,\n" +
		"    w.last_event_time AS last_event_time,\n" +
		"    w.first_event_unix_ms AS first_event_unix_ms,\n" +
		"    w.last_event_unix_ms AS last_event_unix_ms,\n" +
		"    w.total_turns AS total_turns,\n" +
		"    w.total_events AS total_events,\n" +
		"    coalesce(r.mode, 'chat') AS mode,\n" +
		"    (coalesce(r.latest_terminal_turn_seq, 0) = w.total_turns\n" +
		"     AND coalesce(r.latest_terminal_payload_type, '') = 'task_complete') AS completed,\n" +
		"    CASE WHEN coalesce(r.latest_source_name, '') = 'omp'\n" +
		"      THEN coalesce(nullif(x.latest_metadata_title, ''), nullif(x.latest_metadata_name, ''),\n" +
		"           nullif(x.latest_metadata_summary, ''), nullif(x.omp_dispatch_title, ''), '')\n" +
		"      ELSE coalesce(x.latest_session_meta_title, '') END AS title,\n" +
		"    coalesce(r.latest_source_name, '') AS source,\n" +
		"    coalesce(r.latest_harness, '') AS harness,\n" +
		"    coalesce(x.session_slug, '') AS session_slug,\n" +
		"    CASE WHEN coalesce(r.latest_source_name, '') = 'omp'\n" +
		"      THEN coalesce(nullif(x.latest_metadata_summary, ''), nullif(x.latest_metadata_title, ''),\n" +
		"           nullif(x.latest_metadata_name, ''), nullif(x.omp_dispatch_title, ''), '')\n" +
		"      ELSE coalesce(x.latest_session_meta_summary, '') END AS session_summary,\n" +
		"    coalesce(x.latest_explicit_title, '') AS explicit_title,\n" +
		"    CASE WHEN coalesce(r.latest_harness, '') = 'codex'\n" +
		"      THEN coalesce(r.codex_user_message_preview, '') ELSE '' END AS codex_user_message_preview\n" +
		"  FROM page_sessions AS w\n" +
		"  LEFT JOIN event_rollups AS r ON r.session_id = w.session_id\n" +
		"  LEFT JOIN metadata_rollups AS x ON x.session_id = w.session_id\n" +
		")\n" +
		"SELECT session_id, first_event_time, last_event_time, first_event_unix_ms, last_event_unix_ms,\n" +
		"  total_turns, total_events, mode, completed, title, source, harness,\n" +
		"  session_slug, session_summary, explicit_title, codex_user_message_preview\n" +
		"FROM candidate_sessions\n" +
		"ORDER BY last_event_unix_ms " + dirSQL(sort) + ", session_id " + dirSQL(sort)

	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []lsItem{}
	for rows.Next() {
		var item lsItem
		var first, last time.Time
		var title, source, harness, slug, summary, explicit, preview string
		var firstMs, lastMs int64
		if err := rows.Scan(&item.SessionID, &first, &last, &firstMs, &lastMs,
			&item.TotalTurns, &item.TotalEvents, &item.Mode, &item.Completed, &title, &source, &harness,
			&slug, &summary, &explicit, &preview); err != nil {
			return nil, err
		}
		item.FirstEventUnixMs, item.LastEventUnixMs = firstMs, lastMs
		item.FirstEventTime, item.LastEventTime = qFormatDateTime(first), qFormatDateTime(last)
		item.Title = strPtr(title)
		item.Source, item.Harness = strPtr(source), strPtr(harness)
		item.SessionSlug, item.SessionSummary = strPtr(slug), strPtr(summary)
		item.DisplayLabel = lsDisplayLabel(explicit, preview, title, summary, slug, harness, lsParseMode(item.Mode), item.LastEventTime, item.TotalTurns)
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func lsMetadataRollups(idsSQL string) string {
	str := func(expr string) string { return "nullif(json_extract_string(payload_json, '$." + expr + "'), '')" }
	title, name, summ := str("title"), str("name"), str("summary")
	slug := str("slug")
	meta := "event_kind = 'session_meta'"
	firstNonEmpty := func(label, pred, arg string) string {
		return "coalesce(" + lsArgMaxIf(arg, pred) + ", '') AS " + label
	}
	return "metadata_rollups AS (\n" +
		"  SELECT session_id,\n" +
		"    " + firstNonEmpty("latest_explicit_title",
		meta+" AND (trim(coalesce("+title+", '')) <> '' OR trim(coalesce("+name+", '')) <> '')",
		"coalesce("+title+", "+name+")") + ",\n" +
		"    " + firstNonEmpty("latest_session_meta_title", meta, "coalesce("+title+", "+name+", "+summ+")") + ",\n" +
		"    " + firstNonEmpty("latest_session_meta_summary", meta, "coalesce("+summ+", "+title+", "+name+")") + ",\n" +
		"    " + firstNonEmpty("latest_metadata_title",
		meta+" OR (source_name = 'omp' AND json_extract_string(payload_json, '$.type') IN ('title', 'title_change'))", title) + ",\n" +
		"    " + firstNonEmpty("latest_metadata_name", meta, name) + ",\n" +
		"    " + firstNonEmpty("latest_metadata_summary", meta, summ) + ",\n" +
		"    coalesce(arg_min(nullif(trim(regexp_replace(string_split(replace(source_file, '\\', '/'), '/')[-1], '[.]jsonl$', '')), ''),\n" +
		"      struct_pack(a := event_ts, b := source_name, c := source_file, d := source_generation,\n" +
		"                  e := source_offset, f := source_line_no, g := event_uid))\n" +
		"      FILTER (WHERE source_name = 'omp' AND session_id <> '' AND ends_with(source_file, '.jsonl')\n" +
		"        AND NOT ends_with(source_file, session_id || '.jsonl')\n" +
		"        AND nullif(trim(regexp_replace(string_split(replace(source_file, '\\', '/'), '/')[-1], '[.]jsonl$', '')), '') IS NOT NULL), '') AS omp_dispatch_title,\n" +
		"    " + firstNonEmpty("session_slug", meta, slug) + "\n" +
		"  FROM (\n" +
		"    SELECT session_id, event_ts, event_uid, event_kind, source_name, source_file,\n" +
		"      source_generation, source_offset, source_line_no, meta_json AS payload_json\n" +
		"    FROM events WHERE session_id IN (" + idsSQL + ")\n" +
		"      AND (event_kind = 'session_meta' OR source_name = 'omp')\n" +
		"  ) AS e\n" +
		"  GROUP BY session_id\n" +
		")"
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func qQuoteJoin(ids []string) string {
	quoted := make([]string, len(ids))
	for i, id := range ids {
		quoted[i] = sqlQuote(id)
	}
	return strings.Join(quoted, ", ")
}

func lsDisplayLabel(explicitTitle, userMessagePreview, wireTitle, summary, slug, harness, mode, updatedAt string, totalTurns int) string {
	if strings.TrimSpace(explicitTitle) != "" {
		return explicitTitle
	}
	if label := lsCompactPreview(userMessagePreview); label != "" {
		return label
	}
	for _, candidate := range []string{wireTitle, summary, slug} {
		if strings.TrimSpace(candidate) != "" {
			return candidate
		}
	}
	h := lsReadableHarness(harness)
	turns := strconv.Itoa(totalTurns) + " turns"
	if totalTurns == 1 {
		turns = "1 turn"
	}
	return h + ", " + lsReadableMode(mode) + ", " + strings.TrimSpace(updatedAt) + ", " + turns
}

func lsCompactPreview(preview string) string {
	trimmed := strings.TrimSpace(preview)
	if trimmed == "" {
		return ""
	}
	firstLine := strings.TrimSpace(strings.SplitN(trimmed, "\n", 2)[0])
	if firstLine == "" {
		return ""
	}
	runes := []rune(firstLine)
	if len(runes) <= 120 {
		return firstLine
	}
	return string(runes[:120]) + "…"
}

func lsReadableHarness(harness string) string {
	switch harness {
	case "codex":
		return "Codex"
	case "claude-code":
		return "Claude Code"
	case "cursor":
		return "Cursor"
	case "hermes":
		return "Hermes"
	case "kiro-cli":
		return "Kiro CLI"
	case "kimi-cli":
		return "Kimi CLI"
	case "nac":
		return "NAC"
	case "opencode":
		return "OpenCode"
	case "pi-coding-agent":
		return "Pi Coding Agent"
	case "prime-agent":
		return "Prime Agent"
	case "qwen-code":
		return "Qwen Code"
	}
	return harness
}

func lsReadableMode(mode string) string {
	switch mode {
	case "web_search":
		return "web search"
	case "tool_calling":
		return "tool session"
	case "mcp_internal":
		return "MCP session"
	case "chat":
		return "chat"
	}
	return "session"
}

func lsParseMode(raw string) string {
	switch raw {
	case "web_search", "mcp_internal", "tool_calling":
		return raw
	}
	return "chat"
}
