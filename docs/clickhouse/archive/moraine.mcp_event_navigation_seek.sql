CREATE TABLE moraine.mcp_event_navigation_seek
(
    `session_id` String,
    `sort_time` DateTime64(3),
    `source_file` String,
    `source_generation` UInt32,
    `source_offset` UInt64,
    `source_line_no` UInt64,
    `emission_index` UInt32,
    `event_uid` String,
    `event_version` UInt64,
    `event_ts` DateTime64(3),
    `turn_index` UInt32,
    `is_user_message` UInt8,
    `display_time` DateTime64(3),
    `event_kind` LowCardinality(String),
    `actor_kind` LowCardinality(String),
    `payload_type` LowCardinality(String),
    `tool_call_id` String,
    `tool_name` LowCardinality(String),
    `tool_phase` LowCardinality(String),
    `op_status` LowCardinality(String),
    `item_id` String,
    `harness` LowCardinality(String),
    `inference_provider` LowCardinality(String),
    `source_name` LowCardinality(String),
    `is_metadata_bearing` UInt8,
    `has_session_title` UInt8,
    `has_session_name` UInt8,
    `has_session_summary` UInt8,
    `has_session_slug` UInt8
)
ENGINE = ReplacingMergeTree
PARTITION BY cityHash64(session_id) % 64
ORDER BY (session_id, sort_time, source_file, source_generation, source_offset, source_line_no, emission_index, event_uid, event_version)
SETTINGS index_granularity = 256
