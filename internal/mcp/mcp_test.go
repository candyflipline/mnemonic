package mcp

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/query"
)

type stubTool struct {
	name string
	data any
	text string
	err  error
}

func (s stubTool) Name() string { return s.name }

func (s stubTool) Call(_ context.Context, _ *sql.DB, _ json.RawMessage) (any, string, error) {
	return s.data, s.text, s.err
}

func mcpServer(t *testing.T, tools ...query.Tool) *httptest.Server {
	t.Helper()
	lookup := func(name string) (query.Tool, bool) {
		for _, tool := range tools {
			if tool.Name() == name {
				return tool, true
			}
		}
		return nil, false
	}
	srv := httptest.NewServer(auth.Middleware(map[string]string{auth.Hash("tok"): "chris"})(Handler(nil, lookup)))
	t.Cleanup(srv.Close)
	return srv
}

func postRPC(t *testing.T, srv *httptest.Server, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/mcp", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decode(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	return payload
}

func TestInitializeNegotiatesProtocolVersion(t *testing.T) {
	srv := mcpServer(t)
	for _, tc := range []struct{ requested, want string }{
		{"2025-06-18", "2025-06-18"},
		{"2024-11-05", "2024-11-05"},
		{"unsupported", "2024-11-05"},
	} {
		resp := postRPC(t, srv, fmt.Sprintf(
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":%q}}`, tc.requested))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", tc.requested, resp.StatusCode)
		}
		payload := decode(t, resp)
		result, _ := payload["result"].(map[string]any)
		if got := result["protocolVersion"]; got != tc.want {
			t.Errorf("%s: protocolVersion = %v, want %v", tc.requested, got, tc.want)
		}
		info, _ := result["serverInfo"].(map[string]any)
		if info["name"] != "mnemonic" {
			t.Errorf("serverInfo.name = %v, want mnemonic", info["name"])
		}
		if _, ok := result["capabilities"].(map[string]any); !ok {
			t.Error("result.capabilities missing")
		}
	}
}

func TestNotificationReturns202Empty(t *testing.T) {
	srv := mcpServer(t)
	resp := postRPC(t, srv, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d, want 202", resp.StatusCode)
	}
	var body bytes.Buffer
	if _, err := body.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	if body.Len() != 0 {
		t.Errorf("notification body = %q, want empty", body.String())
	}
}

func TestToolsListServesToolsJSONVerbatim(t *testing.T) {
	srv := mcpServer(t)
	resp := postRPC(t, srv, `{"jsonrpc":"2.0","id":"tools","method":"tools/list"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	payload := decode(t, resp)
	result, _ := payload["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	var ext []any
	if err := json.Unmarshal(toolsExtJSON, &ext); err != nil {
		t.Fatal(err)
	}
	if len(tools) != 5+len(ext) {
		t.Fatalf("tools/list returned %d tools, want %d", len(tools), 5+len(ext))
	}
	tools = tools[:5]

	var wantTools []any
	if err := json.Unmarshal(toolsJSON, &wantTools); err != nil {
		t.Fatal(err)
	}
	var gotNames []string
	for _, tool := range tools {
		m, _ := tool.(map[string]any)
		gotNames = append(gotNames, m["name"].(string))
	}
	var wantNames []string
	for _, tool := range wantTools {
		m, _ := tool.(map[string]any)
		wantNames = append(wantNames, m["name"].(string))
	}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Errorf("tool names = %v, want %v", gotNames, wantNames)
	}
	if !reflect.DeepEqual(tools, wantTools) {
		t.Error("tools/list output is not the tools.json surface")
	}
}

func TestToolsCallWrapsStubInEnvelope(t *testing.T) {
	srv := mcpServer(t, stubTool{
		name: "open",
		data: map[string]any{"kind": "session", "turns": []any{}},
		text: "Opened session abc.",
	})
	resp := postRPC(t, srv, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"open","arguments":{"id":"session:abc"}}}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	payload := decode(t, resp)
	if payload["id"] != float64(7) {
		t.Errorf("id = %v, want 7", payload["id"])
	}
	result, _ := payload["result"].(map[string]any)
	content, _ := result["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content has %d blocks, want 1", len(content))
	}
	block, _ := content[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "Opened session abc." {
		t.Errorf("content[0] = %v, want text \"Opened session abc.\"", block)
	}
	if result["isError"] != false {
		t.Errorf("isError = %v, want false", result["isError"])
	}
	env, _ := result["structuredContent"].(map[string]any)
	if env["schema_version"] != "mnemonic.mcp.open.v1" {
		t.Errorf("schema_version = %v, want mnemonic.mcp.open.v1", env["schema_version"])
	}
	if env["tool"] != "open" {
		t.Errorf("tool = %v, want open", env["tool"])
	}
	data, _ := env["data"].(map[string]any)
	if data["kind"] != "session" {
		t.Errorf("data = %v, want kind session", data)
	}
	req, _ := env["request"].(map[string]any)
	if req["id"] != "session:abc" {
		t.Errorf("request = %v, want the echoed arguments", env["request"])
	}
	if warnings, ok := env["warnings"].([]any); !ok || len(warnings) != 0 {
		t.Errorf("warnings = %v, want []", env["warnings"])
	}
	perf, _ := env["performance"].(map[string]any)
	if ms, ok := perf["elapsed_ms"].(float64); !ok || ms < 0 {
		t.Errorf("performance = %v, want integer elapsed_ms >= 0", env["performance"])
	}
}

func TestToolsCallArgErrorIsErrorResult(t *testing.T) {
	srv := mcpServer(t, stubTool{
		name: "search_sessions",
		err:  &query.ArgError{Msg: "query must be a non-empty string"},
	})
	resp := postRPC(t, srv, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search_sessions","arguments":{"query":""}}}`)
	payload := decode(t, resp)
	result, _ := payload["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("isError = %v, want true", result["isError"])
	}
	env, _ := result["structuredContent"].(map[string]any)
	if env["schema_version"] != "mnemonic.mcp.error.v1" {
		t.Errorf("schema_version = %v, want mnemonic.mcp.error.v1", env["schema_version"])
	}
	if env["tool"] != "search_sessions" {
		t.Errorf("tool = %v, want search_sessions", env["tool"])
	}
	errObj, _ := env["error"].(map[string]any)
	if errObj["code"] != "invalid_request" || errObj["message"] != "query must be a non-empty string" {
		t.Errorf("error = %v, want invalid_request / \"query must be a non-empty string\"", errObj)
	}
	block, _ := result["content"].([]any)[0].(map[string]any)
	if block["text"] != "search_sessions error (invalid_request): query must be a non-empty string" {
		t.Errorf("content[0].text = %v", block["text"])
	}
}

func TestToolsCallUnknownToolIsErrorResult(t *testing.T) {
	srv := mcpServer(t)
	resp := postRPC(t, srv, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"nope","arguments":{}}}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	payload := decode(t, resp)
	if _, has := payload["error"]; has {
		t.Error("unknown tool must not be a JSON-RPC error")
	}
	result, _ := payload["result"].(map[string]any)
	want := map[string]any{
		"content": []any{map[string]any{"type": "text", "text": "unknown tool: nope"}},
		"isError": true,
	}
	if !reflect.DeepEqual(result, want) {
		t.Errorf("result = %v, want %v", result, want)
	}
}

func TestMissingAuthorizationIs401(t *testing.T) {
	srv := mcpServer(t)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/mcp",
		bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status %d, want 401", resp.StatusCode)
	}
}
