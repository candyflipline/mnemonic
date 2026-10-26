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

const (
	streamWindow = 34 << 20
	connWindow   = 128 << 20
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

	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	hs := &http.Server{
		Addr:      cfg.Listen,
		Handler:   srv.Handler(),
		Protocols: &protocols,
		HTTP2: &http.HTTP2Config{
			MaxReceiveBufferPerStream:     streamWindow,
			MaxReceiveBufferPerConnection: connWindow,
			SendPingTimeout:               45 * time.Second,
			PingTimeout:                   15 * time.Second,
		},
	}
	shutdown := make(chan struct{})
	go func() {
		defer close(shutdown)
		<-ctx.Done()
		srv.Drain()
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
	<-shutdown
	return nil
}
