package query

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestListSessionsParsesAndCanonicalizes(t *testing.T) {
	raw := json.RawMessage(`{
		"start_datetime": "2026-04-30T09:00:00-04:00",
		"end_datetime": "2026-04-30T13:00:00-04:00",
		"mode": "chat",
		"sort": "asc",
		"limit": 12,
		"cursor": "abc"
	}`)
	a, err := lsParseArgs(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.StartUnixMs != 1777554000000 || a.EndUnixMs != 1777568400000 {
		t.Fatalf("range = %d..%d, want 1777554000000..1777568400000", a.StartUnixMs, a.EndUnixMs)
	}
	if a.Mode != "chat" || a.Sort != "asc" || a.Limit != 12 || a.Cursor != "abc" {
		t.Fatalf("mode/sort/limit/cursor = %q/%q/%d/%q", a.Mode, a.Sort, a.Limit, a.Cursor)
	}
}

func TestListSessionsRejectsMissingUnknownAndNaive(t *testing.T) {
	if _, err := lsParseArgs(json.RawMessage(`{}`)); err == nil || err.Error() != "list_sessions requires start_datetime and end_datetime with explicit timezones" {
		t.Fatalf("missing range: got %v", err)
	}
	raw := json.RawMessage(`{
		"start_datetime": "2026-04-30T09:00:00Z",
		"end_datetime": "2026-04-30T13:00:00Z",
		"extra": true
	}`)
	if _, err := lsParseArgs(raw); err == nil || err.Error() != "list_sessions expects a JSON object with start_datetime and end_datetime" {
		t.Fatalf("unknown field: got %v", err)
	}
	naive := json.RawMessage(`{
		"start_datetime": "2026-04-30T09:00:00",
		"end_datetime": "2026-04-30T13:00:00Z"
	}`)
	if _, err := lsParseArgs(naive); err == nil || !strings.Contains(err.Error(), "start_datetime") {
		t.Fatalf("naive start: got %v", err)
	}
}

func TestListSessionsRejectsNonIncreasingRangeAndLimitOverCap(t *testing.T) {
	equal := json.RawMessage(`{
		"start_datetime": "2026-04-30T09:00:00Z",
		"end_datetime": "2026-04-30T09:00:00Z"
	}`)
	if _, err := lsParseArgs(equal); err == nil || err.Error() != "end_datetime must be later than start_datetime" {
		t.Fatalf("equal range: got %v", err)
	}
	over := json.RawMessage(`{
		"start_datetime": "2026-04-30T09:00:00Z",
		"end_datetime": "2026-04-30T13:00:00Z",
		"limit": 26
	}`)
	if _, err := lsParseArgs(over); err == nil || err.Error() != "limit must be between 1 and 25" {
		t.Fatalf("limit over cap: got %v", err)
	}
}

func TestListSessionsRejectsNegativeLimit(t *testing.T) {
	raw := json.RawMessage(`{
		"start_datetime": "2026-04-30T09:00:00Z",
		"end_datetime": "2026-04-30T13:00:00Z",
		"limit": -1
	}`)
	if _, err := lsParseArgs(raw); err == nil || err.Error() != "limit must be between 1 and 25" {
		t.Fatalf("negative limit: got %v", err)
	}
}

func TestListSessionsShapesSessionMetadataOnly(t *testing.T) {
	page := &lsPage{Items: []lsItem{{
		SessionID:        "sess-open",
		FirstEventUnixMs: 1777554000000,
		LastEventUnixMs:  1777554600000,
		TotalTurns:       3,
		TotalEvents:      17,
		Mode:             "chat",
		Completed:        false,
		DisplayLabel:     "Codex, web search, Apr 30 13:10 UTC, 29 turns",
	}}}
	data := lsDataJSON(&lsArgs{Limit: 50}, page)
	sessions, ok := data["sessions"].([]any)
	if !ok || len(sessions) != 1 {
		t.Fatalf("sessions = %#v", data["sessions"])
	}
	item := sessions[0].(map[string]any)
	if item["id"] != "session:c2Vzcy1vcGVu" {
		t.Fatalf("id = %v", item["id"])
	}
	if item["rank"] != 1 {
		t.Fatalf("rank = %v", item["rank"])
	}
	if _, ok := item["originator"]; ok {
		t.Fatal("originator must not be exposed")
	}
	if _, ok := item["project"]; ok {
		t.Fatal("project must not be exposed")
	}
	session := item["session"].(map[string]any)
	if session["harness"] != nil || session["source"] != nil || session["title"] != nil {
		t.Fatalf("metadata = %#v", session)
	}
	if session["started_at"] != "2026-04-30T13:00:00.000Z" || session["updated_at"] != "2026-04-30T13:10:00.000Z" {
		t.Fatalf("times = %v..%v", session["started_at"], session["updated_at"])
	}
	if session["turn_count"] != 3 || session["event_count"] != int64(17) {
		t.Fatalf("counts = %v/%v", session["turn_count"], session["event_count"])
	}
	open := item["open"].(map[string]any)
	if open["session_id"] != "session:c2Vzcy1vcGVu" {
		t.Fatalf("open = %v", open)
	}
}

func TestListSessionsTextUsesDisplayLabelOverWireMetadata(t *testing.T) {
	title := "Legacy wire title"
	summary := "Legacy summary"
	page := &lsPage{Items: []lsItem{{
		SessionID:        "sess-open",
		FirstEventUnixMs: 1777554000000,
		LastEventUnixMs:  1777554600000,
		DisplayLabel:     "Genuine bounded request",
		Title:            &title,
		SessionSummary:   &summary,
	}}}
	payload := map[string]any{
		"request": lsCanonicalRequest(&lsArgs{
			StartDatetime: "2026-04-30T13:00:00.000Z", EndDatetime: "2026-04-30T13:10:00.000Z", Limit: 50,
		}),
		"data": lsDataJSON(&lsArgs{Limit: 50}, page),
	}
	text := lsText(payload)
	if !strings.Contains(text, "Genuine bounded request") {
		t.Fatalf("text missing display label:\n%s", text)
	}
	if strings.Contains(text, "Legacy wire title") || strings.Contains(text, "Legacy summary") {
		t.Fatalf("text leaks wire metadata:\n%s", text)
	}
}

func TestListSessionsUntitledGetsPrivacySafeDisplayLabel(t *testing.T) {
	page := &lsPage{Items: []lsItem{{
		SessionID:        "sess-open",
		FirstEventUnixMs: 1777554000000,
		LastEventUnixMs:  1777554600000,
		DisplayLabel:     "Codex, web search, Apr 30 13:10 UTC, 29 turns",
	}}}
	data := lsDataJSON(&lsArgs{Limit: 50}, page)
	payload := map[string]any{
		"request": map[string]any{"start_datetime": "x", "end_datetime": "y"},
		"data":    data,
	}
	text := lsText(payload)
	if !strings.Contains(text, "Codex, web search, Apr 30 13:10 UTC, 29 turns") {
		t.Fatalf("text missing label:\n%s", text)
	}
	if strings.Contains(text, "untitled session") {
		t.Fatalf("text leaks untitled fallback:\n%s", text)
	}
}

func TestListSessionsBlankTitleAndSummaryFallThroughToSlug(t *testing.T) {
	blank := ""
	page := &lsPage{Items: []lsItem{{
		SessionID:        "sess-open",
		FirstEventUnixMs: 1777554000000,
		LastEventUnixMs:  1777554600000,
		Mode:             "chat",
		Title:            &blank,
		SessionSummary:   &blank,
		DisplayLabel:     "useful-slug",
	}}}
	data := lsDataJSON(&lsArgs{Limit: 50}, page)
	payload := map[string]any{
		"request": map[string]any{"start_datetime": "x", "end_datetime": "y"},
		"data":    data,
	}
	text := lsText(payload)
	if !strings.Contains(text, "useful-slug") {
		t.Fatalf("text missing slug:\n%s", text)
	}
	if strings.Contains(text, "chat     ") {
		t.Fatalf("text falls back to mode padding:\n%s", text)
	}
}

func TestListSessionsSkipsRowsWithEmptySessionID(t *testing.T) {
	page := &lsPage{Items: []lsItem{
		{SessionID: ""},
		{SessionID: "real-session"},
	}}
	data := lsDataJSON(&lsArgs{Limit: 50}, page)
	if data["result_count"] != 1 {
		t.Fatalf("result_count = %v", data["result_count"])
	}
	sessions := data["sessions"].([]any)
	item := sessions[0].(map[string]any)
	if item["rank"] != 1 || item["id"] != "session:cmVhbC1zZXNzaW9u" {
		t.Fatalf("item = %#v", item)
	}
}

func TestListSessionsSkipsTrailingWhitespaceRowsAndPreservesCursor(t *testing.T) {
	page := &lsPage{
		Items: []lsItem{
			{SessionID: "real-session"},
			{SessionID: "   "},
			{SessionID: ""},
		},
		NextCursor: strPtr("opaque-cursor"),
	}
	data := lsDataJSON(&lsArgs{Limit: 50}, page)
	if data["result_count"] != 1 {
		t.Fatalf("result_count = %v", data["result_count"])
	}
	if data["truncated"] != true {
		t.Fatalf("truncated = %v", data["truncated"])
	}
	if data["next_cursor"] != "opaque-cursor" {
		t.Fatalf("next_cursor = %v", data["next_cursor"])
	}
}
