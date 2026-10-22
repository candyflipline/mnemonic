package cache

func init() { RegisterSpans("claude-code", claudeCodeSpans) }

// claudeCodeSpans: the session is the root invoke_agent; main-thread events
// form one turn span per turn_seq; a sidechain's agent_run_id is an
// invoke_agent under the tool call its rows name in parent_tool_call_id, else
// under the root; each request_id is a chat span under its subagent or turn;
// each tool_call_id is an execute_tool span under the chat of its tool_call
// row, else under the tool call named by parent_tool_call_id. Every row of a
// request carries the request's usage, so a chat's tokens are the max over
// its rows; the root, a turn and a subagent carry the sum over the chats
// below them.
func claudeCodeSpans(where string) string {
	return `WITH ev AS (
  SELECT session_id, event_uid, event_order, event_time, turn_seq, event_kind,
    coalesce(nullif(request_id, ''), nullif(trace_id, '')) AS req,
    nullif(tool_call_id, '') AS call, nullif(parent_tool_call_id, '') AS parent_call,
    tool_name, tool_error,
    CASE WHEN is_substream = 1 THEN nullif(agent_run_id, '') END AS sub,
    agent_label, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens
  FROM events WHERE ` + where + `),
root AS (
  SELECT session_id, ` + spanID("'session'", "session_id") + ` AS span_id, ` + spanBounds + `
  FROM ev GROUP BY session_id),
turn AS (
  SELECT session_id, turn_seq, ` + spanID("'turn'", "session_id", "cast(turn_seq AS VARCHAR)") + ` AS span_id, ` + spanBounds + `
  FROM ev WHERE sub IS NULL GROUP BY session_id, turn_seq),
agent AS (
  SELECT session_id, sub, ` + spanID("'agent'", "session_id", "sub") + ` AS span_id,
    coalesce(nullif(max(agent_label), ''), sub) AS name, ` + spanBounds + `
  FROM ev WHERE sub IS NOT NULL GROUP BY session_id, sub),
chat AS (
  SELECT session_id, req, ` + spanID("'chat'", "session_id", "req") + ` AS span_id,
    max(model) AS model, max(sub) AS sub, min(turn_seq) FILTER (WHERE sub IS NULL) AS turn_seq,
    coalesce(max(input_tokens), 0) AS input_tokens, coalesce(max(output_tokens), 0) AS output_tokens,
    coalesce(max(cache_read_tokens), 0) AS cache_read_tokens, coalesce(max(cache_write_tokens), 0) AS cache_write_tokens, ` + spanBounds + `
  FROM ev WHERE req IS NOT NULL GROUP BY session_id, req),
tool AS (
  SELECT session_id, call, ` + spanID("'tool'", "session_id", "call") + ` AS span_id,
    max(tool_name) AS name, max(tool_error) AS tool_error, max(sub) AS sub,
    arg_min(req, event_order) FILTER (WHERE event_kind = 'tool_call') AS req,
    max(parent_call) FILTER (WHERE parent_call <> call) AS parent_call,
    min(turn_seq) FILTER (WHERE sub IS NULL) AS turn_seq, ` + spanBounds + `
  FROM ev WHERE call IS NOT NULL GROUP BY session_id, call),
agent_parent AS (
  SELECT e.session_id, e.sub, arg_min(p.span_id, e.event_order) AS span_id, arg_min(p.turn_seq, e.event_order) AS turn_seq
  FROM ev e JOIN tool p ON p.session_id = e.session_id AND p.call = e.parent_call AND p.sub IS DISTINCT FROM e.sub
  WHERE e.sub IS NOT NULL GROUP BY e.session_id, e.sub),
chat_parent AS (
  SELECT c.*, coalesce(a.span_id, t.span_id, r.span_id) AS parent_span_id, t.span_id AS turn_span_id
  FROM chat c JOIN root r USING (session_id)
  LEFT JOIN agent a ON a.session_id = c.session_id AND a.sub = c.sub
  LEFT JOIN agent_parent ap ON ap.session_id = c.session_id AND ap.sub = c.sub
  LEFT JOIN turn t ON t.session_id = c.session_id AND t.turn_seq = coalesce(c.turn_seq, ap.turn_seq)),
tokens AS (
  SELECT key, sum(input_tokens) AS input_tokens, sum(output_tokens) AS output_tokens,
    sum(cache_read_tokens) AS cache_read_tokens, sum(cache_write_tokens) AS cache_write_tokens
  FROM (
    SELECT r.span_id AS key, c.* FROM chat_parent c JOIN root r USING (session_id)
    UNION ALL SELECT turn_span_id AS key, * FROM chat_parent WHERE turn_span_id IS NOT NULL
    UNION ALL SELECT a.span_id AS key, c.* FROM chat_parent c JOIN agent a USING (session_id, sub))
  GROUP BY key)
SELECT s.session_id, s.span_id, s.parent_span_id, 'invoke_agent' AS kind, s.name,
  s.start_time, s.end_time, 'ok' AS status, '' AS model,
  coalesce(k.input_tokens, 0) AS input_tokens, coalesce(k.output_tokens, 0) AS output_tokens,
  coalesce(k.cache_read_tokens, 0) AS cache_read_tokens, coalesce(k.cache_write_tokens, 0) AS cache_write_tokens,
  NULL::DOUBLE AS cost, s.first_event_uid, s.last_event_uid
FROM (
  SELECT session_id, span_id, '' AS parent_span_id, 'claude-code' AS name,
    start_time, end_time, first_event_uid, last_event_uid
  FROM root
  UNION ALL
  SELECT a.session_id, a.span_id, coalesce(p.span_id, r.span_id), a.name,
    a.start_time, a.end_time, a.first_event_uid, a.last_event_uid
  FROM agent a JOIN root r USING (session_id)
  LEFT JOIN agent_parent p ON p.session_id = a.session_id AND p.sub = a.sub) s
LEFT JOIN tokens k ON k.key = s.span_id
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
SELECT o.session_id, o.span_id, coalesce(c.span_id, p.span_id, a.span_id, t.span_id, r.span_id) AS parent_span_id,
  'execute_tool' AS kind, coalesce(nullif(o.name, ''), 'tool') AS name, o.start_time, o.end_time,
  CASE WHEN o.tool_error > 0 THEN 'error' ELSE 'ok' END AS status, '' AS model,
  0 AS input_tokens, 0 AS output_tokens, 0 AS cache_read_tokens, 0 AS cache_write_tokens,
  NULL::DOUBLE AS cost, o.first_event_uid, o.last_event_uid
FROM tool o JOIN root r USING (session_id)
LEFT JOIN tool p ON p.session_id = o.session_id AND p.call = o.parent_call
LEFT JOIN chat c ON c.session_id = o.session_id AND c.req = o.req
LEFT JOIN agent a ON a.session_id = o.session_id AND a.sub = o.sub
LEFT JOIN turn t ON t.session_id = o.session_id AND t.turn_seq = o.turn_seq`
}
