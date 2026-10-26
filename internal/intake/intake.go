// Package intake is every transport onto the store's one write path: the
// push stream (gRPC Push.Sync over h2c on the server's one listener) and,
// until its expiry, the HTTP PUT and manifest adapter. Each parses its wire
// into store types here, at the edge, and answers only with what
// store.Ticket.Wait returned.
//
// SKETCH (docs/design/uploader.md): signatures only, bodies panic.
package intake

import (
	"net/http"

	"google.golang.org/grpc"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/store"
	"github.com/candyflipline/mnemonic/internal/wire"
	"github.com/candyflipline/mnemonic/internal/wire/pushpb"
)

const (
	// windowAppends and windowBytes are the Window Welcome advertises; the
	// server's per-stream in-flight channel has windowAppends slots, so a
	// client that ignores the window only stalls itself.
	windowAppends = 64
	windowBytes   = 96 << 20 // three max chunks (push.maxChunk is 32 MiB)
	// maxMsg admits one max chunk plus its fields.
	maxMsg = 33 << 20
	// welcomePage is how many SourceStates one Welcome page carries, about
	// 0.5 MB, under every default message limit.
	welcomePage = 4096
)

// Mount serves Push.Sync and the adapter routes on mux, all behind the push
// scope (the bearer token rides as gRPC metadata "authorization", which is
// the HTTP header Require already reads). server.New calls it once.
func Mount(mux *http.ServeMux, st *store.Committer, tokens auth.Tokens) {
	// TODO:
	// gs := grpc.NewServer(grpc.MaxRecvMsgSize(maxMsg))
	// pushpb.RegisterPushServer(gs, &service{st: st})
	// mux.Handle("POST /mnemonic.push.v1.Push/", tokens.Require(auth.Push)(gs))
	// mux.Handle("/v1/", tokens.Require(auth.Push)(adapter{st}))  // until expiry
	panic("not implemented")
}

type service struct {
	pushpb.UnimplementedPushServer
	st *store.Committer
}

// Sync runs one client's session. The receive goroutine parses each Append
// (parseAppend), submits it and queues its ticket on a channel of
// windowAppends slots; this goroutine waits on the tickets in order and sends
// each Ack. A Submit or Wait error ends the stream with Unavailable; a parse
// error with InvalidArgument; a server-owned machine or harness with
// PermissionDenied. When the stream ends, submitted requests still commit;
// their acks are dropped and the next Welcome reports them.
func (s *service) Sync(stream grpc.BidiStreamingServer[pushpb.ClientMsg, pushpb.ServerMsg]) error {
	// TODO:
	// author := auth.Author(stream.Context())
	// hello := first Recv, must be Hello with a valid machine
	// for _, m := range welcome(s.st.Sources(author, machine)) { Send(m) }
	// tickets := make(chan pending, windowAppends)
	// go recv loop: Recv -> parseAppend -> s.st.Submit -> tickets <- {seq, t}
	//   (seq must be the previous + 1, from 1)
	// for p := range tickets { a, err := p.t.Wait(ctx); Send(ackOf(p.seq, a)) }
	panic("not implemented")
}

type pending struct {
	seq    uint64
	ticket *store.Ticket
}

// parseAppend is the edge: segments and author through store.NewClientSource,
// the offset, head hash, names, raw_end and body through store.ParseOp with
// the PUT's rules.
func parseAppend(author, machine string, a *pushpb.Append) (store.ClientSource, store.Op, error) {
	panic("not implemented")
}

// ackOf is the only way to an Ack message, and it takes a store.Ack, which
// only a durable batch makes. An invalid Ack is a server bug: the stream
// ends with Internal and nothing is acked.
func ackOf(seq uint64, a store.Ack) (*pushpb.ServerMsg, error) { panic("not implemented") }

// welcome pages sources into Welcome messages of welcomePage entries; the
// last has more false and carries the Window. No sources is one empty page.
func welcome(sources []wire.Source) []*pushpb.ServerMsg { panic("not implemented") }
