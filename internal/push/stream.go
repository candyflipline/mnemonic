package push

// SKETCH (docs/design/uploader.md, "Usage"): the client end of the push
// stream. Lane C deletes fetchManifest, putSource, the 409 retry loop in
// syncSource and syncTail, reconcileEvery and conflictRetry, and Watch and
// Run take no *http.Client. decide, firstChunk, rawEndOf, redactTail and
// redactComplete stay as they are: they cut the chunks, so the stream sends
// the bytes today's PUTs send.

import (
	"context"

	"github.com/candyflipline/mnemonic/internal/config"
	"github.com/candyflipline/mnemonic/internal/wire"
	"github.com/candyflipline/mnemonic/internal/wire/pushpb"
)

// put is one chunk the decision logic cut: a PUT's fields, now an Append.
type put struct {
	path, harness, srcID, sourceName string
	offset                           int64
	headHash                         string
	rawEnd                           int64
	body                             []byte
}

// ack is a parsed pushpb.Ack: the path it answers (from the in-flight
// record of its seq) and the source's state after it.
type ack struct {
	path     string
	srcID    string
	conflict bool
	state    wire.Source
}

// uplink is one open Sync stream. sources starts as what Welcome said, by
// src_id, and is then the server's state plus what is in flight: send
// advances a source's Size optimistically, and an APPLIED ack writes the
// server's state back only when no later append of that source is in flight
// (unacked[src] reaches 0), so a flush during the ack latency never re-sends
// from a stale offset. A CONFLICT ack always writes the server's state back.
// Only the watcher goroutine touches sources and unacked: the ack reader
// goroutine only parses and delivers on acked, and the watcher applies each
// ack in its select loop (applyAck).
type uplink struct {
	stream  pushpb.Push_SyncClient
	cancel  context.CancelFunc
	sources map[string]*wire.Source
	unacked map[string]int
	window  window
	seq     uint64
	sent    chan inflight // cap window.appends: send blocks when full
	acked   chan ack
	done    chan error // the stream's end: nil after close, else why
}

type window struct {
	appends int
	bytes   int64
}

type inflight struct {
	seq   uint64
	path  string
	bytes int64
}

// dial opens Sync on cfg.Server (https: TLS with system roots on :443 unless
// the URL names a port; http: plaintext h2c, tests only), sends Hello with
// cfg.Machine and the bearer token as metadata, reads Welcome pages until
// more is false, and starts the ack reader.
func dial(ctx context.Context, cfg config.Client) (*uplink, error) { panic("not implemented") }

// send writes one Append. It blocks while window.appends appends or
// window.bytes body bytes are unacked, and returns the stream's error once
// the stream ended.
func (u *uplink) send(ctx context.Context, p put) error { panic("not implemented") }

// acks delivers each Ack in send order; a seq that is not the oldest
// in-flight one ends the stream (protocol error).
func (u *uplink) acks() <-chan ack { return u.acked }

// applyAck folds a into sources and unacked, as the uplink comment says, and
// reports whether a's path must be re-decided (a conflict).
func (u *uplink) applyAck(a ack) (redo bool) { panic("not implemented") }

// ended fires once with the stream's error.
func (u *uplink) ended() <-chan error { return u.done }

// close half-closes, waits for every in-flight ack, and returns the stream's
// error.
func (u *uplink) close() error { panic("not implemented") }

// streamSource replaces syncSource: the same skip rules against
// u.sources[srcID], the same decide/firstChunk/rawEndOf cut, every chunk sent
// without waiting for its ack (the next chunk's offset is the previous one's
// end, as syncSource already assumes at push.go:176). A conflict ack is the
// watcher's: it sets u.sources[srcID] to the ack's state and marks the path
// dirty, and the next flush re-decides it once from that state.
func streamSource(ctx context.Context, red Redactor, u *uplink, f fileRef, st *Stats) error {
	// TODO:
	// entry := u.sources[wire.SrcID(f.path)]
	// skip exactly as push.go:94-106
	// tail path (entry.RawEnd > 0 && entry.Size > 0): redactTail, then for
	//   each firstChunk: u.send(put{offset: entry.Size, ...}); entry.Size += len
	//   (fallback to the full path when redactTail returns nil, as today)
	// full path: redactComplete; loop decide(entry, head, body):
	//   noop -> legacy backfill send (empty body, raw_end) when entry.RawEnd==0
	//   else send firstChunk at offset; entry = {Size: offset+len, HeadHash: head}
	panic("not implemented")
}
