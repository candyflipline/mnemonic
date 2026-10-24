//go:build server

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/config"
	"github.com/candyflipline/mnemonic/internal/index"
	"github.com/candyflipline/mnemonic/internal/layout"
	"github.com/candyflipline/mnemonic/internal/mcp"
	pushrun "github.com/candyflipline/mnemonic/internal/push"
	"github.com/candyflipline/mnemonic/internal/redact"
	"github.com/candyflipline/mnemonic/internal/store"
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

// settleSource is how long a file's mtime must lag the newest one before it
// settles into a cold part, what serve uses.
const settleSource = 15 * time.Minute

// runLocal indexes this machine's harness files in place under
// opts.dataDir (the engine's KindPlain sources, no raw copy, no store, no HTTP
// and no push loop), feeds push.Watch's file events to the index ingest, and
// serves MCP with every tool result redacted at the output boundary.
//
// The files are config.DefaultSources; there is no config file and no server.
// MCP is loopback-only (requireLoopback) and answers the bearer token "local"
// as this machine's user with every scope. Session controls load from
// dataDir, empty until a control tool writes its first record through the
// store under the same dataDir.
func runLocal(ctx context.Context, opts localOptions, out, logOut io.Writer) error {
	if err := requireLoopback(opts.listen); err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if err := refuseClientDir(opts.dataDir, home); err != nil {
		return err
	}
	if err := os.MkdirAll(opts.dataDir, 0o755); err != nil {
		return err
	}
	logger := log.New(logOut, "mnemonic local: ", log.LstdFlags)
	srcs, err := expandSources(config.DefaultSources)
	if err != nil {
		return err
	}
	author, machine := localIdentity()
	red, err := redact.New(nil)
	if err != nil {
		return err
	}
	ix, err := index.Open(layout.IndexDir(opts.dataDir))
	if err != nil {
		return err
	}
	defer ix.Close()
	ctl, err := index.LoadControls(opts.dataDir)
	if err != nil {
		return err
	}
	st := &store.Store{DataDir: opts.dataDir}
	if err := firstIngest(ix, srcs, logger, author, machine); err != nil {
		return err
	}
	tokens := auth.NewTokens(map[string]auth.Token{
		auth.Hash("local"): {Author: author, Scopes: slices.Clone(auth.Scopes[:])},
	})
	tools := mcp.Handler(ix, index.Lookup, red)
	mux := http.NewServeMux()
	mux.Handle("/mcp", tokens.Require(auth.Read)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tools.ServeHTTP(w, r.WithContext(index.WithControls(store.WithStore(r.Context(), st), ctl)))
	})))
	hs := &http.Server{Addr: opts.listen, Handler: mux}
	sink := indexSink{ix: ix, srcs: srcs, author: author, machine: machine}
	go func() {
		if err := pushrun.WatchSink(ctx, config.Client{Sources: srcs}, sink, out); err != nil {
			logger.Printf("watch: %v", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := hs.Shutdown(shutdownCtx); err != nil {
			logger.Printf("shutdown: %v", err)
		}
	}()
	logger.Printf("listening on %s (data_dir %s, author %s, machine %s)", opts.listen, opts.dataDir, author, machine)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("listen on %s: %w", opts.listen, err)
	}
	return nil
}

// expandSources resolves the ~ in the client's source globs against this
// machine's HOME. A config file load expands them on the way in; local mode
// runs on DefaultSources with no config file, and neither the watcher nor a
// glob expands ~ itself.
func expandSources(srcs []config.SourceGlob) ([]config.SourceGlob, error) {
	out := make([]config.SourceGlob, len(srcs))
	for i, src := range srcs {
		glob, err := config.ExpandHome(src.Glob)
		if err != nil {
			return nil, err
		}
		src.Glob = glob
		out[i] = src
	}
	return out, nil
}

// firstIngest indexes the machine's files once before MCP serves, so the
// store answers as soon as the socket is up; the watcher keeps it current.
func firstIngest(ix *index.Index, srcs []config.SourceGlob, logger *log.Logger, author, machine string) error {
	start := time.Now()
	st, err := ix.IngestPlain(srcs, author, machine, index.IngestOptions{Quiet: settleSource})
	logger.Printf("index: unchanged=%d resumed=%d fresh=%d reparsed=%d in %s err=%v",
		st[index.Unchanged], st[index.Resumed], st[index.Fresh], st[index.Reparsed],
		time.Since(start).Round(time.Millisecond), err)
	return err
}

// indexSink is the watcher's sink: every changed path is subsumed by an
// ingest of the whole machine, whose outcomes the printed stats line reports.
// An ingest has no ctx (the server's cycles have none either); it runs to
// completion and the next sync's stamp tests pick up whatever moved since.
type indexSink struct {
	ix      *index.Index
	srcs    []config.SourceGlob
	author  string
	machine string
}

func (s indexSink) Sync(_ context.Context, _ []string) (pushrun.Stats, error) {
	st, err := s.ix.IngestPlain(s.srcs, s.author, s.machine, index.IngestOptions{Quiet: settleSource})
	return pushrun.Stats{
		Appended:  st[index.Resumed] + st[index.Fresh],
		Replaced:  st[index.Reparsed],
		Unchanged: st[index.Unchanged],
	}, err
}

// refuseClientDir fails when dir is home's .mnemonic or anything inside it, by
// absolute path with symlinks resolved, and ignoring case on darwin, whose
// default volumes do not tell ~/.Mnemonic from ~/.mnemonic.
func refuseClientDir(dir, home string) error {
	got, err := resolvePath(dir)
	if err != nil {
		return err
	}
	client, err := resolvePath(filepath.Join(home, ".mnemonic"))
	if err != nil {
		return err
	}
	if runtime.GOOS == "darwin" {
		got, client = strings.ToLower(got), strings.ToLower(client)
	}
	if rel, err := filepath.Rel(client, got); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("--data-dir ~/.mnemonic belongs to mnemonic push; pick another directory")
	}
	return nil
}

// resolvePath makes p absolute and resolves symlinks through its deepest
// existing ancestor, so a path that does not exist yet still lands where
// MkdirAll will create it. A dangling symlink is an error.
func resolvePath(p string) (string, error) {
	cur, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	rest := ""
	for {
		r, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return filepath.Join(r, rest), nil
		}
		if _, lerr := os.Lstat(cur); lerr == nil || !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", err
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
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
