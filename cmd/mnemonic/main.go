// mnemonic collects agent session traces and serves them over MCP.
//
//	mnemonic push [--watch] [--config ~/.mnemonic/config.toml]   one pass, or resident and woken by file writes
//	mnemonic serve --config server.toml                HTTP /v1 + /mcp, one lake and index loop
//	mnemonic import-ch --config server.toml --author A <export_dir>
//
// push builds with CGO_ENABLED=0; serve and import-ch need the server build
// tag (DuckDB): go build -tags server ./cmd/mnemonic
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/candyflipline/mnemonic/internal/config"
	pushrun "github.com/candyflipline/mnemonic/internal/push"
	"github.com/candyflipline/mnemonic/internal/redact"
)

var commands = map[string]func(args []string) error{
	"push":  push,
	"token": token,
	"init":  initCmd,
}

func main() {
	if len(os.Args) < 2 || commands[os.Args[1]] == nil {
		fmt.Fprintln(os.Stderr, "usage: mnemonic init|push|serve|import-ch|token [flags]")
		os.Exit(2)
	}
	if err := commands[os.Args[1]](os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "mnemonic:", err)
		os.Exit(1)
	}
}

func push(args []string) error {
	fs := flag.NewFlagSet("push", flag.ContinueOnError)
	configPath := fs.String("config", "~/.mnemonic/config.toml", "path to the client config TOML")
	watch := fs.Bool("watch", false, "stay resident and push files within 60 s of their writes")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.LoadClient(*configPath)
	if err != nil {
		return err
	}
	red, err := redact.New(cfg.Redaction.ExtraPatterns)
	if err != nil {
		return err
	}
	hc := &http.Client{Timeout: 60 * time.Second}
	if *watch {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return pushrun.Watch(ctx, cfg, red, hc, os.Stdout)
	}
	stats, err := pushrun.Run(context.Background(), cfg, red, hc)
	if err != nil {
		return err
	}
	fmt.Println(stats)
	return nil
}
