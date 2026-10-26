package index

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/candyflipline/mnemonic/internal/schema"
	"github.com/candyflipline/mnemonic/internal/wire"
	"github.com/klauspost/compress/zstd"
)

// TestAnnotateRebuildsNoSessionPart pins the annotate contract: an annotate
// call makes the next put cycle build at most the annotation's own data — the
// settled session parts stay published and unchanged, the sessions' event
// counts and authors are untouched, and sessionAnnotations answers the new
// annotation for each target session.
func TestAnnotateRebuildsNoSessionPart(t *testing.T) {
	root, store := t.TempDir(), t.TempDir()
	const (
		aliceSess = "11111111-1111-4111-8111-111111111111"
		bobSess   = "22222222-2222-4222-8222-222222222222"
	)
	annotRebuildSource(t, root, "alice", "claude-code", "s1", annotSessionLines(aliceSess))
	annotRebuildSource(t, root, "bob", "claude-code", "s1", annotSessionLines(bobSess))
	if _, err := Ingest(root, store, time.Hour, true); err != nil {
		t.Fatal(err)
	}

	db0, err := OpenDB(store)
	if err != nil {
		t.Fatal(err)
	}
	events0 := annotEventCounts(t, db0, aliceSess, bobSess)
	authors0 := map[string][]string{
		aliceSess: db0.sessionAuthors(aliceSess),
		bobSess:   db0.sessionAuthors(bobSess),
	}
	man0, err := readManifest(store)
	if err != nil {
		t.Fatal(err)
	}
	hashes := map[string]string{}
	for _, id := range man0.Parts {
		h, err := annotPartHash(partDir(store, id))
		if err != nil {
			t.Fatal(err)
		}
		hashes[id] = h
	}
	db0.Close()

	// One annotation source by carol, one annotation per session, then one
	// non-settling cycle over it.
	ts := time.Date(2026, 1, 20, 12, 0, 0, 0, time.UTC)
	ann := func(id, sess string) schema.Annotation {
		return schema.Annotation{ID: id, Target: schema.Target{SessionID: sess},
			Name: "grade", Value: schema.Label("pass"), Source: schema.SourceHuman, TS: ts}
	}
	annotRebuildSource(t, root, "carol", schema.AnnotationHarness, "ann1", []string{
		annotLine(t, ann("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", aliceSess)),
		annotLine(t, ann("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", bobSess)),
	})
	if _, err := Ingest(root, store, time.Hour, false); err != nil {
		t.Fatal(err)
	}

	// (a) every part recorded before is still published and unchanged.
	man1, err := readManifest(store)
	if err != nil {
		t.Fatal(err)
	}
	published := map[string]bool{}
	for _, id := range man1.Parts {
		published[id] = true
	}
	for _, id := range man0.Parts {
		if !published[id] {
			t.Fatalf("annotating unpublished part %s: CURRENT no longer names it", id)
		}
		h, err := annotPartHash(partDir(store, id))
		if err != nil {
			t.Fatal(err)
		}
		if h != hashes[id] {
			t.Fatalf("annotating rewrote part %s", id)
		}
	}

	// (b) both sessions' event counts and authors are unchanged.
	db1, err := OpenDB(store)
	if err != nil {
		t.Fatal(err)
	}
	defer db1.Close()
	events1 := annotEventCounts(t, db1, aliceSess, bobSess)
	for _, sess := range []string{aliceSess, bobSess} {
		if events1[sess] != events0[sess] {
			t.Fatalf("session %s: event_count after annotate = %d, want %d", sess, events1[sess], events0[sess])
		}
		if got, want := db1.sessionAuthors(sess), authors0[sess]; fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("session %s: authors after annotate = %v, want %v", sess, got, want)
		}
	}

	// (c) sessionAnnotations answers the one annotation per session, carol's.
	for sess, id := range map[string]string{aliceSess: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", bobSess: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"} {
		notes := db1.sessionAnnotations(sess)
		if len(notes) != 1 {
			t.Fatalf("sessionAnnotations(%s): %d notes, want 1", sess, len(notes))
		}
		if notes[0].ID != id {
			t.Fatalf("sessionAnnotations(%s): id %s, want %s", sess, notes[0].ID, id)
		}
		if notes[0].Author != "carol" {
			t.Fatalf("sessionAnnotations(%s): author %q, want carol", sess, notes[0].Author)
		}
	}
}

// annotSessionLines is a two-line claude-code session whose id is session.
func annotSessionLines(session string) []string {
	return []string{
		fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":"/work/demo","uuid":"u1","timestamp":"2026-01-19T15:58:40.000Z","message":{"role":"user","content":"quorum stands."}}`, session),
		fmt.Sprintf(`{"type":"assistant","sessionId":%q,"cwd":"/work/demo","uuid":"a1","parentUuid":"u1","requestId":"req-1","timestamp":"2026-01-19T15:58:41.421Z","message":{"model":"claude-opus-4-5-20251101","role":"assistant","usage":{"input_tokens":9,"output_tokens":5},"content":[{"type":"text","text":"done."}]}}`, session),
	}
}

// annotLine is the raw line q_annotate appends: the annotation as JSON.
func annotLine(t *testing.T, a schema.Annotation) string {
	t.Helper()
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// annotRebuildSource writes one pushed source: the lines as one zstd frame
// under root's raw layout plus its manifest, updated at a fixed old time.
func annotRebuildSource(t *testing.T, root, author, harness, srcID string, lines []string) {
	t.Helper()
	var frame bytes.Buffer
	w, err := zstd.NewWriter(&frame)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lines {
		if _, err := w.Write([]byte(l + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	m := struct {
		wire.Source
		ZstSize int64 `json:"zst_size"`
	}{wire.Source{
		SrcID: srcID, Harness: harness, SourceName: harness, Path: "/p/" + srcID + ".jsonl",
		Size: int64(frame.Len()), UpdatedMs: 1000,
	}, int64(frame.Len())}
	key := "author=" + author + "/machine=box/harness=" + harness
	dir := filepath.Join(root, "raw", key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, srcID+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, srcID+".jsonl.zst"), frame.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// annotEventCounts is each session's list_sessions event_count.
func annotEventCounts(t *testing.T, db *DB, sessions ...string) map[string]int64 {
	t.Helper()
	data, _, err := Call(context.Background(), db, "list_sessions",
		json.RawMessage(`{"start_datetime":"2026-01-01T00:00:00Z","end_datetime":"2026-03-01T00:00:00Z","limit":25}`))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Sessions []struct {
			ID      string `json:"id"`
			Session struct {
				EventCount int64 `json:"event_count"`
			} `json:"session"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	byID := map[string]int64{}
	for _, s := range out.Sessions {
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s.ID, "session:"))
		if err != nil {
			t.Fatalf("list_sessions id %q: %v", s.ID, err)
		}
		byID[string(raw)] = s.Session.EventCount
	}
	want := map[string]int64{}
	for _, sess := range sessions {
		n, ok := byID[sess]
		if !ok {
			t.Fatalf("list_sessions does not list %s", sess)
		}
		want[sess] = n
	}
	return want
}

// annotPartHash hashes every file of a part dir, so a rewrite shows.
func annotPartHash(dir string) (string, error) {
	h := sha256.New()
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range ents {
		if e.IsDir() {
			return "", fmt.Errorf("unexpected subdir %s in %s", e.Name(), dir)
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s %d\n", e.Name(), len(b))
		h.Write(b)
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
