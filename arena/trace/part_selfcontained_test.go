package trace

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

func TestOpenedPartListsSessionsAfterItsDirIsGone(t *testing.T) {
	session, err := os.ReadFile("../../testdata/fixtures/claude-code/session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	raw := bytes.Repeat(session, 10)
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
	store := t.TempDir()
	if err := Ingest(root, store, time.Minute, true); err != nil {
		t.Fatal(err)
	}
	parts, _, err := ListParts(store)
	if err != nil || len(parts) != 1 {
		t.Fatalf("parts %v, err %v", parts, err)
	}
	p, err := OpenPartDir(parts[0].Dir)
	if err != nil {
		t.Fatal(err)
	}
	db, err := OpenParts([]*Part{p})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := os.RemoveAll(parts[0].Dir); err != nil {
		t.Fatal(err)
	}
	args := json.RawMessage(`{"start_datetime":"2000-01-01T00:00:00Z","end_datetime":"2100-01-01T00:00:00Z"}`)
	out, _, err := Call(db, "list_sessions", args)
	if err != nil {
		t.Fatalf("list_sessions after the part dir was removed: %v", err)
	}
	if !bytes.Contains(out, []byte(`"session_id"`)) {
		t.Fatalf("no sessions listed: %s", out)
	}
}
