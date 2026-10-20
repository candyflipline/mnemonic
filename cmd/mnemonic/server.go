//go:build server

package main

import (
	"errors"
	"flag"
	"fmt"

	"github.com/candyflipline/mnemonic/internal/config"
	"github.com/candyflipline/mnemonic/internal/lake"
)

func init() {
	commands["serve"] = serve
	commands["import-ch"] = importCH
}

// serve: owned by lane mcp. Loads config.LoadServer, mounts
// auth.Middleware around store.Handler on /v1/ and mcp.Handler on /mcp,
// runs lake.Run then cache.Build every 120 s, and binds Listen.
func serve(args []string) error { panic("lane mcp") }

// importCH folds per-month ClickHouse parquet exports into the lake as the
// author's archive rows.
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
