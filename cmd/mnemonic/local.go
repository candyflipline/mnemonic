//go:build server

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"

	"github.com/candyflipline/mnemonic/internal/config"
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

// runLocal indexes this machine's harness files in place under
// opts.dataDir (the engine's KindPlain sources, no raw copy, no store, no HTTP
// and no push loop), feeds push.Watch's file events to the index ingest, and
// serves MCP with every tool result redacted at the output boundary.
func runLocal(ctx context.Context, opts localOptions, out, logOut io.Writer) error {
	panic("lane U10")
}

// publishToken replaces path with tok through a temp file and a rename, so a
// reader sees the old token or the new one, never a torn file.
func publishToken(path, tok string) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".token.*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	_, err = f.WriteString(tok + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, path)
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
