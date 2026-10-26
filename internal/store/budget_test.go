package store

import (
	"bytes"
	"context"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// TestSubmitDropsBody pins lie 3's store half: the budget counts frames, so
// after Submit returns a queued request must hold no uncompressed body — only
// its frame and the metadata plan reads (the body's length). The committer is
// held inside its barrier with the request queued while the caller's copies
// are dropped and the heap is collected: if the request still referenced the
// body, it stays allocated.
func TestSubmitDropsBody(t *testing.T) {
	const bodyBytes = 64 << 20

	var once sync.Once
	entered := make(chan struct{})
	release := make(chan struct{})
	b := barrier(func(ts touched) error {
		once.Do(func() { close(entered) })
		<-release
		return nil
	})
	c, err := open(t.TempDir(), b, func(error) {})
	if err != nil {
		t.Fatal(err)
	}

	heap := func() int64 {
		runtime.GC()
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return int64(ms.HeapAlloc)
	}
	base := heap()

	lines := bytes.Repeat([]byte(strings.Repeat("x", 127)+"\n"), bodyBytes/128)
	l, err := ParseLines(lines)
	if err != nil {
		t.Fatal(err)
	}
	src, err := NewClientSource("a", "m", "h", "s")
	if err != nil {
		t.Fatal(err)
	}
	op := Op(Replace{HeadHash: strings.Repeat("a", 64), SourceName: "s", Path: "p", Lines: l})
	tk, err := c.Submit(context.Background(), src, op)
	if err != nil {
		t.Fatal(err)
	}
	op, lines, l = Op(Replace{}), nil, Lines{} // drop the caller's copies

	<-entered // the committer holds the queued batch inside its barrier
	retained := heap() - base
	close(release)

	a, err := tk.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a.Verdict() != Applied {
		t.Fatalf("verdict %v, want Applied", a.Verdict())
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	for _, s := range c.Sources("a", "m") {
		if s.Size != bodyBytes {
			t.Fatalf("Size %d, want %d: plan's count changed when the body was dropped", s.Size, int64(bodyBytes))
		}
	}
	if retained > bodyBytes/2 {
		t.Fatalf("queued request still holds the uncompressed body: %d bytes retained after Submit returned and the caller dropped its copies; want only the frame", retained)
	}
}
