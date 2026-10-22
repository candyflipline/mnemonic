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

	"github.com/candyflipline/mnemonic/internal/config"
	"github.com/candyflipline/mnemonic/internal/lake"
	"github.com/candyflipline/mnemonic/internal/server"
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

	srv, err := server.New(cfg.DataDir, authors)
	if err != nil {
		return err
	}
	defer srv.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	looped := make(chan struct{})
	go func() {
		defer close(looped)
		srv.Run(ctx)
	}()
	defer func() {
		stop()
		<-looped
	}()

	hs := &http.Server{Addr: cfg.Listen, Handler: srv.Handler()}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := hs.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}()
	log.Printf("mnemonic serve: listening on %s (data_dir %s)", cfg.Listen, cfg.DataDir)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("listen on %s: %w", cfg.Listen, err)
	}
	return nil
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
