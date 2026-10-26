package index

import (
	"fmt"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestSettleAfterHotDeltaAllocGrowsLinearlyWithMembers builds one component
// of n archive members and one raw source naming all n sessions, settles it,
// makes it hot with two appends (the second leaves a delta in the manifest),
// then settles it again. The settle builds a cold part over retired archive
// members, so rematerialize and the build open their holder, the delta, whose
// image is decoded on open. At n = 40, 80 and 160 it fails unless the bytes
// the settle cycle allocates per added member stay within 1.75x from the first
// doubling to the second: opening the holder once per member decodes the
// whole component once per member, which doubles that marginal cost.
func TestSettleAfterHotDeltaAllocGrowsLinearlyWithMembers(t *testing.T) {
	a40, a80, a160 := settleAfterDeltaAlloc(t, 40), settleAfterDeltaAlloc(t, 80), settleAfterDeltaAlloc(t, 160)
	first := (float64(a80) - float64(a40)) / 40
	second := (float64(a160) - float64(a80)) / 80
	t.Logf("the settle cycle allocates %d B at 40 members, %d B at 80, %d B at 160: %.0f then %.0f B per added member", a40, a80, a160, first, second)
	if second > 1.75*first {
		t.Fatalf("bytes per added member grew from %.0f (40 to 80) to %.0f (80 to 160), want at most 1.75x", first, second)
	}
}

func settleAfterDeltaAlloc(t *testing.T, n int) uint64 {
	t.Helper()
	root, store := t.TempDir(), t.TempDir()
	sid := func(i int) string { return fmt.Sprintf("%08d-0000-4000-8000-000000000000", i) }
	line := func(i, minute int, uuid string) []byte {
		ts := time.Date(2026, 3, 1, 10, minute, 0, 0, time.UTC).Format("2006-01-02T15:04:05Z")
		return []byte(fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":"/work/p","uuid":%q,"timestamp":%q,"message":{"role":"user","content":"raw line"}}`+"\n",
			sid(i), uuid, ts))
	}
	var first []byte
	var rows []chRow
	for i := range n {
		first = append(first, line(i, 0, fmt.Sprintf("r-%d", i))...)
		rows = append(rows, archParityRow(fmt.Sprintf("arch-%d", i), sid(i), "archive row", 1, 6))
	}
	writeChParquet(t, filepath.Join(root, "events", "author=a", "machine=archive-ch", "harness=claude-code", "ch-export.parquet"), rows)
	chunks := [][]byte{first}
	appendRawSource(t, root, "a", "m", "claude-code", "s1", "/p/s1.jsonl", chunks, 1)
	if _, err := Ingest(root, store, time.Minute, true); err != nil {
		t.Fatal(err)
	}
	for k := range 2 {
		chunks = append(chunks, line(k, k+1, fmt.Sprintf("more-%d", k)))
		appendRawSource(t, root, "a", "m", "claude-code", "s1", "/p/s1.jsonl", chunks, time.Now().Add(time.Hour).UnixMilli())
		if _, err := Ingest(root, store, time.Minute, false); err != nil {
			t.Fatal(err)
		}
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if _, err := Ingest(root, store, time.Minute, true); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}
