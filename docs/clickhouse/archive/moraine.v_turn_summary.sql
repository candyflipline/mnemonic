CREATE VIEW moraine.v_turn_summary
(
    `session_id` String,
    `turn_seq` UInt32,
    `turn_id` String,
    `started_at` DateTime64(3),
    `ended_at` DateTime64(3),
    `total_events` UInt64,
    `user_messages` UInt64,
    `assistant_messages` UInt64,
    `tool_calls` UInt64,
    `tool_results` UInt64,
    `reasoning_items` UInt64
)
AS SELECT
    session_id,
    turn_seq,
    anyIf(turn_id, turn_id != '') AS turn_id,
    min(event_time) AS started_at,
    max(event_time) AS ended_at,
    count() AS total_events,
    countIf((actor_role = 'user') AND (event_class = 'message')) AS user_messages,
    countIf((actor_role = 'assistant') AND (event_class = 'message')) AS assistant_messages,
    countIf(event_class = 'tool_call') AS tool_calls,
    countIf(event_class = 'tool_result') AS tool_results,
    countIf(event_class = 'reasoning') AS reasoning_items
FROM moraine.v_conversation_trace
GROUP BY
    session_id,
    turn_seq
