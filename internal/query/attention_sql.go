package query

import (
	"context"
	"database/sql"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// DuckDB ports of clickhouse_repo/file_attention.rs. The mcp_event_locator
// and mcp_event_navigation tables have no cache counterpart: the exact pass
// reads the canonical events columns directly, and both passes join the trace
// view for (event_time, event_order, turn_seq). The durable
// file_attention_project_roots mapping is likewise absent, so project scope
// runs on registered roots alone.

type faQuery struct {
	Rel string
	// NormalizedPID is the resolved project's identity (git:/dir: digest).
	NormalizedPID string
	// NormalizedRoots are the registered worktree roots for the resolved root.
	NormalizedRoots []string
	DeriveLegacy    bool
	ProjectScope    bool
	StartUnixMs     *int64
	EndUnixMs       *int64
	Tool            string
	Harness         string
	SourceName      string
	MutationsOnly   bool
	MaxRows         int
}

// faTouch ports FileAttentionTouch (rows.rs).
type faTouch struct {
	SessionID     string
	EventUID      string
	ToolCallID    string
	Harness       string
	SourceName    string
	ToolName      string
	ToolPhase     string
	MatchedPath   string
	MatchKind     string
	WorktreeRoot  string
	CWD           string
	EventUnixMs   *int64
	EventOrder    uint64
	TurnSeq       *uint32
	InputPreview  string
	OutputPreview string
}

const faReadToolNames = "'read', 'readfile', 'read_file', 'notebookread', 'notebook_read', 'view', 'cat', 'grep', 'rg', 'glob', 'ls', 'list', 'find'"

var faPathKeys = []string{
	"file_path", "notebook_path", "path", "target_file", "relativeWorkspacePath",
	"relative_workspace_path", "filepath", "file", "filename",
}

// faTouches ports file_attention_impl: the exact normalized-path pass, then
// the suffix pass, merged newest-first and capped at MaxRows+1.
func faTouches(ctx context.Context, db *sql.DB, q faQuery) ([]faTouch, error) {
	if q.Rel == "" {
		return nil, qErr("file_attention requires a non-empty path tail")
	}
	if q.ProjectScope && q.NormalizedPID == "" {
		return []faTouch{}, nil
	}
	limitPlus := q.MaxRows + 1
	touches, err := faExact(ctx, db, q, limitPlus)
	if err != nil {
		return nil, err
	}
	suffix, err := faSuffix(ctx, db, q, limitPlus)
	if err != nil {
		return nil, err
	}
	return faMergeTouches(append(touches, suffix...), limitPlus), nil
}

// faSinglePath ports single_path_sql: a value fit to attribute a worktree
// root from (no separators or shell metacharacters).
func faSinglePath(expr string) string {
	var b strings.Builder
	b.WriteString("(")
	b.WriteString(expr)
	b.WriteString(" != ''")
	for _, bad := range []string{"chr(0)", "chr(10)", "chr(13)", "';'", "'|'", "'&'", "'`'"} {
		b.WriteString(" AND strpos(")
		b.WriteString(expr)
		b.WriteString(", ")
		b.WriteString(bad)
		b.WriteString(") = 0")
	}
	b.WriteString(")")
	return b.String()
}

// faNav ports file_attention_navigation_source over the trace view, scoped to
// the already-matched events.
func faNav(scope string) string {
	return "nav AS (\n" +
		"  SELECT session_id, event_uid, event_time,\n" +
		"    cast(event_order AS UBIGINT) AS event_order,\n" +
		"    cast(turn_seq AS UBIGINT) AS turn_seq\n" +
		"  FROM trace\n" +
		"  WHERE (session_id, event_uid) IN (SELECT session_id, event_uid FROM " + scope + ")\n" +
		")"
}

// faFilters renders the shared request-side clauses over a scope prefix
// ("e." for events, "" for the matched subquery).
func faFilters(prefix string, q faQuery) []string {
	toolName := prefix + "tool_name"
	clauses := []string{
		prefix + "tool_phase = 'request'",
		"NOT (lower(" + toolName + ") = 'file_attention' OR ends_with(lower(" + toolName + "), '_file_attention'))",
	}
	if q.Tool != "" {
		clauses = append(clauses, "lower("+toolName+") = lower("+qQuote(q.Tool)+")")
	}
	if q.Harness != "" {
		clauses = append(clauses, prefix+"harness = "+qQuote(q.Harness))
	}
	if q.MutationsOnly {
		clauses = append(clauses, "lower("+toolName+") NOT IN ("+faReadToolNames+")")
	}
	return clauses
}

// faTimeClauses renders the outer start/end/source clauses over the joined
// trace row.
func faTimeClauses(q faQuery) []string {
	var outer []string
	if q.StartUnixMs != nil {
		outer = append(outer, "epoch_ms(tr.event_time) >= "+strconv.FormatInt(*q.StartUnixMs, 10))
	}
	if q.EndUnixMs != nil {
		outer = append(outer, "epoch_ms(tr.event_time) < "+strconv.FormatInt(*q.EndUnixMs, 10))
	}
	if q.SourceName != "" {
		outer = append(outer, "ti.source_name = "+qQuote(q.SourceName))
	}
	return outer
}

func faOuterWhere(q faQuery) string {
	if outer := faTimeClauses(q); len(outer) > 0 {
		return strings.Join(outer, "\n    AND ")
	}
	return "1"
}

// faExact ports file_attention_exact_impl.
func faExact(ctx context.Context, db *sql.DB, q faQuery, limitPlus int) ([]faTouch, error) {
	pred := "e.project_id != ''"
	if q.ProjectScope {
		pred = "e.project_id = " + qQuote(q.NormalizedPID)
	}
	clauses := append(faFilters("e.", q), pred+" AND e.repo_rel_path = "+qQuote(q.Rel))
	single := faSinglePath("ti.worktree_root")

	qry := "WITH\nmatched AS (\n" +
		"  SELECT e.session_id, e.event_uid, e.tool_call_id, e.harness, e.source_name,\n" +
		"    e.tool_name, e.tool_phase,\n" +
		"    coalesce(json_extract_string(json_extract(e.payload_json, '$.moraine_tool_io'), '$.input_preview'), '') AS input_preview,\n" +
		"    coalesce(json_extract_string(json_extract(e.payload_json, '$.moraine_tool_io'), '$.output_preview'), '') AS output_preview,\n" +
		"    e.repo_rel_path, e.worktree_root, e.cwd\n" +
		"  FROM events e\n" +
		"  WHERE " + strings.Join(clauses, "\n    AND ") + "\n" +
		"),\n" + faNav("matched") + "\n" +
		"SELECT\n" +
		"  ti.session_id, ti.event_uid, ti.tool_call_id, ti.harness, ti.source_name,\n" +
		"  ti.tool_name, ti.tool_phase,\n" +
		"  if(" + single + ", ti.worktree_root || '/' || ti.repo_rel_path, ti.repo_rel_path) AS matched_path,\n" +
		"  'path_suffix' AS match_kind,\n" +
		"  if(" + single + ", ti.worktree_root, '') AS worktree_root,\n" +
		"  ti.cwd,\n" +
		"  epoch_ms(tr.event_time) AS event_unix_ms,\n" +
		"  coalesce(tr.event_order, 0) AS event_order,\n" +
		"  tr.turn_seq,\n" +
		"  ti.input_preview, ti.output_preview\n" +
		"FROM matched ti\n" +
		"LEFT JOIN nav tr ON tr.session_id = ti.session_id AND tr.event_uid = ti.event_uid\n" +
		"WHERE " + faOuterWhere(q) + "\n" +
		"ORDER BY (event_unix_ms IS NULL) ASC, event_unix_ms DESC, event_order DESC, ti.event_uid DESC\n" +
		"LIMIT " + strconv.Itoa(limitPlus)
	return faScanTouches(ctx, db, qry)
}

// faJStr extracts a JSON string key the way ClickHouse JSONExtractString does:
// ” on a missing key or malformed JSON, never an error.
func faJStr(input, key string) string {
	return "CASE WHEN json_valid(" + input + ") THEN coalesce(json_extract_string(" + input + ", '$." + key + "'), '') ELSE '' END"
}

// faSuffix ports file_attention_suffix_impl on its non-indexed path: the
// payload moraine_tool_io block is matched by path key, nested JSON value, or
// shell command text.
func faSuffix(ctx context.Context, db *sql.DB, q faQuery, limitPlus int) ([]faTouch, error) {
	rel := q.Rel
	relQ := qQuote(rel)
	slashRelQ := qQuote("/" + rel)
	relRe := regexp.QuoteMeta(rel)
	slashRelRe := regexp.QuoteMeta("/" + rel)

	keyMatch := make([]string, 0, len(faPathKeys))
	for _, key := range faPathKeys {
		v := faJStr("ti.input_json", key)
		keyMatch = append(keyMatch, "(ends_with("+v+", "+slashRelQ+") OR "+v+" = "+relQ+")")
	}
	keyRe := strings.Join(faPathKeys, "|")
	nestedScalarRe := qQuote("\"(?:" + keyRe + ")\"[[:space:]]*:[[:space:]]*\"((?:[^\"\\\\]|\\\\.)*" + slashRelRe + "|" + relRe + ")\"")
	nestedArrayRe := qQuote("\"(?:" + keyRe + ")\"[[:space:]]*:[[:space:]]*\\[[^\\]]*\"((?:[^\"\\\\]|\\\\.)*" + slashRelRe + "|" + relRe + ")\"")
	nestedScalarExtract := "regexp_extract(ti.input_json, " + nestedScalarRe + ", 1)"
	nestedArrayExtract := "regexp_extract(ti.input_json, " + nestedArrayRe + ", 1)"
	nestedPathExpr := "ifnull(nullif(" + nestedScalarExtract + ", ''), " + nestedArrayExtract + ")"
	nestedMatch := "(" + nestedScalarExtract + " != '' OR " + nestedArrayExtract + " != '')"

	commandExpr := "CASE WHEN json_valid(ti.input_json)\n" +
		"    THEN CASE WHEN coalesce(json_extract_string(ti.input_json, '$.command'), '') != ''\n" +
		"      THEN json_extract_string(ti.input_json, '$.command')\n" +
		"      ELSE coalesce(json_extract_string(ti.input_json, '$.cmd'), '') END\n" +
		"    ELSE '' END"
	shellRe := qQuote("(^|[[:space:]'\"`=(])(/[^/][^[:space:]'\"`<>|;&),]*" + slashRelRe + "|/" + relRe + "|" + relRe + ")([[:space:]'\"`,;|&<>)]|$)")
	shellMatch := "(json_valid(ti.input_json) AND regexp_matches(" + commandExpr + ", " + shellRe + "))"

	toolEvents := "(SELECT\n" +
		"  e.session_id AS session_id, e.event_uid AS event_uid, e.tool_call_id AS tool_call_id,\n" +
		"  e.harness AS harness, e.tool_name AS tool_name, e.tool_phase AS tool_phase,\n" +
		"  coalesce(json_extract_string(json_extract(e.payload_json, '$.moraine_tool_io'), '$.input_json'), '') AS input_json,\n" +
		"  coalesce(json_extract_string(json_extract(e.payload_json, '$.moraine_tool_io'), '$.input_preview'), '') AS input_preview,\n" +
		"  coalesce(json_extract_string(json_extract(e.payload_json, '$.moraine_tool_io'), '$.output_preview'), '') AS output_preview,\n" +
		"  coalesce(json_extract_string(json_extract(e.payload_json, '$.moraine_tool_io'), '$.project_id'), '') AS project_id,\n" +
		"  coalesce(json_extract_string(json_extract(e.payload_json, '$.moraine_tool_io'), '$.repo_rel_path'), '') AS repo_rel_path,\n" +
		"  coalesce(json_extract_string(json_extract(e.payload_json, '$.moraine_tool_io'), '$.worktree_root'), '') AS worktree_root,\n" +
		"  e.source_name AS source_name, e.cwd AS cwd,\n" +
		"  e.project_id AS event_project_id, e.worktree_root AS event_worktree_root\n" +
		"FROM events e\n" +
		"WHERE json_extract(e.payload_json, '$.moraine_tool_io') IS NOT NULL)"

	inner := append(faFilters("", q),
		"("+strings.Join(keyMatch, "\n        OR ")+"\n        OR "+nestedMatch+"\n        OR "+shellMatch+")")

	scalarPaths := make([]string, 0, len(faPathKeys))
	for _, key := range faPathKeys {
		// json_valid-gated: rows with non-JSON input_json (free-text tool
		// input) match nothing but still flow through the CTE ladder.
		scalarPaths = append(scalarPaths, faJStr("ti.input_json", key))
	}
	legacyList := "[" + strings.Join(scalarPaths, ", ") + "]"
	legacyFirst := "coalesce(list_filter(" + legacyList + ", p -> ends_with(p, " + slashRelQ + ") OR p = " + relQ + ")[1], '')"

	provenanceRe := qQuote("\"(?:" + keyRe + "|command|cmd)\"[[:space:]]*:")
	provenanceCount := "len(regexp_extract_all(ti.input_json, " + provenanceRe + "))"
	legacyScalarSingle := faSinglePath("legacy_scalar_path")
	cwdSingle := faSinglePath("coalesce(ti.cwd, '')")
	var candidateRoot string
	if q.DeriveLegacy && !strings.HasPrefix(rel, "/") {
		candidateRoot = "CASE WHEN " + provenanceCount + " = 1\n" +
			"       AND len(list_filter(legacy_scalar_paths, p -> p != '')) = 1\n" +
			"       AND " + legacyScalarSingle + "\n" +
			"       AND " + cwdSingle + "\n" +
			"       AND (legacy_scalar_path = " + relQ + " OR legacy_scalar_path = concat(coalesce(ti.cwd, ''), '/', " + relQ + "))\n" +
			"     THEN coalesce(ti.cwd, '') ELSE '' END"
	} else {
		candidateRoot = "''"
	}

	normalizedRootCondition := "(ti.project_id != '' AND ti.repo_rel_path = " + relQ + " AND " + faSinglePath("ti.worktree_root") + ")"

	var outer []string
	withProjectRoots := ""
	verifiedColumn := ""
	legacyRoot := "legacy_candidate_root"
	if q.ProjectScope {
		roots := make([]string, 0, len(q.NormalizedRoots))
		for _, root := range q.NormalizedRoots {
			if root != "" {
				roots = append(roots, root)
			}
		}
		sort.SliceStable(roots, func(i, j int) bool { return len(roots[i]) > len(roots[j]) })
		roots = dedupAdjacent(roots)
		quoted := make([]string, len(roots))
		for i, root := range roots {
			quoted[i] = qQuote(root)
		}
		rootsLiteral := "[" + strings.Join(quoted, ", ") + "]"
		withProjectRoots = rootsLiteral + " AS project_roots,\n"
		verifiedColumn = ",\n  coalesce(list_filter(" + rootsLiteral + ", r -> legacy_candidate_root = r)[1], '') AS verified_project_root"
		eventRootExpr := "coalesce(ti.event_worktree_root, '')"
		legacyToolScope := "(ti.project_id != '' AND NOT starts_with(ti.project_id, 'git:') AND " +
			faSinglePath("ti.worktree_root") + " AND list_contains(" + rootsLiteral + ", ti.worktree_root))"
		legacyEventScope := "(ti.event_project_id != '' AND NOT starts_with(ti.event_project_id, 'git:') AND " +
			faSinglePath(eventRootExpr) + " AND list_contains(" + rootsLiteral + ", " + eventRootExpr + "))"
		outer = append(outer, "(ti.project_id = "+qQuote(q.NormalizedPID)+"\n"+
			"      OR "+legacyToolScope+"\n"+
			"      OR (ti.project_id = '' AND ti.event_project_id = "+qQuote(q.NormalizedPID)+")\n"+
			"      OR (ti.project_id = '' AND legacy_candidate_root != ''\n"+
			"          AND ("+legacyEventScope+" OR (ti.event_project_id = '' AND verified_project_root != ''))))")
		legacyRoot = "verified_project_root"
	}
	outer = append(outer, faTimeClauses(q)...)
	outerWhere := "1"
	if len(outer) > 0 {
		outerWhere = strings.Join(outer, "\n    AND ")
	}
	worktreeRootExpr := "CASE WHEN " + normalizedRootCondition + " THEN ti.worktree_root\n" +
		"  WHEN legacy_candidate_root != '' THEN " + legacyRoot + "\n" +
		"  ELSE '' END"

	qry := "WITH " + withProjectRoots + "matched AS (\n" +
		"  SELECT * FROM (" + toolEvents + ") AS ti\n" +
		"  WHERE " + strings.Join(inner, "\n    AND ") + "\n" +
		"),\n" +
		"paths AS (\n" +
		"  SELECT ti.*,\n" +
		"    " + legacyList + " AS legacy_scalar_paths\n" +
		"  FROM matched ti\n" +
		"),\n" +
		"paths2 AS (\n" +
		"  SELECT ti.*,\n" +
		"    " + legacyFirst + " AS legacy_scalar_path,\n" +
		"    " + nestedPathExpr + " AS nested_path\n" +
		"  FROM paths ti\n" +
		"),\n" +
		"annotated AS (\n" +
		"  SELECT ti.*,\n" +
		"    if(legacy_scalar_path != '', legacy_scalar_path, nested_path) AS matched_path,\n" +
		"    " + candidateRoot + " AS legacy_candidate_root\n" +
		"  FROM paths2 ti\n" +
		"),\n" +
		"scoped AS (\n" +
		"  SELECT ti.*,\n" +
		"    if(matched_path != '', 'path_suffix', 'shell_path') AS match_kind,\n" +
		"    " + worktreeRootExpr + " AS worktree_root_final" + verifiedColumn + "\n" +
		"  FROM annotated ti\n" +
		"),\n" + faNav("scoped") + "\n" +
		"SELECT\n" +
		"  ti.session_id, ti.event_uid, ti.tool_call_id, ti.harness, ti.source_name,\n" +
		"  ti.tool_name, ti.tool_phase,\n" +
		"  ti.matched_path, ti.match_kind, ti.worktree_root_final AS worktree_root, ti.cwd,\n" +
		"  epoch_ms(tr.event_time) AS event_unix_ms,\n" +
		"  coalesce(tr.event_order, 0) AS event_order,\n" +
		"  tr.turn_seq,\n" +
		"  ti.input_preview, ti.output_preview\n" +
		"FROM scoped ti\n" +
		"LEFT JOIN nav tr ON tr.session_id = ti.session_id AND tr.event_uid = ti.event_uid\n" +
		"WHERE " + outerWhere + "\n" +
		"ORDER BY (event_unix_ms IS NULL) ASC, event_unix_ms DESC, event_order DESC, ti.event_uid DESC\n" +
		"LIMIT " + strconv.Itoa(limitPlus)
	return faScanTouches(ctx, db, qry)
}

func faScanTouches(ctx context.Context, db *sql.DB, qry string) ([]faTouch, error) {
	rows, err := db.QueryContext(ctx, qry)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []faTouch{}
	for rows.Next() {
		var t faTouch
		var eventUnixMs sql.NullInt64
		var eventOrder uint64
		var turnSeq sql.NullInt64
		if err := rows.Scan(
			&t.SessionID, &t.EventUID, &t.ToolCallID, &t.Harness, &t.SourceName,
			&t.ToolName, &t.ToolPhase, &t.MatchedPath, &t.MatchKind, &t.WorktreeRoot, &t.CWD,
			&eventUnixMs, &eventOrder, &turnSeq, &t.InputPreview, &t.OutputPreview); err != nil {
			return nil, err
		}
		if eventUnixMs.Valid {
			ms := eventUnixMs.Int64
			t.EventUnixMs = &ms
		}
		t.EventOrder = eventOrder
		if turnSeq.Valid {
			seq := uint32(turnSeq.Int64)
			t.TurnSeq = &seq
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// faMergeTouches ports merge_file_attention_touches: dedupe on
// (session, call, uid), newest first, null timestamps last.
func faMergeTouches(touches []faTouch, limit int) []faTouch {
	seen := make(map[string]bool, len(touches))
	merged := make([]faTouch, 0, len(touches))
	for _, t := range touches {
		key := t.SessionID + "\x00" + t.ToolCallID + "\x00" + t.EventUID
		if !seen[key] {
			seen[key] = true
			merged = append(merged, t)
		}
	}
	sort.SliceStable(merged, func(i, j int) bool {
		return faTouchLess(&merged[i], &merged[j])
	})
	if len(merged) > limit {
		merged = merged[:limit]
	}
	return merged
}

func faTouchLess(a, b *faTouch) bool {
	switch {
	case a.EventUnixMs != nil && b.EventUnixMs != nil:
		if *a.EventUnixMs != *b.EventUnixMs {
			return *a.EventUnixMs > *b.EventUnixMs
		}
	case a.EventUnixMs != nil:
		return true
	case b.EventUnixMs != nil:
		return false
	}
	if a.EventOrder != b.EventOrder {
		return a.EventOrder > b.EventOrder
	}
	return a.EventUID > b.EventUID
}

func dedupAdjacent(sorted []string) []string {
	out := sorted[:0]
	for i, s := range sorted {
		if i == 0 || s != sorted[i-1] {
			out = append(out, s)
		}
	}
	return out
}
