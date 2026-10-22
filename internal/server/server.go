// Package server is `mnemonic serve` without its flags: the HTTP surface
// (/v1 pushes, /mcp) over one cache, and the loop that parses raw into the
// lake and converges the cache.
package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/cache"
	"github.com/candyflipline/mnemonic/internal/lake"
	"github.com/candyflipline/mnemonic/internal/mcp"
	_ "github.com/candyflipline/mnemonic/internal/normalize/all"
	"github.com/candyflipline/mnemonic/internal/otlp"
	"github.com/candyflipline/mnemonic/internal/query"
	"github.com/candyflipline/mnemonic/internal/store"
)

type Server struct {
	dataDir string
	ix      *cache.Index
	wake    chan struct{}
	handler http.Handler
	otlp    *otlp.Receiver
}

// New opens the cache under dataDir and mounts the store and MCP behind
// auth, with authors mapping auth.Hash(token) to author. MCP tools find the
// store with store.From on their ctx.
func New(dataDir string, authors map[string]string) (*Server, error) {
	ix, err := cache.Open(dataDir, cache.Options{MemoryLimit: "1GB", FlagsSQL: query.SearchFlagsSQL()})
	if err != nil {
		return nil, err
	}
	wake := make(chan struct{}, 1)
	st := &store.Store{DataDir: dataDir, Wake: wake}
	tools := mcp.Handler(ix.DB(), query.Lookup)
	rcv := otlp.NewReceiver(st)
	mux := http.NewServeMux()
	mux.Handle("/v1/", auth.Middleware(authors)(st.Handler()))
	mux.Handle("POST /v1/traces", auth.Middleware(authors)(rcv.Traces()))
	mux.Handle("POST /v1/logs", auth.Middleware(authors)(rcv.Logs()))
	mux.Handle("/mcp", auth.Middleware(authors)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tools.ServeHTTP(w, r.WithContext(store.WithStore(r.Context(), st)))
	})))
	return &Server{dataDir: dataDir, ix: ix, wake: wake, handler: mux, otlp: rcv}, nil
}

func (s *Server) Handler() http.Handler { return s.handler }

// StoreOTLPContent sets whether OTLP message and tool content is stored; it
// is off until set.
func (s *Server) StoreOTLPContent(on bool) { s.otlp.SetStoreContent(on) }

func (s *Server) Close() error { return s.ix.Close() }

// Cycle parses and converges everything pending now, retrying failed lake
// files, and returns once the parser sweep has no stale source left.
func (s *Server) Cycle(ctx context.Context) error {
	sweep := lake.Sweep{}
	for {
		ls, err := s.cycle(ctx, "reconcile", func() bool { return false }, sweep)
		if err != nil || ls.Stale == 0 {
			return err
		}
		sweep = ls.Next
	}
}

const (
	settleQuiet = time.Second
	settleMax   = 3 * time.Second
	reconcile   = 5 * time.Minute
)

// Run converges at boot, after each push once pushes settle (1 s quiet, 3 s
// at most), and every 5 min without one, until ctx is done.
func (s *Server) Run(ctx context.Context) {
	pushPending := func() bool { return len(s.wake) > 0 }
	ls, _ := s.cycle(ctx, "boot", pushPending, lake.Sweep{})
	reconciled := time.Now()
	for {
		reason := "sweep"
		if due := time.Until(reconciled.Add(reconcile)); due <= 0 {
			reason = "reconcile"
		} else if ls.Stale == 0 || pushPending() {
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
		ls, _ = s.cycle(ctx, reason, pushPending, ls.Next)
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

func (s *Server) cycle(ctx context.Context, reason string, pushPending func() bool, sweep lake.Sweep) (lake.Stats, error) {
	start := time.Now()
	ls, lerr := lake.Run(s.dataDir, pushPending, sweep)
	lakeWall := time.Since(start)
	failed := cache.SkipFailed
	if reason == "reconcile" {
		failed = cache.RetryFailed
	}
	cs, cerr := s.ix.Converge(ctx, failed)
	log.Printf("cycle %s: lake sources=%d rewrote=%d unchanged=%d raw_only=%d failed=%d stale=%d events=%d in %s err=%v; "+
		"converge files=%d batches=%d inserted=%d deleted=%d moved=%d reordered=%d sessions=%d in %s err=%v; wall %s",
		reason, ls.Sources, ls.Rewrote, ls.Unchanged, ls.RawOnly, ls.Failed, ls.Stale, ls.Events, lakeWall.Round(time.Millisecond), lerr,
		cs.Files, cs.Batches, cs.Inserted, cs.Deleted, cs.Moved, cs.Reordered, cs.Sessions,
		(time.Since(start) - lakeWall).Round(time.Millisecond), cerr, time.Since(start).Round(time.Millisecond))
	return ls, errors.Join(lerr, cerr)
}
