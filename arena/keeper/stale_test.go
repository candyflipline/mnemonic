package keeper

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/candyflipline/mnemonic/arena/obj"
)

type listFails struct {
	obj.Store
	fail atomic.Bool
}

func (s *listFails) List(ctx context.Context, prefix obj.Key) ([]obj.Entry, error) {
	if s.fail.Load() {
		return nil, errors.New("list unavailable")
	}
	return s.Store.List(ctx, prefix)
}

func TestReadRefusesWhileGenUnchecked(t *testing.T) {
	inner, err := obj.Open("file://" + t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &listFails{Store: inner}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	k, err := Take(ctx, s, Config{Tenant: "acme", GrantTTL: time.Second, DeleteSlack: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	go k.Run(ctx, time.Hour)
	served := func() (bool, error) {
		var ran bool
		err := k.Read(func(Head) error { ran = true; return nil })
		return ran, err
	}

	if ran, err := served(); !ran || err != nil {
		t.Fatalf("fresh keeper: served %v err %v, want served", ran, err)
	}
	s.fail.Store(true)
	time.Sleep(2500 * time.Millisecond)
	ran, err := served()
	if ran || err == nil {
		t.Fatalf("2.5 s of failed gen checks with DeleteSlack 3s: served %v err %v, want refused", ran, err)
	}
	if errors.Is(err, ErrLost) {
		t.Fatalf("a failed List was taken as a lost gen: %v", err)
	}
	s.fail.Store(false)
	time.Sleep(1500 * time.Millisecond)
	if ran, err := served(); !ran || err != nil {
		t.Fatalf("after a good check: served %v err %v, want served", ran, err)
	}
}
