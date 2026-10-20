CREATE TABLE moraine.mcp_session_mutation_signals
(
    `session_id` String,
    `unique_versions` AggregateFunction(uniqExact, UInt64),
    `fingerprint_a` AggregateFunction(sumDistinct, UInt64),
    `fingerprint_b` AggregateFunction(sumDistinct, UInt64)
)
ENGINE = AggregatingMergeTree
ORDER BY session_id
SETTINGS index_granularity = 1
