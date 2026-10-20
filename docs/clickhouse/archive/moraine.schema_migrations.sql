CREATE TABLE moraine.schema_migrations
(
    `version` String,
    `name` String,
    `applied_at` DateTime64(3) DEFAULT now64(3)
)
ENGINE = ReplacingMergeTree(applied_at)
ORDER BY version
SETTINGS index_granularity = 8192
