CREATE TABLE moraine_personal._predeploy_20260804_042300_tool_io
(
    `ingested_at` DateTime64(3) DEFAULT now64(3),
    `event_uid` String,
    `session_id` String,
    `harness` LowCardinality(String),
    `inference_provider` LowCardinality(String) DEFAULT '',
    `source_name` LowCardinality(String),
    `tool_call_id` String,
    `parent_tool_call_id` String,
    `tool_name` LowCardinality(String),
    `tool_phase` LowCardinality(String),
    `tool_error` UInt8,
    `input_json` String,
    `output_json` String,
    `output_text` String,
    `input_bytes` UInt32,
    `output_bytes` UInt32,
    `input_preview` String,
    `output_preview` String,
    `io_hash` UInt64,
    `project_id` LowCardinality(String) DEFAULT '',
    `repo_rel_path` String DEFAULT '',
    `worktree_root` String DEFAULT '',
    `source_ref` String,
    `event_version` UInt64
)
ENGINE = ReplacingMergeTree(event_version)
PARTITION BY toYYYYMM(ingested_at)
ORDER BY (session_id, tool_call_id, event_uid)
SETTINGS index_granularity = 8192
