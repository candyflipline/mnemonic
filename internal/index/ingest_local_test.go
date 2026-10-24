package index

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/candyflipline/mnemonic/internal/config"
	"github.com/candyflipline/mnemonic/internal/schema"
	"github.com/candyflipline/mnemonic/internal/store"
)

// TestIngestPlainReportsToolRecords pins local mode's contract: an IngestPlain
// over a data dir reports the records the tools write through the store under
// <dataDir>/raw, so an annotation is searchable after the write itself and not
// only after the next harness file change.
func TestIngestPlainReportsToolRecords(t *testing.T) {
	dataDir := t.TempDir()
	const session = "7c666c01-d38e-4658-8650-854ffb5b626e"
	globs := []config.SourceGlob{{
		Harness:    "claude-code",
		SourceName: "claude",
		Glob:       filepath.Join("..", "..", "testdata", "fixtures", "claude-code", "session.jsonl"),
	}}
	const author, machine = "tester", "testbox"
	ix, err := Open(filepath.Join(dataDir, "index"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()

	st, err := ix.IngestPlain(globs, author, machine, IngestOptions{SettleAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if st[Fresh] != 1 {
		t.Fatalf("first ingest: fresh=%d unchanged=%d, want fresh=1 (the session file)", st[Fresh], st[Unchanged])
	}

	// The record annotate appends: one schema.Annotation line through the
	// store under the data dir, the write the local MCP ctx carries.
	line, err := json.Marshal(schema.Annotation{
		ID:     "0123456789abcdef0123456789abcdef",
		Target: schema.Target{SessionID: session},
		Name:   "grade",
		Value:  schema.Label("pass"),
		Source: schema.SourceHuman,
		TS:     time.Date(2026, 1, 19, 16, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	stStore := &store.Store{DataDir: dataDir}
	if _, duplicate, err := stStore.Append(store.Today(author, schema.AnnotationHarness, ""), "", append(line, '\n')); err != nil || duplicate {
		t.Fatalf("append annotation: duplicate=%v err=%v", duplicate, err)
	}

	st, err = ix.IngestPlain(globs, author, machine, IngestOptions{SettleAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if st[Fresh] != 1 || st[Unchanged] != 1 {
		t.Fatalf("second ingest: fresh=%d unchanged=%d, want fresh=1 (the annotation source) unchanged=1 (the session file)", st[Fresh], st[Unchanged])
	}

	db, release := ix.Snapshot()
	defer release()
	notes := db.sessionAnnotations(session)
	if len(notes) != 1 {
		t.Fatalf("sessionAnnotations(%s): %d notes, want 1", session, len(notes))
	}
	if notes[0].Name != "grade" || notes[0].ID != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("annotation is %s=%v id %s, want grade id 0123456789abcdef0123456789abcdef", notes[0].Name, notes[0].Value, notes[0].ID)
	}
	if label, ok := notes[0].Value.Label(); !ok || label != "pass" {
		t.Fatalf("annotation value is %v, want label pass", notes[0].Value)
	}
}
