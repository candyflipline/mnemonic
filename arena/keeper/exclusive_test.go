package keeper

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/candyflipline/mnemonic/arena/obj"
)

type putIgnoresExclusive struct{ obj.Store }

func (s putIgnoresExclusive) PutExclusive(ctx context.Context, k obj.Key, r io.Reader, size int64) error {
	return s.Put(ctx, k, r, size)
}

func TestTakeRefusesStoreWithoutExclusiveCreate(t *testing.T) {
	ctx := context.Background()
	s, err := obj.Open("file://" + t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Take(ctx, putIgnoresExclusive{s}, Config{Tenant: "acme"}); !errors.Is(err, ErrNotExclusive) {
		t.Fatalf("Take on a store that ignores If-None-Match = %v, want ErrNotExclusive", err)
	}
	k, err := Take(ctx, s, Config{Tenant: "acme"})
	if err != nil {
		t.Fatalf("Take on an exclusive store: %v", err)
	}
	if k.Gen() != 1 {
		t.Errorf("gen %d, want 1", k.Gen())
	}
	left, err := s.List(ctx, "t/acme/x/")
	if err != nil || len(left) != 0 {
		t.Errorf("probe keys left behind: %v err %v", left, err)
	}
}
