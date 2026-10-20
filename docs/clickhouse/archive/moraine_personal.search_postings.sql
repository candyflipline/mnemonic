CREATE TABLE moraine_personal.search_postings
(
    `post_version` UInt64,
    `term` String,
    `doc_id` String,
    `session_id` String,
    `source_name` LowCardinality(String),
    `harness` LowCardinality(String),
    `inference_provider` LowCardinality(String),
    `event_class` LowCardinality(String),
    `payload_type` LowCardinality(String),
    `actor_role` LowCardinality(String),
    `name` LowCardinality(String),
    `phase` LowCardinality(String),
    `source_ref` String,
    `doc_len` UInt32,
    `tf` UInt16
)
ENGINE = ReplacingMergeTree(post_version)
PARTITION BY cityHash64(term) % 32
ORDER BY (term, doc_id)
SETTINGS index_granularity = 8192
