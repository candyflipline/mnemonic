// Package server is `mnemonic serve` without its flags: the HTTP surface
// (/v1 pushes, /mcp) over one index, and the loop that ingests raw into it.
package server

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/index"
	"github.com/candyflipline/mnemonic/internal/layout"
	"github.com/candyflipline/mnemonic/internal/mcp"
	"github.com/candyflipline/mnemonic/internal/otlp"
	"github.com/candyflipline/mnemonic/internal/store"
)

type Server struct {
	dataDir string
	ix      *index.Index
	lock    *index.IndexLock
	wake    chan struct{}
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
	wake := make(chan struct{}, 1)
	st := &store.Store{DataDir: dataDir, Wake: wake}
	tools := mcp.Handler(ix, index.Lookup)
	rcv := otlp.NewReceiver(st)
	mux := http.NewServeMux()
	mux.Handle("/v1/", tokens.Require(auth.Push)(st.Handler()))
	mux.Handle("POST /v1/traces", tokens.Require(auth.Push)(rcv.Traces()))
	mux.Handle("POST /v1/logs", tokens.Require(auth.Push)(rcv.Logs()))
	mux.Handle("/mcp", tokens.Require(auth.Read)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tools.ServeHTTP(w, r.WithContext(index.WithControls(store.WithStore(r.Context(), st), ctl)))
	})))
	return &Server{dataDir: dataDir, ix: ix, lock: lock, wake: wake, handler: mux, otlp: rcv}, nil
}

func (s *Server) Handler() http.Handler { return s.handler }

// StoreOTLPContent sets whether OTLP message and tool content is stored; it
// is off until set.
func (s *Server) StoreOTLPContent(on bool) { s.otlp.SetStoreContent(on) }

func (s *Server) Close() error {
	err := s.ix.Close()
	if lerr := s.lock.Unlock(); err == nil {
		err = lerr
	}
	return err
}

// Cycle ingests everything pending now.
func (s *Server) Cycle(context.Context) error { return s.cycle("reconcile") }

const (
	settleQuiet = time.Second
	settleMax   = 3 * time.Second
	reconcile   = 5 * time.Minute
	// settleSource is how long a source's manifest must go without a push
	// before it leaves its hot part for a cold one.
	settleSource = 15 * time.Minute
)

// Run ingests at boot, after each push once pushes settle (1 s quiet, 3 s
// at most), and every 5 min without one, until ctx is done. A parser version
// bump leaves sources for the sweep: cycles then run back to back, each
// parsing stale sources under the sweep budget, until none are left.
func (s *Server) Run(ctx context.Context) {
	pushPending := func() bool { return len(s.wake) > 0 }
	s.cycle("boot")
	reconciled := time.Now()
	for {
		reason := "sweep"
		if due := time.Until(reconciled.Add(reconcile)); due <= 0 {
			reason = "reconcile"
		} else if s.ix.SweepStale() == 0 || pushPending() {
			reason = "reconcile"
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
				reason = "put"
				settle(ctx, s.wake)
			case <-time.After(due):
			}
		}
		if ctx.Err() != nil {
			return
		}
		if reason == "reconcile" {
			reconciled = time.Now()
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
	st, err := s.ix.Ingest(s.dataDir, index.IngestOptions{Quiet: settleSource})
	cs := s.ix.LastCycle()
	log.Printf("cycle %s: sources unchanged=%d resumed=%d fresh=%d reparsed=%d failed=%d stale=%d; "+
		"wrote logs=%d parts=%d built=%d hot=%d cold=%d in %s err=%v",
		reason, st[index.Unchanged], st[index.Resumed], st[index.Fresh], st[index.Reparsed],
		s.ix.CycleFailed(), s.ix.SweepStale(),
		cs.LogBytes, cs.PartBytes, cs.PartsBuilt, cs.HotParts, cs.ColdParts,
		time.Since(start).Round(time.Millisecond), err)
	return err
}
