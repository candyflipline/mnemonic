package cache

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"regexp"
	"strings"

	"github.com/candyflipline/mnemonic/internal/schema"
	"github.com/candyflipline/mnemonic/internal/wire"

	"github.com/duckdb/duckdb-go/v2"
)

type manifest struct {
	wire.Source
	ZSTSize int64 `json:"zst_size"`
}

type lakeFile struct {
	path  string
	size  int64
	mtime int64
}

func execAll(ctx context.Context, c *sql.Conn, stmts []string) error {
	for _, s := range stmts {
		if _, err := c.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("%s: %w", firstLine(s), err)
		}
	}
	return nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// newDB opens a DuckDB database with TimeZone pinned to UTC on every pooled
// connection: a plain SET is session-scoped, so a database/sql pool would run
// later connections in the process's local zone, and the event_time an apply
// derives from a zoneless record_ts would depend on the connection.
func newDB(dsn string, setup ...string) (*sql.DB, error) {
	c, err := duckdb.NewConnector(dsn, func(execer driver.ExecerContext) error {
		for _, s := range append([]string{`SET TimeZone='UTC'`}, setup...) {
			if _, err := execer.ExecContext(context.Background(), s, nil); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return sql.OpenDB(c), nil
}

func sqlStr(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func parquetSource(files []lakeFile) string {
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = sqlStr(f.path)
	}
	return "read_parquet([" + strings.Join(paths, ", ") +
		"], hive_partitioning=false, filename=true, file_row_number=true)"
}

func md5Key(expr string) string {
	return "cast(md5_number(" + expr + ") >> 64 AS UBIGINT)"
}

var heavyColumns = map[string]bool{"payload_json": true, "token_usage_json": true}

func eventsDDL() string {
	var b strings.Builder
	b.WriteString("CREATE TABLE events (doc UINTEGER NOT NULL")
	for _, c := range schema.EventColumns {
		if heavyColumns[c.Name] {
			continue
		}
		b.WriteString(", " + c.Name + " " + c.DuckDB)
		if c.Name == "text_content" {
			b.WriteString(" USING COMPRESSION zstd")
		}
	}
	b.WriteString(`, author VARCHAR, machine_id VARCHAR, ingested_at TIMESTAMP,
  lake_file VARCHAR, lake_row UBIGINT, emission_index UINTEGER,
  tool_io_input_json VARCHAR USING COMPRESSION zstd, tool_io_input_preview VARCHAR,
  tool_io_output_preview VARCHAR, tool_io_project_id VARCHAR, tool_io_repo_rel_path VARCHAR,
  tool_io_worktree_root VARCHAR, meta_json VARCHAR USING COMPRESSION zstd,
  row_hash UBIGINT NOT NULL, flags USMALLINT NOT NULL, doc_len UINTEGER NOT NULL,
  event_time TIMESTAMP, event_order BIGINT, turn_seq UINTEGER, event_ordinal UINTEGER,
  text_hash UBIGINT NOT NULL, uid_key UBIGINT NOT NULL)`)
	return b.String()
}

// insertEventsSQL copies the winners in temp table w (filename,
// file_row_number, doc) from src into events, deriving the payload fields:
//   - emission_index: payload_json's moraine_emission_index the way
//     JSONExtractUInt reads it (missing or non-numeric is 0);
//   - tool_io_*: the moraine_tool_io fields of tool requests, ” for a
//     missing field and NULL when the request has no moraine_tool_io (the
//     only rows file_attention reads);
//   - meta_json: the whole payload of session_meta rows and omp title rows
//     (the only rows session titles read).
//
// The contains() guards skip JSON parsing on the rows that cannot match.
func insertEventsSQL(src string) string {
	cols := make([]string, 0, len(schema.EventColumns))
	for _, c := range schema.EventColumns {
		if !heavyColumns[c.Name] {
			cols = append(cols, "r."+c.Name)
		}
	}
	tio := func(field string) string {
		return "CASE WHEN tio IS NOT NULL THEN coalesce(json_extract_string(tio, '$." + field + "'), '') END"
	}
	return "INSERT INTO events SELECT w.doc, " + strings.Join(cols, ", ") + `,
  r.author, r.machine_id, r.ingested_at, r.filename, r.file_row_number,
  CASE WHEN contains(payload_json, 'moraine_emission_index')
    THEN coalesce(try_cast(json_extract_string(payload_json, '$.moraine_emission_index') AS UINTEGER), 0)
    ELSE 0 END,
  ` + tio("input_json") + `,
  ` + tio("input_preview") + `,
  ` + tio("output_preview") + `,
  ` + tio("project_id") + `,
  ` + tio("repo_rel_path") + `,
  ` + tio("worktree_root") + `,
  CASE WHEN event_kind = 'session_meta' OR (source_name = 'omp' AND contains(payload_json, 'title')
    AND json_extract_string(payload_json, '$.type') IN ('title', 'title_change')) THEN payload_json END,
  w.row_hash, w.flags, w.doc_len, ` + eventTimeSQL + `, NULL, NULL, NULL,
  ` + md5Key("coalesce(r.text_content, '')") + `, ` + md5Key("r.event_uid") + `
FROM (SELECT *, CASE WHEN tool_phase = 'request' AND contains(payload_json, 'moraine_tool_io')
    THEN json_extract(payload_json, '$.moraine_tool_io') END AS tio
  FROM ` + src + `) r
JOIN w ON w.filename = r.filename AND w.file_row_number = r.file_row_number`
}

// A term is a 2..64 char token of lower(text_content), doc_len counts only
// terms, and docs are the events with at least one term. docs, postings and
// the query-side scorer must use the same definition.
const tokenList = "list_filter(regexp_extract_all(lower(text_content), '[a-z0-9_]+'), lambda tok: len(tok) >= 2 AND len(tok) <= 64)"

const sourcesDDL = `CREATE TABLE sources (
    author VARCHAR NOT NULL,
    machine_id VARCHAR NOT NULL,
    harness VARCHAR NOT NULL,
    source_name VARCHAR NOT NULL,
    source_file VARCHAR NOT NULL,
    src_id VARCHAR NOT NULL,
    size UBIGINT NOT NULL,
    head_hash VARCHAR NOT NULL,
    updated_ms UBIGINT NOT NULL,
    events UBIGINT NOT NULL
)`

var lakeFileRe = regexp.MustCompile(`author=([^/]*)/machine=([^/]*)/harness=([^/]*)/([0-9a-f]+)(?:\.tail)?\.parquet$`)
