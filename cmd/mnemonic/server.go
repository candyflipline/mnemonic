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
	"github.com/candyflipline/mnemonic/internal/server"
)

func init() {
	commands["serve"] = serve
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
	srv, err := server.New(cfg.DataDir, cfg.Identities())
	if err != nil {
		return err
	}
	defer srv.Close()
	srv.StoreOTLPContent(cfg.OTLP.StoreContent)

	httpListener, pushListener, err := listen(cfg)
	if err != nil {
		return err
	}
	defer httpListener.Close()
	defer pushListener.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	looped := make(chan struct{})
	go func() {
		defer close(looped)
		srv.Run(ctx)
	}()

	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	ended := make(chan error, 2)
	go func() { ended <- hs.Serve(httpListener) }()
	go func() { ended <- srv.ServePush(pushListener) }()
	log.Printf("mnemonic serve: HTTP %s, push %s (data_dir %s)", cfg.Listen, cfg.PushListen, cfg.DataDir)
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-ended:
	}
	// Stop accepting push first, drain sessions and transports, then HTTP.
	// The deferred Close runs only after the cycle loop stops, and commits
	// queued writes before releasing the store and index.
	pushListener.Close()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Drain(shutdownCtx)
	if err := hs.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
		hs.Close()
	}
	stop()
	<-looped
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", serveErr)
	}
	return nil
}
