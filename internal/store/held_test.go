package store

import (
	"bytes"
	"context"
	"runtime"
	"testing"
	"time"
	"weak"
)

func TestCommitterDropsCompletedFrame(t *testing.T) {
	c, err := open(t.TempDir(), barrier(func(touched) error { return nil }), func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	src, err := NewClientSource("test", "mac", "codex", "held")
	if err != nil {
		t.Fatal(err)
	}
	frame, ticket := func() (weak.Pointer[byte], *Ticket) {
		lines, err := ParseLines(bytes.Repeat([]byte("a whole line\n"), 4096))
		if err != nil {
			t.Fatal(err)
		}
		encoded := encodeFrame(lines)
		ref := weak.Make(&encoded[0])
		ticket, err := c.submit(ctx, src.key, Replace{HeadHash: "head", SourceName: "held", Path: "held"}, encoded, int64(len(lines.b)))
		if err != nil {
			t.Fatal(err)
		}
		return ref, ticket
	}()
	ack, err := ticket.Wait(ctx)
	if err != nil || !ack.Valid() || ack.Verdict() != Applied || ack.State().Size != 53248 {
		t.Fatalf("completed append: valid=%v verdict=%v size=%d err=%v; want APPLIED size=53248", ack.Valid(), ack.Verdict(), ack.State().Size, err)
	}
	// Wait also permits run to advance past commit's ticket resolution. GC
	// clears weak pointers synchronously; no finalizer timing is involved.
	retained := 1
	for ctx.Err() == nil {
		runtime.GC()
		if frame.Value() == nil {
			retained = 0
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if retained != 0 {
		t.Fatalf("completed frames reachable after GC: %d; want 0", retained)
	}
}
