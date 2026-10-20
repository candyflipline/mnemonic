CREATE TABLE moraine_personal._predeploy_20260804_042300_raw_events
(
    `ingested_at` DateTime64(3) DEFAULT now64(3),
    `source_name` LowCardinality(String),
    `harness` LowCardinality(String),
    `inference_provider` LowCardinality(String) DEFAULT '',
    `source_file` String,
    `source_inode` UInt64,
    `source_generation` UInt32,
    `source_line_no` UInt64,
    `source_offset` UInt64,
    `record_ts` String,
    `top_type` LowCardinality(String),
    `session_id` String,
    `author` String DEFAULT '',
    `raw_json` String,
    `raw_json_hash` UInt64,
    `event_uid` String,
    `cwd` String DEFAULT ''
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(ingested_at)
ORDER BY (source_name, source_file, source_generation, source_offset, source_line_no, event_uid)
SETTINGS index_granularity = 8192
