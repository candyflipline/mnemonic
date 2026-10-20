-- mnemonic cache contract. `cache::build` creates exactly these relations in
-- a fresh `cache-<unix_ms>.duckdb`; every query module reads only these.
-- The cache is derived: deleting it loses nothing, the next build recreates
-- it from the lake (`<lake_dir>/machine=*/harness=*/events/*.parquet`).
--
-- Column names and meanings follow ClickHouse `moraine_personal` so the
-- query modules can port `crates/moraine-conversations/src/clickhouse_repo`
-- SQL with dialect changes only.

-- Every lake events column (crates/mnemonic/src/schema.rs) plus the
-- machine prefix the row came from. One row per event_uid: the highest
-- event_version, ties broken by the newest ingested_at.
CREATE TABLE events (
    machine_id VARCHAR NOT NULL
    -- , <the 59 lake columns, same names and types>
);

-- One row per lake file, from its parquet footer (layout::FooterMeta).
CREATE TABLE sources (
    machine_id VARCHAR NOT NULL,
    harness VARCHAR NOT NULL,
    source_name VARCHAR NOT NULL,
    source_file VARCHAR NOT NULL,
    size UBIGINT NOT NULL,
    head_hash VARCHAR NOT NULL,
    mtime_ns UBIGINT NOT NULL,
    written_at_ms UBIGINT NOT NULL,
    lake_file VARCHAR NOT NULL
);

-- DuckDB ports of the ClickHouse views, same column names and types:
-- trace    <- moraine_personal.v_conversation_trace
-- sessions <- moraine_personal.v_session_summary
-- (definitions: docs/mnemonic/clickhouse-views/*.sql)
-- CREATE VIEW trace AS ...;
-- CREATE VIEW sessions AS ...;

-- BM25 inputs, as ClickHouse mv_search_postings: terms are
-- regexp_extract_all(lower(text_content), '[a-z0-9_]+') with 2 <= length
-- <= 64; documents are events with at least one term.
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
