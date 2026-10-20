CREATE MATERIALIZED VIEW moraine_personal.mv_mcp_session_mutation_signals_from_events TO moraine_personal.mcp_session_mutation_signals
(
    `session_id` String,
    `unique_versions` AggregateFunction(uniqExact, UInt64),
    `fingerprint_a` AggregateFunction(sumDistinct, UInt64),
    `fingerprint_b` AggregateFunction(sumDistinct, UInt64)
)
AS WITH
    ifNull(parseDateTime64BestEffortOrNull(record_ts), toDateTime64('1970-01-01 00:00:00', 3)) AS sort_time,
    toUInt32(JSONExtractUInt(payload_json, 'moraine_emission_index')) AS emission_index,
    cityHash64(event_uid, event_version, sort_time, source_file, source_generation, source_offset, source_line_no, emission_index) AS row_hash_a,
    sipHash64(event_uid, event_version, sort_time, source_file, source_generation, source_offset, source_line_no, emission_index) AS row_hash_b
SELECT
    session_id,
    uniqExactState(row_hash_a) AS unique_versions,
    sumDistinctState(row_hash_a) AS fingerprint_a,
    sumDistinctState(row_hash_b) AS fingerprint_b
FROM moraine_personal.events
WHERE notEmpty(session_id)
GROUP BY session_id
