-- mnemonic cache contract. cache.Build creates exactly these relations in a
-- fresh cache-<unix_ms>.duckdb, then renames it into place; every query reads
-- only these. Deleting the cache loses nothing: the next build recreates it
-- from the lake, <data>/events/author=*/machine=*/harness=*/*.parquet.
--
-- Archive rows (machine=archive-ch) never change. Their events, docs and
-- postings live in cache/archive-<key>.duckdb, built once per set of archive
-- files (names, sizes, mtimes) and attached read-only as arch; archive_ref
-- names it. The cache holds the live rows (live_events, live_docs,
-- live_postings) and the archive docs a live row supersedes (dropped).
--
-- Column names and meanings follow ClickHouse moraine_personal so the query
-- package ports upstream crates/moraine-conversations/src/clickhouse_repo/*
-- and docs/clickhouse/*.sql with dialect changes only.

-- events: one row per event_uid (the highest event_version, ties broken by the
-- newest ingested_at) over archive and live. Every lake column except
-- payload_json and token_usage_json, which stay in the lake and are read at
-- (lake_file, lake_row); plus
--   doc                  UINTEGER, the id docs and postings key on;
--   lake_file, lake_row  the lake parquet path and file_row_number;
--   emission_index       payload_json.moraine_emission_index, 0 when absent;
--   tool_io_input_json, tool_io_input_preview, tool_io_output_preview,
--   tool_io_project_id, tool_io_repo_rel_path, tool_io_worktree_root
--                        payload_json.moraine_tool_io fields of tool requests
--                        ('' when missing, NULL without moraine_tool_io);
--   meta_json            payload_json of session_meta and omp title rows.
-- CREATE VIEW events AS arch.events minus dropped UNION ALL live_events;

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

-- DuckDB ports of the ClickHouse views, same column names and types, minus
-- payload_json and token_usage_json:
--   trace    <- docs/clickhouse/v_conversation_trace.sql
--   sessions <- docs/clickhouse/v_session_summary.sql
-- CREATE VIEW trace AS ...;
-- CREATE VIEW sessions AS ...;

-- BM25 inputs, as docs/clickhouse/mv_search_postings.sql: terms are
-- regexp_extract_all(lower(text_content), '[a-z0-9_]+') with
-- 2 <= length <= 64; documents are events with at least one term.
-- all_postings (term, doc, tf USMALLINT, doc_len UINTEGER) is arch.postings
-- minus dropped UNION ALL live_postings, each stored sorted by term; a term's
-- df is its row count there.
-- CREATE VIEW all_postings AS ...;
-- CREATE VIEW docs (event_uid VARCHAR, doc_len UINTEGER) AS ...;
-- CREATE VIEW postings (term VARCHAR, event_uid VARCHAR, tf USMALLINT) AS ...;

CREATE TABLE corpus (
    docs UBIGINT NOT NULL,
    avg_doc_len DOUBLE NOT NULL
);
