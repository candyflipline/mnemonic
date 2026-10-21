//go:build server

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/cache"
	"github.com/candyflipline/mnemonic/internal/config"
	"github.com/candyflipline/mnemonic/internal/lake"
	"github.com/candyflipline/mnemonic/internal/mcp"
	_ "github.com/candyflipline/mnemonic/internal/normalize/all"
	"github.com/candyflipline/mnemonic/internal/query"
	"github.com/candyflipline/mnemonic/internal/store"
)

func init() {
	commands["serve"] = serve
	commands["import-ch"] = importCH
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	home, _ := os.UserHomeDir()
	configPath := fs.String("config", filepath.Join(home, ".mnemonic", "server.toml"), "server config file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.LoadServer(*configPath)
	if err != nil {
		return err
	}
	authors := make(map[string]string, len(cfg.Tokens))
	for _, t := range cfg.Tokens {
		authors[t.SHA256] = t.Author
	}

	ix, err := cache.Open(cfg.DataDir, cache.Options{MemoryLimit: "1GB", FlagsSQL: query.SearchFlagsSQL()})
	if err != nil {
		return err
	}
	defer ix.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	wake := make(chan struct{}, 1)
	looped := make(chan struct{})
	go func() {
		defer close(looped)
		convergeLoop(ctx, cfg.DataDir, ix, wake)
	}()
	defer func() {
		stop()
		<-looped
	}()

	srv := &http.Server{Addr: cfg.Listen, Handler: serveMux(cfg.DataDir, authors, ix, wake)}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}()
	log.Printf("mnemonic serve: listening on %s (data_dir %s)", cfg.Listen, cfg.DataDir)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("listen on %s: %w", cfg.Listen, err)
	}
	return nil
}

func serveMux(dataDir string, authors map[string]string, ix *cache.Index, wake chan<- struct{}) http.Handler {
	mux := http.NewServeMux()
	st := &store.Store{DataDir: dataDir, Wake: wake}
	mux.Handle("/v1/", auth.Middleware(authors)(st.Handler()))
	mux.Handle("/mcp", auth.Middleware(authors)(mcp.Handler(ix.DB(), query.Lookup)))
	return mux
}

const (
	settleQuiet = time.Second
	settleMax   = 3 * time.Second
	reconcile   = 5 * time.Minute
)

func convergeLoop(ctx context.Context, dataDir string, ix *cache.Index, wake <-chan struct{}) {
	pushPending := func() bool { return len(wake) > 0 }
	stale := cycle(ctx, dataDir, ix, "boot", pushPending)
	for {
		reason := "sweep"
		if stale == 0 || pushPending() {
			reason = "reconcile"
			select {
			case <-ctx.Done():
				return
			case <-wake:
				reason = "put"
				settle(ctx, wake)
			case <-time.After(reconcile):
			}
		}
		if ctx.Err() != nil {
			return
		}
		stale = cycle(ctx, dataDir, ix, reason, pushPending)
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

func cycle(ctx context.Context, dataDir string, ix *cache.Index, reason string, pushPending func() bool) int {
	start := time.Now()
	ls, lerr := lake.Run(dataDir, pushPending)
	lakeWall := time.Since(start)
	cs, cerr := ix.Converge(ctx)
	log.Printf("cycle %s: lake sources=%d rewrote=%d unchanged=%d raw_only=%d failed=%d stale=%d events=%d in %s err=%v; "+
		"converge files=%d batches=%d inserted=%d deleted=%d moved=%d reordered=%d sessions=%d in %s err=%v; wall %s",
		reason, ls.Sources, ls.Rewrote, ls.Unchanged, ls.RawOnly, ls.Failed, ls.Stale, ls.Events, lakeWall.Round(time.Millisecond), lerr,
		cs.Files, cs.Batches, cs.Inserted, cs.Deleted, cs.Moved, cs.Reordered, cs.Sessions,
		(time.Since(start) - lakeWall).Round(time.Millisecond), cerr, time.Since(start).Round(time.Millisecond))
	return ls.Stale
}

func importCH(args []string) error {
	fs := flag.NewFlagSet("import-ch", flag.ContinueOnError)
	configPath := fs.String("config", "", "server config toml")
	author := fs.String("author", "", "author to import as (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *author == "" {
		return errors.New("import-ch: --author is required")
	}
	if *configPath == "" {
		return errors.New("import-ch: --config is required")
	}
	if fs.NArg() != 1 {
		return errors.New("usage: mnemonic import-ch --config <server.toml> --author <author> <export_dir>")
	}
	cfg, err := config.LoadServer(*configPath)
	if err != nil {
		return err
	}
	st, err := lake.ImportClickHouse(cfg.DataDir, fs.Arg(0), *author)
	if err != nil {
		return err
	}
	fmt.Printf("import-ch: sources=%d rewrote=%d unchanged=%d raw_only=%d failed=%d events=%d\n",
		st.Sources, st.Rewrote, st.Unchanged, st.RawOnly, st.Failed, st.Events)
	return nil
}
