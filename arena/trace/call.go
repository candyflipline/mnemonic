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
	v, text, err := dispatch(db, tool, args)
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
