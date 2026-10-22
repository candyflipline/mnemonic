package cache

func init() { RegisterSpans("otlp", otlpSpans) }

// otlpSpans: each events row with payload_type 'span' and an op_kind of chat,
// execute_tool or invoke_agent is a span with the author and the OTLP span id
// as its key; other spans are not emitted and their children hang under the
// nearest emitted ancestor, else the root. Of the emitted spans on a parent
// cycle (a span that is its own ancestor), the one with the smallest OTLP span
// id is the root. A chat carries its own usage; an invoke_agent carries the
// sum over the chats beneath it, else its own. The parent link is
// origin_event_id, the event_uid of the parent span's row.
func otlpSpans(where string) string {
	return `WITH RECURSIVE ev AS (
  SELECT session_id, author, event_uid, origin_event_id, trace_id, item_id, op_kind, text_content, op_status, model,
    input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, latency_ms, event_time,
    token_usage_native_units['cost_usd'] AS cost,
    op_kind IN ('chat', 'execute_tool', 'invoke_agent') AS emitted
  FROM events WHERE payload_type = 'span' AND (` + where + `)),
up(uid, cur, depth) AS (
  SELECT event_uid, origin_event_id, 0 FROM ev WHERE origin_event_id <> ''
  UNION ALL
  SELECT u.uid, p.origin_event_id, u.depth + 1 FROM up u JOIN ev p ON p.event_uid = u.cur
  WHERE p.origin_event_id <> '' AND u.depth < 64),
nearest(uid, parent_uid) AS (
  SELECT uid, cur FROM (
    SELECT uid, cur, depth, row_number() OVER (PARTITION BY uid ORDER BY depth) AS rn
    FROM up JOIN ev p ON p.event_uid = up.cur WHERE p.emitted)
  WHERE rn = 1),
below(agent_uid, chat_uid) AS (
  SELECT DISTINCT a.event_uid, c.event_uid FROM ev c JOIN up ON up.uid = c.event_uid JOIN ev a ON a.event_uid = up.cur
  WHERE c.op_kind = 'chat' AND a.op_kind = 'invoke_agent'),
cyclic(uid) AS (SELECT DISTINCT uid FROM up WHERE uid = cur),
roots(uid) AS (
  SELECT c.uid FROM cyclic c JOIN ev x ON x.event_uid = c.uid AND x.emitted
  WHERE NOT EXISTS (
    SELECT 1 FROM up a JOIN up b ON b.uid = a.cur AND b.cur = a.uid JOIN ev m ON m.event_uid = a.cur AND m.emitted
    WHERE a.uid = c.uid AND m.item_id < x.item_id)),
sums AS (
  SELECT b.agent_uid, sum(c.input_tokens) AS input_tokens, sum(c.output_tokens) AS output_tokens,
    sum(c.cache_read_tokens) AS cache_read_tokens, sum(c.cache_write_tokens) AS cache_write_tokens, sum(c.cost) AS cost
  FROM below b JOIN ev c ON c.event_uid = b.chat_uid GROUP BY b.agent_uid)
SELECT e.session_id, ` + spanID("'otlp'", "e.author", "e.trace_id", "e.item_id") + ` AS span_id,
  CASE WHEN p.event_uid IS NULL OR r.uid IS NOT NULL THEN '' ELSE ` + spanID("'otlp'", "p.author", "p.trace_id", "p.item_id") + ` END AS parent_span_id,
  e.op_kind AS kind, coalesce(nullif(e.text_content, ''), e.op_kind) AS name,
  e.event_time AS start_time, e.event_time + to_milliseconds(cast(e.latency_ms AS BIGINT)) AS end_time,
  CASE WHEN e.op_status = 'error' THEN 'error' ELSE 'ok' END AS status, e.model,
  coalesce(s.input_tokens, e.input_tokens) AS input_tokens, coalesce(s.output_tokens, e.output_tokens) AS output_tokens,
  coalesce(s.cache_read_tokens, e.cache_read_tokens) AS cache_read_tokens,
  coalesce(s.cache_write_tokens, e.cache_write_tokens) AS cache_write_tokens,
  coalesce(s.cost, e.cost) AS cost, e.event_uid AS first_event_uid, e.event_uid AS last_event_uid
FROM ev e
LEFT JOIN nearest n ON n.uid = e.event_uid
LEFT JOIN ev p ON p.event_uid = n.parent_uid
LEFT JOIN roots r ON r.uid = e.event_uid
LEFT JOIN sums s ON s.agent_uid = e.event_uid
WHERE e.emitted`
}
