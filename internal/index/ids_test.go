package index

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

func TestIngestPartIDsAreContentAddressed(t *testing.T) {
	session, err := os.ReadFile("../../testdata/fixtures/claude-code/session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	raw := bytes.Repeat(session, 100)
	enc, _ := zstd.NewWriter(nil)
	zst := enc.EncodeAll(raw, nil)

	root := t.TempDir()
	dir := filepath.Join(root, "raw", "author=a", "machine=m", "harness=claude-code")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	m, _ := json.Marshal(map[string]any{"src_id": "s1", "harness": "claude-code", "source_name": "claude",
		"path": "/p/s1.jsonl", "size": len(raw), "updated_ms": 1, "zst_size": len(zst)})
	if err := os.WriteFile(filepath.Join(dir, "s1.json"), m, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "s1.jsonl.zst"), zst, 0o644); err != nil {
		t.Fatal(err)
	}

	var ids [2][]string
	for i := range ids {
		store := t.TempDir()
		if _, err := Ingest(root, store, time.Minute, true); err != nil {
			t.Fatal(err)
		}
		m, err := readManifest(store)
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = append(ids[i], m.Parts...)
		slices.Sort(ids[i])
		time.Sleep(5 * time.Millisecond)
	}
	if len(ids[0]) != 1 {
		t.Fatalf("want 1 part, got %d", len(ids[0]))
	}
	if !slices.Equal(ids[0], ids[1]) {
		t.Fatalf("fresh ingests of one raw named different parts: %v vs %v", ids[0], ids[1])
	}
}
