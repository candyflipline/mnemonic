package index

import (
	"fmt"
	"hash/fnv"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestSectionHeapDoesNotScaleWithAnotherSession puts one huge session H and
// one small session S in one settled part, at two sizes of H (k and 4k tool
// pairs, 4k about 50 MB of spans JSON). It measures the heap that (a) an
// append to S's source and the ingest cycle that rebuilds its component, and
// (b) sessionAuthors, sessionSpans and sessionAnnotations of S on a freshly
// opened DB, add above the heap before them, at the peak. The growth at 4k
// minus the growth at k must stay under 8 MB: what a call about S holds must
// not depend on the size of H or of the part's other sections.
func TestSectionHeapDoesNotScaleWithAnotherSession(t *testing.T) {
	if testing.Short() {
		t.Skip("builds 150k rows")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(25))
	const k = 15_000
	appendSmall, readSmall := sectionGrowth(t, k)
	appendLarge, readLarge := sectionGrowth(t, 4*k)
	t.Logf("append cycle adds %d B at %d pairs, %d B at %d pairs", appendSmall, k, appendLarge, 4*k)
	t.Logf("reads of S add %d B at %d pairs, %d B at %d pairs", readSmall, k, readLarge, 4*k)
	const bound = 8 << 20
	if d := appendLarge - appendSmall; d > bound {
		t.Fatalf("the append cycle's peak heap grew %d B with H, want <= %d", d, bound)
	}
	if d := readLarge - readSmall; d > bound {
		t.Fatalf("reading S's sections grew the heap %d B with H, want <= %d", d, bound)
	}
}

// sectionGrowth settles H (pairs tool calls) and S into one part, measures the
// reads of S on a fresh DB, then appends a line to S and measures the cycle.
func sectionGrowth(t *testing.T, pairs int) (appendGrowth, readGrowth int64) {
	t.Helper()
	root, store := t.TempDir(), t.TempDir()
	shard := func(sid string) uint32 {
		h := fnv.New32a()
		h.Write([]byte(sid))
		return h.Sum32() % partShards
	}
	const hid = "00000000-0000-4000-8000-0000000000a1"
	sid := ""
	for i := 2; ; i++ {
		if s := fmt.Sprintf("00000000-0000-4000-8000-%012d", i); shard(s) == shard(hid) {
			sid = s
			break
		}
	}
	const hour = int64(time.Hour / time.Millisecond)
	huge := claudeToolRows(hid, pairs)
	small := claudeToolRows(sid, 2)
	writeRawSource(t, root, "h", "m", "claude-code", hid, "/p/"+hid+".jsonl", huge, 1000)
	writeRawSource(t, root, "s", "m", "claude-code", sid, "/p/"+sid+".jsonl", small, 1000)
	huge = nil
	if _, err := Ingest(root, store, time.Hour, true); err != nil {
		t.Fatal(err)
	}
	if n := len(mustManifest(t, store).Parts); n != 1 {
		t.Fatalf("%d parts after the settle, want 1", n)
	}

	readGrowth = peakAbove(func() {
		db, err := OpenDB(store)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if got := db.sessionAuthors(sid); !slices.Equal(got, []string{"s"}) {
			t.Fatalf("authors of S %v, want [s]", got)
		}
		if got := len(db.sessionSpans(sid)); got != 6 {
			t.Fatalf("S has %d spans, want 6", got)
		}
		if got := len(db.sessionAnnotations(sid)); got != 0 {
			t.Fatalf("S has %d annotations, want 0", got)
		}
	})

	small = claudeToolRows(sid, 3)
	writeRawSource(t, root, "s", "m", "claude-code", sid, "/p/"+sid+".jsonl", small, 1000+2*hour)
	appendGrowth = peakAbove(func() {
		if _, err := Ingest(root, store, time.Hour, false); err != nil {
			t.Fatal(err)
		}
	})

	db, err := OpenDB(store)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if got := db.sessionAuthors(sid); !slices.Equal(got, []string{"s"}) {
		t.Fatalf("after the append, authors of S %v, want [s]", got)
	}
	if got := len(db.sessionSpans(sid)); got != 8 {
		t.Fatalf("after the append S has %d spans, want 8", got)
	}
	if got := db.sessionAuthors(hid); !slices.Equal(got, []string{"h"}) {
		t.Fatalf("after the append, authors of H %v, want [h]", got)
	}
	return appendGrowth, readGrowth
}

// claudeToolRows is a Claude Code session: a prompt, then pairs of an
// assistant row calling a tool and the user row returning its result.
func claudeToolRows(sid string, pairs int) []byte {
	t0 := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	ts := func(i int) string { return t0.Add(time.Duration(i) * time.Second).Format("2006-01-02T15:04:05Z") }
	var b strings.Builder
	fmt.Fprintf(&b, `{"type":"user","sessionId":%q,"cwd":"/work/sec","uuid":"p-0","timestamp":%q,"message":{"role":"user","content":"go"}}`+"\n", sid, ts(0))
	for i := 1; i <= pairs; i++ {
		fmt.Fprintf(&b, `{"type":"assistant","sessionId":%q,"cwd":"/work/sec","uuid":"a-%d","requestId":"req-%d","timestamp":%q,"message":{"model":"claude-test","role":"assistant","usage":{"input_tokens":3,"output_tokens":2},"content":[{"type":"tool_use","id":"toolu_%d","name":"Read","input":{"path":"f%d"}}]}}`+"\n", sid, i, i, ts(2*i-1), i, i)
		fmt.Fprintf(&b, `{"type":"user","sessionId":%q,"cwd":"/work/sec","uuid":"u-%d","parentToolUseID":"toolu_%d","toolUseID":"toolu_%d","timestamp":%q,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_%d","content":"ok","is_error":false}]}}`+"\n", sid, i, i, i, ts(2*i), i)
	}
	return []byte(b.String())
}

// peakAbove runs f and returns how far the live heap rose above its size
// before f, at its peak, sampled every 2 ms.
func peakAbove(f func()) int64 {
	runtime.GC()
	debug.FreeOSMemory()
	base := heapBytes()
	peak := base
	stop, done := make(chan struct{}), make(chan struct{})
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	go func() {
		defer close(done)
		for {
			metrics.Read(sample)
			if v := sample[0].Value.Uint64(); v > peak {
				peak = v
			}
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()
	f()
	close(stop)
	<-done
	return int64(peak) - int64(base)
}
