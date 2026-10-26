package push

// ARCHITECTURE SKETCH: existing Run/Watch/WatchSink remain untouched.
import (
	"context"
	"github.com/candyflipline/mnemonic/internal/config"
)

// StreamClient owns the connection snapshot, speculative offsets, ack ledger,
// reconnect and byte credits. Sync implements Sink for the existing watcher.
// It imports no store, parser or index; cfg and Redactor are the existing types.
type StreamClient struct{ session *uploadSession }
type uploadSession struct {
	// TODO: closed states connecting -> snapshotting -> ready -> disconnected;
	// one sender, one ack reader; per-source ordered ledger, <=8 unacked appends.
}

func OpenStream(context.Context, config.Client, Redactor) (*StreamClient, error) {
	// TODO: TLS + bearer RPC metadata; receive offsets until Ready; watch before
	// scanning; reconnect with existing 5s..1m backoff and rebuild from snapshot.
	panic("not implemented")
}
func (*StreamClient) Sync(context.Context, []string) (Stats, error) {
	// TODO: reuse decide/firstChunk/redactTail/rawEndOf exactly; pipeline several
	// complete legacy chunks, fragment transport only at 1 MiB. Stamp a file
	// synced only after every associated ack. Never retry on-stream at an offset.
	panic("not implemented")
}
func (*StreamClient) Close() error { panic("not implemented") }
