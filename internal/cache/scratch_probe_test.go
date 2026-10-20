package cache

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/duckdb/duckdb-go/v2"
)

func TestScratchProbe4(t *testing.T) {
	db, err := sql.Open("duckdb", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	one := func(q string) string {
		var s sql.NullString
		if err := db.QueryRow(q).Scan(&s); err != nil {
			t.Logf("%s -> ERR %v", q, err)
			return ""
		}
		return s.String
	}
	// record_ts forms: Z with ms, offset, no zone
	t.Logf("z-ms: %v", one(`SELECT try_cast('2026-01-02T03:04:05.678Z' AS TIMESTAMPTZ) AT TIME ZONE 'UTC'`))
	t.Logf("z-noms: %v", one(`SELECT try_cast('2026-01-02T03:04:05Z' AS TIMESTAMPTZ) AT TIME ZONE 'UTC'`))
	t.Logf("offset: %v", one(`SELECT try_cast('2026-01-02T03:04:05.678+09:00' AS TIMESTAMPTZ) AT TIME ZONE 'UTC'`))
	t.Logf("nozone: %v", one(`SELECT try_cast('2026-01-02 03:04:05.678' AS TIMESTAMPTZ) AT TIME ZONE 'UTC'`))
	// local-tz dependence for no-offset strings
	var local string
	db.QueryRow("SELECT current_setting('TimeZone')").Scan(&local)
	t.Logf("session tz: %v", local)
	// read-only db: can we SET TimeZone?
	dir := t.TempDir()
	ro := filepath.Join(dir, "ro.duckdb")
	db2, _ := sql.Open("duckdb", ro)
	db2.Exec(`CREATE TABLE t AS SELECT 1 AS x`)
	db2.Exec(`CHECKPOINT`)
	db2.Close()
	db3, err := sql.Open("duckdb", ro+"?access_mode=READ_ONLY")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db3.Exec(`SET TimeZone='UTC'`); err != nil {
		t.Logf("set tz on read-only: ERR %v", err)
	} else {
		t.Logf("set tz on read-only: ok")
	}
	t.Logf("tz now: %v", one(`SELECT current_setting('TimeZone')`))
	// list_filter lambda
	t.Logf("list_filter: %v", one(`SELECT len(list_filter(regexp_extract_all(lower('a bb ccc DD_dd x'), '[a-z0-9_]+'), tok -> len(tok) >= 2 AND len(tok) <= 64))`))
	// filename format from read_parquet
	db2.Exec("") // noop
	db4, _ := sql.Open("duckdb", filepath.Join(dir, "w.duckdb"))
	db4.Exec(`COPY (SELECT 1 AS a) TO '` + filepath.Join(dir, "f.parquet") + `' (FORMAT parquet)`)
	var fn sql.NullString
	if err := db4.QueryRow(`SELECT filename FROM read_parquet('` + filepath.Join(dir, "*.parquet") + `', filename=true) LIMIT 1`).Scan(&fn); err != nil {
		t.Logf("filename: ERR %v", err)
	} else {
		t.Logf("filename: %q", fn.String)
	}
}
