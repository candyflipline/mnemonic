package cache

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/candyflipline/mnemonic/internal/cache/cachetest"
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
	if path != st.Path {
		t.Fatalf("OpenNewest %s, built %s", path, st.Path)
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
