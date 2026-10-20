package cache

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/candyflipline/mnemonic/internal/cache/cachetest"
	"github.com/candyflipline/mnemonic/internal/layout"
	"github.com/candyflipline/mnemonic/internal/wire"
)

// TestBuild covers the whole cache lifecycle over a real two-source lake.
func TestBuild(t *testing.T) {
	dataDir := t.TempDir()
	if err := cachetest.Lake(dataDir, "codex", "claude_code"); err != nil {
		t.Fatal(err)
	}

	st, err := Build(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	// Literals over testdata/goldens codex (15 events) + claude_code (6).
	if st.LakeFiles != 2 || st.Events != 21 || st.Docs != 14 || st.Postings != 48 {
		t.Fatalf("build 1 stats: %+v", st)
	}
	db, path, err := OpenNewest(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if path != st.Path {
		t.Fatalf("OpenNewest %s, built %s", path, st.Path)
	}

	var got string
	// The codex session_meta row carries record_ts 2026-02-15T03:50:40.000Z:
	// the trace must parse it as UTC, not fall back to ingested_at.
	if err := db.QueryRow(`SELECT CAST(event_time AS VARCHAR) FROM trace
		WHERE event_uid = '48c9b0b29f26283de60b80544f64752fdb806352658b0798beb29617b8b170b4'`).
		Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "2026-02-15 03:50:40" {
		t.Fatalf("trace event_time: %q", got)
	}

	var n int64
	if err := db.QueryRow(`SELECT count(*) FROM trace`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 21 {
		t.Fatalf("trace rows: %d", n)
	}
	if err := db.QueryRow(`SELECT count(*) FROM sources WHERE events = 0`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("sources with 0 events: %d", n)
	}
	db.Close()

	st2, err := Build(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if st2 != (Stats{}) {
		t.Fatalf("second build should be a no-op, got %+v", st2)
	}

	touch(t, dataDir)
	st3, err := Build(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if st3.Path == st.Path || st3.Events != st.Events {
		t.Fatalf("rebuild after touch: %+v (was %+v)", st3, st)
	}
	files, err := filepath.Glob(filepath.Join(dataDir, "cache", "cache-*.duckdb"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("kept %d cache files: %v", len(files), files)
	}
	if _, path, err := OpenNewest(dataDir); err != nil || path != st3.Path {
		t.Fatalf("OpenNewest %s, built %s, err %v", path, st3.Path, err)
	}
}

// TestSourcesSameSrcIDTwoMachines: src_id is only the hash of the client
// path, so two machines can hold the same one. Two different goldens share a
// forced src_id here, so src_id-keyed counting would report the same (wrong)
// count for both; each sources row must count its own parquet.
func TestSourcesSameSrcIDTwoMachines(t *testing.T) {
	dataDir := t.TempDir()
	srcID := wire.SrcID("/fixtures/codex/session.jsonl")
	m1 := layout.Key{Author: "test", Machine: "m1", Harness: "codex", SrcID: srcID}
	m2 := layout.Key{Author: "test", Machine: "m2", Harness: "claude-code", SrcID: srcID}
	if err := cachetest.Source(dataDir, m1, "codex"); err != nil {
		t.Fatal(err)
	}
	if err := cachetest.Source(dataDir, m2, "claude_code"); err != nil {
		t.Fatal(err)
	}
	st, err := Build(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if st.LakeFiles != 2 || st.Events != 21 {
		t.Fatalf("stats: %+v", st)
	}
	db, _, err := OpenNewest(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT machine_id, harness, events FROM sources ORDER BY machine_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var machine, harness string
		var events int64
		if err := rows.Scan(&machine, &harness, &events); err != nil {
			t.Fatal(err)
		}
		got = append(got, machine+"/"+harness+"="+strconv.FormatInt(events, 10))
	}
	if len(got) != 2 || got[0] != "m1/codex=15" || got[1] != "m2/claude-code=6" {
		t.Fatalf("sources rows: %v", got)
	}
}

func touch(t *testing.T, dataDir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dataDir, "events", "*", "*", "*", "*.parquet"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("no lake parquet: %v %v", matches, err)
	}
	now := time.Now()
	for _, m := range matches {
		if err := os.Chtimes(m, now, now); err != nil {
			t.Fatal(err)
		}
	}
}
