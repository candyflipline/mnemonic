package index

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func archParityRow(uid, sid, text string, ver uint64, minute int) chRow {
	ts := time.Date(2026, 3, 1, 10, minute, 0, 0, time.UTC)
	return chRow{
		EventUID: uid, SessionID: sid, RecordTS: ts.Format("2006-01-02T15:04:05Z"), EventTS: ts.UnixMicro(),
		SourceName: "claude-code", Harness: "claude-code", SourceFile: "/orig/" + sid + ".jsonl",
		SourceOffset: uint64(minute), SourceLineNo: uint64(minute),
		EventKind: "message", ActorKind: "user", PayloadType: "message",
		TextContent: text, TextPreview: text,
		EventVersion: ver, Author: "a", MachineID: "archive-ch",
	}
}

func archParityDB(t *testing.T, files map[string][]chRow) *DB {
	t.Helper()
	root, store := t.TempDir(), t.TempDir()
	for name, rows := range files {
		writeChParquet(t, filepath.Join(root, "events", "author=a", "machine=archive-ch", "harness=claude-code", name), rows)
	}
	if _, err := Ingest(root, store, time.Minute, true); err != nil {
		t.Fatal(err)
	}
	db, err := OpenDB(store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return db
}

func TestArchiveParityCrossSessionUID(t *testing.T) {
	const s1, s2 = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	db := archParityDB(t, map[string][]chRow{"ch-export.parquet": {
		archParityRow("dup", s1, "stale copy", 1, 1),
		archParityRow("s1-own", s1, "first session", 1, 2),
		archParityRow("dup", s2, "current copy", 2, 3),
		archParityRow("s2-own", s2, "second session", 2, 4),
	}})
	if n := docsTotal(db); n != 3 {
		t.Fatalf("docs %d, want 3", n)
	}
	if db.scoredDocs != 3 || db.totalLen != 6 {
		t.Fatalf("scored docs %d total len %d, want 3 and 6", db.scoredDocs, db.totalLen)
	}
	if r := findRow(t, db, "dup"); r.Session != s2 || r.Text != "current copy" {
		t.Fatalf("dup kept in %s with %q, want %s with %q", r.Session, r.Text, s2, "current copy")
	}
}

func TestArchiveParityVersionWinner(t *testing.T) {
	const s = "33333333-3333-4333-8333-333333333333"
	db := archParityDB(t, map[string][]chRow{
		"ch-a.parquet": {archParityRow("v", s, "newer copy", 2, 1), archParityRow("a-own", s, "file a", 2, 2)},
		"ch-b.parquet": {archParityRow("v", s, "older copy", 1, 3), archParityRow("b-own", s, "file b", 1, 4)},
	})
	if r := findRow(t, db, "v"); r.Text != "newer copy" {
		t.Fatalf("v kept %q, want %q", r.Text, "newer copy")
	}
}

func TestArchiveParityRawAndArchiveUID(t *testing.T) {
	const sid = "44444444-4444-4444-8444-444444444444"
	line := func(uuid string, minute int, content string) []byte {
		ts := time.Date(2026, 3, 1, 10, minute, 0, 0, time.UTC).Format("2006-01-02T15:04:05Z")
		return []byte(fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":"/work/p","uuid":%q,"timestamp":%q,"message":{"role":"user","content":%q}}`+"\n",
			sid, uuid, ts, content))
	}
	raw := append(line("r-0", 0, "raw zero"), line("r-1", 1, "raw one")...)
	uids := rawUIDs(t, raw)

	root, store := t.TempDir(), t.TempDir()
	writeRawSource(t, root, "a", "m", "claude-code", "s1", "/p/s1.jsonl", raw, 1)
	writeChParquet(t, filepath.Join(root, "events", "author=a", "machine=archive-ch", "harness=claude-code", "ch-export.parquet"), []chRow{
		archParityRow(uids[0], sid, "archive zero", 0, 5),
		archParityRow(uids[1], sid, "archive one", 5, 6),
	})
	if _, err := Ingest(root, store, time.Minute, true); err != nil {
		t.Fatal(err)
	}
	db, err := OpenDB(store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if r := findRow(t, db, uids[0]); r.Text != "raw zero" {
		t.Fatalf("equal versions kept %q, want the raw copy %q", r.Text, "raw zero")
	}
	if r := findRow(t, db, uids[1]); r.Text != "archive one" {
		t.Fatalf("higher archive version kept %q, want %q", r.Text, "archive one")
	}
}
