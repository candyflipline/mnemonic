package keeper

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/candyflipline/mnemonic/arena/obj"
)

func TestTakeRefusesDeleteSlackBelowFloor(t *testing.T) {
	s, err := obj.Open("file://" + t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, slack := range []time.Duration{0, 2999 * time.Millisecond} {
		if _, err := Take(context.Background(), s, Config{Tenant: "acme", DeleteSlack: slack}); !errors.Is(err, ErrDeleteSlack) {
			t.Fatalf("Take with DeleteSlack %s = %v, want ErrDeleteSlack", slack, err)
		}
	}
	if _, err := Take(context.Background(), s, Config{Tenant: "acme", DeleteSlack: MinDeleteSlack}); err != nil {
		t.Fatalf("Take with DeleteSlack %s: %v", MinDeleteSlack, err)
	}
}
