package index

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

func TestSessionAuthorsLookupAllocatesNothing(t *testing.T) {
	root, store := t.TempDir(), t.TempDir()
	const s1 = "00000000-0000-4000-8000-000000000001"
	const s2 = "00000000-0000-4000-8000-000000000002"
	const s3 = "00000000-0000-4000-8000-000000000003"
	for _, c := range []struct{ author, sid string }{{"b", s1}, {"a", s1}, {"c", s1}, {"a", s2}, {"d", s3}} {
		src := c.author + "-" + c.sid
		writeRawSource(t, root, c.author, "m", "claude-code", src, "/p/"+src+".jsonl", claudeToolRows(c.sid, 2), 1000)
	}
	if _, err := Ingest(root, store, time.Hour, true); err != nil {
		t.Fatal(err)
	}
	db, err := OpenDB(store)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for sid, want := range map[string][]string{s1: {"a", "b", "c"}, s2: {"a"}, s3: {"d"}} {
		if got := db.sessionAuthors(sid); !slices.Equal(got, want) {
			t.Fatalf("authors of %s %v, want %v", sid, got, want)
		}
	}
	if got := db.sessionAuthors("00000000-0000-4000-8000-0000000000ff"); got != nil {
		t.Fatalf("authors of a session the index does not hold %v, want nil", got)
	}
	allocs := testing.AllocsPerRun(100, func() { db.sessionAuthors(s1) })
	if allocs != 0 {
		t.Fatal(fmt.Sprintf("sessionAuthors allocates %v per lookup after warm-up, want 0", allocs))
	}
}
