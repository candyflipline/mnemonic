package keeper

import (
	"context"
	"testing"
	"time"

	"github.com/candyflipline/mnemonic/arena/obj"
)

func TestReadServesAsSoonAsRunStarts(t *testing.T) {
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
	time.Sleep(2500 * time.Millisecond)
	if err := k.Read(func(Head) error { return nil }); err != ErrUnchecked {
		t.Fatalf("2.5 s after Take, before Run: Read = %v, want ErrUnchecked", err)
	}
	go k.Run(ctx, time.Hour)
	var err2 error
	for deadline := time.Now().Add(500 * time.Millisecond); ; time.Sleep(10 * time.Millisecond) {
		if err2 = k.Read(func(Head) error { return nil }); err2 == nil || time.Now().After(deadline) {
			break
		}
	}
	if err2 != nil {
		t.Fatalf("500 ms after Run started with checkEvery 1s: Read = %v, want served", err2)
	}
}
