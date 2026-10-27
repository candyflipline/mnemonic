package index

import (
	"testing"
	"time"
)

func TestProvisionalSourceUnchanged(t *testing.T) {
	root, store := t.TempDir(), t.TempDir()
	line := []byte(`{"type":"user","sessionId":"44444444-4444-4444-8444-444444444444","cwd":"/w","uuid":"u-0","message":{"role":"user","content":"no timestamp"}}` + "\n")
	appendRawSource(t, root, "a", "m", "claude-code", "p1", "/p/p1.jsonl", [][]byte{line}, 1)
	if _, err := Ingest(root, store, time.Hour, false); err != nil {
		t.Fatal(err)
	}
	st, err := Ingest(root, store, time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := readCkpt(store, "a__m__claude-code__p1")
	if st != (IngestStats{1, 0, 0, 0}) || c.Gen != 0 {
		t.Fatalf("second ingest: stats %v gen %d, want [1 0 0 0] gen 0", st, c.Gen)
	}
}
