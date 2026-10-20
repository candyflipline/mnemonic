CREATE TABLE moraine_personal._predeploy_20260804_042300_schema_migrations
(
    `version` String,
    `name` String,
    `applied_at` DateTime64(3) DEFAULT now64(3)
)
ENGINE = ReplacingMergeTree(applied_at)
ORDER BY version
SETTINGS index_granularity = 8192
