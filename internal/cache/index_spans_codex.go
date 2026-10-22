package cache

func init() {
	RegisterSpans("codex", flatSpans("codex", `CASE WHEN payload_type = 'token_count' THEN event_uid END`))
}

// flatSpans derives the tree for harnesses whose rows name no subagent: the
// session is the root invoke_agent, each turn_seq a turn under it, each
// chatKey a chat under its turn, and each tool_call_id an execute_tool under
// the chat whose chatKey its call row carries, else under its turn. chatKey
// is a SQL expression over events that is NULL for rows outside a chat; a
// harness that records no model call boundary passes NULL and gets no chats.
// A chat's tokens are the max over its rows, because a provider reports one
// usage per call; root and turns carry the sum over the chats below them.
func flatSpans(root, chatKey string) SpanDerivation {
	return func(where string) string {
		return `WITH ev AS (
  SELECT session_id, event_uid, event_order, event_time, turn_seq, nullif(` + chatKey + `, '') AS ckey,
    nullif(tool_call_id, '') AS call, tool_name, tool_error,
    model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens
  FROM events WHERE ` + where + `),
root AS (
  SELECT session_id, ` + spanID("'session'", "session_id") + ` AS span_id, ` + spanBounds + `
  FROM ev GROUP BY session_id),
turn AS (
  SELECT session_id, turn_seq, ` + spanID("'turn'", "session_id", "cast(turn_seq AS VARCHAR)") + ` AS span_id, ` + spanBounds + `
  FROM ev GROUP BY session_id, turn_seq),
chat AS (
  SELECT session_id, ckey, ` + spanID("'chat'", "session_id", "ckey") + ` AS span_id,
    max(model) AS model, min(turn_seq) AS turn_seq,
    max(input_tokens) AS input_tokens, max(output_tokens) AS output_tokens,
    max(cache_read_tokens) AS cache_read_tokens, max(cache_write_tokens) AS cache_write_tokens, ` + spanBounds + `
  FROM ev WHERE ckey IS NOT NULL GROUP BY session_id, ckey),
tool AS (
  SELECT session_id, call, ` + spanID("'tool'", "session_id", "call") + ` AS span_id,
    max(tool_name) AS name, max(tool_error) AS tool_error, max(ckey) AS ckey,
    min(turn_seq) AS turn_seq, ` + spanBounds + `
  FROM ev WHERE call IS NOT NULL GROUP BY session_id, call),
chat_parent AS (
  SELECT c.*, t.span_id AS parent_span_id
  FROM chat c JOIN turn t ON t.session_id = c.session_id AND t.turn_seq = c.turn_seq),
tokens AS (
  SELECT key, sum(input_tokens) AS input_tokens, sum(output_tokens) AS output_tokens,
    sum(cache_read_tokens) AS cache_read_tokens, sum(cache_write_tokens) AS cache_write_tokens
  FROM (
    SELECT r.span_id AS key, c.* FROM chat c JOIN root r USING (session_id)
    UNION ALL SELECT parent_span_id AS key, * EXCLUDE (parent_span_id) FROM chat_parent)
  GROUP BY key)
SELECT r.session_id, r.span_id, '' AS parent_span_id, 'invoke_agent' AS kind, ` + sqlStr(root) + ` AS name,
  r.start_time, r.end_time, 'ok' AS status, '' AS model,
  coalesce(k.input_tokens, 0) AS input_tokens, coalesce(k.output_tokens, 0) AS output_tokens,
  coalesce(k.cache_read_tokens, 0) AS cache_read_tokens, coalesce(k.cache_write_tokens, 0) AS cache_write_tokens,
  NULL::DOUBLE AS cost, r.first_event_uid, r.last_event_uid
FROM root r LEFT JOIN tokens k ON k.key = r.span_id
UNION ALL BY NAME
SELECT t.session_id, t.span_id, r.span_id AS parent_span_id, 'turn' AS kind, 'turn ' || t.turn_seq AS name,
  t.start_time, t.end_time, 'ok' AS status, '' AS model,
  coalesce(k.input_tokens, 0) AS input_tokens, coalesce(k.output_tokens, 0) AS output_tokens,
  coalesce(k.cache_read_tokens, 0) AS cache_read_tokens, coalesce(k.cache_write_tokens, 0) AS cache_write_tokens,
  NULL::DOUBLE AS cost, t.first_event_uid, t.last_event_uid
FROM turn t JOIN root r USING (session_id)
LEFT JOIN tokens k ON k.key = t.span_id
UNION ALL BY NAME
SELECT session_id, span_id, parent_span_id, 'chat' AS kind, coalesce(nullif(model, ''), 'chat') AS name,
  start_time, end_time, 'ok' AS status, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
  NULL::DOUBLE AS cost, first_event_uid, last_event_uid
FROM chat_parent
UNION ALL BY NAME
SELECT o.session_id, o.span_id, coalesce(c.span_id, t.span_id) AS parent_span_id,
  'execute_tool' AS kind, coalesce(nullif(o.name, ''), 'tool') AS name, o.start_time, o.end_time,
  CASE WHEN o.tool_error > 0 THEN 'error' ELSE 'ok' END AS status, '' AS model,
  0 AS input_tokens, 0 AS output_tokens, 0 AS cache_read_tokens, 0 AS cache_write_tokens,
  NULL::DOUBLE AS cost, o.first_event_uid, o.last_event_uid
FROM tool o
LEFT JOIN chat c ON c.session_id = o.session_id AND c.ckey = o.ckey
LEFT JOIN turn t ON t.session_id = o.session_id AND t.turn_seq = o.turn_seq`
	}
}
