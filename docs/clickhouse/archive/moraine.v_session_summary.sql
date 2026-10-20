CREATE VIEW moraine.v_session_summary
(
    `session_id` String,
    `first_event_time` DateTime64(3),
    `last_event_time` DateTime64(3),
    `total_turns` UInt32,
    `total_events` UInt64,
    `tool_calls` UInt64,
    `tool_results` UInt64,
    `user_messages` UInt64,
    `assistant_messages` UInt64
)
AS SELECT
    session_id,
    min(event_time) AS first_event_time,
    max(event_time) AS last_event_time,
    max(turn_seq) AS total_turns,
    count() AS total_events,
    countIf(event_class = 'tool_call') AS tool_calls,
    countIf(event_class = 'tool_result') AS tool_results,
    countIf((actor_role = 'user') AND (event_class = 'message')) AS user_messages,
    countIf((actor_role = 'assistant') AND (event_class = 'message')) AS assistant_messages
FROM moraine.v_conversation_trace
GROUP BY session_id
