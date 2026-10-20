CREATE TABLE moraine_personal.search_hit_log
(
    `ts` DateTime64(3) DEFAULT now64(3),
    `query_id` String,
    `rank` UInt16,
    `event_uid` String,
    `session_id` String,
    `source_name` LowCardinality(String) DEFAULT '',
    `harness` LowCardinality(String) DEFAULT '',
    `inference_provider` LowCardinality(String) DEFAULT '',
    `score` Float64,
    `matched_terms` UInt16,
    `doc_len` UInt32,
    `event_class` LowCardinality(String),
    `payload_type` LowCardinality(String),
    `actor_role` LowCardinality(String),
    `name` LowCardinality(String),
    `source_ref` String
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(ts)
ORDER BY (query_id, rank, ts)
SETTINGS index_granularity = 8192
