package index

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/candyflipline/mnemonic/internal/normalize"
	"github.com/candyflipline/mnemonic/internal/schema"
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

// TestHotDeltaWinnerMatchesFullBuild pins the one-winner rule: a uid two
// members of one component share must get the same winner whether the
// component was built in full or grew by a delta append. The archive member
// joins an existing raw session's component, so the append path deltas over
// the raw member's part; the archive member's path (the events dir) sorts
// after the raw path, so the delta's Path-only rule handed the shared uid to
// the archive copy, where the full build's memberBeats keeps the raw copy.
func TestHotDeltaWinnerMatchesFullBuild(t *testing.T) {
	const sid = "42424242-4242-4242-8424-424242424242"
	at := func(min int) (string, int64) {
		ts := time.Date(2026, 3, 2, 10, min, 0, 0, time.UTC)
		return ts.Format("2006-01-02T15:04:05Z"), ts.UnixMicro()
	}
	claudeLine := func(uuid, ts, content string) []byte {
		return []byte(fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":"/work/win","uuid":%q,"timestamp":%q,"message":{"role":"user","content":%q}}`+"\n",
			sid, uuid, ts, content))
	}
	// Eight raw lines; the second's uid the archive file copies.
	var raw []byte
	for i := 1; i <= 8; i++ {
		ts, _ := at(i)
		text := fmt.Sprintf("filler %02d question", i)
		if i == 2 {
			text = "shared beta question"
		}
		raw = append(raw, claudeLine(fmt.Sprintf("raw-%d", i), ts, text)...)
	}
	overlapUID := plantedUIDs(t, raw)[1]

	archRow := func(uid, recordTS string, eventUS int64, text string, ver uint64) chRow {
		return chRow{
			EventUID: uid, SessionID: sid, RecordTS: recordTS, EventTS: eventUS,
			SourceName: "claude-code", Harness: "claude-code",
			SourceFile: "/orig/" + sid + ".jsonl",
			// The columns search's default event-type mask reads.
			EventKind: "message", ActorKind: "user", PayloadType: "message",
			TextContent: text, TextPreview: text,
			EventVersion: ver, Author: "a", MachineID: "archive-ch",
		}
	}
	plantArchive := func(root string) {
		chPath := filepath.Join(root, "events", "author=a", "machine=archive-ch", "harness=claude-code", "ch-export.parquet")
		ts5, us5 := at(30)
		ts6, us6 := at(31)
		writeChParquet(t, chPath, []chRow{
			archRow(overlapUID, ts5, us5, "shared beta ARCHIVE COPY", 100),
			archRow("arch-uid-note", ts6, us6, "archonly delta note", 101),
		})
	}

	// Hot: the raw source builds alone while hot (real member lengths, so
	// the part can act as a delta base), then the archive file joins its
	// session and the component grows by a delta append.
	root, hot := t.TempDir(), t.TempDir()
	writeRawSource(t, root, "a", "a2", "claude-code", "s1", "/p/s1.jsonl", raw, 1)
	if _, err := Ingest(root, hot, 15*time.Minute, false); err != nil {
		t.Fatal(err)
	}
	mf, err := readManifest(hot)
	if err != nil {
		t.Fatal(err)
	}
	if len(mf.Hot) != 1 {
		t.Fatalf("first build: %d hot parts, want 1", len(mf.Hot))
	}
	plantArchive(root)
	if _, err = Ingest(root, hot, 15*time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if mf, err = readManifest(hot); err != nil {
		t.Fatal(err)
	}
	if len(mf.Hot) != 1 {
		t.Fatalf("append: %d hot parts, want 1", len(mf.Hot))
	}
	if _, isDelta, err := readDelta(partDir(hot, mf.Hot[0])); err != nil || !isDelta {
		t.Fatalf("append did not hold a delta: isDelta %v err %v", isDelta, err)
	}
	db, err := OpenDB(hot)
	if err != nil {
		t.Fatal(err)
	}
	got := findRow(t, db, overlapUID)
	db.Close()

	// Cold: the same sources, one full build.
	root2, cold := t.TempDir(), t.TempDir()
	writeRawSource(t, root2, "a", "a2", "claude-code", "s1", "/p/s1.jsonl", raw, 1)
	plantArchive(root2)
	if _, err := Ingest(root2, cold, time.Minute, true); err != nil {
		t.Fatal(err)
	}
	db, err = OpenDB(cold)
	if err != nil {
		t.Fatal(err)
	}
	want := findRow(t, db, overlapUID)
	db.Close()

	if got.Text != want.Text {
		t.Fatalf("hot delta winner %q, full build %q", got.Text, want.Text)
	}
}

// plantedUIDs parses plain lines the way ingest will, so the test can plant a
// raw row's uid in the archive file.
func plantedUIDs(t *testing.T, plain []byte) []string {
	t.Helper()
	parser, ok := normalize.For("claude-code")
	if !ok {
		t.Fatal("no claude-code parser")
	}
	var uids []string
	_, err := parser.Parse(bytes.NewReader(plain),
		normalize.SourceMeta{SourceName: "claude-code", Harness: "claude-code", SourceFile: "/p/s1.jsonl"},
		normalize.State{}, func(ev *schema.Event) error {
			uids = append(uids, ev.EventUID)
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(uids) != 8 {
		t.Fatalf("raw lines yielded %d events, want 8", len(uids))
	}
	return uids
}
