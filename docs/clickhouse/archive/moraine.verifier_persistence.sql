CREATE TABLE moraine.verifier_persistence
(
    `marker` String,
    `created_at` DateTime64(3)
)
ENGINE = MergeTree
ORDER BY created_at
SETTINGS index_granularity = 8192
