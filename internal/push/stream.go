package push

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	"github.com/candyflipline/mnemonic/internal/config"
	"github.com/candyflipline/mnemonic/internal/qnet"
	"github.com/candyflipline/mnemonic/internal/wire"
	"github.com/candyflipline/mnemonic/internal/wire/pushpb"
)

const (
	pingEvery    = 30 * time.Second
	pingTimeout  = 20 * time.Second
	welcomeLimit = time.Minute
)

type window struct {
	appends int
	bytes   int64
}

type inflight struct {
	seq   uint64
	path  string
	srcID string
	bytes int64
}

type ack struct {
	seq      uint64
	conflict bool
	state    wire.Source
}

type ledger struct {
	sources map[string]*wire.Source
	unacked map[string]int
	queue   []inflight
	queued  int64
	redo    []string
	full    map[string]bool
}

func (l *ledger) sent(in inflight, next wire.Source) {
	l.queue = append(l.queue, in)
	l.queued += in.bytes
	l.unacked[in.srcID]++
	if prev := l.sources[in.srcID]; prev != nil {
		next.UpdatedMs = prev.UpdatedMs
	}
	l.sources[in.srcID] = &next
}

func (l *ledger) answer(a ack) error {
	if len(l.queue) == 0 || l.queue[0].seq != a.seq {
		return fmt.Errorf("sync: ack %d does not answer the oldest append in flight", a.seq)
	}
	head := l.queue[0]
	l.queue = l.queue[1:]
	l.queued -= head.bytes
	l.unacked[head.srcID]--
	if l.unacked[head.srcID] == 0 {
		delete(l.unacked, head.srcID)
	}
	switch {
	case a.conflict:
		l.redo = append(l.redo, head.path)
		l.full[head.srcID] = true
		if a.state.Size == 0 && a.state.HeadHash == "" {
			delete(l.sources, head.srcID)
			return nil
		}
	case l.unacked[head.srcID] > 0:
		return nil
	}
	s := a.state
	s.SrcID = head.srcID
	l.sources[head.srcID] = &s
	return nil
}

func (l *ledger) busy(srcID string) bool { return l.unacked[srcID] > 0 }

func (l *ledger) takeRedo() []string {
	r := l.redo
	l.redo = nil
	return r
}

type uplink struct {
	ledger
	stream pushpb.Push_SyncClient
	cancel context.CancelFunc
	window window
	seq    uint64
	acked  chan ack
	fin    chan struct{}
	err    error
}

func dial(ctx context.Context, cfg config.Client) (*uplink, error) {
	priv, err := qnet.LoadOrCreateKey(cfg.KeyFile)
	if err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient("passthrough:///"+cfg.Addr(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			return qnet.Dial(ctx, addr, priv, cfg.ServerKey, qnet.ALPNPush)
		}),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: pingEvery, Timeout: pingTimeout}),
	)
	if err != nil {
		return nil, err
	}
	sctx, cancel := context.WithCancel(ctx)
	u := &uplink{cancel: cancel, ledger: ledger{sources: make(map[string]*wire.Source), unacked: make(map[string]int), full: make(map[string]bool)}, fin: make(chan struct{})}
	stop := time.AfterFunc(welcomeLimit, cancel)
	err = u.handshake(sctx, conn, cfg.Machine)
	if !stop.Stop() && err == nil {
		err = context.DeadlineExceeded
	}
	if err != nil {
		cancel()
		conn.Close()
		return nil, err
	}
	u.acked = make(chan ack, u.window.appends+1)
	go u.read(sctx, conn)
	return u, nil
}

func (u *uplink) handshake(ctx context.Context, conn *grpc.ClientConn, machine string) error {
	stream, err := pushpb.NewPushClient(conn).Sync(ctx)
	if err != nil {
		return err
	}
	u.stream = stream
	if err := stream.Send(&pushpb.ClientMsg{Msg: &pushpb.ClientMsg_Hello{Hello: &pushpb.Hello{Machine: machine}}}); err != nil {
		return u.recvErr(err)
	}
	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}
		w := msg.GetWelcome()
		if w == nil {
			return errors.New("sync: server sent an ack before Welcome ended")
		}
		for _, s := range w.Sources {
			u.sources[s.SrcId] = &wire.Source{
				SrcID: s.SrcId, Harness: s.Harness, Size: s.Size, HeadHash: s.HeadHash,
				RawEnd: s.RawEnd, UpdatedMs: s.UpdatedMs,
			}
		}
		if w.More {
			continue
		}
		if w.Window == nil || w.Window.Appends == 0 || w.Window.Bytes == 0 {
			return errors.New("sync: last Welcome page carries no window")
		}
		u.window = window{appends: int(w.Window.Appends), bytes: int64(w.Window.Bytes)}
		return nil
	}
}

// recvErr turns a Send failure into the stream's real status: a Send that
// fails with io.EOF means the stream ended, and Recv says why.
func (u *uplink) recvErr(err error) error {
	if errors.Is(err, io.EOF) {
		if _, rerr := u.stream.Recv(); rerr != nil {
			return rerr
		}
	}
	return err
}

func (u *uplink) read(ctx context.Context, conn *grpc.ClientConn) {
	defer func() {
		u.cancel()
		conn.Close()
		close(u.fin)
	}()
	for {
		msg, err := u.stream.Recv()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				u.err = err
			}
			return
		}
		a := msg.GetAck()
		if a == nil || a.State == nil {
			u.err = errors.New("sync: server sent a message that is not an ack")
			return
		}
		s := a.State
		parsed := ack{seq: a.Seq, conflict: a.Result == pushpb.Ack_CONFLICT, state: wire.Source{
			SrcID: s.SrcId, Harness: s.Harness, Size: s.Size, HeadHash: s.HeadHash,
			RawEnd: s.RawEnd, UpdatedMs: s.UpdatedMs,
		}}
		if a.Result != pushpb.Ack_APPLIED && a.Result != pushpb.Ack_CONFLICT {
			u.err = fmt.Errorf("sync: ack %d has result %v", a.Seq, a.Result)
			return
		}
		select {
		case u.acked <- parsed:
		case <-ctx.Done():
			return
		}
	}
}

type put struct {
	path, harness, srcID, sourceName string
	offset                           int64
	headHash                         string
	rawEnd                           int64
	body                             []byte
}

func (u *uplink) send(ctx context.Context, p put) error {
	if err := u.sendOne(ctx, p); err != nil {
		return streamErr{err}
	}
	return nil
}

type streamErr struct{ error }

func (e streamErr) Unwrap() error { return e.error }

func (u *uplink) sendOne(ctx context.Context, p put) error {
	size := int64(len(p.body))
	for len(u.queue) >= u.window.appends || (len(u.queue) > 0 && u.queued+size > u.window.bytes) {
		select {
		case a := <-u.acked:
			if err := u.consume(a); err != nil {
				return err
			}
		case <-u.fin:
			return u.cause()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	select {
	case <-u.fin:
		return u.cause()
	default:
	}
	u.seq++
	err := u.stream.Send(&pushpb.ClientMsg{Msg: &pushpb.ClientMsg_Append{Append: &pushpb.Append{
		Seq: u.seq, Harness: p.harness, SrcId: p.srcID, Offset: p.offset, HeadHash: p.headHash,
		SourceName: p.sourceName, Path: p.path, RawEnd: p.rawEnd, Body: p.body,
	}}})
	if err != nil {
		select {
		case <-u.fin:
			return u.cause()
		case <-time.After(time.Second):
		}
		return err
	}
	u.sent(inflight{seq: u.seq, path: p.path, srcID: p.srcID, bytes: size}, wire.Source{SrcID: p.srcID, Harness: p.harness, Size: p.offset + size, HeadHash: p.headHash, RawEnd: p.rawEnd})
	return nil
}

func (u *uplink) cause() error {
	if u.err != nil {
		return u.err
	}
	return io.EOF
}

func (u *uplink) consume(a ack) error {
	if err := u.answer(a); err != nil {
		u.cancel()
		return err
	}
	return nil
}

func (u *uplink) drain(ctx context.Context) error {
	for len(u.queue) > 0 {
		select {
		case a := <-u.acked:
			if err := u.consume(a); err != nil {
				return err
			}
		case <-u.fin:
			select {
			case a := <-u.acked:
				if err := u.consume(a); err != nil {
					return err
				}
				continue
			default:
			}
			return fmt.Errorf("sync: stream ended with %d appends unacked: %w", len(u.queue), u.cause())
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (u *uplink) close(ctx context.Context) error {
	defer func() {
		u.cancel()
		<-u.fin
	}()
	if err := u.stream.CloseSend(); err != nil {
		return err
	}
	return u.drain(ctx)
}

func (u *uplink) ended() <-chan struct{} { return u.fin }

func streamSource(ctx context.Context, red Redactor, u *uplink, f fileRef, st *Stats) error {
	srcID := wire.SrcID(f.path)
	entry := u.sources[srcID]
	full := u.full[srcID]
	delete(u.full, srcID)
	if !full && entry != nil && entry.RawEnd > 0 && f.size == entry.RawEnd {
		st.Unchanged++
		return nil
	}
	// The server stamps UpdatedMs after it received the bytes, so a file last
	// written skewMs before that stamp has nothing new. Entries with RawEnd 0,
	// the legacy shape, are saved the read by this skip alone.
	if !full && entry != nil && f.mtimeMs < entry.UpdatedMs-skewMs {
		st.Unchanged++
		return nil
	}
	legacy := !full && entry != nil && entry.RawEnd == 0
	chunk := func(offset int64, head string, body []byte, rawEnd int64) error {
		return u.send(ctx, put{
			path: f.path, harness: f.harness, srcID: srcID, sourceName: f.sourceName,
			offset: offset, headHash: head, rawEnd: rawEnd, body: body,
		})
	}
	sent, replaced := false, false
	if !full && entry != nil && entry.RawEnd > 0 && entry.Size > 0 {
		tail, err := redactTail(f.path, red, entry, f.head)
		if err != nil {
			return err
		}
		if tail != nil {
			size, body, rawEnds := entry.Size, tail.body, tail.rawEnds
			for len(body) > 0 {
				piece := firstChunk(body, maxChunk)
				lines := bytes.Count(piece, []byte{'\n'})
				if err := chunk(size, tail.head, piece, rawEnds[lines-1]); err != nil {
					return err
				}
				st.Bytes += int64(len(piece))
				sent = true
				size += int64(len(piece))
				body, rawEnds = body[len(piece):], rawEnds[lines:]
			}
			if sent {
				st.Appended++
			} else {
				st.Unchanged++
			}
			return nil
		}
	}
	file, err := redactComplete(f.path, red)
	if err != nil {
		return err
	}
	if file == nil {
		st.Unchanged++
		return nil
	}
	cur := entry
	for {
		body, offset, noop := decide(cur, file.head, file.body)
		if noop {
			// A legacy entry (RawEnd 0) whose redacted bytes the server already
			// holds in full would read whole every pass forever: one empty
			// append at Size records raw_end so the next pass takes the exact
			// skip.
			if legacy && !sent {
				if err := chunk(cur.Size, file.head, nil, file.rawEnds[len(file.rawEnds)-1]); err != nil {
					return err
				}
			}
			switch {
			case !sent:
				st.Unchanged++
			case replaced:
				st.Replaced++
			default:
				st.Appended++
			}
			return nil
		}
		body = firstChunk(body, maxChunk)
		if err := chunk(offset, file.head, body, rawEndOf(file.rawEnds, file.body, offset, body)); err != nil {
			return err
		}
		st.Bytes += int64(len(body))
		sent, replaced = true, replaced || offset == 0
		cur = &wire.Source{SrcID: srcID, Size: offset + int64(len(body)), HeadHash: file.head}
	}
}
