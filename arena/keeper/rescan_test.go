package keeper

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/candyflipline/mnemonic/arena/obj"
)

func TestSweepDoomsOlderGenObjectUploadedAfterTheFirstScan(t *testing.T) {
	ctx := context.Background()
	s, err := obj.Open("file://" + t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Tenant: "acme", GrantTTL: time.Second, DeleteSlack: MinDeleteSlack}
	k, err := Take(ctx, s, cfg)
	if err != nil {
		t.Fatal(err)
	}
	k.install(Manifest{Tenant: "acme", Gen: k.gen, Seq: 1}, nil, local{})
	t0 := time.Now()
	if err := k.sweep(ctx, t0); err != nil {
		t.Fatal(err)
	}

	stale := partKey("acme", "ab12", k.gen-1)
	if err := s.Put(ctx, stale, bytes.NewReader([]byte("x")), 1); err != nil {
		t.Fatal(err)
	}
	found := t0.Add(11 * time.Minute)
	if err := k.sweep(ctx, found); err != nil {
		t.Fatal(err)
	}
	notBefore, ok := k.doomed[stale]
	if !ok {
		t.Fatalf("an older-gen part put after the first scan was not doomed by a sweep 11 minutes later")
	}
	if want := found.Add(cfg.GrantTTL + cfg.DeleteSlack); notBefore.Before(want) {
		t.Fatalf("notBefore %s is earlier than found+grantTTL+deleteSlack %s", notBefore, want)
	}

	if err := k.sweep(ctx, notBefore); err != nil {
		t.Fatal(err)
	}
	if es, err := s.List(ctx, partPrefix("acme")); err != nil || len(es) != 0 {
		t.Fatalf("after notBefore: parts %v err %v, want none", es, err)
	}
}
