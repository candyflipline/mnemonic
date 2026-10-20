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
	"fmt"
	"os"
)

// commands holds every subcommand this build has; server.go adds the rest.
var commands = map[string]func(args []string) error{
	"push":  push,
	"token": token,
}

func main() {
	if len(os.Args) < 2 || commands[os.Args[1]] == nil {
		fmt.Fprintln(os.Stderr, "usage: mnemonic push|serve|import-ch|token [flags]")
		os.Exit(2)
	}
	if err := commands[os.Args[1]](os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "mnemonic:", err)
		os.Exit(1)
	}
}

// push: owned by lane push.
func push(args []string) error { panic("lane push") }
