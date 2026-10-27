// Package server is `mnemonic serve` without its flags: the HTTP surface
// (/v1 pushes, /mcp) over one index, and the loop that ingests raw into it.
package server

import (
	"context"
	"crypto/ed25519"
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
	ctl     *index.Controls
	ix      *index.Index
	lock    *index.IndexLock
	st      *store.Committer
	lap     *index.Lap
	push    *grpc.Server
	drain   func()
	booted  chan struct{}
	boot    *index.BootStatus
	bootOne sync.Once
	handler http.Handler
	otlp    *otlp.Receiver
}

// New opens the index under dataDir, loads the session controls from raw,
// and mounts the store (push scope) and MCP (read scope) behind auth.Require. MCP
// tools find the store with store.From and the controls with
// index.WithControls on their ctx. The server takes the index dir's writer
// lock for its whole life — it is the dir's one writer — and refuses to start
// under another one, naming the holder.
func New(dataDir string) (*Server, error) {
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
	boot := &index.BootStatus{}
	rcv := otlp.NewReceiver(st)
	mux := http.NewServeMux()
	push, drain := intake.Mount(st)
	mux.Handle("POST /v1/traces", auth.Require(auth.Push)(rcv.Traces()))
	mux.Handle("POST /v1/logs", auth.Require(auth.Push)(rcv.Logs()))
	mux.Handle("GET /v1/export/{kind}", auth.Require(auth.Read)(export.Handler(ix, ctl)))
	mux.Handle("/mcp", auth.Require(auth.Read)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tools.ServeHTTP(w, r.WithContext(index.WithBootStatus(index.WithControls(store.WithStore(r.Context(), st), ctl), boot)))
	})))
	return &Server{dataDir: dataDir, ctl: ctl, ix: ix, lock: lock, st: st, push: push, drain: drain, booted: make(chan struct{}), boot: boot, handler: mux, otlp: rcv}, nil
}

// Identify is the handshake's one admission rule: a key keys configures is
// its Identity, else a key a live invite admitted is that member; every
// other key is refused. Members are read from the controls at each call, so
// a join counts at the next handshake and a revoke refuses it.
func (s *Server) Identify(keys auth.Keys) func(device ed25519.PublicKey) (auth.Identity, bool) {
	keys = keys.WithMembers(s.ctl.Members)
	return func(device ed25519.PublicKey) (auth.Identity, bool) {
		return keys.Identify(auth.EncodeKey(device))
	}
}

func (s *Server) Booted() <-chan struct{} { return s.booted }

func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) StoreOTLPContent(on bool) { s.otlp.SetStoreContent(on) }

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

func (s *Server) Cycle(context.Context) error { return s.cycle("settle") }

var bootFirstDelay, bootMaxDelay = 5 * time.Second, 60 * time.Second

const (
	settleQuiet  = time.Second
	settleMax    = 3 * time.Second
	settleEvery  = 5 * time.Minute
	settleSource = 15 * time.Minute
)

func (s *Server) Run(ctx context.Context) {
	changed := s.st.Changed()
	pushPending := func() bool { return len(changed) > 0 }
	if !s.bootRetrying(ctx) {
		return
	}
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

func (s *Server) bootRetrying(ctx context.Context) bool {
	delay := bootFirstDelay
	for attempt := 1; ; attempt++ {
		err := s.cycle("boot")
		if s.lap != nil {
			s.boot.Booted()
			return true
		}
		s.boot.Failed(attempt, err)
		log.Printf("index boot attempt %d failed: %v; retrying in %s", attempt, err, delay)
		select {
		case <-ctx.Done():
			return false
		case <-time.After(delay):
		}
		delay = min(2*delay, bootMaxDelay)
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
