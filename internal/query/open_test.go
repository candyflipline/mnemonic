package query

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/candyflipline/mnemonic/internal/cache"
	"github.com/candyflipline/mnemonic/internal/cache/cachetest"
)

func TestOpenArgValidation(t *testing.T) {
	sessionID := formatSessionID("sess-1")
	validCursor, err := encodeOpenCursor(openCursor{
		Version:  2,
		TargetID: sessionID,
		Limit:    3,
		After:    openCursorAfter{Kind: "event", EventOrdinal: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		raw     string
		wantErr string
	}{
		{`{}`, "id is required"},
		{`{"id":""}`, "id must be a non-empty string"},
		{`{"id":"bogus"}`, "ID must begin with session:, turn:, or event:"},
		{`{"id":"` + formatEventID("evt-1") + `","limit":2}`, "limit is only supported for session and turn IDs"},
		{`{"id":"` + sessionID + `","limit":0}`, "limit must be between 1 and 25"},
		{`{"id":"` + sessionID + `","limit":26}`, "limit must be between 1 and 25"},
		{`{"cursor":"###"}`, "cursor is malformed; reopen the target"},
		{`{"cursor":"eyJ2ZXJzaW9uIjoxfQ"}`, "cursor snapshot is stale; reopen the target to restart expansion"},
		{`{"cursor":"` + validCursor + `","id":"` + sessionID + `"}`, "cursor must be provided without id or limit"},
	}
	for _, tc := range cases {
		_, err := parseOpenArgs([]byte(tc.raw))
		if err == nil {
			t.Fatalf("%s: expected error", tc.raw)
		}
		if err.Error() != tc.wantErr {
			t.Fatalf("%s: got %q, want %q", tc.raw, err.Error(), tc.wantErr)
		}
	}

	args, err := parseOpenArgs([]byte(`{"cursor":"` + validCursor + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	if args.target.kind != kindSession || args.target.session != "sess-1" {
		t.Fatalf("cursor target: %v", args.target)
	}
	if args.page.limit != 3 || args.page.after.EventOrdinal != 1 {
		t.Fatalf("cursor decode: %+v", args.page)
	}
}

func TestCompactTextKeepsKnownEventHandle(t *testing.T) {
	eventID := formatEventID("event-empty")
	compact := compactTextContent(eventID, nil)
	if compact == nil {
		t.Fatal("compact is nil")
	}
	m := compact.(map[string]any)
	if m["event_id"] != eventID || m["event_id"] != "event:ZXZlbnQtZW1wdHk" {
		t.Fatalf("event_id: %v", m["event_id"])
	}
	if m["text"] != nil || m["truncated"] != false {
		t.Fatalf("compact: %v", m)
	}
	if compactTextContent(nil, nil) != nil {
		t.Fatal("unknown handle must collapse to null")
	}
}

func TestSessionOpenUsesTypedIDsAndCompactTurns(t *testing.T) {
	turn := &turnAccum{
		metadata: &turnSummary{
			sessionID: "session-a", turnSeq: 1, turnID: "raw-turn-1",
			startedAt: "2026-04-29T12:00:00.000Z", startedAtUnixMs: 1777463200000,
			endedAt: "2026-04-29T12:05:00.000Z", endedAtUnixMs: 1777463500000,
			totalEvents: 3, userMessages: 1, assistantMessages: 1, toolCalls: 1,
		},
		userInput: &navRow{
			sessionID: "session-a", eventUID: "event-user",
			eventClass: "message", payloadType: "message", actorRole: "user",
			textContent: "Please check the failing monitor startup.",
			eventUnixMs: 1777463200000, eventTime: "2026-04-29T12:00:00.000Z",
			turnSeq: 1, eventOrder: 1,
		},
		finalResponse: &navRow{
			sessionID: "session-a", eventUID: "event-final",
			eventClass: "message", payloadType: "message", actorRole: "assistant",
			textContent: "Fixed the startup guard.",
			eventUnixMs: 1777463500000, eventTime: "2026-04-29T12:05:00.000Z",
			turnSeq: 1, eventOrder: 3,
		},
		toolsCalled:          []string{"exec_command"},
		normalizedEventTypes: []string{"user_input", "tool_call", "assistant_response"},
		completed:            true,
		terminalEventUID:     "event-final",
		firstEvent:           navEventRef(mustRow(rowInput{uid: "event-user", order: 1})),
		lastEvent:            navEventRef(mustRow(rowInput{uid: "event-final", order: 3})),
	}
	scan := &sessionScan{
		metadata: sessionMetadata{
			sessionID: "session-a", firstEventUnixMs: 1777463200000,
			lastEventUnixMs: 1777463500000, totalTurns: 1, totalEvents: 3,
			mode: "tool_calling", firstEventUID: "event-user", lastEventUID: "event-final",
		},
		source: "codex", harness: "cli", inferenceProvider: "openai",
		turns: map[uint32]*turnAccum{1: turn},
		metadataRows: []*navRow{{
			sessionID:   "session-a",
			eventClass:  "session_meta",
			payloadJSON: `{"title":"Investigate startup","summary":"Session summary","slug":"startup"}`,
		}},
		completed:        true,
		terminalEventUID: "event-final",
	}
	data := openSessionData(scan, []any{openSessionTurnSummary(compactTurn(turn, summaryPreviewChars))}, nil)

	if data["kind"] != "session" {
		t.Fatalf("kind: %v", data["kind"])
	}
	session := data["session"].(map[string]any)
	if session["id"] != formatSessionID("session-a") {
		t.Fatalf("session id: %v", session["id"])
	}
	for key, want := range map[string]any{
		"title": "Investigate startup", "source": "codex", "harness": "cli",
		"inference_provider": "openai", "session_slug": "startup",
		"session_summary": "Session summary", "completed": true,
		"terminal_event_id": formatEventID("event-final"),
	} {
		if session[key] != want {
			t.Fatalf("session[%s] = %v, want %v", key, session[key], want)
		}
	}
	turns := data["turns"].([]any)
	first := turns[0].(map[string]any)
	if first["id"] != formatTurnID("session-a", 1) {
		t.Fatalf("turn id: %v", first["id"])
	}
	if first["terminal_event_id"] != formatEventID("event-final") {
		t.Fatalf("terminal event: %v", first["terminal_event_id"])
	}
	if got := first["user_input"].(map[string]any)["event_id"]; got != formatEventID("event-user") {
		t.Fatalf("user_input event: %v", got)
	}
	if got := first["final_response"].(map[string]any)["event_id"]; got != formatEventID("event-final") {
		t.Fatalf("final_response event: %v", got)
	}
	blob, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "payload_json") {
		t.Fatalf("compact turns leak payload_json: %s", blob)
	}
}

type rowInput struct {
	uid     string
	class   string
	actor   string
	order   uint64
	text    string
	content string
}

func mustRow(in rowInput) *navRow {
	class := in.class
	if class == "" {
		class = "message"
	}
	actor := in.actor
	if actor == "" {
		actor = "user"
	}
	return &navRow{
		sessionID:   "session-a",
		eventUID:    in.uid,
		eventClass:  class,
		payloadType: class,
		actorRole:   actor,
		textContent: in.text,
		eventUnixMs: 1777463200000,
		eventTime:   "2026-04-29T12:00:00.000Z",
		turnSeq:     1,
		eventOrder:  in.order,
	}
}

func TestTurnOpenReturnsCompactEventHandlesWithoutPayloads(t *testing.T) {
	toolRow := &navRow{
		sessionID: "session-a", eventUID: "event-tool",
		eventClass: "tool_call", payloadType: "function_call", actorRole: "tool",
		callID: "call-1", name: "exec_command",
		textContent: "tool_call preview",
		eventUnixMs: 1777464000000, eventTime: "2026-04-29T12:00:00.000Z",
		turnSeq: 1, eventOrder: 2,
	}
	userRow := mustRow(rowInput{uid: "event-user", order: 1, text: "user_input preview"})
	finalRow := mustRow(rowInput{uid: "event-final", order: 3, actor: "assistant", text: "assistant_response preview"})
	turn := &turnAccum{
		metadata: &turnSummary{
			sessionID: "session-a", turnSeq: 1, turnID: "raw-turn-1",
			startedAtUnixMs: 1777463200000, endedAtUnixMs: 1777463500000, totalEvents: 3,
		},
		userInput:            userRow,
		finalResponse:        finalRow,
		toolsCalled:          []string{"exec_command"},
		normalizedEventTypes: []string{"user_input", "tool_call", "assistant_response"},
		completed:            true,
		terminalEventUID:     "event-final",
		firstEvent:           navEventRef(userRow),
		lastEvent:            navEventRef(finalRow),
		selected:             []*navRow{userRow, toolRow, finalRow},
	}
	nextTurn := &turnAccum{metadata: &turnSummary{
		sessionID: "session-a", turnSeq: 2, turnID: "raw-turn-2",
		startedAtUnixMs: 1777463520000, endedAtUnixMs: 1777463580000, totalEvents: 1,
	}}
	scan := &sessionScan{
		metadata: sessionMetadata{sessionID: "session-a", totalTurns: 2, totalEvents: 4, mode: "tool_calling"},
		source:   "codex",
		turns:    map[uint32]*turnAccum{1: turn, 2: nextTurn},
	}
	events := []any{}
	for i, row := range turn.selected {
		events = append(events, openTurnEventSummary(row, i+1, "event-final"))
	}
	data := openTurnData(scan, turn, compactTurn(turn, summaryPreviewChars), events, nil)

	if data["kind"] != "turn" {
		t.Fatalf("kind: %v", data["kind"])
	}
	if got := data["session"].(map[string]any)["source"]; got != "codex" {
		t.Fatalf("source: %v", got)
	}
	page := data["events"].([]any)
	if got := page[1].(map[string]any)["tool_name"]; got != "exec_command" {
		t.Fatalf("tool_name: %v", got)
	}
	if got := page[2].(map[string]any)["terminal"]; got != true {
		t.Fatalf("terminal: %v", got)
	}
	first, err := json.Marshal(page[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(first), "payload") {
		t.Fatalf("event summary leaks payload: %s", first)
	}
	traversal := data["traversal"].(map[string]any)
	if got := traversal["next_turn_id"]; got != formatTurnID("session-a", 2) {
		t.Fatalf("next_turn_id: %v", got)
	}
	if got := traversal["first_event_id"]; got != formatEventID("event-user") {
		t.Fatalf("first_event_id: %v", got)
	}
	if got := traversal["last_event_id"]; got != formatEventID("event-final") {
		t.Fatalf("last_event_id: %v", got)
	}
}

func TestEncryptedReasoningSummary(t *testing.T) {
	row := &navRow{
		sessionID: "session-a", eventUID: "event-reasoning",
		eventClass: "reasoning", payloadType: "reasoning", actorRole: "assistant",
		textContent: `{"type":"reasoning","summary":[],"encrypted_content":"` +
			strings.Repeat("x", 1000) + `"}`,
		eventUnixMs: 1777464000000, eventTime: "2026-04-29T12:00:00.000Z",
		turnSeq: 1, eventOrder: 2, eventOrdinal: 2,
	}
	summary, truncated := compactEventSummary(row)
	if summary != "[encrypted reasoning omitted]" || truncated {
		t.Fatalf("summary %q truncated %v", summary, truncated)
	}
	m := openTurnEventSummary(row, 2, "")
	if m["summary"] != "[encrypted reasoning omitted]" || m["truncated"] != false {
		t.Fatalf("event summary: %v", m)
	}
	blob, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "encrypted_content") {
		t.Fatalf("leaks encrypted content: %s", blob)
	}
}

func TestEventOpenIncludesFullToolCallContent(t *testing.T) {
	target := &navRow{
		sessionID: "session-a", eventUID: "event-tool",
		eventClass: "tool_call", payloadType: "tool_call", actorRole: "assistant",
		callID: "call-1", name: "exec_command",
		payloadJSON: `{"arguments":{"cmd":"cargo test","workdir":"/repo"},"model":"gpt-5"}`,
		eventUnixMs: 1777464001123, eventTime: "2026-04-29T12:00:01.123Z",
		turnSeq: 1, eventOrder: 2, eventOrdinal: 2,
	}
	parentTurn := &turnAccum{
		metadata: &turnSummary{
			sessionID: "session-a", turnSeq: 1, turnID: "raw-turn-1",
			startedAtUnixMs: 1777463200000, endedAtUnixMs: 1777463500000, totalEvents: 3,
		},
		completed:        true,
		terminalEventUID: "event-final",
	}
	scan := &sessionScan{
		metadata: sessionMetadata{
			sessionID: "session-a", firstEventUnixMs: 1777463200000,
			lastEventUnixMs: 1777463500000, totalTurns: 1, totalEvents: 3,
			mode: "tool_calling",
		},
		source:           "claude",
		turns:            map[uint32]*turnAccum{1: parentTurn},
		completed:        true,
		terminalEventUID: "event-final",
		previousEvent:    navEventRef(mustRow(rowInput{uid: "event-user", order: 1})),
		nextEvent:        navEventRef(mustRow(rowInput{uid: "event-final", order: 3, actor: "assistant"})),
	}
	data := openEventData(scan, target, parentTurn)

	if data["kind"] != "event" {
		t.Fatalf("kind: %v", data["kind"])
	}
	event := data["event"].(map[string]any)
	if event["tool_name"] != "exec_command" {
		t.Fatalf("tool_name: %v", event["tool_name"])
	}
	if event["ordinal"] != uint32(2) {
		t.Fatalf("ordinal: %v", event["ordinal"])
	}
	if event["timestamp"] != "2026-04-29T12:00:01.123Z" {
		t.Fatalf("timestamp: %v", event["timestamp"])
	}
	if event["model"] != "gpt-5" {
		t.Fatalf("model: %v", event["model"])
	}
	if got := data["session"].(map[string]any)["source"]; got != "claude" {
		t.Fatalf("source: %v", got)
	}
	content := data["content"].(map[string]any)
	if content["format"] != "tool_call" || content["truncated"] != false {
		t.Fatalf("content: %v", content)
	}
	if got := content["arguments"].(map[string]any)["cmd"]; got != "cargo test" {
		t.Fatalf("arguments: %v", content["arguments"])
	}
	if got := data["traversal"].(map[string]any)["previous_event_id"]; got != formatEventID("event-user") {
		t.Fatalf("previous_event_id: %v", got)
	}
}

func TestOpenResultTextProse(t *testing.T) {
	t.Run("event body", func(t *testing.T) {
		body := "search label near the start. " + strings.Repeat("x", 4000) + " synthetic-fact-42"
		data := map[string]any{
			"kind":  "event",
			"event": map[string]any{"type": "user_input", "timestamp": "2026-04-29T12:00:01.123Z"},
			"turn":  map[string]any{"id": "turn:t1"},
			"content": map[string]any{
				"format": "text", "text": body, "truncated": false,
			},
			"traversal": map[string]any{"previous_event_id": "event:prev", "next_event_id": nil},
		}
		text := openResultText(map[string]any{"id": "event:abc"}, data)
		if !strings.HasPrefix(text, "Opened event event:abc.") {
			t.Fatalf("header: %q", text)
		}
		if !strings.Contains(text, "synthetic-fact-42") {
			t.Fatal("body beyond any preview must be visible")
		}
		if !strings.Contains(text, "type=user_input turn=turn:t1") {
			t.Fatalf("meta line missing: %q", text)
		}
		if !strings.Contains(text, "previous_event=event:prev") {
			t.Fatalf("neighbors missing: %q", text)
		}
		if strings.Contains(text, "next_event=") {
			t.Fatalf("null neighbor rendered: %q", text)
		}
	})

	t.Run("tool call", func(t *testing.T) {
		data := map[string]any{
			"kind":  "event",
			"event": map[string]any{"type": "tool_response", "tool_name": "exec_command"},
			"content": map[string]any{
				"format": "tool_response", "text": "ok", "exit_code": int64(2),
				"arguments": map[string]any{"cmd": "cargo test"},
			},
		}
		text := openResultText(map[string]any{"id": "event:tool"}, data)
		for _, want := range []string{
			"tool=exec_command",
			`arguments: {"cmd":"cargo test"}`,
			"exit_code: 2",
		} {
			if !strings.Contains(text, want) {
				t.Fatalf("missing %q in:\n%s", want, text)
			}
		}
	})

	t.Run("turn and session", func(t *testing.T) {
		turn := map[string]any{
			"kind": "turn",
			"turn": map[string]any{"event_count": 2},
			"summary": map[string]any{
				"user_input":     map[string]any{"text": "Please check startup."},
				"final_response": map[string]any{"text": "Fixed the guard."},
			},
			"events": []any{
				map[string]any{"ordinal": 1, "type": "user_input", "summary": "Please check startup.", "id": "event:1"},
				map[string]any{"ordinal": 2, "type": "tool_call", "tool_name": "exec_command", "summary": "cargo test", "id": "event:2"},
			},
			"next_cursor": nil,
		}
		text := openResultText(map[string]any{"id": "turn:t1", "limit": 2}, turn)
		if !strings.Contains(text, "user: Please check startup.") {
			t.Fatalf("user line missing:\n%s", text)
		}
		if !strings.Contains(text, "2. tool_call [exec_command]: cargo test (event:2)") {
			t.Fatalf("child line missing:\n%s", text)
		}
		if !strings.Contains(text, "Opened turn turn:t1 with the final 2 events.") {
			t.Fatalf("final header missing:\n%s", text)
		}

		session := map[string]any{
			"kind":    "session",
			"session": map[string]any{"title": "Investigate startup", "turn_count": 1},
			"turns": []any{map[string]any{
				"ordinal": 1, "id": "turn:t1",
				"user_input":     map[string]any{"text": "Please check startup."},
				"final_response": map[string]any{"text": "Fixed the guard."},
			}},
		}
		text = openResultText(map[string]any{"id": "session:s1"}, session)
		if !strings.Contains(text, "title: Investigate startup") {
			t.Fatalf("title line missing:\n%s", text)
		}
		if !strings.Contains(text, "1. user: Please check startup. | assistant: Fixed the guard. (turn:t1)") {
			t.Fatalf("turn line missing:\n%s", text)
		}
	})

	t.Run("error text", func(t *testing.T) {
		if got := OpenErrorText("not_found", "no such id"); got != "open failed (not_found): no such id" {
			t.Fatalf("error text: %q", got)
		}
	})
}

const (
	fixtureCodexSession  = "019c5f6a-49bd-7920-ac67-1dd8e33b0e95"
	fixtureClaudeSession = "7c666c01-d38e-4658-8650-854ffb5b626e"
	fixtureClaudeToolUID = "b7a60c19257e13643a612279032f92153761008a208238f7ed2948420698cc0d"
)

func TestOpenFixture(t *testing.T) {
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
	tool := openTool{}
	sessionID := formatSessionID(fixtureCodexSession)
	turnID := formatTurnID(fixtureCodexSession, 1)

	data, text, err := tool.Call(ctx, db, []byte(`{"id":"`+sessionID+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	session := data.(map[string]any)["session"].(map[string]any)
	if got := session["mode"]; got != "web_search" {
		t.Fatalf("mode: %v", got)
	}
	if got := session["source"]; got != "golden-codex" {
		t.Fatalf("source: %v", got)
	}
	for key, want := range map[string]int64{"turn_count": 1, "event_count": 15} {
		if got := jsonAsInt64(session[key]); got != want {
			t.Fatalf("%s = %v, want %d", key, session[key], want)
		}
	}
	if session["started_at"] != "2026-02-15T03:50:40.000Z" ||
		session["updated_at"] != "2026-02-15T03:50:51.000Z" {
		t.Fatalf("session times: %v", session)
	}
	if session["terminal_event_id"] != nil || session["completed"] != false {
		t.Fatalf("terminal/completed: %v", session)
	}
	turns := data.(map[string]any)["turns"].([]any)
	if len(turns) != 0 {
		t.Fatalf("summary turns: %v", turns)
	}
	wantText := "Opened session " + sessionID +
		" summary only. 1 turns available; call open with id and limit to expand."
	if text != wantText {
		t.Fatalf("prose:\n%s\nwant:\n%s", text, wantText)
	}

	data, text, err = tool.Call(ctx, db, []byte(`{"id":"`+turnID+`","limit":3}`))
	if err != nil {
		t.Fatal(err)
	}
	turnData := data.(map[string]any)
	events := turnData["events"].([]any)
	if len(events) != 3 {
		t.Fatalf("page 1 events: %v", events)
	}
	wantIDs := []string{
		"event:NDhjOWIwYjI5ZjI2MjgzZGU2MGI4MDU0NGY2NDc1MmZkYjgwNjM1MjY1OGIwNzk4YmViMjk2MTdiOGIxNzBiNA",
		"event:NmNjYzk0ZjEzODcxZmI2NWM2YWIxYWJhMGMyNzcyOGNhNTdhYmM3MmY0ODJjOWE4YWJlZjRkMjM1NmUzZTNkMA",
		"event:OWM1MTVmNGMyYmM1OTgxYWMyNTIyOTcyZjk1MzBjNDAzNjlmYWI5MWMzM2Q1OWUyNWI0YmE0ZjdlMDViZDhhOA",
	}
	for i, want := range wantIDs {
		m := events[i].(map[string]any)
		if m["id"] != want {
			t.Fatalf("page1 event %d: %v", i, m["id"])
		}
		if int(jsonAsInt64(m["ordinal"])) != i+1 {
			t.Fatalf("page1 ordinal %d: %v", i, m["ordinal"])
		}
	}
	cursor, ok := turnData["next_cursor"].(string)
	if !ok || cursor == "" {
		t.Fatalf("next_cursor: %v", turnData["next_cursor"])
	}
	decoded, err := decodeOpenCursor(cursor)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Version != 2 || decoded.TargetID != turnID ||
		decoded.Limit != 3 || decoded.After.Kind != "event" || decoded.After.EventOrdinal != 3 {
		t.Fatalf("cursor: %+v", decoded)
	}
	if !strings.HasPrefix(text, "Opened turn "+turnID+" with 3 events. More are available with next_cursor.") {
		t.Fatalf("page prose: %s", text)
	}

	gotOrdinals := []int{1, 2, 3}
	pages := 1
	for cursor != "" {
		data, text, err = tool.Call(ctx, db, []byte(`{"cursor":"`+cursor+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		turnData = data.(map[string]any)
		events = turnData["events"].([]any)
		for _, event := range events {
			gotOrdinals = append(gotOrdinals, int(jsonAsInt64(event.(map[string]any)["ordinal"])))
		}
		pages++
		next, _ := turnData["next_cursor"].(string)
		cursor = next
	}
	if pages != 5 || len(gotOrdinals) != 15 {
		t.Fatalf("pages %d ordinals %v", pages, gotOrdinals)
	}
	for i, n := range gotOrdinals {
		if n != i+1 {
			t.Fatalf("ordinal %d: %d", i, n)
		}
	}
	if !strings.HasPrefix(text, "Opened turn "+turnID+" with the final 3 events.") {
		t.Fatalf("final prose: %s", text)
	}

	data, _, err = tool.Call(ctx, db, []byte(
		`{"cursor":"eyJ2ZXJzaW9uIjoyLCJ0YXJnZXRfaWQiOiJ0dXJuOk1ERTVZelZtTm1FdE5EbGlaQzAzT1RJd0xXRmpOamN0TVdSa09HVXpNMkl3WlRrMToxIiwibGltaXQiOjMsImFmdGVyIjp7ImtpbmQiOiJldmVudCIsImV2ZW50X29yZGluYWwiOjN9fQ"}`))
	if err != nil {
		t.Fatal(err)
	}
	events = data.(map[string]any)["events"].([]any)
	wantPage2 := []string{
		"event:NTg3NGQ0YWEzMDViNjVhNWYyMWZiMjY2ZmUwYTA4ZjY5NDdkNzI2ZjkxZmZlOWRhMzFiNzRmMjRlMWU0ZmExZA",
		"event:OTZjNmQ4ZTlkMWY0Mjg4NWZjZjQ5M2JjY2MwZDBlMTRjZDgxN2E3NGE1NzIzMmM0OWE3MTA2ODE2YWU0MmUwOQ",
		"event:NjQ0YTA4OTJmNGM1NzE2NzEwY2FmNmUyNmQxMzM5ZmYxOTczYTFhNWRjMjUwM2Q4MjNkMGE0YTJiYjI5ZGUzZQ",
	}
	for i, want := range wantPage2 {
		m := events[i].(map[string]any)
		if m["id"] != want || int(jsonAsInt64(m["ordinal"])) != i+4 {
			t.Fatalf("page2 event %d: %v", i, m)
		}
	}

	eventID := formatEventID(fixtureClaudeToolUID)
	data, text, err = tool.Call(ctx, db, []byte(`{"id":"`+eventID+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	eventData := data.(map[string]any)
	if eventData["kind"] != "event" {
		t.Fatalf("kind: %v", eventData["kind"])
	}
	event := eventData["event"].(map[string]any)
	if event["tool_name"] != "Read" || event["type"] != "tool_call" {
		t.Fatalf("event: %v", event)
	}
	if event["ordinal"] != uint32(3) || event["timestamp"] != "2026-01-19T15:58:41.421Z" {
		t.Fatalf("ordinal/time: %v %v", event["ordinal"], event["timestamp"])
	}
	content := eventData["content"].(map[string]any)
	if got := content["arguments"].(map[string]any)["path"]; got != "Cargo.toml" {
		t.Fatalf("arguments: %v", content["arguments"])
	}
	payload := content["payload"].(map[string]any)
	io := payload["moraine_tool_io"].(map[string]any)
	if hash, ok := io["io_hash"].(json.Number); !ok || hash.String() != "15999973548508414156" {
		t.Fatalf("io_hash: %v (%T)", io["io_hash"], io["io_hash"])
	}
	buckets := content["token_usage_buckets"].(map[string]uint64)
	if buckets["input_cache_write"] != 19630 {
		t.Fatalf("buckets: %v", buckets)
	}
	traversal := eventData["traversal"].(map[string]any)
	if traversal["previous_event_id"] != "event:ODgxNGM1ZjI4YjhkOTA3MTlkNTE0MmE2ZGY3ZGMxYTRmY2Q4NTIxY2U1NTRkZmFlMWQwNWJjYzJiZDJjY2EwMA" ||
		traversal["next_event_id"] != "event:ZDE4ZmE1NWM0MDI3MWIwNDA5NGQ1OGRmZWZhOTNjMjdkYjVmNjUzM2I4YjQ3ZjhkYzQ5OGQyMTRhM2EzYmU4YQ" {
		t.Fatalf("neighbors: %v", traversal)
	}
	if !strings.HasPrefix(text, "Opened event "+eventID+".") {
		t.Fatalf("event prose header: %s", text)
	}
	for _, want := range []string{
		`arguments: {"path":"Cargo.toml"}`,
		`"io_hash": 15999973548508414156`,
		"previous_event=event:ODgxNGM1ZjI4YjhkOTA3MTlkNTE0MmE2ZGY3ZGMxYTRmY2Q4NTIxY2U1NTRkZmFlMWQwNWJjYzJiZDJjY2EwMA next_event=event:ZDE4ZmE1NWM0MDI3MWIwNDA5NGQ1OGRmZWZhOTNjMjdkYjVmNjUzM2I4YjQ3ZjhkYzQ5OGQyMTRhM2EzYmU4YQ",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("event prose missing %q:\n%s", want, text)
		}
	}

	for _, tc := range []struct{ raw, want string }{
		{`{"id":"session:bm9wZQ"}`, "session not found"},
		{`{"id":"` + formatTurnID(fixtureCodexSession, 99) + `"}`, "turn not found"},
		{`{"id":"` + formatEventID("nope") + `"}`, "event not found"},
	} {
		_, _, err := tool.Call(ctx, db, []byte(tc.raw))
		if err == nil || err.Error() != tc.want {
			t.Fatalf("%s: got %v, want %q", tc.raw, err, tc.want)
		}
	}
}
