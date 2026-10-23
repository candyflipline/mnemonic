package trace

import (
	"bytes"
	"encoding/json"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

const fixtureSession = "7c666c01-d38e-4658-8650-854ffb5b626e"

func writeSources(t *testing.T, root string, raws map[string][]byte) {
	t.Helper()
	dir := filepath.Join(root, "raw", "author=a", "machine=m", "harness=claude-code")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	enc, _ := zstd.NewWriter(nil)
	for id, raw := range raws {
		zst := enc.EncodeAll(raw, nil)
		m, _ := json.Marshal(map[string]any{"src_id": id, "harness": "claude-code", "source_name": "claude",
			"path": "/p/" + id + ".jsonl", "size": len(raw), "updated_ms": 1, "zst_size": len(zst)})
		if err := os.WriteFile(filepath.Join(dir, id+".json"), m, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, id+".jsonl.zst"), zst, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func fixtureSessionLines(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../../testdata/fixtures/claude-code/session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func packedParts(t *testing.T, store string) (ids []string, shas map[string]string, blobs map[string]string) {
	t.Helper()
	parts, _, err := ListParts(store)
	if err != nil {
		t.Fatal(err)
	}
	shas, blobs = map[string]string{}, map[string]string{}
	for _, p := range parts {
		f, err := os.CreateTemp(t.TempDir(), "blob")
		if err != nil {
			t.Fatal(err)
		}
		sha, _, err := Pack(p.Dir, f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, p.ID)
		shas[p.ID], blobs[p.ID] = sha, f.Name()
	}
	slices.Sort(ids)
	return ids, shas, blobs
}

func restoreInto(t *testing.T, store string, ids []string, shas, blobs map[string]string) {
	t.Helper()
	for _, id := range ids {
		_, err := RestorePart(store, id, shas[id], func(w io.Writer) error {
			f, err := os.Open(blobs[id])
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(w, f)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestRestoredStoreIngestsNothing(t *testing.T) {
	session := fixtureSessionLines(t)
	other := bytes.ReplaceAll(session, []byte(fixtureSession), []byte("11111111-d38e-4658-8650-854ffb5b626e"))
	root := t.TempDir()
	writeSources(t, root, map[string][]byte{"s1": bytes.Repeat(session, 20), "s2": bytes.Repeat(other, 20)})

	a := t.TempDir()
	if _, err := Ingest(root, a, time.Minute, true); err != nil {
		t.Fatal(err)
	}
	ids, shas, blobs := packedParts(t, a)

	b := t.TempDir()
	restoreInto(t, b, ids, shas, blobs)
	stats, err := Ingest(root, b, time.Minute, true)
	if err != nil {
		t.Fatal(err)
	}
	if stats != (IngestStats{2, 0, 0, 0}) {
		t.Fatalf("ingest after restore: %v, want [2 0 0 0]", stats)
	}
	got, _, _ := packedParts(t, b)
	want := []string{"2a14222eddb2ae753d677bf80dec852083d212f0a4e02d5f7775a44ec756ee85", "9059d76a94e289df8761710465c07357c67aace0bc88071304e5b6591367f1d6"}
	if !slices.Equal(got, want) || !slices.Equal(ids, want) {
		t.Fatalf("restored store names parts %v, original %v, want %v", got, ids, want)
	}
}

func TestReparseAfterRestoreDoesNotResurrectALoser(t *testing.T) {
	session := fixtureSessionLines(t)
	other := bytes.ReplaceAll(session, []byte(fixtureSession), []byte("11111111-d38e-4658-8650-854ffb5b626e"))
	root := t.TempDir()
	writeSources(t, root, map[string][]byte{"s1": bytes.Repeat(session, 20), "s2": bytes.Repeat(session, 20)})

	a := t.TempDir()
	if _, err := Ingest(root, a, time.Minute, true); err != nil {
		t.Fatal(err)
	}
	ids, shas, blobs := packedParts(t, a)
	b := t.TempDir()
	restoreInto(t, b, ids, shas, blobs)

	writeSources(t, root, map[string][]byte{"s2": bytes.Repeat(other, 20)})
	stats, err := Ingest(root, b, time.Minute, true)
	if err != nil {
		t.Fatal(err)
	}
	if stats != (IngestStats{0, 0, 0, 2}) {
		t.Fatalf("ingest after rewriting the winner: %v, want [0 0 0 2]", stats)
	}
	if got, want := sharedSessionRows(t, b), map[string]int{"/p/s1.jsonl": 6}; !maps.Equal(got, want) {
		t.Fatalf("rows of the shared session by source: %v, want %v", got, want)
	}
}

func TestLoserParsesAgainAfterAFailedCycle(t *testing.T) {
	session := fixtureSessionLines(t)
	other := bytes.ReplaceAll(session, []byte(fixtureSession), []byte("11111111-d38e-4658-8650-854ffb5b626e"))
	root := t.TempDir()
	writeSources(t, root, map[string][]byte{"s1": bytes.Repeat(session, 20), "s2": bytes.Repeat(session, 20)})

	a := t.TempDir()
	if _, err := Ingest(root, a, time.Minute, true); err != nil {
		t.Fatal(err)
	}
	ids, shas, blobs := packedParts(t, a)
	b := t.TempDir()
	restoreInto(t, b, ids, shas, blobs)

	writeSources(t, root, map[string][]byte{"s2": bytes.Repeat(other, 20)})
	blocked := logPath(b, "a__m__claude-code__s1", 1, "light")
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Ingest(root, b, time.Minute, true); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(blocked); err != nil {
		t.Fatal(err)
	}
	if _, err := Ingest(root, b, time.Minute, true); err != nil {
		t.Fatal(err)
	}
	if got, want := sharedSessionRows(t, b), map[string]int{"/p/s1.jsonl": 6}; !maps.Equal(got, want) {
		t.Fatalf("rows of the shared session by source: %v, want %v", got, want)
	}
}

func sharedSessionRows(t *testing.T, b string) map[string]int {
	t.Helper()
	rows := map[string]int{}
	parts, _, err := ListParts(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, lp := range parts {
		p, err := OpenPartDir(lp.Dir)
		if err != nil {
			t.Fatal(err)
		}
		for d := 0; d < p.meta.Docs; d++ {
			r, err := p.row(d)
			if err != nil {
				t.Fatal(err)
			}
			if r.Session == fixtureSession {
				rows[r.SourceFile]++
			}
		}
		p.Close()
	}
	return rows
}
