package index

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/candyflipline/mnemonic/internal/schema"
	"github.com/candyflipline/mnemonic/internal/wire"
)

func TestAnnotateRebuildsNoSessionPart(t *testing.T) {
	const sessA = "11111111-4658-8650-854f-0c666c01d38e"
	const sessB = "22222222-4658-8650-854f-0c666c01d38e"
	root, store := t.TempDir(), t.TempDir()
	writeSource(t, root, "alice", "claude-code", wire.SrcID("a"), "alice.jsonl", plainLines(t, sessA))
	writeSource(t, root, "bob", "claude-code", wire.SrcID("b"), "bob.jsonl", plainLines(t, sessB))
	if _, err := Ingest(root, store, time.Minute, true); err != nil {
		t.Fatal(err)
	}

	counts, authors := lsAndAuthors(t, store)
	before := map[string]string{}
	m, err := readManifest(store)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range m.Parts {
		h, err := dirHash(partDir(store, id))
		if err != nil {
			t.Fatal(err)
		}
		before[id] = h
	}
	if len(before) == 0 {
		t.Fatal("no parts after the settling ingest")
	}

	note := func(sess, id string) []byte {
		line, err := json.Marshal(schema.Annotation{
			ID:     id,
			Target: schema.Target{SessionID: sess},
			Name:   "grade",
			Value:  schema.Label("pass"),
			Source: schema.SourceHuman,
			TS:     time.Date(2026, 10, 26, 16, 0, 0, 0, time.UTC),
		})
		if err != nil {
			t.Fatal(err)
		}
		return append(line, '\n')
	}
	const idA = "0123456789abcdef0123456789abcdef"
	const idB = "fedcba9876543210fedcba9876543210"
	writeSource(t, root, "carol", schema.AnnotationHarness, wire.SrcID("2026-10-26"), "annotation/2026-10-26",
		note(sessA, idA), note(sessB, idB))
	if _, err := Ingest(root, store, time.Minute, false); err != nil {
		t.Fatal(err)
	}

	m, err = readManifest(store)
	if err != nil {
		t.Fatal(err)
	}
	published := map[string]bool{}
	for _, id := range m.Parts {
		published[id] = true
	}
	for id, want := range before {
		if !published[id] {
			t.Fatalf("part %s is no longer published: the annotation cycle rebuilt the session parts", id)
		}
		got, err := dirHash(partDir(store, id))
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("part %s changed: the annotation cycle rewrote it", id)
		}
	}

	counts2, authors2 := lsAndAuthors(t, store)
	for sess, want := range counts {
		if counts2[sess] != want {
			t.Fatalf("%s: event_count %d after the annotation, want %d (the annotation row counted as an event)", sess, counts2[sess], want)
		}
		if !slices.Equal(authors2[sess], authors[sess]) {
			t.Fatalf("%s: authors %v after the annotation, want %v (the annotation's author became a session author)", sess, authors2[sess], authors[sess])
		}
	}

	ix, err := Open(store)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	db, release := ix.Snapshot()
	defer release()
	for sess, id := range map[string]string{sessA: idA, sessB: idB} {
		notes := db.sessionAnnotations(sess)
		if len(notes) != 1 || notes[0].ID != id || notes[0].Author != "carol" {
			b, _ := json.Marshal(notes)
			t.Fatalf("%s: annotations %s, want one %s by carol", sess, b, id)
		}
	}
}

func plainLines(t *testing.T, session string) []byte {
	t.Helper()
	p := filepath.Join(t.TempDir(), "session.jsonl")
	plainSession(t, p, session, "alpha stands.")
	return mustRead(t, p)
}

func lsAndAuthors(t *testing.T, store string) (map[string]int64, map[string][]string) {
	t.Helper()
	ix, err := Open(store)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	db, release := ix.Snapshot()
	defer release()
	data, _, err := Call(t.Context(), nil, db, "list_sessions",
		json.RawMessage(`{"start_datetime":"2026-01-01T00:00:00Z","end_datetime":"2027-01-01T00:00:00Z","limit":20}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(data)
	var out struct {
		Sessions []struct {
			ID      string `json:"id"`
			Session struct {
				EventCount int64 `json:"event_count"`
			} `json:"session"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("list_sessions: %v: %s", err, b)
	}
	counts, authors := map[string]int64{}, map[string][]string{}
	for _, s := range out.Sessions {
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s.ID, "session:"))
		if err != nil {
			t.Fatalf("session id %q: %v", s.ID, err)
		}
		counts[string(raw)] = s.Session.EventCount
		authors[string(raw)] = db.sessionAuthors(string(raw))
	}
	return counts, authors
}

func dirHash(dir string) (string, error) {
	h := sha256.New()
	var names []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		names = append(names, path)
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(names)
	for _, p := range names {
		fi, err := os.Stat(p)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s %d\n", filepath.Base(p), fi.Size())
		b, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		h.Write(b)
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
