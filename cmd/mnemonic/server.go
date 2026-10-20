//go:build server

package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/cache"
	"github.com/candyflipline/mnemonic/internal/config"
	"github.com/candyflipline/mnemonic/internal/lake"
	"github.com/candyflipline/mnemonic/internal/mcp"
	_ "github.com/candyflipline/mnemonic/internal/normalize/all"
	"github.com/candyflipline/mnemonic/internal/store"
)

func init() {
	commands["serve"] = serve
	commands["import-ch"] = importCH
}

// serve: owned by lane mcp. Loads config.LoadServer, mounts
// auth.Middleware around store.Handler on /v1/ and mcp.Handler on /mcp,
// runs lake.Run then cache.Build every 120 s, and binds Listen.
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

	// The queries read one DuckDB file; the rebuild loop swaps it in when a
	// newer cache appears. The old handle stays valid for in-flight calls
	// until the swap is done.
	var dbMu sync.Mutex
	var db *sql.DB
	var dbPath string
	open := func() (*sql.DB, error) {
		dbMu.Lock()
		defer dbMu.Unlock()
		if db != nil {
			return db, nil
		}
		conn, path, err := cache.OpenNewest(cfg.DataDir)
		if err != nil {
			return nil, err
		}
		db, dbPath = conn, path
		return db, nil
	}
	rebuild := func() {
		if stats, err := lake.Run(cfg.DataDir); err != nil {
			log.Printf("lake: %v", err)
		} else {
			log.Printf("lake: sources=%d rewrote=%d unchanged=%d raw_only=%d failed=%d events=%d",
				stats.Sources, stats.Rewrote, stats.Unchanged, stats.RawOnly, stats.Failed, stats.Events)
		}
		if stats, err := cache.Build(cfg.DataDir); err != nil {
			log.Printf("cache: %v", err)
		} else {
			log.Printf("cache: %s lake_files=%d events=%d docs=%d postings=%d",
				stats.Path, stats.LakeFiles, stats.Events, stats.Docs, stats.Postings)
		}
		conn, path, err := cache.OpenNewest(cfg.DataDir)
		if err != nil {
			log.Printf("cache: open newest: %v", err)
			return
		}
		dbMu.Lock()
		defer dbMu.Unlock()
		if path == dbPath && db != nil {
			conn.Close() // nothing changed; keep the open handle
			return
		}
		old := db
		db, dbPath = conn, path
		if old != nil {
			old.Close()
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		rebuild()
		ticker := time.NewTicker(120 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				rebuild()
			}
		}
	}()

	mux := http.NewServeMux()
	st := store.Store{DataDir: cfg.DataDir}
	mux.Handle("/v1/", auth.Middleware(authors)(st.Handler()))
	mux.Handle("/mcp", auth.Middleware(authors)(mcp.Handler(open)))
	srv := &http.Server{Addr: cfg.Listen, Handler: mux}
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

// importCH folds per-month ClickHouse parquet exports into the lake as the
// author's archive rows.
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
