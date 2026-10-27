package index

import (
	"context"
	"sync"
)

type BootStatus struct {
	mu       sync.Mutex
	err      string
	attempts int
}

func (b *BootStatus) Failed(attempt int, err error) {
	b.mu.Lock()
	b.attempts, b.err = attempt, err.Error()
	b.mu.Unlock()
}

func (b *BootStatus) Booted() {
	b.mu.Lock()
	b.attempts, b.err = 0, ""
	b.mu.Unlock()
}

type bootStatusKey struct{}

func WithBootStatus(ctx context.Context, b *BootStatus) context.Context {
	return context.WithValue(ctx, bootStatusKey{}, b)
}

func bootFailureFrom(ctx context.Context) (string, int, bool) {
	b, _ := ctx.Value(bootStatusKey{}).(*BootStatus)
	if b == nil {
		return "", 0, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err, b.attempts, b.attempts > 0
}
