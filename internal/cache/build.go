package cache

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/candyflipline/mnemonic/internal/layout"
	"github.com/candyflipline/mnemonic/internal/schema"
	"github.com/candyflipline/mnemonic/internal/wire"

	"github.com/duckdb/duckdb-go/v2"
)

type manifest struct {
	wire.Source
	ZSTSize int64 `json:"zst_size"`
}

// Build writes cache-<ms>.duckdb.tmp from the lake, renames it, and keeps the
// newest two. It returns the zero Stats and no error when nothing changed
// since the newest cache. Archive rows (machine=archive-ch) never change, so
// their index lives in archive-<key>.duckdb, built once per set of archive
// files and attached by every cache.
func Build(dataDir string) (Stats, error) {
	changed, err := lakeChanged(dataDir)
	if err != nil {
		return Stats{}, err
	}
	if !changed {
		return Stats{}, nil
	}
	archive, live, err := lakeFiles(dataDir)
	if err != nil {
		return Stats{}, err
	}
	if err := os.MkdirAll(layout.CacheDir(dataDir), 0o755); err != nil {
		return Stats{}, err
	}
	// Build has one caller, the serve loop, so any partial file here belongs
	// to a build a restart killed.
	leftovers, err := filepath.Glob(filepath.Join(layout.CacheDir(dataDir), "*.tmp*"))
	if err != nil {
		return Stats{}, err
	}
	for _, l := range leftovers {
		if err := os.Remove(l); err != nil {
			return Stats{}, err
		}
	}
	spill := filepath.Join(layout.CacheDir(dataDir), "spill")
	if err := os.RemoveAll(spill); err != nil {
		return Stats{}, err
	}
	if err := os.MkdirAll(spill, 0o755); err != nil {
		return Stats{}, err
	}
	defer os.RemoveAll(spill)

	archPath, err := ensureArchive(dataDir, spill, archive)
	if err != nil {
		return Stats{}, err
	}
	ms := time.Now().UnixMilli()
	tmp := layout.CacheFile(dataDir, ms) + ".tmp"
	st, err := buildCache(tmp, spill, archPath, dataDir, live)
	if err != nil {
		os.Remove(tmp)
		return Stats{}, err
	}
	final := layout.CacheFile(dataDir, ms)
	if err := os.Rename(tmp, final); err != nil {
		return Stats{}, err
	}
	if err := prune(dataDir, archPath); err != nil {
		return Stats{}, err
	}
	st.LakeFiles = len(archive) + len(live)
	st.Path = final
	return st, nil
}

func lakeChanged(dataDir string) (bool, error) {
	var newest int64
	for _, pat := range []string{
		filepath.Join(dataDir, layout.EventsGlob),
		filepath.Join(dataDir, "raw", "author=*", "machine=*", "harness=*", "*.json"),
	} {
		matches, err := filepath.Glob(pat)
		if err != nil {
			return false, err
		}
		for _, m := range matches {
			info, err := os.Stat(m)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return false, err
			}
			newest = max(newest, info.ModTime().UnixMilli())
		}
	}
	caches, err := cacheFiles(dataDir)
	if err != nil {
		return false, err
	}
	if len(caches) == 0 {
		return true, nil
	}
	return newest > caches[len(caches)-1].ms, nil
}

type lakeFile struct {
	path  string
	size  int64
	mtime int64
}

func lakeFiles(dataDir string) (archive, live []lakeFile, err error) {
	matches, err := filepath.Glob(filepath.Join(dataDir, layout.EventsGlob))
	if err != nil {
		return nil, nil, err
	}
	sort.Strings(matches)
	for _, m := range matches {
		info, err := os.Stat(m)
		if err != nil {
			return nil, nil, err
		}
		f := lakeFile{path: m, size: info.Size(), mtime: info.ModTime().UnixNano()}
		if strings.Contains(filepath.ToSlash(m), "/machine="+layout.ArchiveMachine+"/") {
			archive = append(archive, f)
		} else {
			live = append(live, f)
		}
	}
	return archive, live, nil
}

type cacheFile struct {
	ms   int64
	path string
}

func cacheFiles(dataDir string) ([]cacheFile, error) {
	matches, err := filepath.Glob(filepath.Join(layout.CacheDir(dataDir), "cache-*.duckdb"))
	if err != nil {
		return nil, err
	}
	out := make([]cacheFile, 0, len(matches))
	for _, m := range matches {
		base := filepath.Base(m)
		ms, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(base, "cache-"), ".duckdb"), 10, 64)
		if err != nil {
			continue
		}
		out = append(out, cacheFile{ms: ms, path: m})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ms < out[j].ms })
	return out, nil
}

func prune(dataDir, archPath string) error {
	caches, err := cacheFiles(dataDir)
	if err != nil {
		return err
	}
	for _, c := range caches[:max(0, len(caches)-2)] {
		if err := os.Remove(c.path); err != nil {
			return err
		}
	}
	archives, err := filepath.Glob(filepath.Join(layout.CacheDir(dataDir), "archive-*.duckdb"))
	if err != nil {
		return err
	}
	for _, a := range archives {
		if a != archPath {
			if err := os.Remove(a); err != nil {
				return err
			}
		}
	}
	return nil
}

func archiveKey(dataDir string, files []lakeFile) string {
	h := sha256.New()
	h.Write([]byte(strings.Join(archiveStmts("src"), "\n") + "\n"))
	for _, f := range files {
		rel, _ := filepath.Rel(dataDir, f.path)
		fmt.Fprintf(h, "%s\t%d\t%d\n", rel, f.size, f.mtime)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func ensureArchive(dataDir, spill string, files []lakeFile) (string, error) {
	path := filepath.Join(layout.CacheDir(dataDir), "archive-"+archiveKey(dataDir, files)+".duckdb")
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	tmp := path + ".tmp"
	os.Remove(tmp)
	err := withBuildConn(tmp, spill, "", func(ctx context.Context, c *sql.Conn) error {
		stmts := archiveStmts("")
		if len(files) > 0 {
			stmts = archiveStmts(parquetSource(files))
		}
		return execAll(ctx, c, stmts)
	})
	if err != nil {
		os.Remove(tmp)
		return "", err
	}
	return path, os.Rename(tmp, path)
}

func archiveStmts(src string) []string {
	stmts := []string{eventsDDL("events"), docsDDL("docs"), postingsDDL("postings")}
	if src == "" {
		return stmts
	}
	stmts = append(stmts,
		"CREATE TEMP TABLE w AS SELECT filename, file_row_number, "+
			"cast(row_number() OVER () AS UINTEGER) AS doc FROM ("+winnersSQL(src)+")",
		insertEventsSQL("events", src),
		"DROP TABLE w")
	return append(stmts, insertPostingsSQL("events", "docs", "postings")...)
}

func buildCache(path, spill, archPath, dataDir string, live []lakeFile) (Stats, error) {
	var st Stats
	err := withBuildConn(path, spill, archPath, func(ctx context.Context, c *sql.Conn) error {
		stmts := []string{
			"CREATE TABLE archive_ref AS SELECT " + sqlStr(filepath.Base(archPath)) + " AS name",
			eventsDDL("live_events"), docsDDL("live_docs"), postingsDDL("live_postings"),
			"CREATE TABLE dropped (doc UINTEGER NOT NULL)",
		}
		if len(live) > 0 {
			src := parquetSource(live)
			stmts = append(stmts,
				"CREATE TEMP TABLE lw AS "+winnersSQL(src),
				`DELETE FROM lw WHERE event_uid IN (
  SELECT a.event_uid FROM arch.events a JOIN lw ON a.event_uid = lw.event_uid
  WHERE a.event_version > lw.event_version)`,
				"INSERT INTO dropped SELECT a.doc FROM arch.events a JOIN lw ON a.event_uid = lw.event_uid",
				`CREATE TEMP TABLE w AS SELECT filename, file_row_number,
  cast((SELECT coalesce(max(doc), 0) FROM arch.events) + row_number() OVER () AS UINTEGER) AS doc FROM lw`,
				insertEventsSQL("live_events", src),
				"DROP TABLE w", "DROP TABLE lw")
			stmts = append(stmts, insertPostingsSQL("live_events", "live_docs", "live_postings")...)
		}
		stmts = append(stmts, viewsSQL...)
		stmts = append(stmts, traceOrderDDL, traceDDL, sessionsDDL, corpusDDL, sourcesDDL)
		if err := execAll(ctx, c, stmts); err != nil {
			return err
		}
		if err := insertSources(ctx, c, dataDir, live); err != nil {
			return err
		}
		for _, q := range []struct {
			sql string
			n   *int64
		}{
			{"SELECT count(*) FROM events", &st.Events},
			{"SELECT docs FROM corpus", &st.Docs},
			{"SELECT count(*) FROM all_postings", &st.Postings},
		} {
			if err := c.QueryRowContext(ctx, q.sql).Scan(q.n); err != nil {
				return err
			}
		}
		return nil
	})
	return st, err
}

// withBuildConn runs fn on one connection of an in-memory DuckDB with path
// attached as the default catalog, written at the latest storage version (the
// only one that zstd-compresses long strings), and archPath attached
// read-only as arch. One connection, because USE is per session.
func withBuildConn(path, spill, archPath string, fn func(context.Context, *sql.Conn) error) error {
	db, err := newDB("")
	if err != nil {
		return err
	}
	defer db.Close()
	ctx := context.Background()
	c, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	setup := []string{
		"SET temp_directory = " + sqlStr(spill),
		"SET preserve_insertion_order = false",
		"SET threads = 2",
		"SET memory_limit = '4GB'",
		"ATTACH " + sqlStr(path) + " AS out (STORAGE_VERSION 'latest')",
		"USE out",
	}
	if archPath != "" {
		setup = append(setup, "ATTACH "+sqlStr(archPath)+" AS arch (READ_ONLY)")
	}
	if err := execAll(ctx, c, setup); err != nil {
		return err
	}
	if err := fn(ctx, c); err != nil {
		return err
	}
	if _, err := c.ExecContext(ctx, "CHECKPOINT out"); err != nil {
		return err
	}
	if _, err := c.ExecContext(ctx, "USE memory"); err != nil {
		return err
	}
	_, err = c.ExecContext(ctx, "DETACH out")
	return err
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
// later connections in the process's local zone and event_time-derived views
// would drift between queries. Views compute event_time at query time, so the
// build and every reader must agree on the zone.
func newDB(dsn string) (*sql.DB, error) {
	c, err := duckdb.NewConnector(dsn, func(execer driver.ExecerContext) error {
		_, err := execer.ExecContext(context.Background(), `SET TimeZone='UTC'`, nil)
		return err
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

func winnersSQL(src string) string {
	return "SELECT filename, file_row_number, event_uid, event_version, ingested_at FROM " + src +
		" QUALIFY row_number() OVER (PARTITION BY event_uid ORDER BY event_version DESC, ingested_at DESC) = 1"
}

var heavyColumns = map[string]bool{"payload_json": true, "token_usage_json": true}

func eventsDDL(table string) string {
	var b strings.Builder
	b.WriteString("CREATE TABLE " + table + " (doc UINTEGER NOT NULL")
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
  tool_io_worktree_root VARCHAR, meta_json VARCHAR USING COMPRESSION zstd)`)
	return b.String()
}

// insertEventsSQL copies the winners in temp table w (filename,
// file_row_number, doc) from src into table, deriving the payload fields:
//   - emission_index: payload_json's moraine_emission_index the way
//     JSONExtractUInt reads it (missing or non-numeric is 0);
//   - tool_io_*: the moraine_tool_io fields of tool requests, ” for a
//     missing field and NULL when the request has no moraine_tool_io (the
//     only rows file_attention reads);
//   - meta_json: the whole payload of session_meta rows and omp title rows
//     (the only rows session titles read).
//
// The contains() guards skip JSON parsing on the rows that cannot match.
func insertEventsSQL(table, src string) string {
	cols := make([]string, 0, len(schema.EventColumns))
	for _, c := range schema.EventColumns {
		if !heavyColumns[c.Name] {
			cols = append(cols, "r."+c.Name)
		}
	}
	tio := func(field string) string {
		return "CASE WHEN tio IS NOT NULL THEN coalesce(json_extract_string(tio, '$." + field + "'), '') END"
	}
	return "INSERT INTO " + table + " SELECT w.doc, " + strings.Join(cols, ", ") + `,
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
    AND json_extract_string(payload_json, '$.type') IN ('title', 'title_change')) THEN payload_json END
FROM (SELECT *, CASE WHEN tool_phase = 'request' AND contains(payload_json, 'moraine_tool_io')
    THEN json_extract(payload_json, '$.moraine_tool_io') END AS tio
  FROM ` + src + `) r
JOIN w ON w.filename = r.filename AND w.file_row_number = r.file_row_number`
}

// A term is a 2..64 char token of lower(text_content), doc_len counts only
// terms, and docs are the events with at least one term. docs, postings and
// the query-side scorer must use the same definition.
const tokenList = "list_filter(regexp_extract_all(lower(text_content), '[a-z0-9_]+'), lambda tok: len(tok) >= 2 AND len(tok) <= 64)"

func docsDDL(table string) string {
	return "CREATE TABLE " + table + " (doc UINTEGER NOT NULL, doc_len UINTEGER NOT NULL)"
}

func postingsDDL(table string) string {
	return "CREATE TABLE " + table +
		" (term VARCHAR NOT NULL, doc UINTEGER NOT NULL, tf USMALLINT NOT NULL, doc_len UINTEGER NOT NULL)"
}

// insertPostingsSQL counts each event's terms in one per-row histogram, so
// no global (term, doc) aggregation spills, then writes postings sorted by
// term: the search's term filter then reads only the row groups that hold
// its terms. tf clamps to 65535 rather than letting the USMALLINT cast fail:
// a term can occur more than 65535 times in one event's 200k-char
// text_content, and upstream's toUInt16(count()) only wrapped silently.
func insertPostingsSQL(events, docs, postings string) []string {
	return []string{
		"CREATE TEMP TABLE tok AS SELECT doc, list_aggregate(toks, 'histogram') AS h, len(toks) AS n FROM (SELECT doc, " +
			tokenList + " AS toks FROM " + events + ")",
		"INSERT INTO " + docs + " SELECT doc, cast(n AS UINTEGER) FROM tok WHERE n > 0",
		"INSERT INTO " + postings + " SELECT term, doc, cast(least(tf, 65535) AS USMALLINT), cast(n AS UINTEGER) FROM (" +
			"SELECT doc, n, unnest(map_keys(h)) AS term, unnest(map_values(h)) AS tf FROM tok WHERE n > 0) ORDER BY term",
		"DROP TABLE tok",
	}
}

var viewsSQL = []string{
	`CREATE VIEW events AS
SELECT * FROM arch.events WHERE doc NOT IN (SELECT doc FROM dropped)
UNION ALL SELECT * FROM live_events`,
	`CREATE VIEW all_postings AS
SELECT * FROM arch.postings WHERE doc NOT IN (SELECT doc FROM dropped)
UNION ALL SELECT * FROM live_postings`,
	`CREATE VIEW docs AS
SELECT e.event_uid, d.doc_len FROM (
  SELECT * FROM arch.docs WHERE doc NOT IN (SELECT doc FROM dropped)
  UNION ALL SELECT * FROM live_docs) d
JOIN events e ON e.doc = d.doc`,
	`CREATE VIEW postings AS
SELECT p.term, e.event_uid, p.tf FROM all_postings p JOIN events e ON e.doc = p.doc`,
}

const corpusDDL = `
CREATE TABLE corpus AS
SELECT cast(sum(n) AS UBIGINT) AS docs,
       cast(coalesce(sum(len) / nullif(sum(n), 0), 0) AS DOUBLE) AS avg_doc_len
FROM (SELECT count(*) AS n, sum(doc_len) AS len FROM arch.docs WHERE doc NOT IN (SELECT doc FROM dropped)
      UNION ALL SELECT count(*), sum(doc_len) FROM live_docs)`

// traceOrderDDL computes each event's place in its session once per build,
// so trace reads need no window over whole sessions (one session holds 1.5M
// events). event_time mirrors parseDateTime64BestEffortOrNull (TIMESTAMPTZ
// parsing honors a trailing Z or numeric offset, and the build runs with
// TimeZone='UTC' so zoneless record_ts reads as UTC).
const traceOrderDDL = `
CREATE TABLE trace_order AS
WITH base AS (
    SELECT doc, session_id, source_file, source_generation, source_offset, source_line_no,
           emission_index, event_uid, turn_index, actor_kind, event_kind,
           coalesce(try_cast(record_ts AS TIMESTAMPTZ), ingested_at AT TIME ZONE 'UTC')
               AT TIME ZONE 'UTC' AS event_time
    FROM events
)
SELECT doc, event_time,
       row_number() OVER w AS event_order,
       CASE WHEN coalesce(turn_index, 0) > 0
            THEN turn_index
            ELSE cast(greatest(1, sum(CASE WHEN actor_kind = 'user' AND event_kind = 'message'
                                           THEN 1 ELSE 0 END) OVER (w
                    ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW)) AS UINTEGER)
       END AS turn_seq
FROM base
WINDOW w AS (PARTITION BY session_id ORDER BY event_time, source_file,
    source_generation, source_offset, source_line_no, emission_index, event_uid)`

const traceDDL = `
CREATE VIEW trace AS
SELECT e.session_id, e.session_date, e.event_uid,
       e.origin_event_id AS compacted_parent_uid,
       e.source_file, e.source_generation, e.source_line_no, e.source_offset, e.source_ref,
       o.event_time, o.event_order, o.turn_seq,
       cast(e.turn_index AS VARCHAR) AS turn_id,
       e.actor_kind AS actor_role,
       e.event_kind AS event_class,
       e.payload_type,
       e.tool_call_id AS call_id,
       e.tool_name AS name,
       coalesce(nullif(e.tool_phase, ''), e.op_status) AS phase,
       e.item_id, e.text_content, e.endpoint_kind,
       e.token_usage_buckets, e.token_usage_native_units
FROM events e JOIN trace_order o ON o.doc = e.doc`

const sessionsDDL = `
CREATE VIEW sessions AS
SELECT session_id,
       min(event_time) AS first_event_time,
       max(event_time) AS last_event_time,
       cast(max(turn_seq) AS UINTEGER) AS total_turns,
       cast(count(*) AS UBIGINT) AS total_events,
       cast(count(*) FILTER (WHERE event_class = 'tool_call') AS UBIGINT) AS tool_calls,
       cast(count(*) FILTER (WHERE event_class = 'tool_result') AS UBIGINT) AS tool_results,
       cast(count(*) FILTER (WHERE actor_role = 'user' AND event_class = 'message') AS UBIGINT) AS user_messages,
       cast(count(*) FILTER (WHERE actor_role = 'assistant' AND event_class = 'message') AS UBIGINT) AS assistant_messages
FROM trace
GROUP BY session_id`

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

var lakeFileRe = regexp.MustCompile(`author=([^/]*)/machine=([^/]*)/harness=([^/]*)/([0-9a-f]+)\.parquet$`)

type sourceKey struct {
	author, machine, harness, srcID string
}

func insertSources(ctx context.Context, c *sql.Conn, dataDir string, live []lakeFile) error {
	counts := map[sourceKey]int64{}
	if len(live) > 0 {
		paths := make([]string, len(live))
		for i, f := range live {
			paths[i] = sqlStr(f.path)
		}
		rows, err := c.QueryContext(ctx, "SELECT file_name, num_rows FROM parquet_file_metadata(["+
			strings.Join(paths, ", ")+"])")
		if err != nil {
			return err
		}
		for rows.Next() {
			var name string
			var n int64
			if err := rows.Scan(&name, &n); err != nil {
				rows.Close()
				return err
			}
			m := lakeFileRe.FindStringSubmatch(filepath.ToSlash(name))
			if m == nil {
				rows.Close()
				return fmt.Errorf("lake parquet outside the source layout: %s", name)
			}
			counts[sourceKey{m[1], m[2], m[3], m[4]}] = n
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}

	matches, err := filepath.Glob(filepath.Join(dataDir, "raw", "author=*", "machine=*", "harness=*", "*.json"))
	if err != nil {
		return err
	}
	stmt, err := c.PrepareContext(ctx, "INSERT INTO sources VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, m := range matches {
		var src manifest
		b, err := os.ReadFile(m)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if err := json.Unmarshal(b, &src); err != nil {
			return fmt.Errorf("%s: %w", m, err)
		}
		rel, err := filepath.Rel(dataDir, m)
		if err != nil {
			return err
		}
		parts := strings.Split(rel, string(filepath.Separator))
		author, _ := strings.CutPrefix(parts[1], "author=")
		machine, _ := strings.CutPrefix(parts[2], "machine=")
		harness, _ := strings.CutPrefix(parts[3], "harness=")
		k := sourceKey{author, machine, harness, src.SrcID}
		if _, err := stmt.ExecContext(ctx, author, machine, harness, src.SourceName, src.Path,
			src.SrcID, src.Size, src.HeadHash, src.UpdatedMs, counts[k]); err != nil {
			return err
		}
	}
	return stmt.Close()
}

// NewestPath is the cache OpenNewest would open. A caller holding that path
// open must not reopen it: DuckDB shares one instance per file in a process,
// so the second ATTACH of the archive fails.
func NewestPath(dataDir string) (string, error) {
	caches, err := cacheFiles(dataDir)
	if err != nil {
		return "", err
	}
	if len(caches) == 0 {
		return "", fmt.Errorf("no cache in %s: run cache.Build first", layout.CacheDir(dataDir))
	}
	return caches[len(caches)-1].path, nil
}

// OpenNewest opens the newest complete cache read-only with its archive
// index attached as arch, which its views read.
func OpenNewest(dataDir string) (*sql.DB, string, error) {
	path, err := NewestPath(dataDir)
	if err != nil {
		return nil, "", err
	}
	db, err := newDB(path + "?access_mode=READ_ONLY")
	if err != nil {
		return nil, "", err
	}
	var name string
	if err := db.QueryRow("SELECT name FROM archive_ref").Scan(&name); err != nil {
		db.Close()
		return nil, "", err
	}
	if _, err := db.Exec("ATTACH " + sqlStr(filepath.Join(layout.CacheDir(dataDir), name)) + " AS arch (READ_ONLY)"); err != nil {
		db.Close()
		return nil, "", err
	}
	return db, path, nil
}
