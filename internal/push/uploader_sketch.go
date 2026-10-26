package push

// ARCHITECTURE SKETCH: existing Run/Watch/WatchSink remain untouched.
import (
	"context"
	"time"

	"github.com/candyflipline/mnemonic/internal/config"
)

// StreamClient owns the connection snapshot, speculative offsets, ack ledger,
// reconnect and byte credits. Sync implements Sink for the existing watcher.
// It imports no store, parser or index; cfg and Redactor are the existing types.
type StreamClient struct {
	cfg     config.Client
	red     Redactor
	session uploadSession
}
type uploadSession interface{ uploadSession() }
type connectingUpload struct{ retryAt time.Time }
type snapshottingUpload struct {
	sources map[uploadSource]remotePosition
}
type readyUpload struct {
	sources      map[uploadSource]sourceProgress
	flights      []uploadFlight
	nextSequence uint64
}
type disconnectedUpload struct {
	dirtyPaths map[string]struct{}
	retryAt    time.Time
}
type uploadSource struct{ harness, srcID string }
type remotePosition struct {
	size, rawEnd, updatedMS int64
	headHash                string
}
type sourceProgress struct{ committed, speculative remotePosition }
type uploadFlight struct {
	sequence uint64
	source   uploadSource
	through  remotePosition
	path     string
	target   stamp
}

func (*connectingUpload) uploadSession()   { panic("not implemented") }
func (*snapshottingUpload) uploadSession() { panic("not implemented") }
func (*readyUpload) uploadSession()        { panic("not implemented") }
func (*disconnectedUpload) uploadSession() { panic("not implemented") }

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
