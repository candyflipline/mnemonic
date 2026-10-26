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
	go func() {
		select {
		case <-srv.Booted():
			log.Printf("mnemonic serve: push serving (index booted)")
			ended <- srv.ServePush(pushListener)
		case <-ctx.Done():
		}
	}()
	log.Printf("mnemonic serve: HTTP %s, push %s (data_dir %s)", cfg.Listen, cfg.PushListen, cfg.DataDir)
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-ended:
	}
	stop()
	pushListener.Close()
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), pushDrainDeadline)
	srv.Drain(drainCtx)
	cancelDrain()
	httpCtx, cancelHTTP := context.WithTimeout(context.Background(), httpShutdownDeadline)
	if err := hs.Shutdown(httpCtx); err != nil {
		log.Printf("shutdown: %v", err)
		hs.Close()
	}
	cancelHTTP()
	<-looped
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", serveErr)
	}
	return nil
}
