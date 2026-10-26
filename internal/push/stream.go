package push

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"

	"github.com/candyflipline/mnemonic/internal/config"
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

// ack is a parsed pushpb.Ack. conflict and state are all applyAck needs.
type ack struct {
	seq      uint64
	conflict bool
	state    wire.Source
}

// uplink is one open Sync stream. sources starts as what Welcome said, by
// src_id, and is then the server's state plus what is in flight: send
// advances a source's entry optimistically, and an APPLIED ack writes the
// server's state back only when no later append of that source is in flight
// (unacked[src] reaches 0). A CONFLICT ack always writes the server's state
// back and records the path in redo. Only the goroutine that owns the uplink
// touches sources, unacked, queue and redo; the reader goroutine only parses
// acks and delivers them on acked.
type uplink struct {
	stream  pushpb.Push_SyncClient
	cancel  context.CancelFunc
	sources map[string]*wire.Source
	unacked map[string]int
	queue   []inflight
	queued  int64
	redo    []string
	full    map[string]bool
	window  window
	seq     uint64
	acked   chan ack
	fin     chan struct{}
	err     error // why the stream ended; valid once fin is closed
}

// pushPort is where the server's push listener answers an https server URL.
const pushPort = "8741"

// target maps the config's server URL to a gRPC target and credentials:
// https dials the URL's host on pushPort with the system roots, http dials
// the URL's host:port in plaintext HTTP/2 (the box and tests). A non-empty
// pushAddress replaces the host:port and keeps the scheme's credentials.
func target(server, pushAddress string) (string, credentials.TransportCredentials, error) {
	u, err := url.Parse(server)
	if err != nil || u.Hostname() == "" {
		return "", nil, fmt.Errorf("server %q is not an http(s) URL", server)
	}
	var addr string
	var creds credentials.TransportCredentials
	switch u.Scheme {
	case "https":
		addr = net.JoinHostPort(u.Hostname(), pushPort)
		creds = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	case "http":
		port := u.Port()
		if port == "" {
			port = "80"
		}
		addr = net.JoinHostPort(u.Hostname(), port)
		creds = insecure.NewCredentials()
	default:
		return "", nil, fmt.Errorf("server %q is not an http(s) URL", server)
	}
	if pushAddress != "" {
		addr = pushAddress
	}
	return "passthrough:///" + addr, creds, nil
}

// dial opens Sync on cfg.Server, sends Hello with cfg.Machine and the bearer
// token as metadata, reads Welcome pages until more is false, and starts the
// ack reader. The client pings every pingEvery while the stream is open.
func dial(ctx context.Context, cfg config.Client) (*uplink, error) {
	tgt, creds, err := target(cfg.Server, cfg.PushAddress)
	if err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient(tgt,
		grpc.WithTransportCredentials(creds),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: pingEvery, Timeout: pingTimeout}),
	)
	if err != nil {
		return nil, err
	}
	sctx, cancel := context.WithCancel(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+cfg.Token))
	u := &uplink{cancel: cancel, sources: make(map[string]*wire.Source), unacked: make(map[string]int), full: make(map[string]bool), fin: make(chan struct{})}
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

// put is one chunk the decision logic cut: a PUT's fields, now an Append.
type put struct {
	path, harness, srcID, sourceName string
	offset                           int64
	headHash                         string
	rawEnd                           int64
	body                             []byte
}

// send writes one Append. It blocks while window.appends appends or
// window.bytes body bytes are unacked, applying the acks that arrive, and
// returns the stream's error once the stream ended.
func (u *uplink) send(ctx context.Context, p put) error {
	if err := u.sendOne(ctx, p); err != nil {
		return streamErr{err}
	}
	return nil
}

// streamErr marks an error that ended the stream, as opposed to one that
// failed a single file.
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
	u.queue = append(u.queue, inflight{seq: u.seq, path: p.path, srcID: p.srcID, bytes: size})
	u.queued += size
	u.unacked[p.srcID]++
	prev := u.sources[p.srcID]
	next := &wire.Source{SrcID: p.srcID, Harness: p.harness, Size: p.offset + size, HeadHash: p.headHash, RawEnd: p.rawEnd}
	if prev != nil {
		next.UpdatedMs = prev.UpdatedMs
	}
	u.sources[p.srcID] = next
	return nil
}

// cause is why a finished stream ended.
func (u *uplink) cause() error {
	if u.err != nil {
		return u.err
	}
	return io.EOF
}

// consume applies one delivered ack; an ack out of order ends the stream.
func (u *uplink) consume(a ack) error {
	if len(u.queue) == 0 || u.queue[0].seq != a.seq {
		u.cancel()
		return fmt.Errorf("sync: ack %d does not answer the oldest append in flight", a.seq)
	}
	if u.applyAck(a) {
		u.redo = append(u.redo, u.queue[0].path)
		u.full[u.queue[0].srcID] = true
	}
	head := u.queue[0]
	u.queue = u.queue[1:]
	u.queued -= head.bytes
	return nil
}

// applyAck folds a into sources and unacked, as the uplink comment says, and
// reports whether the path of the append it answers must be re-decided (a
// conflict). The append stays on the queue until consume pops it.
func (u *uplink) applyAck(a ack) (redo bool) {
	src := u.queue[0].srcID
	u.unacked[src]--
	if u.unacked[src] == 0 {
		delete(u.unacked, src)
	}
	if !a.conflict && u.unacked[src] > 0 {
		return false
	}
	if a.conflict && a.state.Size == 0 && a.state.HeadHash == "" {
		delete(u.sources, src)
	} else {
		s := a.state
		s.SrcID = src
		u.sources[src] = &s
	}
	return a.conflict
}

// busy reports whether appends of the source are still unacked.
func (u *uplink) busy(srcID string) bool { return u.unacked[srcID] > 0 }

// takeRedo returns the paths whose append was answered CONFLICT since the
// last call.
func (u *uplink) takeRedo() []string {
	r := u.redo
	u.redo = nil
	return r
}

// drain applies acks until nothing is in flight or the stream ended.
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

// close half-closes, waits for every in-flight ack, and returns the stream's
// error.
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

// ended returns a channel closed when the stream ended; cause says why.
func (u *uplink) ended() <-chan struct{} { return u.fin }

// streamSource replaces syncSource: the same skip rules against
// u.sources[srcID], the same decide/firstChunk/rawEndOf cut, every chunk sent
// without waiting for its ack (the next chunk's offset is the previous one's
// end). A conflict ack is the caller's: u.takeRedo names the path, and the
// next streamSource of it re-decides from the server's state alone (no skip
// shortcut, no tail), as the 409 retry did.
func streamSource(ctx context.Context, red Redactor, u *uplink, f fileRef, st *Stats) error {
	srcID := wire.SrcID(f.path)
	entry := u.sources[srcID]
	full := u.full[srcID]
	delete(u.full, srcID)
	if !full && entry != nil && entry.RawEnd > 0 && f.size == entry.RawEnd {
		st.Unchanged++
		return nil
	}
	// The server stamps UpdatedMs after it received the bytes, so a file whose
	// last write is older than that stamp (with a 5-minute margin for
	// client/server clock skew) has nothing new; skip it unread. Entries with
	// RawEnd 0 — the legacy shape — are saved the read by this skip alone.
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
