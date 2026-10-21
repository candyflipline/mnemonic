package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/candyflipline/mnemonic/internal/cache/cachetest"
	"github.com/candyflipline/mnemonic/internal/layout"
	"github.com/candyflipline/mnemonic/internal/wire"
)

func TestConverge(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	if err := cachetest.Lake(dataDir, "codex", "claude_code"); err != nil {
		t.Fatal(err)
	}
	ix, err := Open(dataDir, Options{MemoryLimit: "1GB", FlagsSQL: "0::USMALLINT"})
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	counts := func() string {
		var s string
		if err := ix.DB().QueryRow(`SELECT concat_ws(' ', (SELECT count(*) FROM events), (SELECT count(*) FROM postings),
  (SELECT count(*) FROM lake_rows), (SELECT count(*) FROM sessions), (SELECT count(*) FROM turns),
  (SELECT docs || '/' || total_len || '/' || next_doc FROM corpus))`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}

	st, err := ix.Converge(ctx, RetryFailed)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%+v", st); got != "{Files:2 Batches:1 Inserted:21 Deleted:0 Moved:0 Reordered:21 Sessions:2}" {
		t.Fatalf("bootstrap: %s", got)
	}
	if got := counts(); got != "21 48 21 2 2 14/49/21" {
		t.Fatalf("bootstrap counts: %s", got)
	}

	claude := layout.Key{Author: cachetest.Author, Machine: cachetest.Machine, Harness: "claude-code",
		SrcID: wire.SrcID("/fixtures/claude-code/session.jsonl")}
	if err := cachetest.Source(dataDir, claude, "codex"); err != nil {
		t.Fatal(err)
	}
	st, err = ix.Converge(ctx, RetryFailed)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%+v", st); got != "{Files:1 Batches:1 Inserted:0 Deleted:6 Moved:15 Reordered:0 Sessions:1}" {
		t.Fatalf("rewrite: %s", got)
	}
	if got := counts(); got != "15 33 30 1 1 10/34/21" {
		t.Fatalf("rewrite counts: %s", got)
	}

	st, err = ix.Converge(ctx, RetryFailed)
	if err != nil {
		t.Fatal(err)
	}
	if st != (ConvergeStats{}) {
		t.Fatalf("rerun wrote: %+v", st)
	}
}

func TestConvergeKeepsOneDocPerUID(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	if err := cachetest.Lake(dataDir, "claude_code"); err != nil {
		t.Fatal(err)
	}
	ix, err := Open(dataDir, Options{MemoryLimit: "1GB", FlagsSQL: "0::USMALLINT"})
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	if _, err := ix.Converge(ctx, RetryFailed); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"INSERT INTO events SELECT * REPLACE (doc + 1000 AS doc) FROM events",
		"DELETE FROM lake_rows",
		"DELETE FROM lake_files",
	} {
		if _, err := ix.DB().Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	st, err := ix.Converge(ctx, RetryFailed)
	if err != nil {
		t.Fatal(err)
	}
	var got string
	if err := ix.DB().QueryRow("SELECT count(*) || '/' || count(DISTINCT event_uid) FROM events").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "6/6" || st.Inserted != 0 || st.Deleted != 6 {
		t.Fatalf("events %s after %+v", got, st)
	}
}

func TestConvergeReparseKeepsRows(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	if err := cachetest.Lake(dataDir, "claude_code"); err != nil {
		t.Fatal(err)
	}
	ix, err := Open(dataDir, Options{MemoryLimit: "1GB", FlagsSQL: "0::USMALLINT"})
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	if _, err := ix.Converge(ctx, RetryFailed); err != nil {
		t.Fatal(err)
	}
	path := layout.Key{Author: cachetest.Author, Machine: cachetest.Machine, Harness: "claude-code",
		SrcID: wire.SrcID("/fixtures/claude-code/session.jsonl")}.Events(dataDir)
	if _, err := ix.DB().Exec("COPY (SELECT * REPLACE (event_version + 1000 AS event_version, ingested_at + INTERVAL 1 HOUR AS ingested_at) FROM read_parquet(" +
		sqlStr(path) + ")) TO " + sqlStr(path+".tmp") + " (FORMAT parquet)"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		t.Fatal(err)
	}
	st, err := ix.Converge(ctx, RetryFailed)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%+v", st); got != "{Files:1 Batches:1 Inserted:0 Deleted:0 Moved:0 Reordered:0 Sessions:0}" {
		t.Fatalf("re-parse: %s", got)
	}
	var v string
	if err := ix.DB().QueryRow("SELECT min(event_version) || '/' || max(event_version) FROM events").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != "1/1" {
		t.Fatalf("event_version: %s", v)
	}
}

func TestOpenRebuildsOnlyOnStaleMeta(t *testing.T) {
	dataDir := t.TempDir()
	open := func(flags string) error {
		ix, err := Open(dataDir, Options{MemoryLimit: "1GB", FlagsSQL: flags})
		if err == nil {
			ix.Close()
		}
		return err
	}
	broken := func() int {
		m, err := filepath.Glob(IndexPath(dataDir) + ".*.broken")
		if err != nil {
			t.Fatal(err)
		}
		return len(m)
	}
	if err := open("0::USMALLINT"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(IndexPath(dataDir), 0); err != nil {
		t.Fatal(err)
	}
	if err := open("0::USMALLINT"); err == nil || broken() != 0 {
		t.Fatalf("unreadable index: err %v, %d moved aside", err, broken())
	}
	if err := os.Chmod(IndexPath(dataDir), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, flags := range []string{"1::USMALLINT", "2::USMALLINT"} {
		if err := open(flags); err != nil {
			t.Fatal(err)
		}
	}
	if n := broken(); n != 1 {
		t.Fatalf("%d broken indexes kept, want 1", n)
	}
}

func TestConvergeRefreshesSourceRows(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	k := layout.Key{Author: cachetest.Author, Machine: cachetest.Machine, Harness: "raw-only", SrcID: wire.SrcID("/raw-only.jsonl")}
	write := func(size, ms int64) {
		b, err := json.Marshal(wire.Source{SourceName: "raw-only", Path: "/raw-only.jsonl", Size: size, HeadHash: "h", UpdatedMs: ms})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(k.Manifest(dataDir)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(k.Manifest(dataDir), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ix, err := Open(dataDir, Options{MemoryLimit: "1GB", FlagsSQL: "0::USMALLINT"})
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	var got string
	for _, m := range [][2]int64{{10, 1000}, {99999, 2000}} {
		write(m[0], m[1])
		if _, err := ix.Converge(ctx, RetryFailed); err != nil {
			t.Fatal(err)
		}
		if err := ix.DB().QueryRow("SELECT string_agg(size || '/' || updated_ms, ' ') FROM sources").Scan(&got); err != nil {
			t.Fatal(err)
		}
	}
	if got != "99999/2000" {
		t.Fatalf("sources: %s", got)
	}
}

func TestConvergeHoldsFailedBatchUntilReconcile(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	if err := cachetest.Lake(dataDir, "codex", "claude_code"); err != nil {
		t.Fatal(err)
	}
	ix, err := Open(dataDir, Options{MemoryLimit: "1GB", FlagsSQL: "0::USMALLINT"})
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	poison := layout.Key{Author: cachetest.Author, Machine: cachetest.Machine, Harness: "codex", SrcID: wire.SrcID("/poison")}.Events(dataDir)
	if _, err := ix.DB().Exec("COPY (SELECT 1 AS x) TO " + sqlStr(poison) + " (FORMAT parquet)"); err != nil {
		t.Fatal(err)
	}
	events := func() (n int) {
		if err := ix.DB().QueryRow("SELECT count(*) FROM events").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if _, err := ix.Converge(ctx, SkipFailed); err == nil || events() != 0 {
		t.Fatalf("poisoned batch: err %v, %d events", err, events())
	}
	if st, err := ix.Converge(ctx, SkipFailed); err != nil || st.Files != 0 {
		t.Fatalf("push after a failed batch: %+v err %v", st, err)
	}
	if _, err := ix.Converge(ctx, RetryFailed); err == nil || events() != 21 {
		t.Fatalf("reconcile: err %v, %d events", err, events())
	}
}
