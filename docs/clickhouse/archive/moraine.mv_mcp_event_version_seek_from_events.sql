CREATE MATERIALIZED VIEW moraine.mv_mcp_event_version_seek_from_events TO moraine.mcp_event_version_seek
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
AS SELECT
    event_uid,
    event_version,
    session_id,
    ifNull(parseDateTime64BestEffortOrNull(record_ts), toDateTime64('1970-01-01 00:00:00', 3)) AS sort_time,
    source_file,
    source_generation,
    source_offset,
    source_line_no,
    toUInt32(JSONExtractUInt(payload_json, 'moraine_emission_index')) AS emission_index
FROM moraine.events
WHERE notEmpty(session_id)
