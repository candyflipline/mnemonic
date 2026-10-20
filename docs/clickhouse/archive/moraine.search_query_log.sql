CREATE TABLE moraine.search_query_log
(
    `ts` DateTime64(3) DEFAULT now64(3),
    `query_id` String,
    `source` LowCardinality(String),
    `session_hint` String,
    `raw_query` String,
    `normalized_terms` Array(String),
    `term_count` UInt16,
    `result_limit` UInt16,
    `min_should_match` UInt16,
    `min_score` Float64,
    `include_tool_events` UInt8,
    `exclude_codex_mcp` UInt8,
    `response_ms` UInt32,
    `result_count` UInt16,
    `metadata_json` String
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(ts)
ORDER BY (ts, query_id)
SETTINGS index_granularity = 8192
