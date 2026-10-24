`events.tsv` is the column list source for the Event columns contract: the
column names, their order and their ClickHouse type strings, which
`go run ./scripts/genschema` generates into `internal/schema/events.go`.
It is not a ClickHouse integration; the name is the types' origin. The
ClickHouse DDL of the retired store is in git history (cc9aac9, under
`docs/clickhouse/`), and the rows imported from it live on as the
`machine=archive-ch` parquet under `events/`.
