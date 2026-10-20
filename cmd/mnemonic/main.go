// mnemonic collects agent session traces and serves them over MCP.
//
//	mnemonic push [--config ~/.mnemonic/config.toml]   one pass, client
//	mnemonic serve --config server.toml                HTTP /v1 + /mcp, normalize and cache loops
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
	"time"

	"github.com/candyflipline/mnemonic/internal/config"
	pushrun "github.com/candyflipline/mnemonic/internal/push"
	"github.com/candyflipline/mnemonic/internal/redact"
)

// commands holds every subcommand this build has; server.go adds the rest.
var commands = map[string]func(args []string) error{
	"push": push,
}

func main() {
	if len(os.Args) < 2 || commands[os.Args[1]] == nil {
		fmt.Fprintln(os.Stderr, "usage: mnemonic push|serve|import-ch [flags]")
		os.Exit(2)
	}
	if err := commands[os.Args[1]](os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "mnemonic:", err)
		os.Exit(1)
	}
}

// push: one client pass — load the config, scan, diff against the server's
// manifest, send redacted whole lines.
func push(args []string) error {
	fs := flag.NewFlagSet("push", flag.ContinueOnError)
	configPath := fs.String("config", "~/.mnemonic/config.toml", "path to the client config TOML")
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
	// The pass is one manifest GET plus at most a couple of PUTs per source;
	// 60 s covers a slow link without pinning a wedged connection.
	stats, err := pushrun.Run(context.Background(), cfg, red, &http.Client{Timeout: 60 * time.Second})
	if err != nil {
		return err
	}
	fmt.Printf("scanned %d, appended %d, replaced %d, unchanged %d, failed %d, sent %d bytes\n",
		stats.Scanned, stats.Appended, stats.Replaced, stats.Unchanged, stats.Failed, stats.Bytes)
	return nil
}
