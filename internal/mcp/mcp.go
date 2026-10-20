// Package mcp serves MCP JSON-RPC over HTTP POST /mcp: initialize,
// tools/list (tools.json verbatim) and tools/call (query.Lookup, wrapped in
// the envelope tools.json outputSchema names: schema_version, tool, request,
// data, warnings, error, performance). Owned by lane mcp.
package mcp

import (
	"database/sql"
	_ "embed"
	"net/http"
)

//go:embed tools.json
var toolsJSON []byte

// Handler answers /mcp with the newest cache from open.
func Handler(open func() (*sql.DB, error)) http.Handler { panic("lane mcp") }
