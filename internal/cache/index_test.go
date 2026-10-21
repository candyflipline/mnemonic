package cache

import (
	"context"
	"fmt"
	"os"
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

	st, err := ix.Converge(ctx)
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
	st, err = ix.Converge(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%+v", st); got != "{Files:1 Batches:1 Inserted:0 Deleted:6 Moved:15 Reordered:0 Sessions:1}" {
		t.Fatalf("rewrite: %s", got)
	}
	if got := counts(); got != "15 33 30 1 1 10/34/21" {
		t.Fatalf("rewrite counts: %s", got)
	}

	st, err = ix.Converge(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st != (ConvergeStats{}) {
		t.Fatalf("rerun wrote: %+v", st)
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
	if _, err := ix.Converge(ctx); err != nil {
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
	st, err := ix.Converge(ctx)
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
