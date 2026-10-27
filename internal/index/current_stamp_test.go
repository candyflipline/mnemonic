package index

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/candyflipline/mnemonic/internal/config"
	"github.com/candyflipline/mnemonic/internal/redact"
)

func TestCurrentStampChangesWhenContentChangesInPlace(t *testing.T) {
	store := t.TempDir()
	cur := filepath.Join(store, "CURRENT")
	if err := os.WriteFile(cur, []byte(`{"parts":["a"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	before, ok := CurrentStamp(store)
	if !ok {
		t.Fatal("no stamp")
	}
	if err := os.WriteFile(cur, []byte(`{"parts":["b"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	after, _ := CurrentStamp(store)
	if before == after {
		t.Fatalf("stamp %d unchanged after CURRENT's bytes changed", before)
	}
}

func TestOpenCurrentNamingMissingPart(t *testing.T) {
	store := filepath.Join(t.TempDir(), "index")

	ix, err := Open(store)
	if err != nil {
		t.Fatalf("open a store without CURRENT: %v", err)
	}

	root := t.TempDir()
	src := filepath.Join(root, "session.jsonl")
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", "claude-code", "session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.IngestPlain([]config.SourceGlob{{
		Harness:    "claude-code",
		SourceName: "claude",
		Glob:       src,
	}}, "tester", "testbox", redact.Default(), IngestOptions{SettleAll: true}); err != nil {
		t.Fatal(err)
	}
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}
	mb, err := os.ReadFile(filepath.Join(store, "CURRENT"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Parts []string `json:"parts"`
	}
	if err := json.Unmarshal(mb, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Parts) == 0 {
		t.Fatalf("CURRENT names no parts: %s", mb)
	}

	if err := os.RemoveAll(filepath.Join(store, "parts", m.Parts[0])); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(store); err == nil {
		t.Fatal("Open with a deleted part: err=<nil>, want the missing part named")
	}

	openRetryHook = func() {
		if err := os.WriteFile(filepath.Join(store, "CURRENT"), []byte(`{"parts":[],"sources":[]}`), 0o644); err != nil {
			t.Error(err)
		}
	}
	defer func() { openRetryHook = nil }()
	ix2, err := Open(store)
	if err != nil {
		t.Fatalf("open after the newer CURRENT: %v", err)
	}
	db, release := ix2.Snapshot()
	if n := len(db.parts); n != 0 {
		t.Errorf("parts=%d, want 0 (the newer CURRENT names none)", n)
	}
	release()
	if err := ix2.Close(); err != nil {
		t.Fatal(err)
	}
}
