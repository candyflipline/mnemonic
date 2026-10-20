CREATE TABLE moraine.ingest_checkpoints
(
    `updated_at` DateTime64(3) DEFAULT now64(3),
    `source_name` LowCardinality(String),
    `source_file` String,
    `source_inode` UInt64,
    `source_generation` UInt32,
    `last_offset` UInt64,
    `last_line_no` UInt64,
    `status` LowCardinality(String),
    `cursor_json` String DEFAULT '',
    `source_fingerprint` UInt64 DEFAULT 0,
    `schema_fingerprint` UInt64 DEFAULT 0,
    `host` String
)
ENGINE = ReplacingMergeTree(updated_at)
PARTITION BY toYYYYMM(updated_at)
PRIMARY KEY (source_name, source_file, source_generation)
ORDER BY (source_name, source_file, source_generation, host)
SETTINGS index_granularity = 8192
