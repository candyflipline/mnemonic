//go:build server

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/config"
	"github.com/candyflipline/mnemonic/internal/qnet"
	"github.com/candyflipline/mnemonic/internal/server"
)

const (
	pushDrainDeadline    = 10 * time.Second
	httpShutdownDeadline = 10 * time.Second
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
	priv, err := qnet.LoadOrCreateKey(cfg.QUIC.KeyFile)
	if err != nil {
		return err
	}
	var share *server.Share
	if cfg.Share != nil {
		share = &server.Share{BaseURL: cfg.Share.BaseURL, QUICAddr: cfg.Share.QUICAddr, ServerKey: qnet.PublicKey(priv)}
	}
	srv, err := server.New(cfg.DataDir, share)
	if err != nil {
		return err
	}
	defer srv.Close()
	srv.StoreOTLPContent(cfg.OTLP.StoreContent)

	qs, err := qnet.Listen(cfg.QUIC.Listen, priv, srv.Identify(cfg.Identities()))
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.QUIC.Listen, err)
	}
	defer qs.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	looped := make(chan struct{})
	go func() {
		defer close(looped)
		srv.Run(ctx)
	}()

	hs := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if id, ok := qnet.IdentityOf(c); ok {
				return auth.NewContext(ctx, id)
			}
			return ctx
		},
	}
	ended := make(chan error, 3)
	go func() { ended <- hs.Serve(qs.HTTP) }()
	var public *http.Server
	if cfg.Share != nil {
		ln, err := net.Listen("tcp", cfg.Share.Listen)
		if err != nil {
			return fmt.Errorf("[share] listen on %s: %w", cfg.Share.Listen, err)
		}
		public = &http.Server{Handler: srv.Public(), ReadHeaderTimeout: 10 * time.Second}
		go func() { ended <- public.Serve(ln) }()
		log.Printf("mnemonic serve: links on %s (TCP, loopback) as %s", ln.Addr(), cfg.Share.BaseURL)
	}
	go func() {
		select {
		case <-srv.Booted():
			log.Printf("mnemonic serve: push serving (index booted)")
			ended <- srv.ServePush(qs.Push)
		case <-ctx.Done():
		}
	}()
	log.Printf("mnemonic serve: QUIC %s (UDP), server key %s, data_dir %s", qs.Addr(), qnet.PublicKey(priv), cfg.DataDir)
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-ended:
	}
	stop()
	qs.Push.Close()
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), pushDrainDeadline)
	srv.Drain(drainCtx)
	cancelDrain()
	httpCtx, cancelHTTP := context.WithTimeout(context.Background(), httpShutdownDeadline)
	if err := hs.Shutdown(httpCtx); err != nil {
		log.Printf("shutdown: %v", err)
		hs.Close()
	}
	if public != nil {
		if err := public.Shutdown(httpCtx); err != nil {
			log.Printf("shutdown links: %v", err)
			public.Close()
		}
	}
	cancelHTTP()
	qs.Close()
	<-looped
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", serveErr)
	}
	return nil
}
