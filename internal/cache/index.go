package cache

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/candyflipline/mnemonic/internal/layout"
	"github.com/candyflipline/mnemonic/internal/schema"
)

const indexVersion = 7

const (
	batchRows   = 500_000
	chunkRows   = 200_000
	inlineLimit = 2048
)

type Options struct {
	MemoryLimit string
	FlagsSQL    string
}

type Index struct {
	db      *sql.DB
	dataDir string
	flags   string
}

type ConvergeStats struct {
	Files, Batches                      int
	Inserted, Deleted, Moved, Reordered int64
	Sessions                            int64
}

func (s *ConvergeStats) add(o ConvergeStats) {
	s.Files += o.Files
	s.Batches += o.Batches
	s.Inserted += o.Inserted
	s.Deleted += o.Deleted
	s.Moved += o.Moved
	s.Reordered += o.Reordered
	s.Sessions += o.Sessions
}

func IndexPath(dataDir string) string {
	return filepath.Join(layout.CacheDir(dataDir), "index.duckdb")
}

func Open(dataDir string, o Options) (*Index, error) {
	if o.FlagsSQL == "" || o.MemoryLimit == "" {
		return nil, errors.New("cache.Open: MemoryLimit and FlagsSQL are required")
	}
	spill := filepath.Join(layout.CacheDir(dataDir), "index-spill")
	if err := os.RemoveAll(spill); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(spill, 0o755); err != nil {
		return nil, err
	}
	path := IndexPath(dataDir)
	setup := []string{
		"SET threads = 2",
		"SET preserve_insertion_order = false",
		"SET memory_limit = " + sqlStr(o.MemoryLimit),
		"SET temp_directory = " + sqlStr(spill),
	}
	if _, err := os.Stat(path); err == nil {
		db, err := openCurrent(path, o.FlagsSQL, setup)
		if err != nil {
			return nil, err
		}
		if db != nil {
			return &Index{db: db, dataDir: dataDir, flags: o.FlagsSQL}, nil
		}
		old, err := filepath.Glob(path + ".*.broken*")
		if err != nil {
			return nil, err
		}
		for _, f := range old {
			if err := os.Remove(f); err != nil {
				return nil, err
			}
		}
		aside := fmt.Sprintf("%s.%d.broken", path, time.Now().UnixMilli())
		if err := os.Rename(path, aside); err != nil {
			return nil, err
		}
		if err := os.Rename(path+".wal", aside+".wal"); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	db, err := newDB(path+"?storage_compatibility_version=latest", setup...)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	c, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		return nil, err
	}
	defer c.Close()
	stmts := append([]string{"BEGIN"}, indexDDL()...)
	stmts = append(stmts,
		"INSERT INTO meta VALUES ("+strconv.Itoa(indexVersion)+", "+sqlStr(o.FlagsSQL)+")",
		"INSERT INTO corpus VALUES (0, 0, 0)",
		"COMMIT")
	if err := execAll(ctx, c, stmts); err != nil {
		db.Close()
		return nil, err
	}
	return &Index{db: db, dataDir: dataDir, flags: o.FlagsSQL}, nil
}

func openCurrent(path, flagsSQL string, setup []string) (*sql.DB, error) {
	db, err := newDB(path, setup...)
	if err != nil {
		return nil, err
	}
	var hasMeta bool
	var v int
	var flags string
	err = db.QueryRow("SELECT count(*) > 0 FROM duckdb_tables() WHERE schema_name = 'main' AND table_name = 'meta'").Scan(&hasMeta)
	if err == nil && hasMeta {
		err = db.QueryRow("SELECT version, flags_sql FROM meta").Scan(&v, &flags)
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	if !hasMeta || v != indexVersion || flags != flagsSQL {
		db.Close()
		return nil, nil
	}
	return db, nil
}

func (ix *Index) DB() *sql.DB { return ix.db }

func (ix *Index) Close() error { return ix.db.Close() }

func indexDDL() []string {
	return append([]string{
		"CREATE TABLE meta (version INTEGER NOT NULL, flags_sql VARCHAR NOT NULL)",
		"CREATE TABLE corpus (docs UBIGINT NOT NULL, total_len UBIGINT NOT NULL, next_doc UINTEGER NOT NULL)",
		"CREATE TABLE lake_files (path VARCHAR NOT NULL, size UBIGINT NOT NULL, mtime_ns BIGINT NOT NULL, rows UBIGINT NOT NULL)",
		`CREATE TABLE lake_rows (lake_file VARCHAR NOT NULL, lake_row UBIGINT NOT NULL, event_uid VARCHAR,
  event_version UBIGINT, ingested_at TIMESTAMP, row_hash UBIGINT NOT NULL, uid_key UBIGINT NOT NULL)`,
		eventsDDL(),
		`CREATE TABLE turns (session_id VARCHAR, turn_seq UINTEGER, event_count BIGINT NOT NULL,
  completed INTEGER NOT NULL, terminal_uid VARCHAR)`,
		`CREATE TABLE sessions (session_id VARCHAR, first_event_time TIMESTAMP, last_event_time TIMESTAMP,
  total_turns UINTEGER, total_events UBIGINT, tool_calls UBIGINT, tool_results UBIGINT,
  user_messages UBIGINT, assistant_messages UBIGINT, completed INTEGER NOT NULL)`,
		sourcesDDL,
		indexTraceDDL,
	}, postingsDDL()...)
}

const indexTraceDDL = `
CREATE VIEW trace AS
SELECT session_id, session_date, event_uid,
       origin_event_id AS compacted_parent_uid,
       source_file, source_generation, source_line_no, source_offset, source_ref,
       event_time, event_order, turn_seq,
       cast(turn_index AS VARCHAR) AS turn_id,
       actor_kind AS actor_role,
       event_kind AS event_class,
       payload_type,
       tool_call_id AS call_id,
       tool_name AS name,
       coalesce(nullif(tool_phase, ''), op_status) AS phase,
       item_id, text_content, endpoint_kind,
       token_usage_buckets, token_usage_native_units
FROM events`

const eventTimeSQL = "coalesce(try_cast(r.record_ts AS TIMESTAMPTZ), r.ingested_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'"

const archiveSQL = "contains(lake_file, '/machine=" + layout.ArchiveMachine + "/')"

const winnerOrder = "event_version DESC, " + archiveSQL + ", ingested_at DESC, lake_file, lake_row"

func rowHashSQL() string {
	cols := make([]string, 0, len(schema.EventColumns)+2)
	for _, c := range schema.EventColumns {
		if c.Name != "event_version" {
			cols = append(cols, c.Name)
		}
	}
	return "hash(" + strings.Join(append(cols, "author", "machine_id"), ", ") + ")"
}

type change struct {
	file lakeFile
	gone bool
	rows int64
}

func (ix *Index) Converge(ctx context.Context) (ConvergeStats, error) {
	c, err := ix.db.Conn(ctx)
	if err != nil {
		return ConvergeStats{}, err
	}
	defer c.Close()
	changes, err := diffLake(ctx, c, ix.dataDir)
	if err != nil {
		return ConvergeStats{}, err
	}
	st, errs := ix.apply(ctx, c, changes)
	if err := syncManifests(ctx, c, ix.dataDir); err != nil {
		errs = append(errs, err)
	}
	if err := compactPostings(ctx, c); err != nil {
		errs = append(errs, err)
	}
	return st, errors.Join(errs...)
}

func (ix *Index) apply(ctx context.Context, c *sql.Conn, changes []change) (ConvergeStats, []error) {
	var st ConvergeStats
	var errs []error
	for _, b := range batches(changes, batchRows) {
		bst, err := applyBatch(ctx, c, ix.flags, ix.dataDir, b)
		if err != nil {
			errs = append(errs, fmt.Errorf("batch of %d files from %s: %w", len(b), b[0].file.path, err))
			continue
		}
		st.add(bst)
	}
	return st, errs
}

func diffLake(ctx context.Context, c *sql.Conn, dataDir string) ([]change, error) {
	matches, err := filepath.Glob(filepath.Join(dataDir, layout.EventsGlob))
	if err != nil {
		return nil, err
	}
	applied := map[string]lakeFile{}
	rows, err := c.QueryContext(ctx, "SELECT path, size, mtime_ns FROM lake_files")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var f lakeFile
		if err := rows.Scan(&f.path, &f.size, &f.mtime); err != nil {
			rows.Close()
			return nil, err
		}
		applied[f.path] = f
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []change
	for _, m := range matches {
		info, err := os.Stat(m)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		f := lakeFile{path: m, size: info.Size(), mtime: info.ModTime().UnixNano()}
		if prev, ok := applied[m]; !ok || prev != f {
			out = append(out, change{file: f})
		}
		delete(applied, m)
	}
	if err := countRows(ctx, c, out); err != nil {
		return nil, err
	}
	for _, f := range applied {
		out = append(out, change{file: f, gone: true})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if sa, sb := sourceOf(a.file.path), sourceOf(b.file.path); sa != sb {
			return sa < sb
		}
		if a.gone != b.gone {
			return a.gone
		}
		return a.file.path < b.file.path
	})
	return out, nil
}

func countRows(ctx context.Context, c *sql.Conn, changes []change) error {
	if len(changes) == 0 {
		return nil
	}
	idx := make(map[string]int, len(changes))
	paths := make([]string, len(changes))
	for i, ch := range changes {
		idx[ch.file.path] = i
		paths[i] = sqlStr(ch.file.path)
	}
	rows, err := c.QueryContext(ctx, "SELECT file_name, num_rows FROM parquet_file_metadata(["+strings.Join(paths, ", ")+"])")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var n int64
		if err := rows.Scan(&name, &n); err != nil {
			return err
		}
		if i, ok := idx[name]; ok {
			changes[i].rows += n
		}
	}
	return rows.Err()
}

// sourceOf is a lake file's path without its .parquet or .tail.parquet
// suffix: the files of one source share it.
func sourceOf(path string) string {
	return strings.TrimSuffix(strings.TrimSuffix(path, ".parquet"), ".tail")
}

// batches splits changes, ordered by source, into runs of about max rows,
// never between two files of one source.
func batches(changes []change, max int64) [][]change {
	var out [][]change
	var cur []change
	var n int64
	for _, ch := range changes {
		if len(cur) > 0 && n+ch.rows > max && sourceOf(cur[len(cur)-1].file.path) != sourceOf(ch.file.path) {
			out = append(out, cur)
			cur, n = nil, 0
		}
		cur = append(cur, ch)
		n += ch.rows
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

func applyBatch(ctx context.Context, c *sql.Conn, flagsSQL, dataDir string, b []change) (st ConvergeStats, err error) {
	if _, err := c.ExecContext(ctx, "BEGIN"); err != nil {
		return st, err
	}
	defer func() {
		if err != nil {
			c.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	st.Files, st.Batches = len(b), 1
	var all, present []lakeFile
	for _, ch := range b {
		all = append(all, ch.file)
		if !ch.gone {
			present = append(present, ch.file)
		}
	}
	inBatch := "lake_file IN (" + strings.Join(quotePaths(all), ", ") + ")"
	stmts := []string{
		`CREATE OR REPLACE TEMP TABLE n (lake_file VARCHAR, lake_row UBIGINT, event_uid VARCHAR,
  event_version UBIGINT, ingested_at TIMESTAMP, row_hash UBIGINT, uid_key UBIGINT)`,
	}
	if len(present) > 0 {
		stmts = append(stmts, "INSERT INTO n SELECT filename, file_row_number, event_uid, event_version, ingested_at, "+
			rowHashSQL()+", "+md5Key("event_uid")+" FROM "+parquetSource(present))
	}
	stmts = append(stmts,
		"CREATE OR REPLACE TEMP TABLE old AS SELECT * FROM lake_rows WHERE "+inBatch,
		`UPDATE n SET event_version = o.event_version, ingested_at = o.ingested_at FROM (
  SELECT lake_file, event_uid, row_hash, max(event_version) AS event_version, max(ingested_at) AS ingested_at
  FROM old GROUP BY ALL) o
WHERE n.lake_file = o.lake_file AND n.event_uid = o.event_uid AND n.row_hash = o.row_hash`,
		`CREATE OR REPLACE TEMP TABLE aff AS SELECT DISTINCT uid_key FROM (
  (SELECT * FROM n EXCEPT ALL SELECT * FROM old) UNION ALL (SELECT * FROM old EXCEPT ALL SELECT * FROM n))`)
	if err := execAll(ctx, c, stmts); err != nil {
		return st, err
	}
	var affected int64
	if err := c.QueryRowContext(ctx, "SELECT count(*) FROM aff").Scan(&affected); err != nil {
		return st, err
	}
	if affected > 0 {
		if err := applyWinners(ctx, c, flagsSQL, inBatch, &st); err != nil {
			return st, err
		}
	}
	stmts = nil
	if affected > 0 {
		stmts = append(stmts, "DELETE FROM lake_rows WHERE "+inBatch, "INSERT INTO lake_rows SELECT * FROM n")
	}
	stmts = append(stmts, "DELETE FROM lake_files WHERE path IN ("+strings.Join(quotePaths(all), ", ")+")")
	for _, ch := range b {
		if !ch.gone {
			stmts = append(stmts, fmt.Sprintf("INSERT INTO lake_files VALUES (%s, %d, %d, %d)",
				sqlStr(ch.file.path), ch.file.size, ch.file.mtime, ch.rows))
		}
	}
	srcStmts, err := sourceRows(dataDir, b)
	if err != nil {
		return st, err
	}
	stmts = append(stmts, srcStmts...)
	stmts = append(stmts, "DROP TABLE n", "DROP TABLE IF EXISTS old", "DROP TABLE IF EXISTS aff", "COMMIT")
	return st, execAll(ctx, c, stmts)
}

func applyWinners(ctx context.Context, c *sql.Conn, flagsSQL, inBatch string, st *ConvergeStats) error {
	var firstDoc int64
	if err := c.QueryRowContext(ctx, "SELECT next_doc FROM corpus").Scan(&firstDoc); err != nil {
		return err
	}
	if err := execAll(ctx, c, []string{
		`CREATE OR REPLACE TEMP TABLE win AS SELECT * FROM (
  SELECT * FROM lake_rows WHERE rowid IN (SELECT rowid FROM lake_rows SEMI JOIN aff USING (uid_key)) AND NOT (` + inBatch + `)
  UNION ALL SELECT * FROM n SEMI JOIN aff USING (uid_key))
QUALIFY row_number() OVER (PARTITION BY event_uid ORDER BY ` + winnerOrder + `) = 1`,
		`CREATE OR REPLACE TEMP TABLE cur AS SELECT doc, event_uid, lake_file, lake_row, row_hash, session_id, doc_len
FROM events WHERE rowid IN (SELECT rowid FROM events SEMI JOIN aff USING (uid_key))`,
		"DROP TABLE old", "DROP TABLE aff",
		`CREATE OR REPLACE TEMP TABLE keep AS SELECT c.event_uid, min(c.doc) AS doc
FROM cur c JOIN win w USING (event_uid) WHERE c.row_hash = w.row_hash GROUP BY c.event_uid`,
		`CREATE OR REPLACE TEMP TABLE del AS SELECT c.doc, c.session_id, c.doc_len FROM cur c ANTI JOIN keep k USING (doc)`,
		`CREATE OR REPLACE TEMP TABLE mv AS SELECT c.doc, w.lake_file, w.lake_row
FROM keep k JOIN cur c USING (doc) JOIN win w ON w.event_uid = k.event_uid
WHERE c.lake_file <> w.lake_file OR c.lake_row <> w.lake_row`,
		`CREATE OR REPLACE TEMP TABLE ins AS SELECT w.lake_file, w.lake_row, w.row_hash,
  cast(` + strconv.FormatInt(firstDoc, 10) + ` + row_number() OVER (ORDER BY w.lake_file, w.lake_row) - 1 AS UINTEGER) AS doc
FROM win w ANTI JOIN keep k USING (event_uid)`,
		"DROP TABLE win", "DROP TABLE cur", "DROP TABLE keep",
	}); err != nil {
		return err
	}
	if err := c.QueryRowContext(ctx, "SELECT count(*) FROM ins").Scan(&st.Inserted); err != nil {
		return err
	}
	split := st.Inserted > chunkRows
	for a := firstDoc; a < firstDoc+st.Inserted; a += chunkRows {
		stmts := []string{}
		if split {
			stmts = append(stmts, "COMMIT", "BEGIN")
		}
		if err := execAll(ctx, c, stmts); err != nil {
			return err
		}
		if err := insertChunk(ctx, c, flagsSQL, a, a+chunkRows); err != nil {
			return err
		}
	}
	if split {
		if err := execAll(ctx, c, []string{"COMMIT", "BEGIN"}); err != nil {
			return err
		}
	}

	inDel, deleted, err := docSet(ctx, c, "doc", "del")
	if err != nil {
		return err
	}
	st.Deleted = deleted
	if deleted > 0 {
		stmts := append([]string{
			`UPDATE corpus SET docs = docs - (SELECT count(*) FILTER (WHERE doc_len > 0) FROM del),
  total_len = total_len - (SELECT coalesce(sum(doc_len), 0) FROM del)`,
		}, deletePostings(inDel)...)
		if err := execAll(ctx, c, append(stmts, "DELETE FROM events WHERE "+inDel)); err != nil {
			return err
		}
	}
	if err := c.QueryRowContext(ctx, "SELECT count(*) FROM mv").Scan(&st.Moved); err != nil {
		return err
	}
	if st.Moved > 0 {
		if _, err := c.ExecContext(ctx, "UPDATE events SET lake_file = m.lake_file, lake_row = m.lake_row FROM mv m WHERE events.doc = m.doc"); err != nil {
			return err
		}
	}
	if err := execAll(ctx, c, []string{
		`CREATE OR REPLACE TEMP TABLE s AS SELECT DISTINCT session_id FROM (
  SELECT session_id FROM del
  UNION ALL SELECT session_id FROM events WHERE doc >= ` + strconv.FormatInt(firstDoc, 10) + `
  UNION ALL SELECT session_id FROM events WHERE event_order IS NULL)`,
	}); err != nil {
		return err
	}
	if err := reorder(ctx, c, st); err != nil {
		return err
	}
	return execAll(ctx, c, []string{"DROP TABLE del", "DROP TABLE mv", "DROP TABLE ins", "DROP TABLE s"})
}

func insertChunk(ctx context.Context, c *sql.Conn, flagsSQL string, from, to int64) error {
	if _, err := c.ExecContext(ctx, fmt.Sprintf(
		"CREATE OR REPLACE TEMP TABLE insc AS SELECT * FROM ins WHERE doc >= %d AND doc < %d", from, to)); err != nil {
		return err
	}
	insFiles, err := queryStrings(ctx, c, "SELECT DISTINCT lake_file FROM insc ORDER BY 1")
	if err != nil {
		return err
	}
	var lo, hi int64
	if err := c.QueryRowContext(ctx, "SELECT min(lake_row), max(lake_row) FROM insc").Scan(&lo, &hi); err != nil {
		return err
	}
	files := make([]lakeFile, len(insFiles))
	for i, f := range insFiles {
		files[i] = lakeFile{path: f}
	}
	src := fmt.Sprintf("(SELECT * FROM %s WHERE file_row_number BETWEEN %d AND %d)", parquetSource(files), lo, hi)
	return execAll(ctx, c, []string{
		`CREATE OR REPLACE TEMP TABLE tok AS
SELECT i.lake_file AS filename, cast(i.lake_row AS BIGINT) AS file_row_number, i.doc, i.row_hash,
  ` + flagsSQL + ` AS flags, list_aggregate(r.toks, 'histogram') AS h, cast(len(r.toks) AS UINTEGER) AS doc_len
FROM (SELECT filename, file_row_number, event_kind, payload_type, actor_kind, source_name, tool_name,
    ` + tokenList + ` AS toks FROM ` + src + `) r
JOIN insc i ON i.lake_file = r.filename AND i.lake_row = r.file_row_number`,
		"CREATE OR REPLACE TEMP TABLE w AS SELECT filename, file_row_number, doc, row_hash, flags, doc_len FROM tok",
		insertEventsSQL(src),
		`INSERT INTO postings_delta SELECT ` + postingBucketSQL + `, term, doc, cast(least(tf, 65535) AS USMALLINT), doc_len, flags FROM (
  SELECT doc, doc_len, flags, unnest(map_keys(h)) AS term, unnest(map_values(h)) AS tf FROM tok WHERE doc_len > 0) ORDER BY 1, term`,
		`UPDATE corpus SET docs = docs + (SELECT count(*) FILTER (WHERE doc_len > 0) FROM w),
  total_len = total_len + (SELECT coalesce(sum(doc_len), 0) FROM w),
  next_doc = next_doc + (SELECT count(*) FROM w)`,
		"DROP TABLE w", "DROP TABLE tok", "DROP TABLE insc",
	})
}

const orderSQL = `
SELECT doc, event_order, turn_seq,
  cast(row_number() OVER (PARTITION BY session_id, turn_seq ORDER BY event_order) AS UINTEGER) AS event_ordinal,
  old_order, old_turn, old_ordinal
FROM (
  SELECT doc, session_id, event_order AS old_order, turn_seq AS old_turn, event_ordinal AS old_ordinal,
    row_number() OVER sw AS event_order,
    CASE WHEN coalesce(turn_index, 0) > 0 THEN turn_index
         ELSE cast(greatest(1, sum(CASE WHEN actor_kind = 'user' AND event_kind = 'message' THEN 1 ELSE 0 END)
           OVER (sw ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW)) AS UINTEGER) END AS turn_seq
  FROM events WHERE %s
  WINDOW sw AS (PARTITION BY session_id ORDER BY event_time, source_file,
    source_generation, source_offset, source_line_no, emission_index, event_uid))`

func reorder(ctx context.Context, c *sql.Conn, st *ConvergeStats) error {
	sessions, err := queryStrings(ctx, c, "SELECT session_id FROM s ORDER BY 1")
	if err != nil {
		return err
	}
	st.Sessions = int64(len(sessions))
	if len(sessions) == 0 {
		return nil
	}
	inS := "session_id IN (SELECT session_id FROM s)"
	if len(sessions) <= inlineLimit {
		inS = "session_id IN (" + strings.Join(quoteAll(sessions), ", ") + ")"
	}
	if err := execAll(ctx, c, []string{
		"CREATE OR REPLACE TEMP TABLE ord AS SELECT * FROM (" + fmt.Sprintf(orderSQL, inS) + `)
WHERE event_order IS DISTINCT FROM old_order OR turn_seq IS DISTINCT FROM old_turn OR event_ordinal IS DISTINCT FROM old_ordinal`,
	}); err != nil {
		return err
	}
	in, changed, err := docSet(ctx, c, "events.doc", "ord")
	if err != nil {
		return err
	}
	st.Reordered = changed
	stmts := []string{}
	if changed > 0 {
		stmts = append(stmts, `UPDATE events SET event_order = o.event_order, turn_seq = o.turn_seq, event_ordinal = o.event_ordinal
FROM ord o WHERE events.doc = o.doc AND `+in)
	}
	stmts = append(stmts,
		"DELETE FROM turns WHERE "+inS,
		"DELETE FROM sessions WHERE "+inS,
		`INSERT INTO turns SELECT session_id, turn_seq, count(*),
  max(CASE WHEN payload_type = 'task_complete' THEN 1 ELSE 0 END),
  arg_max(event_uid, event_order) FILTER (WHERE payload_type IN ('task_complete', 'turn_aborted'))
FROM events WHERE `+inS+` GROUP BY session_id, turn_seq`,
		`INSERT INTO sessions SELECT session_id, min(event_time), max(event_time),
  cast(max(turn_seq) AS UINTEGER), cast(count(*) AS UBIGINT),
  cast(count(*) FILTER (WHERE event_kind = 'tool_call') AS UBIGINT),
  cast(count(*) FILTER (WHERE event_kind = 'tool_result') AS UBIGINT),
  cast(count(*) FILTER (WHERE actor_kind = 'user' AND event_kind = 'message') AS UBIGINT),
  cast(count(*) FILTER (WHERE actor_kind = 'assistant' AND event_kind = 'message') AS UBIGINT),
  max(CASE WHEN payload_type = 'task_complete' THEN 1 ELSE 0 END)
FROM events WHERE `+inS+` GROUP BY session_id`,
		"DROP TABLE ord")
	return execAll(ctx, c, stmts)
}

func docSet(ctx context.Context, c *sql.Conn, col, table string) (string, int64, error) {
	var n int64
	if err := c.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		return "", 0, err
	}
	if n > inlineLimit {
		return col + " IN (SELECT doc FROM " + table + ")", n, nil
	}
	docs, err := queryStrings(ctx, c, "SELECT cast(doc AS VARCHAR) FROM "+table+" ORDER BY doc")
	if err != nil {
		return "", 0, err
	}
	if n == 0 {
		return "false", 0, nil
	}
	return col + " IN (" + strings.Join(docs, ", ") + ")", n, nil
}

// sourceRows rewrites the sources row of every live source in b, with events
// the sum of lake_files.rows over its base and tail.
func sourceRows(dataDir string, b []change) ([]string, error) {
	var stmts []string
	seen := map[layout.Key]bool{}
	for _, ch := range b {
		path := filepath.ToSlash(ch.file.path)
		if strings.Contains(path, "/machine="+layout.ArchiveMachine+"/") {
			continue
		}
		m := lakeFileRe.FindStringSubmatch(path)
		if m == nil {
			return nil, fmt.Errorf("lake parquet outside the source layout: %s", ch.file.path)
		}
		k := layout.Key{Author: m[1], Machine: m[2], Harness: m[3], SrcID: m[4]}
		if seen[k] {
			continue
		}
		seen[k] = true
		stmts = append(stmts, "DELETE FROM sources WHERE "+sourceWhere(k))
		src, ok, err := readManifest(k.Manifest(dataDir))
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		base := sourceOf(ch.file.path)
		stmts = append(stmts, sourceInsert(k, src, 0),
			"UPDATE sources SET events = (SELECT coalesce(sum(rows), 0) FROM lake_files WHERE path IN ("+
				sqlStr(base+".parquet")+", "+sqlStr(base+".tail.parquet")+")) WHERE "+sourceWhere(k))
	}
	return stmts, nil
}

func syncManifests(ctx context.Context, c *sql.Conn, dataDir string) error {
	matches, err := filepath.Glob(filepath.Join(dataDir, "raw", "author=*", "machine=*", "harness=*", "*.json"))
	if err != nil {
		return err
	}
	onDisk := map[layout.Key]string{}
	for _, m := range matches {
		parts := strings.Split(filepath.ToSlash(m), "/")
		n := len(parts)
		author, _ := strings.CutPrefix(parts[n-4], "author=")
		machine, _ := strings.CutPrefix(parts[n-3], "machine=")
		harness, _ := strings.CutPrefix(parts[n-2], "harness=")
		onDisk[layout.Key{Author: author, Machine: machine, Harness: harness, SrcID: strings.TrimSuffix(parts[n-1], ".json")}] = m
	}
	type row struct{ size, updatedMs int64 }
	inIndex := map[layout.Key]row{}
	rows, err := c.QueryContext(ctx, "SELECT author, machine_id, harness, src_id, size, updated_ms FROM sources")
	if err != nil {
		return err
	}
	for rows.Next() {
		var k layout.Key
		var r row
		if err := rows.Scan(&k.Author, &k.Machine, &k.Harness, &k.SrcID, &r.size, &r.updatedMs); err != nil {
			rows.Close()
			return err
		}
		inIndex[k] = r
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	var stmts []string
	for k := range inIndex {
		if _, ok := onDisk[k]; !ok {
			stmts = append(stmts, "DELETE FROM sources WHERE "+sourceWhere(k))
		}
	}
	for k, path := range onDisk {
		src, ok, err := readManifest(path)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		r, indexed := inIndex[k]
		switch {
		case !indexed:
			stmts = append(stmts, sourceInsert(k, src, 0))
		case r.size != src.Size || r.updatedMs != src.UpdatedMs:
			stmts = append(stmts, fmt.Sprintf("UPDATE sources SET source_name = %s, source_file = %s, size = %d, head_hash = %s, updated_ms = %d WHERE %s",
				sqlStr(src.SourceName), sqlStr(src.Path), src.Size, sqlStr(src.HeadHash), src.UpdatedMs, sourceWhere(k)))
		}
	}
	if len(stmts) == 0 {
		return nil
	}
	return execAll(ctx, c, append(append([]string{"BEGIN"}, stmts...), "COMMIT"))
}

func readManifest(path string) (manifest, bool, error) {
	var src manifest
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return src, false, nil
	}
	if err != nil {
		return src, false, err
	}
	if err := json.Unmarshal(b, &src); err != nil {
		return src, false, fmt.Errorf("%s: %w", path, err)
	}
	return src, true, nil
}

func sourceWhere(k layout.Key) string {
	return fmt.Sprintf("author = %s AND machine_id = %s AND harness = %s AND src_id = %s",
		sqlStr(k.Author), sqlStr(k.Machine), sqlStr(k.Harness), sqlStr(k.SrcID))
}

func sourceInsert(k layout.Key, src manifest, events int64) string {
	return fmt.Sprintf("INSERT INTO sources VALUES (%s, %s, %s, %s, %s, %s, %d, %s, %d, %d)",
		sqlStr(k.Author), sqlStr(k.Machine), sqlStr(k.Harness), sqlStr(src.SourceName), sqlStr(src.Path),
		sqlStr(k.SrcID), src.Size, sqlStr(src.HeadHash), src.UpdatedMs, events)
}

func quotePaths(files []lakeFile) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = sqlStr(f.path)
	}
	return out
}

func quoteAll(values []string) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = sqlStr(v)
	}
	return out
}

func queryStrings(ctx context.Context, c *sql.Conn, q string) ([]string, error) {
	rows, err := c.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
