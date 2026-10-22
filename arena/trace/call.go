package trace

import (
	"bytes"
	"encoding/json"
	"fmt"
)

type handler interface {
	Call(db *DB, args json.RawMessage) (any, string, error)
}

type argError struct{ Msg string }

func (e *argError) Error() string { return e.Msg }

var tools = map[string]handler{
	"search_sessions":   searchSessionsTool{},
	"file_attention":    fileAttention{},
	"open":              openTool{},
	"list_sessions":     listSessions{},
	"get_ingest_status": getIngestStatus{},
}

// Call runs one MCP tool over db, as batch answers a line.
func Call(db *DB, tool string, args json.RawMessage) (json.RawMessage, string, error) {
	return encodeAnswer(dispatch(db, tool, args))
}

func encodeAnswer(v any, text string, err error) (json.RawMessage, string, error) {
	if v == nil {
		return nil, text, err
	}
	b, merr := encodeJSON(v)
	if merr != nil {
		return nil, text, merr
	}
	return b, text, err
}

func encodeJSON(v any) (json.RawMessage, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

func dispatch(db *DB, tool string, args json.RawMessage) (data any, text string, err error) {
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
	return t.Call(db, args)
}
