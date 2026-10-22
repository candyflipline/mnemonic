package keeper

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/candyflipline/mnemonic/arena/obj"
)

func TestIngestStatusRefusesWhileGenUnchecked(t *testing.T) {
	inner, err := obj.Open("file://" + t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &listFails{Store: inner}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	k, err := Take(ctx, s, Config{Tenant: "acme", GrantTTL: time.Second, DeleteSlack: MinDeleteSlack})
	if err != nil {
		t.Fatal(err)
	}
	go k.Run(ctx, time.Hour)
	if _, _, err := k.IngestStatus(nil); err != nil {
		t.Fatalf("fresh keeper: IngestStatus = %v, want served", err)
	}
	s.fail.Store(true)
	time.Sleep(2500 * time.Millisecond)
	if _, _, err := k.IngestStatus(nil); !errors.Is(err, ErrUnchecked) {
		t.Fatalf("2.5 s of failed gen checks: IngestStatus = %v, want ErrUnchecked", err)
	}
}
