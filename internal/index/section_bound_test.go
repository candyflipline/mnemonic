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

func TestSectionHeapDoesNotScaleWithAnotherSession(t *testing.T) {
	if testing.Short() {
		t.Skip("builds 150k rows")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(25))
	const k = 15_000
	holderSmall, cycleSmall, readSmall := sectionGrowth(t, k)
	holderLarge, cycleLarge, readLarge := sectionGrowth(t, 4*k)
	t.Logf("the rebuild's holder step adds %d B at %d pairs, %d B at %d pairs", holderSmall, k, holderLarge, 4*k)
	t.Logf("the whole append cycle adds %d B at %d pairs, %d B at %d pairs", cycleSmall, k, cycleLarge, 4*k)
	t.Logf("reads of S add %d B at %d pairs, %d B at %d pairs", readSmall, k, readLarge, 4*k)
	const bound = 8 << 20
	if d := holderLarge - holderSmall; d > bound {
		t.Fatalf("the holder step's peak heap grew %d B with H, want <= %d", d, bound)
	}
	if d := readLarge - readSmall; d > bound {
		t.Fatalf("reading S's sections grew the heap %d B with H, want <= %d", d, bound)
	}
}

func sectionGrowth(t *testing.T, pairs int) (holderGrowth, cycleGrowth, readGrowth int64) {
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

	holderGrowth = peakAbove(func() {
		mf := mustManifest(t, store)
		p, err := OpenPartDir(partDir(store, mf.Parts[0]))
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		got := memberPlan{}.holderAuthors(p)
		if len(got) != 2 || !slices.Equal(got[sid], []string{"s"}) || !slices.Equal(got[hid], []string{"h"}) {
			t.Fatalf("holder authors %v, want S by s and H by h", got)
		}
	})

	small = claudeToolRows(sid, 3)
	writeRawSource(t, root, "s", "m", "claude-code", sid, "/p/"+sid+".jsonl", small, 1000+2*hour)
	cycleGrowth = peakAbove(func() {
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
	return holderGrowth, cycleGrowth, readGrowth
}

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
