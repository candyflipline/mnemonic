package index

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/candyflipline/mnemonic/internal/config"
)

// kindState is the one ckpt's rows and gen after an IngestPlain.
func kindState(t *testing.T, store string) (rows int64, gen int) {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(store, "src"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if filepath.Ext(e.Name()) != ".ckpt" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(store, "src", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var c ckpt
		if err := json.Unmarshal(b, &c); err != nil {
			t.Fatal(err)
		}
		return c.Rows, c.Gen
	}
	t.Fatal("no ckpt")
	return 0, 0
}

// TestPlainIngestAppendsAResumedSourceAndReparsesOnAWindowChange pins the
// local-mode byte test (kind.go): appending to a harness file resumes past
// the checkpoint and indexes the appended rows; a rewrite that changes bytes
// inside the checkpoint window re-parses the file from byte 0 into gen+1 and
// keeps every row the file still holds.
func TestPlainIngestAppendsAResumedSourceAndReparsesOnAWindowChange(t *testing.T) {
	session, err := os.ReadFile("../../testdata/fixtures/claude-code/session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	plain := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(plain, session, 0o644); err != nil {
		t.Fatal(err)
	}
	store := t.TempDir()
	globs := []config.SourceGlob{{Harness: "claude-code", SourceName: "claude", Glob: plain}}
	run := func() IngestStats {
		st, err := IngestPlain(store, globs, "a", "m", time.Minute, true)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}

	if st := run(); st != (IngestStats{Fresh: 1}) {
		t.Fatalf("first plain ingest: want fresh=1, got %v", st)
	}
	if st := run(); st != (IngestStats{Unchanged: 1}) {
		t.Fatalf("second plain ingest: want unchanged=1, got %v", st)
	}
	rows0, gen0 := kindState(t, store)
	if rows0 != 6 || gen0 != 0 {
		t.Fatalf("want 6 rows at gen 0, got %d at gen %d", rows0, gen0)
	}

	lines := strings.Split(strings.TrimSuffix(string(session), "\n"), "\n")
	appended := lines[0] + "\n" // a user line: one more user message
	f, err := os.OpenFile(plain, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(appended); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if st := run(); st != (IngestStats{Resumed: 1}) {
		t.Fatalf("append ingest: want resumed=1, got %v", st)
	}
	rows1, gen1 := kindState(t, store)
	if rows1 != 7 || gen1 != 0 {
		t.Fatalf("after append: want 7 rows at gen 0, got %d at gen %d", rows1, gen1)
	}

	changed := string(session) + lines[1] + "\n" + appended // grows the window's tail
	if err := os.WriteFile(plain, []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	if st := run(); st != (IngestStats{Reparsed: 1}) {
		t.Fatalf("window-change ingest: want reparsed=1, got %v", st)
	}
	rows2, gen2 := kindState(t, store)
	if rows2 != 10 || gen2 != 1 {
		t.Fatalf("after reparse: want 10 rows at gen 1, got %d at gen %d", rows2, gen2)
	}
}
