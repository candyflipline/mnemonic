package mcp

import (
	"bytes"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/candyflipline/mnemonic/internal/query"
)

//go:embed tools.json
var toolsJSON []byte

var Version = "dev"

// The protocol versions the upstream MCP 0.7.x speaks; a requested version
// outside the list negotiates the default.
const defaultProtocolVersion = "2024-11-05"

var supportedProtocolVersions = []string{
	"2024-11-05",
	"2025-03-26",
	"2025-06-18",
	"2025-11-25",
	"2026-07-28",
}

const maxRequestBytes = 32 << 20

type performance struct {
	ElapsedMS int64 `json:"elapsed_ms"`
}

type toolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

type successEnvelope struct {
	SchemaVersion string          `json:"schema_version"`
	Tool          string          `json:"tool"`
	Request       json.RawMessage `json:"request"`
	Data          any             `json:"data"`
	Warnings      []string        `json:"warnings"`
	Performance   performance     `json:"performance"`
}

type errorEnvelope struct {
	SchemaVersion string          `json:"schema_version"`
	Tool          string          `json:"tool"`
	Request       json.RawMessage `json:"request"`
	Error         toolError       `json:"error"`
	Warnings      []string        `json:"warnings"`
	Performance   performance     `json:"performance"`
}

func toolSchemaVersion(tool string) string { return "mnemonic.mcp." + tool + ".v1" }

const errorSchemaVersion = "mnemonic.mcp.error.v1"

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolResult struct {
	Content           []contentBlock `json:"content"`
	StructuredContent any            `json:"structuredContent,omitempty"`
	IsError           bool           `json:"isError"`
}

type rpcErrorObj struct {
	Code    int64  `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string       `json:"jsonrpc"`
	ID      any          `json:"id"`
	Result  any          `json:"result,omitempty"`
	Error   *rpcErrorObj `json:"error,omitempty"`
}

func Handler(db *sql.DB, lookup func(name string) (query.Tool, bool)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if !contentTypeIsJSON(r.Header) {
			statusError(w, http.StatusUnsupportedMediaType, "content type must be application/json")
			return
		}
		if !acceptsJSON(r.Header) {
			statusError(w, http.StatusNotAcceptable, "accept must include application/json")
			return
		}
		if !originIsAllowed(r.Header) {
			statusError(w, http.StatusForbidden, "origin must name a loopback IP address")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
		if err != nil {
			statusError(w, http.StatusBadRequest, "invalid JSON-RPC request: read body")
			return
		}
		if len(body) > maxRequestBytes {
			statusError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber() // echo the id's number formatting back verbatim
		var payload any
		if err := dec.Decode(&payload); err != nil {
			statusError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON-RPC request: %s", err))
			return
		}
		obj, _ := payload.(map[string]any)
		if s, _ := obj["jsonrpc"].(string); s != "2.0" {
			statusError(w, http.StatusBadRequest, "jsonrpc must be 2.0")
			return
		}
		if !jsonrpcShapeIsValid(obj) {
			statusError(w, http.StatusBadRequest, "invalid JSON-RPC request shape")
			return
		}
		id, hasID := obj["id"]
		method, _ := obj["method"].(string)
		if method == "" {
			statusError(w, http.StatusBadRequest, "invalid JSON-RPC request: missing method")
			return
		}
		if !protocolVersionIsAllowed(r.Header, method == "initialize") {
			statusError(w, http.StatusBadRequest, "missing or unsupported MCP-Protocol-Version")
			return
		}
		params, _ := obj["params"].(map[string]any)
		paramsJSON, _ := json.Marshal(obj["params"])

		switch method {
		case "notifications/initialized", "initialized":
			w.WriteHeader(http.StatusAccepted)
		case "initialize":
			if !hasID {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			writeJSON(w, rpcResponse{JSONRPC: "2.0", ID: id, Result: initializeResult(params)})
		case "ping":
			if !hasID {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			writeJSON(w, rpcResponse{JSONRPC: "2.0", ID: id, Result: struct{}{}})
		case "tools/list":
			if !hasID {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			writeJSON(w, rpcResponse{JSONRPC: "2.0", ID: id, Result: toolsListResult()})
		case "tools/call":
			if !hasID {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			h := handler{db: db, lookup: lookup}
			result := h.toolsCall(r, paramsJSON)
			switch result := result.(type) {
			case *rpcErrorObj:
				writeJSON(w, rpcResponse{JSONRPC: "2.0", ID: id, Error: result})
			default:
				writeJSON(w, rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
			}
		default:
			if !hasID {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			writeJSON(w, rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcErrorObj{
				Code:    -32601,
				Message: fmt.Sprintf("method not found: %s", method),
			}})
		}
	})
}

type handler struct {
	db     *sql.DB
	lookup func(name string) (query.Tool, bool)
}

// toolsCall runs one tool and wraps it in the tool envelope. Only a params
// payload that cannot decode is a JSON-RPC error; like upstream, every
// tool-level failure, such as an unknown name, is an isError result
// so the client sees the message as tool output, not a protocol failure.
func (h handler) toolsCall(r *http.Request, paramsJSON json.RawMessage) any {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(paramsJSON, &p); err != nil {
		return &rpcErrorObj{Code: -32602, Message: fmt.Sprintf("invalid params: %s", err)}
	}
	args := p.Arguments
	if len(args) == 0 {
		args = json.RawMessage("null")
	}
	tool, ok := h.lookup(p.Name)
	if !ok {
		return unstructuredToolError(fmt.Sprintf("unknown tool: %s", p.Name))
	}
	start := time.Now()
	data, text, err := tool.Call(r.Context(), h.db, args)
	perf := performance{ElapsedMS: time.Since(start).Milliseconds()}
	if err != nil {
		env := errorEnvelope{
			SchemaVersion: errorSchemaVersion,
			Tool:          p.Name,
			Request:       args,
			Warnings:      []string{},
			Performance:   perf,
		}
		var argErr *query.ArgError
		if errors.As(err, &argErr) {
			env.Error = toolError{Code: "invalid_request", Message: argErr.Msg}
		} else {
			env.Error = toolError{Code: "internal_error", Message: err.Error()}
		}
		return toolResult{
			Content:           []contentBlock{{Type: "text", Text: fmt.Sprintf("%s error (%s): %s", p.Name, env.Error.Code, env.Error.Message)}},
			StructuredContent: env,
			IsError:           true,
		}
	}
	env := successEnvelope{
		SchemaVersion: toolSchemaVersion(p.Name),
		Tool:          p.Name,
		Request:       args,
		Data:          data,
		Warnings:      []string{},
		Performance:   perf,
	}
	return toolResult{
		Content:           []contentBlock{{Type: "text", Text: text}},
		StructuredContent: env,
	}
}

func unstructuredToolError(text string) toolResult {
	return toolResult{
		Content: []contentBlock{{Type: "text", Text: text}},
		IsError: true,
	}
}

func toolsListResult() map[string]json.RawMessage {
	return map[string]json.RawMessage{"tools": toolsJSON}
}

func initializeResult(params map[string]any) map[string]any {
	return map[string]any{
		"protocolVersion": negotiatedProtocolVersion(params),
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":      map[string]any{"name": "mnemonic", "version": Version},
	}
}

func negotiatedProtocolVersion(params map[string]any) string {
	requested, _ := params["protocolVersion"].(string)
	for _, v := range supportedProtocolVersions {
		if requested == v {
			return v
		}
	}
	return defaultProtocolVersion
}

func jsonrpcShapeIsValid(obj map[string]any) bool {
	switch obj["id"].(type) {
	case nil, string, json.Number:
	default:
		return false
	}
	switch obj["params"].(type) {
	case nil, map[string]any, []any:
	default:
		return false
	}
	return true
}

func contentTypeIsJSON(h http.Header) bool {
	ct := h.Get("Content-Type")
	if ct == "" {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(strings.Split(ct, ";")[0]), "application/json")
}

func acceptsJSON(h http.Header) bool {
	for _, header := range h.Values("Accept") {
		for _, entry := range strings.Split(header, ",") {
			parts := strings.Split(entry, ";")
			disabled := false
			for _, param := range parts[1:] {
				name, value, _ := strings.Cut(param, "=")
				if q, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil && q == 0 &&
					strings.EqualFold(strings.TrimSpace(name), "q") {
					disabled = true
				}
			}
			if !disabled && strings.EqualFold(strings.TrimSpace(parts[0]), "application/json") {
				return true
			}
		}
	}
	return false
}

// originIsAllowed ports upstream origin_is_allowed: an Origin naming a
// loopback IP (DNS names are rejected, so a web page cannot hit the server);
// no Origin header is fine.
func originIsAllowed(h http.Header) bool {
	origin := h.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	return ip != nil && ip.IsLoopback()
}

func protocolVersionIsAllowed(h http.Header, initialize bool) bool {
	v := h.Get("Mcp-Protocol-Version")
	if v == "" {
		return initialize
	}
	for _, supported := range supportedProtocolVersions {
		if v == supported {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func statusError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
