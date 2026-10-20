CREATE TABLE moraine.ingest_errors
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
    `error_kind` LowCardinality(String),
    `error_text` String,
    `raw_fragment` String
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(ingested_at)
ORDER BY (source_name, source_file, source_generation, source_offset, source_line_no)
SETTINGS index_granularity = 8192
