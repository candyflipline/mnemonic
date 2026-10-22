package keeper

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/candyflipline/mnemonic/arena/obj"
	"github.com/candyflipline/mnemonic/arena/trace"
)

func TestParkedReadBlocksNeitherCommitNorReadAndKeepsItsPartOpen(t *testing.T) {
	s, err := obj.Open("file://" + t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	k, err := Take(ctx, s, Config{Tenant: "acme", GrantTTL: time.Second, DeleteSlack: MinDeleteSlack})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	closed := map[*trace.Part]int{}
	k.closePart = func(p *trace.Part) { mu.Lock(); closed[p]++; mu.Unlock() }
	closedCount := func(p *trace.Part) int { mu.Lock(); defer mu.Unlock(); return closed[p] }
	go k.Run(ctx, time.Hour)

	a := &trace.Part{}
	k.install(Manifest{Tenant: "acme", Gen: k.gen, Seq: 1}, nil, local{open: map[string]*trace.Part{"a": a}})

	entered, release, parked := make(chan struct{}), make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	var inFn *trace.Part
	go func() {
		defer close(parked)
		k.Read(func(h Head) error {
			inFn = h.Parts["a"]
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	committed := make(chan struct{})
	go func() {
		k.install(Manifest{Tenant: "acme", Gen: k.gen, Seq: 2}, nil, local{open: map[string]*trace.Part{}})
		close(committed)
	}()
	select {
	case <-committed:
	case <-time.After(time.Second):
		t.Fatal("a commit did not complete within 1s while one Read was parked in fn")
	}

	served := make(chan Seq, 1)
	go func() { k.Read(func(h Head) error { served <- h.Manifest.Seq; return nil }) }()
	select {
	case seq := <-served:
		if seq != 2 {
			t.Fatalf("second Read saw seq %d, want 2", seq)
		}
	case <-time.After(time.Second):
		t.Fatal("a second Read was not served within 1s while one Read was parked in fn")
	}

	if inFn != a || closedCount(a) != 0 {
		t.Fatalf("part dropped by the commit: closed %d times while a Read that saw it is parked, want 0", closedCount(a))
	}
	close(release)
	<-parked
	if n := closedCount(a); n != 1 {
		t.Fatalf("part dropped by the commit: closed %d times after the last Read returned, want 1", n)
	}
}
