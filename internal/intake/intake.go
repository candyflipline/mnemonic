// Package intake is the transport onto the store's one write path: the
// push stream (gRPC Push.Sync over h2c on its own native gRPC listener). It
// parses its wire into store types here, at the edge, and answers only with
// what store.Ticket.Wait returned.
package intake

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/store"
	"github.com/candyflipline/mnemonic/internal/wire"
	"github.com/candyflipline/mnemonic/internal/wire/pushpb"
)

const (
	windowAppends = 64
	windowBytes   = 96 << 20
	maxMsg        = 33 << 20
	welcomePage   = 4096
)

var (
	errBadSegment = errors.New("bad path segment")
)

// Mount constructs the native Push server; every stream passes the push-scope
// token check (auth.Tokens.Check) in its interceptor.
// The caller owns its listener. Stop accepting, call drain to end live
// sessions with Unavailable, then GracefulStop (with a bounded Stop fallback)
// before closing the committer. Push must never use grpc.Server.ServeHTTP:
// its handler transport buffers request bodies independently of Recv.
func Mount(st *store.Committer, tokens auth.Tokens) (*grpc.Server, func()) {
	svc := &service{st: st, live: map[streamKey]*session{}}
	gs := grpc.NewServer(
		grpc.MaxRecvMsgSize(maxMsg),
		grpc.InitialWindowSize(64<<10),
		grpc.InitialConnWindowSize(256<<10),
		grpc.MaxConcurrentStreams(4),
		grpc.MaxHeaderListSize(16<<10),
		grpc.ConnectionTimeout(10*time.Second),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime: 20 * time.Second, PermitWithoutStream: true,
		}),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time: 45 * time.Second, Timeout: 20 * time.Second,
			MaxConnectionIdle: 5 * time.Minute,
		}),
		grpc.StreamInterceptor(func(srv any, stream grpc.ServerStream, _ *grpc.StreamServerInfo, next grpc.StreamHandler) error {
			md, _ := metadata.FromIncomingContext(stream.Context())
			authorization := ""
			if values := md.Get("authorization"); len(values) > 0 {
				authorization = values[0]
			}
			ctx, err := tokens.Check(stream.Context(), authorization, auth.Push)
			if err != nil {
				code := codes.PermissionDenied
				if errors.Is(err, auth.ErrUnauthorized) {
					code = codes.Unauthenticated
				}
				return status.Error(code, err.Error())
			}
			return next(srv, &identityStream{ServerStream: stream, ctx: ctx})
		}),
	)
	pushpb.RegisterPushServer(gs, svc)
	return gs, svc.drain
}

type identityStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *identityStream) Context() context.Context { return s.ctx }

type streamKey struct{ author, machine string }

type service struct {
	pushpb.UnimplementedPushServer
	st *store.Committer

	mu      sync.Mutex
	live    map[streamKey]*session
	closing bool
}

// session is one live stream's submit side. The committer resolves tickets in
// submission order, so the last ticket resolving means every ticket of the
// session has.
type session struct {
	ctx    context.Context
	cancel context.CancelCauseFunc

	mu   sync.Mutex
	last *store.Ticket
}

func (s *session) submit(st *store.Committer, src store.ClientSource, op store.Op) (*store.Ticket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return nil, context.Cause(s.ctx)
	}
	t, err := st.Submit(s.ctx, src, op)
	if err == nil {
		s.last = t
	}
	return t, err
}

// end stops the session's submissions with cause and returns once every
// ticket it had submitted has resolved or ctx is done.
func (s *session) end(ctx context.Context, cause error) error {
	s.cancel(cause)
	s.mu.Lock()
	last := s.last
	s.mu.Unlock()
	if last == nil {
		return nil
	}
	last.Wait(ctx)
	return ctx.Err()
}

var (
	errReplaced = status.Error(codes.Aborted, "replaced by a newer stream for this machine")
	errDraining = status.Error(codes.Unavailable, "server is shutting down")
)

func (s *service) enter(k streamKey, sess *session) (*session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return nil, errDraining
	}
	old := s.live[k]
	s.live[k] = sess
	return old, nil
}

func (s *service) leave(k streamKey, sess *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.live[k] == sess {
		delete(s.live, k)
	}
}

func (s *service) drain() {
	s.mu.Lock()
	s.closing = true
	live := make([]*session, 0, len(s.live))
	for _, sess := range s.live {
		live = append(live, sess)
	}
	s.mu.Unlock()
	for _, sess := range live {
		sess.cancel(errDraining)
	}
}

type pending struct {
	seq    uint64
	ticket *store.Ticket
}

// Sync runs one client's session. The receive goroutine parses each Append
// (parseAppend), submits it and queues its ticket on a channel of
// windowAppends slots; this goroutine waits on the tickets in order and sends
// each Ack. A Submit or Wait error ends the stream with Unavailable (or
// ResourceExhausted for a full disk); a parse error with InvalidArgument; a
// server-owned machine or harness with PermissionDenied. When the stream
// ends, submitted requests still commit; their acks are dropped and the next
// Welcome reports them.
func (s *service) Sync(stream grpc.BidiStreamingServer[pushpb.ClientMsg, pushpb.ServerMsg]) error {
	author := auth.Author(stream.Context())
	if author == "" {
		return status.Error(codes.Unauthenticated, "unauthorized")
	}
	first, err := stream.Recv()
	if err != nil {
		return recvStatus(err, "expected Hello")
	}
	hello := first.GetHello()
	if hello == nil || !wire.ValidSegment(hello.GetMachine()) {
		return status.Error(codes.InvalidArgument, "first message must be Hello with a valid machine")
	}
	machine := hello.GetMachine()

	ctx, cancel := context.WithCancelCause(stream.Context())
	defer cancel(nil)
	sess := &session{ctx: ctx, cancel: cancel}
	key := streamKey{author, machine}
	old, err := s.enter(key, sess)
	if err != nil {
		return err
	}
	defer s.leave(key, sess)
	if old != nil {
		if err := old.end(ctx, errReplaced); err != nil {
			return toStatus(context.Cause(ctx))
		}
	}
	for _, m := range welcome(s.st.Sources(author, machine)) {
		if err := stream.Send(m); err != nil {
			return err
		}
	}

	tickets := make(chan pending, windowAppends)
	var recvErr error
	go func() {
		defer close(tickets)
		recvErr = s.receive(sess, stream, author, machine, tickets)
	}()
	for {
		select {
		case p, ok := <-tickets:
			if !ok {
				return recvStatus(recvErr, "")
			}
			a, err := p.ticket.Wait(ctx)
			if err != nil {
				return toStatus(causeOr(ctx, err))
			}
			msg, err := ackOf(p.seq, a)
			if err != nil {
				return status.Error(codes.Internal, err.Error())
			}
			if err := stream.Send(msg); err != nil {
				return err
			}
		case <-ctx.Done():
			return toStatus(context.Cause(ctx))
		}
	}
}

func (s *service) receive(sess *session, stream grpc.BidiStreamingServer[pushpb.ClientMsg, pushpb.ServerMsg], author, machine string, tickets chan<- pending) error {
	for seq := uint64(1); ; seq++ {
		m, err := stream.Recv()
		if err != nil {
			return err
		}
		a := m.GetAppend()
		if a == nil {
			return status.Error(codes.InvalidArgument, "expected Append")
		}
		if a.GetSeq() != seq {
			return status.Errorf(codes.InvalidArgument, "seq %d, want %d", a.GetSeq(), seq)
		}
		src, op, err := parseAppend(author, machine, a)
		if err != nil {
			return status.Error(editCode(err), err.Error())
		}
		t, err := sess.submit(s.st, src, op)
		if err != nil {
			return toStatus(err)
		}
		select {
		case tickets <- pending{seq, t}:
		case <-sess.ctx.Done():
			return toStatus(context.Cause(sess.ctx))
		}
	}
}

func recvStatus(err error, onEOF string) error {
	if errors.Is(err, io.EOF) {
		if onEOF != "" {
			return status.Error(codes.InvalidArgument, onEOF)
		}
		return nil
	}
	return toStatus(err)
}

func causeOr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	return err
}

func editCode(err error) codes.Code {
	if errors.Is(err, store.ErrReserved) {
		return codes.PermissionDenied
	}
	return codes.InvalidArgument
}

func toStatus(err error) error {
	if _, ok := status.FromError(err); ok {
		return err
	}
	switch {
	case errors.Is(err, syscall.ENOSPC):
		return status.Error(codes.ResourceExhausted, err.Error())
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	}
	return status.Error(codes.Unavailable, err.Error())
}

// parseAppend is the edge: segments and author through store.NewClientSource,
// the offset, head hash, names, raw_end and body through store.ParseOp with
// the PUT's rules.
func parseAppend(author, machine string, a *pushpb.Append) (store.ClientSource, store.Op, error) {
	return parse(author, machine, a.GetHarness(), a.GetSrcId(), a.GetOffset(), a.GetHeadHash(), a.GetSourceName(), a.GetPath(), a.GetRawEnd(), a.GetBody())
}

func parse(author, machine, harness, srcID string, offset int64, headHash, sourceName, path string, rawEnd int64, body []byte) (store.ClientSource, store.Op, error) {
	if err := checkSegments(machine, harness, srcID); err != nil {
		return store.ClientSource{}, nil, err
	}
	op, err := store.ParseOp(offset, headHash, sourceName, path, rawEnd, body)
	if err != nil {
		return store.ClientSource{}, nil, err
	}
	src, err := store.NewClientSource(author, machine, harness, srcID)
	if err != nil {
		return store.ClientSource{}, nil, err
	}
	return src, op, nil
}

func checkSegments(segs ...string) error {
	for _, seg := range segs {
		if !wire.ValidSegment(seg) {
			return errBadSegment
		}
	}
	return nil
}

// ackOf is the only way to an Ack message, and it takes a store.Ack, which
// only a durable batch makes. An invalid Ack is a server bug: the stream
// ends with Internal and nothing is acked.
func ackOf(seq uint64, a store.Ack) (*pushpb.ServerMsg, error) {
	if !a.Valid() {
		return nil, errors.New("ack of a batch that is not durable")
	}
	var res pushpb.Ack_Result
	switch a.Verdict() {
	case store.Applied:
		res = pushpb.Ack_APPLIED
	case store.Conflict:
		res = pushpb.Ack_CONFLICT
	default:
		return nil, fmt.Errorf("verdict %d is not a client ack", a.Verdict())
	}
	return &pushpb.ServerMsg{Msg: &pushpb.ServerMsg_Ack{Ack: &pushpb.Ack{Seq: seq, Result: res, State: stateOf(a.State())}}}, nil
}

// welcome pages sources into Welcome messages of welcomePage entries; the
// last has more false and carries the Window. No sources is one empty page.
func welcome(sources []wire.Source) []*pushpb.ServerMsg {
	var out []*pushpb.ServerMsg
	for {
		page := sources[:min(len(sources), welcomePage)]
		sources = sources[len(page):]
		w := &pushpb.Welcome{Sources: make([]*pushpb.SourceState, len(page)), More: len(sources) > 0}
		for i, s := range page {
			w.Sources[i] = stateOf(s)
		}
		if !w.More {
			w.Window = &pushpb.Window{Appends: windowAppends, Bytes: windowBytes}
		}
		out = append(out, &pushpb.ServerMsg{Msg: &pushpb.ServerMsg_Welcome{Welcome: w}})
		if !w.More {
			return out
		}
	}
}

func stateOf(s wire.Source) *pushpb.SourceState {
	return &pushpb.SourceState{SrcId: s.SrcID, Harness: s.Harness, Size: s.Size, HeadHash: s.HeadHash, RawEnd: s.RawEnd, UpdatedMs: s.UpdatedMs}
}
