package index

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

const fixtureSession = "7c666c01-d38e-4658-8650-854ffb5b626e"

func TestDuplicateUIDKeepsTheCopyOfTheLastSourceInLakeOrder(t *testing.T) {
	session, err := os.ReadFile("../../testdata/fixtures/claude-code/session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	dir := filepath.Join(root, "raw", "author=a", "machine=m", "harness=claude-code")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	enc, _ := zstd.NewWriter(nil)
	for _, id := range []string{"s1", "s2"} {
		zst := enc.EncodeAll(session, nil)
		m, _ := json.Marshal(map[string]any{"src_id": id, "harness": "claude-code", "source_name": "claude",
			"path": "/p/" + id + ".jsonl", "size": len(session), "updated_ms": 1, "zst_size": len(zst)})
		if err := os.WriteFile(filepath.Join(dir, id+".json"), m, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, id+".jsonl.zst"), zst, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	store := t.TempDir()
	if _, err := Ingest(root, store, time.Minute, true); err != nil {
		t.Fatal(err)
	}
	db, err := OpenDB(store)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got := map[string]int{}
	for _, p := range db.parts {
		for d := 0; d < p.meta.Docs; d++ {
			r, err := p.row(d)
			if err != nil {
				t.Fatal(err)
			}
			if r.Session == fixtureSession {
				got[r.SourceFile]++
			}
		}
	}
	if want := map[string]int{"/p/s2.jsonl": 6}; !maps.Equal(got, want) {
		t.Fatalf("rows of the shared session by source: %v, want %v", got, want)
	}
}
