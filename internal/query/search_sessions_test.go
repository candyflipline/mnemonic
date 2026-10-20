package query

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/candyflipline/mnemonic/internal/cache"
	"github.com/candyflipline/mnemonic/internal/cache/cachetest"
)

// TestParseSearchSessionsArgsCanonicalizes ports search_sessions_v1.rs
// parses_and_canonicalizes_search_sessions_args.
func TestParseSearchSessionsArgsCanonicalizes(t *testing.T) {
	args, err := parseSearchSessionsArgs([]byte(
		`{"query":"  cargo test failure  ",
		  "event_types":["tool_response","user_input","tool_response"],
		  "harness":" claude-code ","source":" claude ","n_hits":3}`))
	if err != nil {
		t.Fatal(err)
	}
	if args.query != "cargo test failure" {
		t.Fatalf("query: %q", args.query)
	}
	if want := []string{"user_input", "tool_response"}; !reflect.DeepEqual(args.eventTypes, want) {
		t.Fatalf("event types: %v", args.eventTypes)
	}
	if args.harness == nil || *args.harness != "claude-code" {
		t.Fatalf("harness: %v", args.harness)
	}
	if args.source == nil || *args.source != "claude" {
		t.Fatalf("source: %v", args.source)
	}
	if args.nHits != 3 {
		t.Fatalf("n_hits: %d", args.nHits)
	}
}

// The v1 layer defaults absent event_types to the search defaults and
// reports the effective list in the canonical request (ported from
// forwards_effective_event_types_and_reports_them_in_canonical_request).
func TestSearchSessionsDefaultEventTypes(t *testing.T) {
	for _, raw := range []string{
		`{"query":"migration"}`,
		`{"query":"migration","event_types":null}`,
	} {
		args, err := parseSearchSessionsArgs([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"user_input", "assistant_response"}
		if !reflect.DeepEqual(args.eventTypes, want) {
			t.Fatalf("%s: event types %v", raw, args.eventTypes)
		}
		request := args.requestShape()
		got, err := json.Marshal(request["event_types"])
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != `["user_input","assistant_response"]` {
			t.Fatalf("%s: request event_types %s", raw, got)
		}
	}

	args, err := parseSearchSessionsArgs([]byte(
		`{"query":"migration","event_types":["tool_response","user_input","tool_call","tool_response"]}`))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"user_input", "tool_call", "tool_response"}
	if !reflect.DeepEqual(args.eventTypes, want) {
		t.Fatalf("canonical order: %v", args.eventTypes)
	}
}

// TestSearchSessionsArgValidation ports the contract validation messages.
func TestSearchSessionsArgValidation(t *testing.T) {
	cases := []struct {
		raw     string
		wantErr string
	}{
		{`{"n_hits":1}`, "search_sessions expects a JSON object with a string query"},
		{`{}`, "search_sessions expects a JSON object with a string query"},
		{`{"query":"   "}`, "query must be a non-empty string"},
		{`{"query":"` + strings.Repeat("x", 5000) + `"}`, "query must be at most 4096 characters"},
		{`{"query":"migration","n_hits":-1}`, "n_hits must be between 1 and 50"},
		{`{"query":"migration","n_hits":51}`, "n_hits must be between 1 and 50"},
		{`{"query":"migration","event_types":[]}`, "event_types must contain at least one event type"},
		{`{"query":"migration","event_types":["nope"]}`, "unsupported event type: nope"},
		{`{"query":"migration","event_types":["unknown"]}`, "unsupported event type: unknown"},
		{`{"query":"migration","harness":" "}`, "harness must be a non-empty string when provided"},
		{`{"query":"migration","source":""}`, "source must be a non-empty string when provided"},
		{`{"query":"migration","within_id":"bogus"}`, "ID must begin with session:, turn:, or event:"},
	}
	for _, tc := range cases {
		_, err := parseSearchSessionsArgs([]byte(tc.raw))
		if err == nil {
			t.Fatalf("%s: expected error", tc.raw)
		}
		if err.Error() != tc.wantErr {
			t.Fatalf("%s: got %q, want %q", tc.raw, err.Error(), tc.wantErr)
		}
	}

	// rejects_event_scope_before_repository_search.
	eventID := formatEventID("evt-1")
	_, err := parseSearchSessionsArgs([]byte(`{"query":"migration","within_id":"` + eventID + `"}`))
	if err == nil || err.Error() != "within_id accepts session and turn IDs, not event IDs" {
		t.Fatalf("event scope: %v", err)
	}

	// Engine-level: a query whose tokens are all excluded (search.rs
	// search_mcp_events fails before touching the repository).
	_, _, err = (searchSessionsTool{}).Call(context.Background(), nil, []byte(`{"query":"x"}`))
	if err == nil || err.Error() != "query has no searchable terms (tokens shorter than 2 characters are excluded)" {
		t.Fatalf("short tokens: %v", err)
	}
}

// TestSearchHitShape ports shapes_search_hit_with_mcp_ids_and_no_full_payloads.
func TestSearchHitShape(t *testing.T) {
	hit := searchHit{
		rank:                   1,
		eventUID:               "evt-1",
		sessionID:              "sess-1",
		eventType:              "tool_response",
		eventUnixMs:            1777464001123,
		turnSeq:                2,
		turnOrdinal:            2,
		eventOrder:             5,
		eventOrdinal:           3,
		turnEventCount:         4,
		turnCompleted:          true,
		turnTerminalEventUID:   "evt-1",
		sessionStartedAtUnixMs: 1777554000000,
		sessionUpdatedAtUnixMs: 1777554600000,
		sessionSummary:         "Fix tests",
		sessionCompleted:       true,
		sourceName:             "codex",
		harness:                "codex",
		snippet:                "cargo test passed",
		score:                  0.7,
		rawScore:               12.0,
	}
	shaped := searchHitJSON(hit)

	if shaped["id"] != "event:ZXZ0LTE" {
		t.Fatalf("id: %v", shaped["id"])
	}
	event := shaped["event"].(map[string]any)
	turn := shaped["turn"].(map[string]any)
	session := shaped["session"].(map[string]any)
	open := shaped["open"].(map[string]any)
	if open["turn_id"] != "turn:c2Vzcy0x:2" {
		t.Fatalf("open turn id: %v", open["turn_id"])
	}
	if event["type"] != "tool_response" || event["terminal"] != true {
		t.Fatalf("event: %v", event)
	}
	if event["timestamp"] != "2026-04-29T12:00:01.123Z" {
		t.Fatalf("timestamp: %v", event["timestamp"])
	}
	if session["started_at"] != "2026-04-30T13:00:00.000Z" ||
		session["updated_at"] != "2026-04-30T13:10:00.000Z" {
		t.Fatalf("session times: %v", session)
	}
	if session["harness"] != "codex" || session["title"] != "Fix tests" {
		t.Fatalf("session labels: %v", session)
	}
	if turn["completed"] != true || session["completed"] != true {
		t.Fatalf("completed flags: %v %v", turn["completed"], session["completed"])
	}
	blob, err := json.Marshal(shaped)
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"text_content", "payload_json", "full content should not appear"} {
		if strings.Contains(string(blob), banned) {
			t.Fatalf("shaped hit leaks %s: %s", banned, blob)
		}
	}
}

// TestShapedSearchOpenIdsAreValidOpenArgs ports
// shaped_search_open_ids_are_valid_open_args.
func TestShapedSearchOpenIdsAreValidOpenArgs(t *testing.T) {
	hit := searchHit{
		eventUID:  "evt-open",
		sessionID: "sess-open",
		turnSeq:   3,
	}
	shaped := searchHitJSON(hit)
	for _, field := range []string{"event_id", "turn_id", "session_id"} {
		id := shaped["open"].(map[string]any)[field].(string)
		args, err := parseOpenArgs([]byte(`{"id":` + mustJSON(id) + `}`))
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if args.hasCursor {
			t.Fatalf("%s: parsed as continuation", id)
		}
		if got := args.target.String(); got != id {
			t.Fatalf("round trip: %s -> %s", id, got)
		}
	}
}

func mustJSON(v string) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// TestFormatSearchSessionsText ports format_search_sessions_text.
func TestFormatSearchSessionsText(t *testing.T) {
	request := map[string]any{"query": "workspace"}
	first := searchHitJSON(searchHit{
		rank: 1, eventUID: "evt-1", sessionID: "sess-1", eventType: "tool_response",
		eventUnixMs: 1777464001123, snippet: "workspace = true", score: 1,
	})
	second := searchHitJSON(searchHit{
		rank: 2, eventUID: "evt-2", sessionID: "sess-2", eventType: "user_input",
		eventUnixMs: 1777464000123, snippet: "please check workspace", score: 0.5,
	})
	data := map[string]any{
		"result_count": 2,
		"results":      []map[string]any{first, second},
	}
	want := strings.Join([]string{
		`Found 2 search hit(s) for "workspace".`,
		`1. tool_response score=1.00 untitled session: workspace = true (event:ZXZ0LTE)`,
		`2. user_input score=0.50 untitled session: please check workspace (event:ZXZ0LTI)`,
	}, "\n")
	if got := formatSearchSessionsText(request, data); got != want {
		t.Fatalf("prose:\n%s\nwant:\n%s", got, want)
	}
}

// TestSearchSessionsFixture locks the ranked results over the golden lake.
func TestSearchSessionsFixture(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	if err := cachetest.Lake(dataDir, "codex", "claude_code"); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Build(dataDir); err != nil {
		t.Fatal(err)
	}
	db, _, err := cache.OpenNewest(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	tool := searchSessionsTool{}

	// Defaults filter to user_input/assistant_response; both workspace
	// documents are tool_response rows, so nothing matches.
	data, _, err := tool.Call(ctx, db, []byte(`{"query":"workspace","n_hits":5}`))
	if err != nil {
		t.Fatal(err)
	}
	got := data.(map[string]any)
	if got["result_count"] != 0 || got["truncated"] != false || got["limit"] != 5 {
		t.Fatalf("default-filtered search: %v", got)
	}

	// Explicit tool_response scope: one "workspace = true" per session, both
	// scored 1, newest session first.
	data, text, err := tool.Call(ctx, db, []byte(
		`{"query":"workspace","n_hits":5,"event_types":["tool_response"]}`))
	if err != nil {
		t.Fatal(err)
	}
	got = data.(map[string]any)
	if got["result_count"] != 2 {
		t.Fatalf("result_count: %v", got["result_count"])
	}
	results := got["results"].([]map[string]any)
	wantIDs := []string{
		"event:OTZjNmQ4ZTlkMWY0Mjg4NWZjZjQ5M2JjY2MwZDBlMTRjZDgxN2E3NGE1NzIzMmM0OWE3MTA2ODE2YWU0MmUwOQ",
		"event:OGU2NDBlZDc2NTIxMDcyM2YyZmQwZjIyOTY1NWFkNjA1NDgzMGU4M2YyMGZkODkxZWE0ODEwMTRlNDk4ZjNkMg",
	}
	for i, want := range wantIDs {
		if results[i]["id"] != want {
			t.Fatalf("hit %d: got %v, want %s", i, results[i]["id"], want)
		}
		if score := results[i]["score"].(float64); score != 1 {
			t.Fatalf("hit %d score: %v", i, results[i]["score"])
		}
		snippet := results[i]["snippet"].(map[string]any)
		if snippet["text"] != "workspace = true" || snippet["truncated"] != false {
			t.Fatalf("hit %d snippet: %v", i, snippet)
		}
	}
	if got := results[0]["turn"].(map[string]any)["event_count"]; got != int64(15) {
		t.Fatalf("codex turn event_count: %v", got)
	}
	wantText := strings.Join([]string{
		`Found 2 search hit(s) for "workspace".`,
		`1. tool_response score=1.00 untitled session: workspace = true (event:OTZjNmQ4ZTlkMWY0Mjg4NWZjZjQ5M2JjY2MwZDBlMTRjZDgxN2E3NGE1NzIzMmM0OWE3MTA2ODE2YWU0MmUwOQ)`,
		`2. tool_response score=1.00 untitled session: workspace = true (event:OGU2NDBlZDc2NTIxMDcyM2YyZmQwZjIyOTY1NWFkNjA1NDgzMGU4M2YyMGZkODkxZWE0ODEwMTRlNDk4ZjNkMg)`,
	}, "\n")
	if text != wantText {
		t.Fatalf("prose:\n%s\nwant:\n%s", text, wantText)
	}

	// Message defaults: codex assistant line first, claude user prompt second.
	data, _, err = tool.Call(ctx, db, []byte(`{"query":"inspect project","n_hits":10}`))
	if err != nil {
		t.Fatal(err)
	}
	got = data.(map[string]any)
	if got["result_count"] != 2 {
		t.Fatalf("result_count: %v", got["result_count"])
	}
	results = got["results"].([]map[string]any)
	wantSecond := []string{
		"event:OWM1MTVmNGMyYmM1OTgxYWMyNTIyOTcyZjk1MzBjNDAzNjlmYWI5MWMzM2Q1OWUyNWI0YmE0ZjdlMDViZDhhOA",
		"event:M2UyZjMxZjNhYmViZDZmYWRjNTcyMzExOWI2Y2ZiZDYxYzI3NGExYjdlMzM2ZGNmMzBmODNmZTlhNzEwMDIxZg",
	}
	if results[0]["id"] != wantSecond[0] || results[1]["id"] != wantSecond[1] {
		t.Fatalf("ids: %v %v", results[0]["id"], results[1]["id"])
	}
	if score := results[1]["score"].(float64); score != 0.9005524861878453 {
		t.Fatalf("second score: %v", score)
	}
}
