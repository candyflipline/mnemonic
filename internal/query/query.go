package query

import (
	"context"
	"database/sql"
	"encoding/json"
)

// Tool is one MCP tool. Call validates args against its inputSchema (the
// boundary), queries db, and returns the structured data plus the prose.
type Tool interface {
	Name() string
	Call(ctx context.Context, db *sql.DB, args json.RawMessage) (data any, text string, err error)
}

// ArgError is a caller mistake (MCP isError result), not a server failure.
type ArgError struct{ Msg string }

func (e *ArgError) Error() string { return e.Msg }

const published = "event_order IS NOT NULL"

var tools = map[string]Tool{}

func register(t Tool) { tools[t.Name()] = t }

func Lookup(name string) (Tool, bool) {
	t, ok := tools[name]
	return t, ok
}
