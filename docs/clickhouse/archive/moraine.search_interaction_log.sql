CREATE TABLE moraine.search_interaction_log
(
    `ts` DateTime64(3) DEFAULT now64(3),
    `interaction_id` String,
    `query_id` String,
    `event_uid` String,
    `action` LowCardinality(String),
    `actor` String,
    `value` Float64,
    `note` String,
    `metadata_json` String
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(ts)
ORDER BY (ts, query_id, event_uid, action)
SETTINGS index_granularity = 8192
