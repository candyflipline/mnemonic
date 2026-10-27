package index

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestIngestPublishesWhatAnEarlierWriterLeft(t *testing.T) {
	root, store := t.TempDir(), t.TempDir()
	line := func(sid, text string) []byte {
		return []byte(fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":"/w","uuid":"u-%s","timestamp":"2026-03-01T10:00:00Z","message":{"role":"user","content":%q}}`+"\n", sid, sid, text))
	}
	writeRawSource(t, root, "a", "m", "claude-code", "s1", "/p/s1.jsonl", line("11111111-1111-4111-8111-111111111111", "first"), 1)
	if _, err := Ingest(root, store, time.Minute, true); err != nil {
		t.Fatal(err)
	}
	var otherWriter func()
	otherWriter = func() {
		writeRawSource(t, root, "a", "m", "claude-code", "s2", "/p/s2.jsonl", line("22222222-2222-4222-8222-222222222222", "second"), 2)
		if _, err := Ingest(root, store, time.Minute, true); err != nil {
			t.Fatal(err)
		}
		otherWriter = func() {}
	}
	list := func() (*sourceSet, error) {
		sources, err := listSources(root)
		other, lerr := takeLock(store)
		switch {
		case lerr == nil:
			other.f.Close()
			otherWriter()
		case !errors.Is(lerr, ErrLocked):
			t.Fatal(lerr)
		}
		return sources, err
	}
	if _, err := ingest(store, list, time.Minute, true, nil); err != nil {
		t.Fatal(err)
	}
	otherWriter()
	if _, ok := readCkpt(store, "a__m__claude-code__s2"); !ok {
		t.Fatal("s2's checkpoint, published by the ingest that held the lock before this one, is gone: this ingest published a source list read before it took the writer lock")
	}
}
