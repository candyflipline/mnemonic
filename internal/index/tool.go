package index

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
)

// Tool is one MCP tool. Call validates args against its inputSchema (the
// boundary), reads db, and returns the structured data plus the prose. The
// caller's identity (auth.From), session controls (WithControls) and store
// (store.From) ride on ctx.
type Tool interface {
	Name() string
	Call(ctx context.Context, db *DB, args json.RawMessage) (data any, text string, err error)
}

// ArgError is a caller mistake (MCP isError result), not a server failure.
type ArgError struct{ Msg string }

func (e *ArgError) Error() string { return e.Msg }

var tools = map[string]Tool{}

func register(t Tool) { tools[t.Name()] = t }

func Lookup(name string) (Tool, bool) {
	t, ok := tools[name]
	return t, ok
}

// Call runs one tool over db and encodes its data as the batch harness
// answers a line; a panicking tool is an error.
func Call(ctx context.Context, db *DB, tool string, args json.RawMessage) (data json.RawMessage, text string, err error) {
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
	v, text, err := t.Call(ctx, db, args)
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
