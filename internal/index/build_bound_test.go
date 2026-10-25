package index

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"strings"
	"testing"
	"time"
)

// TestBuildHeapGrowsBoundedPerWinnerRow builds one part from a store holding
// one session of N rows of about 1 KB each, at two sizes, and fails unless the
// heap the build adds at its peak grows by at most 300 bytes per extra winner
// row. The heap before the build is subtracted: it holds the checkpoint the
// ingest left, whose parser state grows with the rows and is the build's
// input, not its working set. The part build must not hold a decoded row per
// winner.
func TestBuildHeapGrowsBoundedPerWinnerRow(t *testing.T) {
	small, large := 50_000, 200_000
	if testing.Short() {
		t.Skip("builds 250k rows")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(25))
	growthSmall := buildGrowth(t, small)
	growthLarge := buildGrowth(t, large)
	perRow := (float64(growthLarge) - float64(growthSmall)) / float64(large-small)
	t.Logf("the build adds %d B at %d rows, %d B at %d rows: %.1f B per added row", growthSmall, small, growthLarge, large, perRow)
	if perRow > 300 {
		t.Fatalf("the build's peak heap grew %.1f B per added winner row, want <= 300", perRow)
	}
}

// buildGrowth ingests one session of n rows, then rebuilds its part with
// buildIndex while a sampler records the heap, and returns how far the heap
// rose above its size before the build, at its peak. It also checks the part
// holds n docs and reads one row back.
func buildGrowth(t *testing.T, n int) int64 {
	t.Helper()
	root, store := t.TempDir(), t.TempDir()
	const sid = "b0b0b0b0-1c3b-4e5f-8a7b-9d2e4f6a8b0c"
	vocab := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel"}
	rng := rand.New(rand.NewSource(1))
	t0 := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	var raw []byte
	var sb strings.Builder
	var lastText string
	const turnRows = 50
	for i := 1; i <= n; i++ {
		sb.Reset()
		for sb.Len() < 1000 {
			sb.WriteString(vocab[rng.Intn(len(vocab))])
			sb.WriteByte(' ')
		}
		lastText = sb.String()
		ts := t0.Add(time.Duration(i) * time.Second).Format("2006-01-02T15:04:05Z")
		if (i-1)%turnRows == 0 {
			raw = append(raw, fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":"/work/win","uuid":"row-%d","timestamp":%q,"message":{"role":"user","content":%q}}`+"\n",
				sid, i, ts, sb.String())...)
		} else {
			raw = append(raw, fmt.Sprintf(`{"type":"assistant","sessionId":%q,"cwd":"/work/win","uuid":"row-%d","requestId":"req-%d","timestamp":%q,"message":{"model":"claude-test","role":"assistant","content":[{"type":"text","text":%q}]}}`+"\n",
				sid, i, i/turnRows, ts, sb.String())...)
		}
	}
	writeRawSource(t, root, "a", "m", "claude-code", "big", "/p/big.jsonl", raw, 1)
	raw = nil
	if _, err := Ingest(root, store, 15*time.Minute, false); err != nil {
		t.Fatal(err)
	}
	mf := mustManifest(t, store)
	if len(mf.Parts) != 1 {
		t.Fatalf("%d parts, want 1", len(mf.Parts))
	}
	cks := memberCks(t, store, mf.Parts[0])
	if len(cks) != 1 {
		t.Fatalf("%d members, want 1", len(cks))
	}
	dir := filepath.Join(t.TempDir(), "part")

	runtime.GC()
	debug.FreeOSMemory()
	var peak uint64
	base := heapBytes()
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
	err := buildIndex(store, cks, "bound", dir, nil)
	close(stop)
	<-done
	if err != nil {
		t.Fatal(err)
	}

	p, err := OpenPartDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if p.meta.Docs != n {
		t.Fatalf("part holds %d docs, want %d", p.meta.Docs, n)
	}
	r, err := p.row(n - 1)
	if err != nil {
		t.Fatal(err)
	}
	if r.Session != sid || r.Text != lastText {
		t.Fatalf("last doc: session %q, %d text bytes, want %q and %d", r.Session, len(r.Text), sid, len(lastText))
	}
	t.Logf("%d rows: heap %d B before the build, peak %d B", n, base, peak)
	return int64(peak) - int64(base)
}

func heapBytes() uint64 {
	s := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(s)
	return s[0].Value.Uint64()
}
