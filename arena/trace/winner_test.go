package trace

import (
	"maps"
	"testing"
	"time"
)

func TestDuplicateUIDKeepsTheCopyOfTheLastSourceInLakeOrder(t *testing.T) {
	session := fixtureSessionLines(t)
	root := t.TempDir()
	writeSources(t, root, map[string][]byte{"s1": session, "s2": session})
	store := t.TempDir()
	if _, err := Ingest(root, store, time.Minute, true); err != nil {
		t.Fatal(err)
	}
	if got, want := sharedSessionRows(t, store), map[string]int{"/p/s2.jsonl": 6}; !maps.Equal(got, want) {
		t.Fatalf("rows of the shared session by source: %v, want %v", got, want)
	}
}
