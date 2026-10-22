//go:build server

package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/config"
	pushrun "github.com/candyflipline/mnemonic/internal/push"
	"github.com/candyflipline/mnemonic/internal/redact"
	"github.com/candyflipline/mnemonic/internal/server"
	"github.com/candyflipline/mnemonic/internal/wire"
)

func init() {
	commands["local"] = local
}

type localOptions struct {
	dataDir string
	listen  string
}

func local(args []string) error {
	fs := flag.NewFlagSet("local", flag.ContinueOnError)
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	var opts localOptions
	fs.StringVar(&opts.dataDir, "data-dir", filepath.Join(home, ".mnemonic-local"), "where raw, parquet and the index live")
	fs.StringVar(&opts.listen, "listen", "127.0.0.1:7781", "loopback address for MCP")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runLocal(ctx, opts, os.Stdout, os.Stderr)
}

func runLocal(ctx context.Context, opts localOptions, out, logOut io.Writer) error {
	if err := requireLoopback(opts.listen); err != nil {
		return err
	}
	if home, err := os.UserHomeDir(); err == nil && filepath.Clean(opts.dataDir) == filepath.Join(home, ".mnemonic") {
		return errors.New("--data-dir ~/.mnemonic belongs to mnemonic push; pick another directory")
	}
	if err := os.MkdirAll(opts.dataDir, 0o700); err != nil {
		return err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Errorf("read random bytes: %w", err)
	}
	tok := base64.RawURLEncoding.EncodeToString(raw)
	tokenPath := filepath.Join(opts.dataDir, "token")
	if err := os.WriteFile(tokenPath, []byte(tok+"\n"), 0o600); err != nil {
		return err
	}

	author, machine := localIdentity()
	srv, err := server.New(opts.dataDir, map[string]string{auth.Hash(tok): author})
	if err != nil {
		return err
	}
	defer srv.Close()

	ln, err := net.Listen("tcp", opts.listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", opts.listen, err)
	}
	defer ln.Close()
	base := "http://" + ln.Addr().String()

	red, err := redact.New(nil)
	if err != nil {
		return err
	}
	cfg := config.Client{Server: base, Token: tok, Machine: machine}
	for _, s := range config.DefaultSources {
		glob, err := config.ExpandHome(s.Glob)
		if err != nil {
			return err
		}
		s.Glob = glob
		cfg.Sources = append(cfg.Sources, s)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	hs := &http.Server{Handler: srv.Handler()}
	errs := make(chan error, 3)
	go func() { srv.Run(ctx); errs <- nil }()
	go func() {
		if err := hs.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()
	go func() { errs <- pushrun.Watch(ctx, cfg, red, &http.Client{Timeout: 60 * time.Second}, logOut) }()

	fmt.Fprintf(out, "mnemonic local: MCP at %s/mcp\n", base)
	fmt.Fprintf(out, "mnemonic local: bearer token in %s (author %s, data dir %s)\n", tokenPath, author, opts.dataDir)

	first := <-errs
	cancel()
	shutdownCtx, done := context.WithTimeout(context.Background(), 10*time.Second)
	defer done()
	if err := hs.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	for range 2 {
		<-errs
	}
	return first
}

func requireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("--listen %q: %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("--listen %q is not a loopback address; local mode serves this machine only (use 127.0.0.1 or ::1, or run mnemonic serve for a team server)", addr)
}

var unsafeSegment = regexp.MustCompile(`[^a-z0-9._-]+`)

func localIdentity() (author, machine string) {
	author = "local"
	if u, err := user.Current(); err == nil {
		if a := segment(u.Username); a != "" {
			author = a
		}
	}
	machine = "local"
	if host, err := os.Hostname(); err == nil {
		if m := segment(config.ShortHost(host)); m != "" {
			machine = m
		}
	}
	return author, machine
}

func segment(s string) string {
	s = strings.Trim(unsafeSegment.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if !wire.ValidSegment(s) {
		return ""
	}
	return s
}
