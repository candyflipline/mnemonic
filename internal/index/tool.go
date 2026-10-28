package index

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/store"
)

// Caller is the transport-proven identity a tool runs for, with the
// server's session controls and the store its writes append to.
type Caller struct {
	Identity auth.Identity
	Controls *Controls
	Store    *store.Committer
}

// Tool is one MCP tool. Call validates args against its inputSchema (the
// boundary), reads db, and returns the structured data plus the prose.
type Tool interface {
	Name() string
	Call(ctx context.Context, c *Caller, db *DB, args json.RawMessage) (data any, text string, err error)
}

type ArgError struct{ Msg string }

func (e *ArgError) Error() string { return e.Msg }

var tools = map[string]Tool{}

func register(t Tool) { tools[t.Name()] = t }

func Lookup(name string) (Tool, bool) {
	t, ok := tools[name]
	return t, ok
}

func Call(ctx context.Context, c *Caller, db *DB, tool string, args json.RawMessage) (data json.RawMessage, text string, err error) {
	defer func() {
		if p := recover(); p != nil {
			data, text, err = nil, "", fmt.Errorf("panic: %v", p)
		}
	}()
	t, ok := tools[tool]
	if !ok {
		return nil, "", fmt.Errorf("unknown tool %s", tool)
	}
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	v, text, err := t.Call(ctx, c, db, args)
	if v == nil {
		return nil, text, err
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if merr := enc.Encode(v); merr != nil {
		return nil, text, merr
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), text, err
}
