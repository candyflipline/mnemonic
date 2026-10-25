package index

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// TestAppendRebuildsOnlyTheTouchedComponent: a 10-row append is bounded by
// its component. Other hot components whose quiet window the append's
// updated_ms passes do not settle into their cold shards in the append's
// cycle (b would join e's shard), so an append to a hot source writes its
// own part only, and an append to a settled source writes its own part and
// the one shard it left.
func TestAppendRebuildsOnlyTheTouchedComponent(t *testing.T) {
	session, err := os.ReadFile("../../testdata/fixtures/claude-code/session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	shard := func(sid string) uint32 {
		h := fnv.New32a()
		h.Write([]byte(sid))
		return h.Sum32() % partShards
	}
	sid := func(i int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", i) }
	a, b, c := sid(1), sid(2), sid(3)
	sharing := func(with string) string {
		for i := 4; ; i++ {
			if s := sid(i); shard(s) == shard(with) {
				return s
			}
		}
	}
	d, e := sharing(c), sharing(b)
	if len(map[uint32]bool{shard(a): true, shard(b): true, shard(c): true}) != 3 {
		t.Fatal("fixture sessions share a shard")
	}

	root, store := t.TempDir(), t.TempDir()
	dir := filepath.Join(root, "raw", "author=a", "machine=m", "harness=claude-code")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	enc, _ := zstd.NewWriter(nil)
	write := func(sid string, frames int, updatedMs int64) {
		var zst, plain []byte
		for f := range frames {
			lines := bytes.ReplaceAll(session, []byte(fixtureSession), []byte(sid))
			lines = bytes.ReplaceAll(lines, []byte(`"uuid":"`), []byte(fmt.Sprintf(`"uuid":"f%d-`, f)))
			zst = enc.EncodeAll(lines, zst)
			plain = append(plain, lines...)
		}
		m, _ := json.Marshal(map[string]any{"src_id": sid, "harness": "claude-code", "source_name": "claude",
			"path": "/p/" + sid + ".jsonl", "size": len(plain), "updated_ms": updatedMs, "zst_size": len(zst)})
		if err := os.WriteFile(filepath.Join(dir, sid+".json"), m, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, sid+".jsonl.zst"), zst, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const hour = int64(time.Hour / time.Millisecond)
	cycle := func(settleAll bool) CycleStats {
		t.Helper()
		if _, err := Ingest(root, store, time.Hour, settleAll); err != nil {
			t.Fatal(err)
		}
		return takeCycle(store).stats
	}

	for _, s := range []string{a, b, c, d, e} {
		write(s, 1, 1000)
	}
	cycle(true)
	write(a, 2, 2000)
	write(b, 2, 2000)
	cycle(false)

	write(a, 3, 2000+2*hour)
	if got := cycle(false); got.PartsBuilt != 1 || got.HotParts != 2 || got.ColdParts != 2 {
		t.Fatalf("append to hot a: %+v, want 1 part built (a's), a and b hot, the shards of e and c+d cold", got)
	}

	write(c, 2, 2000+4*hour)
	if got := cycle(false); got.PartsBuilt != 2 || got.HotParts != 3 || got.ColdParts != 2 {
		t.Fatalf("append to settled c: %+v, want 2 parts built (c's and the shard d is left in), a, b, c hot, the shards of e and d cold", got)
	}

	db, err := OpenDB(store)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows := map[string]int{}
	for _, p := range db.parts {
		for doc := 0; doc < p.meta.Docs; doc++ {
			r, err := p.row(doc)
			if err != nil {
				t.Fatal(err)
			}
			rows[r.Session]++
		}
	}
	if want := map[string]int{a: 18, b: 12, c: 12, d: 6, e: 6}; fmt.Sprint(rows) != fmt.Sprint(want) {
		t.Fatalf("rows by session %v, want %v", rows, want)
	}
}
