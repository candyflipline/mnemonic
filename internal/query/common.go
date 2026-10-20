package query

// Shared helpers for the query tools: MCP IDs, timestamps, tokenization and
// the event-type machinery, ported from upstream
// upstream mcp-core/src/contract.rs and
// upstream conversations/src/clickhouse_repo/helpers.rs. Owned by lane
// cache (search_sessions and open).

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	// Upstream RepoConfig defaults (upstream conversations domain.rs).
	bm25K1            = 1.2
	bm25B             = 0.75
	bm25MaxQueryTerms = 16
	minShouldMatch    = 1
	minScore          = 0.0
	previewChars      = 220
	// upstream mcp-core contract.rs.
	maxSnippetChars        = 600
	summaryPreviewChars    = 240
	summaryMaxTools        = 25
	summaryToolNameChars   = 120
	searchMaxQueryChars    = 4096
	openMaxResults         = 25
	encryptedReasoningText = "[encrypted reasoning omitted]"
)

// sqlQuote quotes a literal for DuckDB SQL.
func sqlQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func sqlQuoteInt(n int) string { return fmt.Sprintf("%d", n) }

// b64RawURL is the URL-safe no-pad alphabet upstream uses for MCP IDs.
var b64RawURL = base64.RawURLEncoding

// argErrorf builds a caller mistake with the upstream message text.
func argErrorf(format string, args ...any) *ArgError {
	return &ArgError{Msg: fmt.Sprintf(format, args...)}
}

// idKind discriminates a parsed McpId.
type idKind string

const (
	kindSession idKind = "session"
	kindTurn    idKind = "turn"
	kindEvent   idKind = "event"
)

// mcpID is one parsed session:/turn:/event: MCP ID (contract.rs McpId).
type mcpID struct {
	kind     idKind
	session  string
	turnSeq  uint32
	eventUID string
}

func (id mcpID) String() string {
	switch id.kind {
	case kindSession:
		return "session:" + b64RawURL.EncodeToString([]byte(id.session))
	case kindTurn:
		return "turn:" + b64RawURL.EncodeToString([]byte(id.session)) + ":" + fmt.Sprintf("%d", id.turnSeq)
	default:
		return "event:" + b64RawURL.EncodeToString([]byte(id.eventUID))
	}
}

// parseMcpID ports McpId::from_str; messages are upstream contract.rs.
func parseMcpID(input string) (mcpID, error) {
	switch {
	case strings.HasPrefix(input, "session:"):
		raw, err := decodeRawID("session_id", strings.TrimPrefix(input, "session:"))
		if err != nil {
			return mcpID{}, err
		}
		return mcpID{kind: kindSession, session: raw}, nil
	case strings.HasPrefix(input, "turn:"):
		payload := strings.TrimPrefix(input, "turn:")
		// Upstream rsplit_once: the last colon separates session from seq.
		idx := strings.LastIndex(payload, ":")
		if idx < 0 {
			return mcpID{}, argErrorf("turn ID must contain session and turn sequence")
		}
		raw, err := decodeRawID("session_id", payload[:idx])
		if err != nil {
			return mcpID{}, err
		}
		seq := payload[idx+1:]
		if seq == "" || len(seq) > 10 {
			return mcpID{}, argErrorf("turn sequence must be an unsigned integer")
		}
		var turnSeq uint64
		for _, c := range seq {
			if c < '0' || c > '9' {
				return mcpID{}, argErrorf("turn sequence must be an unsigned integer")
			}
			turnSeq = turnSeq*10 + uint64(c-'0')
		}
		if turnSeq > 0xFFFFFFFF {
			return mcpID{}, argErrorf("turn sequence must be an unsigned integer")
		}
		return mcpID{kind: kindTurn, session: raw, turnSeq: uint32(turnSeq)}, nil
	case strings.HasPrefix(input, "event:"):
		raw, err := decodeRawID("event_uid", strings.TrimPrefix(input, "event:"))
		if err != nil {
			return mcpID{}, err
		}
		return mcpID{kind: kindEvent, eventUID: raw}, nil
	default:
		return mcpID{}, argErrorf("ID must begin with session:, turn:, or event:")
	}
}

// decodeRawID ports contract.rs decode_raw_id.
func decodeRawID(field, encoded string) (string, error) {
	b, err := b64RawURL.DecodeString(encoded)
	if err != nil {
		return "", argErrorf("%s is not valid URL-safe base64", field)
	}
	raw := string(b)
	if !validUTF8(b) {
		return "", argErrorf("%s is not valid UTF-8", field)
	}
	if strings.TrimSpace(raw) == "" {
		return "", argErrorf("%s must be a non-empty string", field)
	}
	return raw, nil
}

func validUTF8(b []byte) bool {
	for len(b) > 0 {
		r, size := rune(b[0]), 1
		switch {
		case b[0]&0x80 == 0:
		case b[0]&0xE0 == 0xC0:
			r, size = rune(b[0]&0x1F), 2
		case b[0]&0xF0 == 0xE0:
			r, size = rune(b[0]&0x0F), 3
		case b[0]&0xF8 == 0xF0:
			r, size = rune(b[0]&0x07), 4
		default:
			return false
		}
		if len(b) < size {
			return false
		}
		for i := 1; i < size; i++ {
			if b[i]&0xC0 != 0x80 {
				return false
			}
			r = r<<6 | rune(b[i]&0x3F)
		}
		if (size == 1 && r > 0x7F) ||
			(size == 2 && r < 0x80) ||
			(size == 3 && r < 0x800) ||
			(size == 4 && r < 0x10000) ||
			r > 0x10FFFF || (r >= 0xD800 && r <= 0xDFFF) {
			return false
		}
		b = b[size:]
	}
	return true
}

// formatRFC3339UTCMillis ports contract.rs format_rfc3339_utc_millis.
func formatRFC3339UTCMillis(unixMs int64) string {
	return time.UnixMilli(unixMs).UTC().Format("2006-01-02T15:04:05.000Z")
}

// formatSessionID / formatTurnID / formatEventID port the Mcp*Id Display impls.
func formatSessionID(raw string) string {
	return "session:" + b64RawURL.EncodeToString([]byte(raw))
}

func formatTurnID(rawSession string, turnSeq uint32) string {
	return "turn:" + b64RawURL.EncodeToString([]byte(rawSession)) + ":" + fmt.Sprintf("%d", turnSeq)
}

func formatEventID(rawUID string) string {
	return "event:" + b64RawURL.EncodeToString([]byte(rawUID))
}

// tokenizeQuery ports helpers.rs tokenize_query: [A-Za-z0-9_]+ tokens,
// lowercased, 2..64 bytes, first-seen order, at most maxTerms.
func tokenizeQuery(text string, maxTerms int) []string {
	var order []string
	seen := map[string]bool{}
	for _, m := range tokenRe.FindAllString(text, -1) {
		token := strings.ToLower(m)
		if len(token) < 2 || len(token) > 64 {
			continue
		}
		if !seen[token] {
			seen[token] = true
			order = append(order, token)
		}
		if len(order) >= maxTerms {
			break
		}
	}
	return order
}

var tokenRe = regexp.MustCompile(`[A-Za-z0-9_]+`)

var safeValueRe = regexp.MustCompile(`^[A-Za-z0-9._:@/-]{1,256}$`)

// isSafeFilterValue gates session and event ids before they reach SQL.
func isSafeFilterValue(value string) bool { return safeValueRe.MatchString(value) }

// compactWhitespace joins split-with pieces with single spaces.
func compactWhitespace(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// compactTextLine ports helpers.rs compact_text_line (rune counts).
func compactTextLine(text string, maxChars int) string {
	compact := compactWhitespace(text)
	if len([]rune(compact)) <= maxChars {
		return compact
	}
	trimmed := string([]rune(compact)[:maxChars-3]) + "..."
	return trimmed
}

func looksTruncated(text string) bool { return strings.HasSuffix(text, "...") }

// compactOptionalLine ports open_v1.rs compact_optional_line.
func compactOptionalLine(text string, maxChars int) any {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	return compactTextLine(trimmed, maxChars)
}

// nonEmpty ports helpers.rs non_empty_string.
func nonEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// hasEncryptedContentField ports open_v1.rs has_encrypted_content_field.
func hasEncryptedContentField(text string) bool {
	t := strings.TrimLeft(text, " \t\n\r")
	if !strings.HasPrefix(t, "{") {
		return false
	}
	const field = `"encrypted_content"`
	for i := 0; ; {
		j := strings.Index(t[i:], field)
		if j < 0 {
			return false
		}
		i += j + len(field)
		if strings.HasPrefix(strings.TrimLeft(t[i:], " \t\n\r"), ":") {
			return true
		}
	}
}

// mcpEventType names, in upstream McpEventType declaration order.
var allEventTypes = []string{
	"user_input", "assistant_response", "reasoning", "tool_call", "tool_response",
	"compaction", "system", "runtime", "unknown",
}

// searchableEventTypes is McpEventType::searchable(), the canonical order the
// search filter is emitted in.
var searchableEventTypes = allEventTypes[:8]

// searchDefaults is McpEventType::search_defaults(): the effective filter when
// the caller omits event_types.
var searchDefaults = []string{"user_input", "assistant_response"}

func isSearchableEventType(name string) bool {
	for _, t := range searchableEventTypes {
		if t == name {
			return true
		}
	}
	return false
}

func isValidEventType(name string) bool {
	for _, t := range allEventTypes {
		if t == name {
			return true
		}
	}
	return false
}

// canonicalizeSearchEventTypeNames ports contract.rs of the same name: deduped
// in searchable order, rejecting unknown and non-searchable names.
func canonicalizeSearchEventTypeNames(names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, argErrorf("event_types must contain at least one event type")
	}
	selected := map[string]bool{}
	for _, raw := range names {
		if !isValidEventType(raw) || !isSearchableEventType(raw) {
			return nil, argErrorf("unsupported event type: %s", raw)
		}
		selected[raw] = true
	}
	canonical := make([]string, 0, len(selected))
	for _, t := range searchableEventTypes {
		if selected[t] {
			canonical = append(canonical, t)
		}
	}
	if len(canonical) == 0 {
		return nil, argErrorf("event_types must contain at least one event type")
	}
	return canonical, nil
}

func isMcpSystemEvent(class, payloadType, actor string) bool {
	return strings.EqualFold(actor, "system") ||
		class == "system" || class == "progress" || class == "file_history_snapshot" ||
		payloadType == "system" || payloadType == "progress" ||
		payloadType == "file-history-snapshot" || payloadType == "file_history_snapshot"
}

func isMcpRuntimeEvent(class, payloadType string) bool {
	if class == "queue_operation" {
		return true
	}
	switch payloadType {
	case "task_started", "task_complete", "turn_aborted", "item_completed", "queue-operation":
		return true
	}
	return false
}

func isMcpMessageEvent(class, payloadType string) bool {
	if class == "message" {
		return true
	}
	if class != "event_msg" {
		return false
	}
	switch payloadType {
	case "user_message", "agent_message", "message", "text", "event_msg":
		return true
	}
	return false
}

func isMcpReasoningEvent(class, payloadType string) bool {
	if class == "reasoning" {
		return true
	}
	switch payloadType {
	case "agent_reasoning", "reasoning", "thinking":
		return true
	}
	return false
}

func isMcpToolCallEvent(class, payloadType string) bool {
	if class == "tool_call" {
		return true
	}
	switch payloadType {
	case "tool_use", "function_call", "custom_tool_call", "web_search_call":
		return true
	}
	return false
}

func isMcpToolResponseEvent(class, payloadType string) bool {
	if class == "tool_result" {
		return true
	}
	switch payloadType {
	case "tool_result", "function_call_output", "custom_tool_call_output", "search_results_received":
		return true
	}
	return false
}

func isMcpCompactionEvent(class, payloadType string) bool {
	switch class {
	case "compacted_raw", "summary":
		return true
	}
	switch payloadType {
	case "compacted", "summary":
		return true
	}
	return false
}

// eventTypeFor ports helpers.rs mcp_event_type_for: the classification order
// is the observable contract.
func eventTypeFor(class, payloadType, actor string) string {
	systemActor := strings.EqualFold(actor, "system")
	switch {
	case strings.EqualFold(actor, "user") && isMcpMessageEvent(class, payloadType):
		return "user_input"
	case strings.EqualFold(actor, "assistant") && isMcpMessageEvent(class, payloadType):
		return "assistant_response"
	case !systemActor && isMcpReasoningEvent(class, payloadType):
		return "reasoning"
	case !systemActor && isMcpToolCallEvent(class, payloadType):
		return "tool_call"
	case !systemActor && isMcpToolResponseEvent(class, payloadType):
		return "tool_response"
	case isMcpCompactionEvent(class, payloadType):
		return "compaction"
	case isMcpRuntimeEvent(class, payloadType):
		return "runtime"
	case isMcpSystemEvent(class, payloadType, actor):
		return "system"
	default:
		return "unknown"
	}
}

// sqlClauses for one event type, ported from helpers.rs single_*_clause with
// lowerUTF8->lower and trimBoth->trim.
func mcpMessageEventClause(class, payload string) string {
	return "(" + class + " = 'message' OR (" + class + " = 'event_msg' AND " + payload + " IN ('user_message', 'agent_message', 'message', 'text', 'event_msg')))"
}

func mcpReasoningEventClause(class, payload string) string {
	return "(" + class + " = 'reasoning' OR " + payload + " IN ('agent_reasoning', 'reasoning', 'thinking'))"
}

func mcpToolCallEventClause(class, payload string) string {
	return "(" + class + " = 'tool_call' OR " + payload + " IN ('tool_use', 'function_call', 'custom_tool_call', 'web_search_call'))"
}

func mcpToolResponseEventClause(class, payload string) string {
	return "(" + class + " = 'tool_result' OR " + payload + " IN ('tool_result', 'function_call_output', 'custom_tool_call_output', 'search_results_received'))"
}

func mcpCompactionEventClause(class, payload string) string {
	return "(" + class + " IN ('compacted_raw', 'summary') OR " + payload + " IN ('compacted', 'summary'))"
}

func mcpRuntimeEventClause(class, payload string) string {
	return "(" + class + " = 'queue_operation' OR " + payload + " IN ('task_started', 'task_complete', 'turn_aborted', 'item_completed', 'queue-operation'))"
}

func mcpSystemEventClause(class, payload, actor string) string {
	return "(lower(" + actor + ") = 'system' OR " + class + " IN ('system', 'progress', 'file_history_snapshot') OR " + payload + " IN ('system', 'progress', 'file-history-snapshot', 'file_history_snapshot'))"
}

func singleMcpEventTypeClause(class, payload, actor, eventType string) string {
	nonSystemActor := "lower(" + actor + ") != 'system'"
	switch eventType {
	case "user_input":
		return "(lower(" + actor + ") = 'user' AND " + mcpMessageEventClause(class, payload) + ")"
	case "assistant_response":
		return "(lower(" + actor + ") = 'assistant' AND " + mcpMessageEventClause(class, payload) + ")"
	case "reasoning":
		return "(" + nonSystemActor + " AND " + mcpReasoningEventClause(class, payload) + ")"
	case "tool_call":
		return "(" + nonSystemActor + " AND " + mcpToolCallEventClause(class, payload) + ")"
	case "tool_response":
		return "(" + nonSystemActor + " AND " + mcpToolResponseEventClause(class, payload) + ")"
	case "compaction":
		return mcpCompactionEventClause(class, payload)
	case "system":
		return mcpSystemEventClause(class, payload, actor)
	case "runtime":
		return mcpRuntimeEventClause(class, payload)
	default: // unknown: NOT (any known type)
		known := make([]string, 0, 8)
		for _, t := range searchableEventTypes {
			known = append(known, singleMcpEventTypeClause(class, payload, actor, t))
		}
		return "NOT (" + strings.Join(known, " OR ") + ")"
	}
}

// mcpEventTypeFilterClause ports helpers.rs mcp_event_type_filter_clause.
func mcpEventTypeFilterClause(class, payload, actor string, eventTypes []string) string {
	clauses := make([]string, 0, len(eventTypes))
	for _, t := range eventTypes {
		clauses = append(clauses, singleMcpEventTypeClause(class, payload, actor, t))
	}
	if len(clauses) == 1 {
		return clauses[0]
	}
	return "(" + strings.Join(clauses, " OR ") + ")"
}

// mcpToolNamesPredicate ports the upstream clickhouse/src/mcp_tool_names.rs
// sql_predicate: the internal tool names, bare or mcp__<server>__-qualified
// (transcripts say either server name).
func mcpToolNamesPredicate(column string) string {
	const names = "'search', 'search_sessions', 'open', 'list_sessions', 'file_attention'"
	norm := "lower(trim(" + column + "))"
	parts := "str_split(" + norm + ", '__')"
	return "(" + norm + " IN (" + names + ") OR (len(" + parts + ") = 3 AND " +
		"list_extract(" + parts + ", 1) = 'mcp' AND list_extract(" + parts + ", 2) IN ('moraine', 'mnemonic') AND " +
		"list_extract(" + parts + ", 3) IN (" + names + ")))"
}

// queryRows runs q and scans each row into a []sql.RawBytes-style map keyed by
// the requested column names, in the order of cols.
func queryRows(ctx context.Context, db *sql.DB, q string, cols []string) ([]map[string]any, error) {
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	var out []map[string]any
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		m := make(map[string]any, len(cols))
		for i, c := range cols {
			v := vals[i]
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			m[c] = v
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// jsonAsString coerces a scanned SQL value to a string ("" for NULL).
func jsonAsString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return fmt.Sprintf("%v", v)
}

func jsonAsInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int32:
		return int64(n)
	case uint32:
		return int64(n)
	case uint64:
		return int64(n)
	case int:
		return int64(n)
	case float64:
		return int64(n)
	case []byte:
		var out int64
		fmt.Sscanf(string(n), "%d", &out)
		return out
	case nil:
		return 0
	default:
		return 0
	}
}

func jsonAsFloat64(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int64:
		return float64(n)
	case int32:
		return float64(n)
	case nil:
		return 0
	default:
		return 0
	}
}

// requestJSON returns the canonical request object the tools echo back; the
// mcp lane wraps it in the envelope.
func requestJSON(v map[string]any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
