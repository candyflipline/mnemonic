// Package server is `mnemonic serve` without its flags: the HTTP surface
// (/v1 pushes, /mcp) over one index, and the loop that ingests raw into it.
package server

import (
	"context"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"google.golang.org/grpc"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/export"
	"github.com/candyflipline/mnemonic/internal/index"
	"github.com/candyflipline/mnemonic/internal/intake"
	"github.com/candyflipline/mnemonic/internal/layout"
	"github.com/candyflipline/mnemonic/internal/mcp"
	"github.com/candyflipline/mnemonic/internal/otlp"
	"github.com/candyflipline/mnemonic/internal/store"
)

type Server struct {
	dataDir string
	ix      *index.Index
	lock    *index.IndexLock
	st      *store.Committer
	lap     *index.Lap
	push    *grpc.Server
	drain   func()
	booted  chan struct{}
	bootOne sync.Once
	handler http.Handler
	otlp    *otlp.Receiver
}

// New opens the index under dataDir, loads the session controls from raw,
// and mounts the store (push scope) and MCP (read scope) behind tokens. MCP
// tools find the store with store.From and the controls with
// index.WithControls on their ctx. The server takes the index dir's writer
// lock for its whole life — it is the dir's one writer — and refuses to start
// under another one, naming the holder.
func New(dataDir string, tokens auth.Tokens) (*Server, error) {
	ctl, err := index.LoadControls(dataDir)
	if err != nil {
		return nil, err
	}
	ix, err := index.Open(layout.IndexDir(dataDir))
	if err != nil {
		return nil, err
	}
	lock, err := index.HoldIndexLock(layout.IndexDir(dataDir))
	if err != nil {
		ix.Close()
		return nil, err
	}
	st, err := store.Open(dataDir)
	if err != nil {
		lock.Unlock()
		ix.Close()
		return nil, err
	}
	tools := mcp.Handler(ix, index.Lookup)
	rcv := otlp.NewReceiver(st)
	mux := http.NewServeMux()
	push, drain := intake.Mount(st, tokens)
	mux.Handle("POST /v1/traces", tokens.Require(auth.Push)(rcv.Traces()))
	mux.Handle("POST /v1/logs", tokens.Require(auth.Push)(rcv.Logs()))
	mux.Handle("GET /v1/export/{kind}", tokens.Require(auth.Read)(export.Handler(ix, ctl)))
	mux.Handle("/mcp", tokens.Require(auth.Read)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tools.ServeHTTP(w, r.WithContext(index.WithControls(store.WithStore(r.Context(), st), ctl)))
	})))
	return &Server{dataDir: dataDir, ix: ix, lock: lock, st: st, push: push, drain: drain, booted: make(chan struct{}), handler: mux, otlp: rcv}, nil
}

// Booted is closed once the index has booted: from then on its steady memory
// is all that sits beside push sessions.
func (s *Server) Booted() <-chan struct{} { return s.booted }

func (s *Server) Handler() http.Handler { return s.handler }

// StoreOTLPContent sets whether OTLP message and tool content is stored; it
// is off until set.
func (s *Server) StoreOTLPContent(on bool) { s.otlp.SetStoreContent(on) }

// ServePush serves native gRPC on the separately bound push listener.
func (s *Server) ServePush(l net.Listener) error { return s.push.Serve(l) }

// Drain ends live sessions with Unavailable. The caller first closes the
// push listener; GracefulStop sends the statuses before closing transports.
// Stop also releases streams stuck before Hello or on an unread response.
func (s *Server) Drain(ctx context.Context) {
	s.drain()
	done := make(chan struct{})
	go func() {
		s.push.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		s.push.Stop()
		<-done
	}
}

// Close commits what the store has queued, then closes the index.
func (s *Server) Close() error {
	s.drain()
	s.push.Stop()
	err := s.st.Close()
	if ierr := s.ix.Close(); err == nil {
		err = ierr
	}
	if lerr := s.lock.Unlock(); err == nil {
		err = lerr
	}
	return err
}

// Cycle ingests everything pending now.
func (s *Server) Cycle(context.Context) error { return s.cycle("settle") }

const (
	settleQuiet = time.Second
	settleMax   = 3 * time.Second
	settleEvery = 5 * time.Minute
	// settleSource is how long a source's manifest must go without a push
	// before it leaves its hot part for a cold one.
	settleSource = 15 * time.Minute
)

// Run lists everything once at boot, then ingests the sources changed since
// the last cycle: after each push once pushes settle (1 s quiet, 3 s at most),
// and every 5 min without one so hot sources settle cold, until ctx is done. A
// parser version bump leaves sources for the sweep: cycles then run back to
// back, each parsing stale sources under the sweep budget, until none are
// left.
func (s *Server) Run(ctx context.Context) {
	changed := s.st.Changed()
	pushPending := func() bool { return len(changed) > 0 }
	s.cycle("boot")
	settled := time.Now()
	for {
		reason := "sweep"
		if due := time.Until(settled.Add(settleEvery)); due <= 0 {
			reason = "settle"
		} else if s.ix.SweepStale() == 0 || pushPending() {
			reason = "settle"
			select {
			case <-ctx.Done():
				return
			case <-changed:
				reason = "put"
				settle(ctx, changed)
			case <-time.After(due):
			}
		}
		if ctx.Err() != nil {
			return
		}
		if reason == "settle" {
			settled = time.Now()
		}
		s.cycle(reason)
	}
}

func settle(ctx context.Context, wake <-chan struct{}) {
	quiet := time.NewTimer(settleQuiet)
	defer quiet.Stop()
	limit := time.NewTimer(settleMax)
	defer limit.Stop()
	for {
		select {
		case <-wake:
			quiet.Reset(settleQuiet)
		case <-quiet.C:
			return
		case <-limit.C:
			return
		case <-ctx.Done():
			return
		}
	}
}

func (s *Server) cycle(reason string) error {
	start := time.Now()
	opts := index.IngestOptions{Quiet: settleSource}
	var st index.IngestStats
	var err error
	if s.lap == nil {
		s.lap, st, err = s.ix.Boot(s.dataDir, s.st.Manifests(), opts)
		if s.lap != nil {
			s.bootOne.Do(func() { close(s.booted) })
		}
	} else {
		st, err = s.lap.Next(s.st.TakeChanged(), opts)
	}
	cs := s.ix.LastCycle()
	log.Printf("cycle %s: sources unchanged=%d resumed=%d fresh=%d reparsed=%d failed=%d stale=%d; "+
		"wrote logs=%d parts=%d built=%d hot=%d cold=%d in %s err=%v",
		reason, st[index.Unchanged], st[index.Resumed], st[index.Fresh], st[index.Reparsed],
		s.ix.CycleFailed(), s.ix.SweepStale(),
		cs.LogBytes, cs.PartBytes, cs.PartsBuilt, cs.HotParts, cs.ColdParts,
		time.Since(start).Round(time.Millisecond), err)
	return err
}
