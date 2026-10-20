-- mnemonic cache contract. cache.Build creates exactly these relations in a
-- fresh cache-<unix_ms>.duckdb, then renames it into place; every query reads
-- only these. Deleting the cache loses nothing: the next build recreates it
-- from the lake, <data>/events/author=*/machine=*/harness=*/*.parquet.
--
-- Column names and meanings follow ClickHouse moraine_personal so the query
-- package ports upstream crates/moraine-conversations/src/clickhouse_repo/*
-- and docs/clickhouse/*.sql with dialect changes only.

-- Every lake column: author, machine_id and ingested_at (stamped by the
-- server) plus schema.EventColumns with their DuckDB types. One row per
-- event_uid: the highest event_version, ties broken by the newest ingested_at.
-- CREATE TABLE events AS SELECT ... FROM read_parquet(<lake glob>, hive_partitioning=true) QUALIFY ...;

-- One row per raw source, from the server's manifest files.
CREATE TABLE sources (
    author VARCHAR NOT NULL,
    machine_id VARCHAR NOT NULL,
    harness VARCHAR NOT NULL,
    source_name VARCHAR NOT NULL,
    source_file VARCHAR NOT NULL,
    src_id VARCHAR NOT NULL,
    size UBIGINT NOT NULL,
    head_hash VARCHAR NOT NULL,
    updated_ms UBIGINT NOT NULL,
    events UBIGINT NOT NULL      -- 0 while the harness has no parser
);

-- DuckDB ports of the ClickHouse views, same column names and types:
--   trace    <- docs/clickhouse/v_conversation_trace.sql
--   sessions <- docs/clickhouse/v_session_summary.sql
-- CREATE VIEW trace AS ...;
-- CREATE VIEW sessions AS ...;

-- BM25 inputs, as docs/clickhouse/mv_search_postings.sql: terms are
-- regexp_extract_all(lower(text_content), '[a-z0-9_]+') with
-- 2 <= length <= 64; documents are events with at least one term.
CREATE TABLE docs (
    event_uid VARCHAR NOT NULL,
    doc_len UINTEGER NOT NULL
);

CREATE TABLE postings (
    term VARCHAR NOT NULL,
    event_uid VARCHAR NOT NULL,
    tf USMALLINT NOT NULL
); -- inserted ORDER BY term

CREATE TABLE term_stats (
    term VARCHAR NOT NULL,
    df UBIGINT NOT NULL
);

CREATE TABLE corpus (
    docs UBIGINT NOT NULL,
    avg_doc_len DOUBLE NOT NULL
);
