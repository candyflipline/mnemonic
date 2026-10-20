package cache

import (
	"context"
	"database/sql"
	"database/sql/driver"
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

// manifest is one raw/<...>.json file: wire.Source plus the ignored zst_size.
type manifest struct {
	wire.Source
	ZSTSize int64 `json:"zst_size"`
}

// Build writes cache-<ms>.duckdb.tmp from the lake, renames it, and keeps the
// newest two. It returns the zero Stats and no error when nothing changed
// since the newest cache.
func Build(dataDir string) (Stats, error) {
	changed, lakeFiles, err := lakeChanged(dataDir)
	if err != nil {
		return Stats{}, err
	}
	if !changed {
		return Stats{}, nil
	}
	if err := os.MkdirAll(layout.CacheDir(dataDir), 0o755); err != nil {
		return Stats{}, err
	}
	ms := time.Now().UnixMilli()
	tmp := layout.CacheFile(dataDir, ms) + ".tmp"
	st, err := buildFile(tmp, dataDir, lakeFiles)
	if err != nil {
		os.Remove(tmp)
		return Stats{}, err
	}
	final := layout.CacheFile(dataDir, ms)
	if err := os.Rename(tmp, final); err != nil {
		return Stats{}, err
	}
	if err := prune(dataDir); err != nil {
		return Stats{}, err
	}
	st.Path = final
	return st, nil
}

// lakeChanged reports whether any lake parquet or manifest is newer than the
// newest complete cache file, and how many lake parquet files exist.
func lakeChanged(dataDir string) (bool, int, error) {
	var newest int64
	files := 0
	for _, pat := range []string{
		filepath.Join(dataDir, layout.EventsGlob),
		filepath.Join(dataDir, "raw", "author=*", "machine=*", "harness=*", "*.json"),
	} {
		matches, err := filepath.Glob(pat)
		if err != nil {
			return false, 0, err
		}
		for _, m := range matches {
			info, err := os.Stat(m)
			if err != nil {
				return false, 0, err
			}
			if info.ModTime().UnixMilli() > newest {
				newest = info.ModTime().UnixMilli()
			}
			if strings.HasSuffix(m, ".parquet") {
				files++
			}
		}
	}
	caches, err := cacheFiles(dataDir)
	if err != nil {
		return false, 0, err
	}
	if len(caches) == 0 {
		return true, files, nil
	}
	return newest > caches[len(caches)-1].ms, files, nil
}

type cacheFile struct {
	ms   int64
	path string
}

// cacheFiles lists complete cache files oldest first.
func cacheFiles(dataDir string) ([]cacheFile, error) {
	matches, err := filepath.Glob(filepath.Join(layout.CacheDir(dataDir), "cache-*.duckdb"))
	if err != nil {
		return nil, err
	}
	out := make([]cacheFile, 0, len(matches))
	for _, m := range matches {
		base := filepath.Base(m) // cache-<ms>.duckdb
		ms, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(base, "cache-"), ".duckdb"), 10, 64)
		if err != nil {
			continue
		}
		out = append(out, cacheFile{ms: ms, path: m})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ms < out[j].ms })
	return out, nil
}

// prune deletes all but the newest two complete cache files.
func prune(dataDir string) error {
	caches, err := cacheFiles(dataDir)
	if err != nil {
		return err
	}
	for _, c := range caches[:max(0, len(caches)-2)] {
		if err := os.Remove(c.path); err != nil {
			return err
		}
	}
	return nil
}

func buildFile(path, dataDir string, lakeFiles int) (Stats, error) {
	db, err := newDB(path)
	if err != nil {
		return Stats{}, err
	}
	defer db.Close()
	// The events dedupe windows over every payload column; at ten million
	// archive rows that outgrows memory, so it must spill. The service sandbox
	// lets DuckDB write only under the data dir.
	spill := filepath.Join(layout.CacheDir(dataDir), "spill")
	if err := os.MkdirAll(spill, 0o755); err != nil {
		return Stats{}, err
	}
	defer os.RemoveAll(spill)
	for _, stmt := range []string{
		"SET temp_directory = '" + spill + "'",
		"SET preserve_insertion_order = false",
		"SET threads = 2",
		eventsDDL(),
		insertEvents(dataDir, lakeFiles),
		sourcesDDL,
		traceDDL,
		sessionsDDL,
		docsDDL,
		postingsDDL,
		termStatsDDL,
		corpusDDL,
	} {
		if _, err := db.Exec(stmt); err != nil {
			return Stats{}, fmt.Errorf("%s: %w", firstLine(stmt), err)
		}
	}
	if err := insertSources(db, dataDir); err != nil {
		return Stats{}, err
	}
	var st Stats
	st.LakeFiles = lakeFiles
	for _, c := range []struct {
		name string
		n    *int64
	}{{"events", &st.Events}, {"docs", &st.Docs}, {"postings", &st.Postings}} {
		if err := db.QueryRow("SELECT count(*) FROM " + c.name).Scan(c.n); err != nil {
			return Stats{}, err
		}
	}
	if _, err := db.Exec("CHECKPOINT"); err != nil {
		return Stats{}, err
	}
	return st, db.Close()
}

func firstLine(s string) string {
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

// eventsDDL declares the events table even when the lake is empty; the column
// list is the lake parquet column list, so an INSERT needs no type casts.
func eventsDDL() string {
	var b strings.Builder
	b.WriteString("CREATE TABLE events (")
	for i, c := range schema.EventColumns {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(c.Name)
		b.WriteByte(' ')
		b.WriteString(c.DuckDB)
	}
	b.WriteString(", author VARCHAR, machine_id VARCHAR, ingested_at TIMESTAMP)")
	return b.String()
}

// eventCols is every events column in lake order.
func eventCols() []string {
	cols := make([]string, 0, len(schema.EventColumns)+3)
	for _, c := range schema.EventColumns {
		cols = append(cols, c.Name)
	}
	return append(cols, "author", "machine_id", "ingested_at")
}

// insertEvents dedupes the lake into events: highest event_version per
// event_uid, ties broken by newest ingested_at.
func insertEvents(dataDir string, lakeFiles int) string {
	if lakeFiles == 0 {
		return "SELECT 1"
	}
	glob := sqlStr(filepath.Join(dataDir, layout.EventsGlob))
	return "INSERT INTO events SELECT " + strings.Join(eventCols(), ", ") +
		" FROM read_parquet(" + glob + ", hive_partitioning=false) " +
		"QUALIFY row_number() OVER (PARTITION BY event_uid ORDER BY event_version DESC, ingested_at DESC) = 1"
}

// sqlStr quotes a literal for DuckDB SQL.
func sqlStr(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// emissionExpr reads payload_json's moraine_emission_index the way
// JSONExtractUInt does: missing or non-numeric is 0.
const emissionExpr = "coalesce(try_cast(json_extract_string(payload_json, '$.moraine_emission_index') AS UINTEGER), 0)"

// traceDDL ports docs/clickhouse/v_conversation_trace.sql; the v_all_events
// renames (origin_event_id->compacted_parent_uid, actor_kind->actor_role,
// event_kind->event_class, tool_call_id->call_id, tool_name->name,
// turn_index->turn_id, phase=tool_phase or op_status) are inlined. event_time
// mirrors parseDateTime64BestEffortOrNull: TIMESTAMPTZ parsing honors a
// trailing Z or numeric offset, and the connection runs with TimeZone='UTC'
// so zoneless record_ts reads as UTC. json_extract_string+TRY_CAST replaces
// JSONExtractUInt (NULL or non-numeric -> 0), try_cast(x AS UINTEGER)
// replaces toUInt32OrZero.
var traceDDL = fmt.Sprintf(`
CREATE VIEW trace AS
WITH base AS (
    SELECT session_id, session_date, event_uid,
           origin_event_id AS compacted_parent_uid,
           source_file, source_generation, source_line_no, source_offset, source_ref,
           coalesce(try_cast(record_ts AS TIMESTAMPTZ), ingested_at AT TIME ZONE 'UTC')
               AT TIME ZONE 'UTC' AS event_time,
           cast(turn_index AS VARCHAR) AS turn_id,
           actor_kind AS actor_role,
           event_kind AS event_class,
           payload_type,
           tool_call_id AS call_id,
           tool_name AS name,
           coalesce(nullif(tool_phase, ''), op_status) AS phase,
           item_id, text_content, payload_json, token_usage_json, endpoint_kind,
           token_usage_buckets, token_usage_native_units
    FROM events
)
SELECT session_id, session_date, event_uid, compacted_parent_uid,
       source_file, source_generation, source_line_no, source_offset, source_ref,
       event_time,
       row_number() OVER (PARTITION BY session_id ORDER BY event_time, source_file,
           source_generation, source_offset, source_line_no, %[1]s,
           event_uid) AS event_order,
       CASE WHEN coalesce(try_cast(turn_id AS UINTEGER), 0) > 0
            THEN cast(turn_id AS UINTEGER)
            ELSE cast(greatest(1, sum(CASE WHEN actor_role = 'user' AND event_class = 'message'
                                           THEN 1 ELSE 0 END) OVER (
                    PARTITION BY session_id ORDER BY event_time, source_file,
                    source_generation, source_offset, source_line_no,
                    %[1]s, event_uid
                    ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW)) AS UINTEGER)
       END AS turn_seq,
       turn_id, actor_role, event_class, payload_type, call_id, name, phase, item_id,
       text_content, payload_json, token_usage_json, endpoint_kind,
       token_usage_buckets, token_usage_native_units
FROM base`, emissionExpr)

// sessionsDDL ports docs/clickhouse/v_session_summary.sql.
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

// The BM25 inputs follow docs/clickhouse/mv_search_postings.sql with
// schema.sql's term definition: a term is a 2..64 char token of
// lower(text_content), doc_len counts only terms, and docs are the events
// with at least one term. docs, postings and the query-side scorer must use
// the same definition.

const tokenList = "list_filter(regexp_extract_all(lower(text_content), '[a-z0-9_]+'), lambda tok: len(tok) >= 2 AND len(tok) <= 64)"

const docsDDL = `
CREATE TABLE docs AS
SELECT event_uid, cast(n AS UINTEGER) AS doc_len
FROM (SELECT event_uid, len(` + tokenList + `) AS n FROM events)
WHERE n > 0`

// postingsDDL clamps tf to 65535 rather than letting the USMALLINT cast fail:
// a term can occur more than 65535 times in one event's 200k-char
// text_content, and upstream's toUInt16(count()) only wrapped silently.
const postingsDDL = `
CREATE TABLE postings AS
SELECT term, event_uid, cast(least(count(*), 65535) AS USMALLINT) AS tf
FROM (SELECT event_uid, unnest(` + tokenList + `) AS term FROM events)
GROUP BY term, event_uid
ORDER BY term`

const termStatsDDL = `
CREATE TABLE term_stats AS
SELECT term, cast(count(*) AS UBIGINT) AS df
FROM postings
GROUP BY term`

const corpusDDL = `
CREATE TABLE corpus AS
SELECT cast(count(*) AS UBIGINT) AS docs,
       cast(coalesce(avg(doc_len), 0) AS DOUBLE) AS avg_doc_len
FROM docs`

// sourcesDDL is the sources table from schema.sql.
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

// lakeFileRe parses a lake parquet path: the hive prefix and src_id. src_id
// alone is not unique — two authors or machines can hold the same client
// path — so row counts key on the full source identity.
var lakeFileRe = regexp.MustCompile(`author=([^/]*)/machine=([^/]*)/harness=([^/]*)/([0-9a-f]+)\.parquet$`)

type sourceKey struct {
	author, machine, harness, srcID string
}

// insertSources loads the manifest files and counts each source's parquet
// rows; a manifest without a parquet (no parser for that harness) has 0.
func insertSources(db *sql.DB, dataDir string) error {
	counts := map[sourceKey]int64{}
	if paths, _ := filepath.Glob(filepath.Join(dataDir, layout.EventsGlob)); len(paths) > 0 {
		rows, err := db.Query("SELECT filename, count(*) FROM read_parquet(" +
			sqlStr(filepath.Join(dataDir, layout.EventsGlob)) + ", filename=true, hive_partitioning=false) GROUP BY 1")
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
	stmt, err := db.Prepare("INSERT INTO sources VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, m := range matches {
		var src manifest
		b, err := os.ReadFile(m)
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
		if _, err := stmt.Exec(author, machine, harness, src.SourceName, src.Path,
			src.SrcID, src.Size, src.HeadHash, src.UpdatedMs, counts[k]); err != nil {
			return err
		}
	}
	return stmt.Close()
}

// OpenNewest opens the newest complete cache read-only.
func OpenNewest(dataDir string) (*sql.DB, string, error) {
	caches, err := cacheFiles(dataDir)
	if err != nil {
		return nil, "", err
	}
	if len(caches) == 0 {
		return nil, "", fmt.Errorf("no cache in %s: run cache.Build first", layout.CacheDir(dataDir))
	}
	path := caches[len(caches)-1].path
	db, err := newDB(path + "?access_mode=READ_ONLY")
	if err != nil {
		return nil, "", err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, "", err
	}
	return db, path, nil
}
