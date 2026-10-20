CREATE TABLE moraine_personal._predeploy_20260804_042300_event_links
(
    `ingested_at` DateTime64(3) DEFAULT now64(3),
    `event_uid` String,
    `linked_event_uid` String,
    `linked_external_id` String,
    `link_type` LowCardinality(String),
    `session_id` String,
    `harness` LowCardinality(String),
    `inference_provider` LowCardinality(String) DEFAULT '',
    `source_name` LowCardinality(String),
    `metadata_json` String,
    `event_version` UInt64,
    CONSTRAINT event_links_link_type_domain CHECK link_type IN ('parent_event', 'compacted_parent', 'parent_uuid', 'tool_use_id', 'source_tool_assistant', 'subagent_parent', 'unknown')
)
ENGINE = ReplacingMergeTree(event_version)
PARTITION BY toYYYYMM(ingested_at)
ORDER BY (session_id, event_uid, link_type, linked_event_uid)
SETTINGS index_granularity = 8192
