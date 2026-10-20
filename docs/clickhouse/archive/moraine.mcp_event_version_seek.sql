CREATE TABLE moraine.mcp_event_version_seek
(
    `event_uid` String,
    `event_version` UInt64,
    `session_id` String,
    `sort_time` DateTime64(3),
    `source_file` String,
    `source_generation` UInt32,
    `source_offset` UInt64,
    `source_line_no` UInt64,
    `emission_index` UInt32
)
ENGINE = ReplacingMergeTree(event_version)
ORDER BY event_uid
SETTINGS index_granularity = 64
