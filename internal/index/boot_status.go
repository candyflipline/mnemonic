package index

import (
	"context"
	"sync"
)

// BootStatus is what the server knows about a boot that has not succeeded
// yet; get_ingest_status reads it from its ctx, like Controls.
type BootStatus struct {
	mu       sync.Mutex
	err      string
	attempts int
}

// Failed records attempt number attempt, which ended in err.
func (b *BootStatus) Failed(attempt int, err error) {
	b.mu.Lock()
	b.attempts, b.err = attempt, err.Error()
	b.mu.Unlock()
}

// Booted clears the failure.
func (b *BootStatus) Booted() {
	b.mu.Lock()
	b.attempts, b.err = 0, ""
	b.mu.Unlock()
}

type bootStatusKey struct{}

// WithBootStatus puts b on ctx for get_ingest_status.
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
