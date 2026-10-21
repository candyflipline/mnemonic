package query

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/candyflipline/mnemonic/internal/cache"
	"github.com/candyflipline/mnemonic/internal/cache/cachetest"
)

// Fixture facts: all four goldens land under author "test", machine "m1";
// codex is one session, claude_code another, and cursor + cursor_sqlite merge
// into one shared session id. Every source reports the same updated_ms.
const (
	fxCursorSessionID = "session:MTExMTExMTEtMjIyMi00MzMzLTg0NDQtNTU1NTU1NTU1NTU1"
	fxCodexSessionID  = "session:MDE5YzVmNmEtNDliZC03OTIwLWFjNjctMWRkOGUzM2IwZTk1"
	fxClaudeSessionID = "session:N2M2NjZjMDEtZDM4ZS00NjU4LTg2NTAtODU0ZmZiNWI2MjZl"
	fxSourceUpdatedMs = 1767225600000
	fxSourceCount     = 4
	fxTotalBytes      = 84870
)

func fixtureDB(t *testing.T) *sql.DB {
	return lakeDB(t, "codex", "claude_code", "cursor", "cursor_sqlite")
}

func lakeDB(t *testing.T, goldens ...string) *sql.DB {
	t.Helper()
	dataDir := t.TempDir()
	if err := cachetest.Lake(dataDir, goldens...); err != nil {
		t.Fatal(err)
	}
	ix, err := cache.Open(dataDir, cache.Options{MemoryLimit: "1GB", FlagsSQL: SearchFlagsSQL()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ix.Close() })
	if _, err := ix.Converge(context.Background(), cache.RetryFailed); err != nil {
		t.Fatal(err)
	}
	return ix.DB()
}

func callTool(t *testing.T, db *sql.DB, tool Tool, args string) (any, string) {
	t.Helper()
	data, text, err := tool.Call(context.Background(), db, json.RawMessage(args))
	if err != nil {
		t.Fatalf("%s: %v", tool.Name(), err)
	}
	return data, text
}

func TestFixtureListSessions(t *testing.T) {
	db := fixtureDB(t)
	raw, text := callTool(t, db, listSessions{}, `{
		"start_datetime": "2020-01-01T00:00:00Z",
		"end_datetime": "2030-01-01T00:00:00Z",
		"limit": 20
	}`)
	data := raw.(map[string]any)
	if data["result_count"] != 3 || data["truncated"] != false {
		t.Fatalf("count/truncated = %v/%v", data["result_count"], data["truncated"])
	}
	sessions := data["sessions"].([]any)
	wantIDs := []string{fxCursorSessionID, fxCodexSessionID, fxClaudeSessionID}
	for i, want := range wantIDs {
		item := sessions[i].(map[string]any)
		if item["id"] != want || item["rank"] != i+1 {
			t.Fatalf("sessions[%d] = %v (rank %v), want %s (rank %d)", i, item["id"], item["rank"], want, i+1)
		}
	}
	if !strings.Contains(text, "Found 3 session(s) overlapping 2020-01-01T00:00:00Z to 2030-01-01T00:00:00Z.") {
		t.Fatalf("text header:\n%s", text)
	}
	cursor := sessions[0].(map[string]any)["session"].(map[string]any)
	if cursor["updated_at"] != "2026-05-08T02:05:58.404Z" || cursor["harness"] != "cursor" {
		t.Fatalf("cursor session = %#v", cursor)
	}
}

func TestFixtureFileAttentionCargoToml(t *testing.T) {
	db := fixtureDB(t)
	raw, text := callTool(t, db, fileAttention{}, `{"path":"Cargo.toml","scope":"all"}`)
	data := raw.(map[string]any)
	if data["result_count"] != 2 {
		t.Fatalf("result_count = %v", data["result_count"])
	}
	sessions := data["sessions"].([]any)
	type want struct {
		id      string
		harness string
		touches int
		tools   string
		last    string
	}
	for i, w := range []want{
		{fxCodexSessionID, "codex", 1, "Read", "2026-02-15T03:50:44.000Z"},
		{fxClaudeSessionID, "claude-code", 1, "Read", "2026-01-19T15:58:41.421Z"},
	} {
		session := sessions[i].(map[string]any)
		if session["id"] != w.id {
			t.Fatalf("sessions[%d] id = %v, want %s", i, session["id"], w.id)
		}
		inner := session["session"].(map[string]any)
		if inner["harness"] != w.harness || inner["touch_count"] != w.touches || inner["last_touch"] != w.last {
			t.Fatalf("sessions[%d] = %#v", i, inner)
		}
		if got := strings.Join(inner["tools"].([]string), ","); got != w.tools {
			t.Fatalf("sessions[%d] tools = %v", i, inner["tools"])
		}
	}
	summary := data["summary"].(map[string]any)
	if summary["total_touches"] != 2 || summary["unknown_root_touches"] != 2 {
		t.Fatalf("summary = %#v", summary)
	}
	if !strings.Contains(text, "file_attention Cargo.toml — 2 touch(es) across 2 session(s) in 1 worktree root(s) [scope=all].") {
		t.Fatalf("text header:\n%s", text)
	}
}

func TestFixtureFileAttentionGameJS(t *testing.T) {
	db := fixtureDB(t)
	raw, _ := callTool(t, db, fileAttention{}, `{"path":"game.js","scope":"all"}`)
	data := raw.(map[string]any)
	if data["result_count"] != 1 {
		t.Fatalf("result_count = %v", data["result_count"])
	}
	session := data["sessions"].([]any)[0].(map[string]any)
	if session["id"] != fxCursorSessionID {
		t.Fatalf("id = %v", session["id"])
	}
	inner := session["session"].(map[string]any)
	if inner["harness"] != "cursor" || inner["source"] != "golden-cursor-sqlite" {
		t.Fatalf("session = %#v", inner)
	}
	if got := strings.Join(inner["tools"].([]string), ","); got != "edit_file_v2" {
		t.Fatalf("tools = %v", inner["tools"])
	}
	if inner["last_touch"] != "2026-05-08T02:05:34.020Z" {
		t.Fatalf("last_touch = %v", inner["last_touch"])
	}
}

func TestFixtureFileAttentionMutationsOnly(t *testing.T) {
	db := fixtureDB(t)
	raw, _ := callTool(t, db, fileAttention{}, `{"path":"Cargo.toml","scope":"all","mutations_only":true}`)
	data := raw.(map[string]any)
	if data["result_count"] != 0 || data["truncated"] != false {
		t.Fatalf("Cargo.toml count = %v", data["result_count"])
	}
	raw, _ = callTool(t, db, fileAttention{}, `{"path":"game.js","scope":"all","mutations_only":true}`)
	data = raw.(map[string]any)
	if data["result_count"] != 1 {
		t.Fatalf("game.js count = %v", data["result_count"])
	}
}

func TestFixtureFileAttentionShellCommand(t *testing.T) {
	db := fixtureDB(t)
	raw, _ := callTool(t, db, fileAttention{}, `{"path":"npm","scope":"all"}`)
	data := raw.(map[string]any)
	sessions, ok := data["sessions"].([]any)
	if !ok || len(sessions) != 1 {
		t.Fatalf("sessions = %#v", data["sessions"])
	}
	session := sessions[0].(map[string]any)
	if session["id"] != fxCursorSessionID {
		t.Fatalf("id = %v", session["id"])
	}
	inner := session["session"].(map[string]any)
	if got := strings.Join(inner["match_kinds"].([]string), ","); got != "shell_path" {
		t.Fatalf("match_kinds = %v", inner["match_kinds"])
	}
	if got := strings.Join(inner["tools"].([]string), ","); got != "run_terminal_command_v2" {
		t.Fatalf("tools = %v", inner["tools"])
	}
}

func TestFixtureGetIngestStatus(t *testing.T) {
	db := fixtureDB(t)
	raw, text := callTool(t, db, getIngestStatus{}, `{}`)
	st := raw.(*stStatusJSONShape)
	if st.ObservedAtUnixMs <= 0 {
		t.Fatalf("observed_at = %v", st.ObservedAtUnixMs)
	}
	if st.Current == nil || st.Current.HeartbeatObservedAtUnixMs != fxSourceUpdatedMs {
		t.Fatalf("current = %#v", st.Current)
	}
	if st.Current.FilesWatched != fxSourceCount {
		t.Fatalf("files_watched = %v, want %d", st.Current.FilesWatched, fxSourceCount)
	}
	p := st.Current.Progress
	if p.FilesTotal != fxSourceCount || p.FilesCompleted != fxSourceCount ||
		p.BytesTotal != fxTotalBytes || p.BytesCompleted != fxTotalBytes ||
		!p.DiscoveryComplete || p.SinkRetrying {
		t.Fatalf("progress = %#v", p)
	}
	conditions := map[string]stConditionJSON{}
	for _, c := range st.Conditions {
		conditions[c.ConditionType] = c
	}
	if conditions["health"].State != stStateFalse || conditions["health"].Reason != "heartbeat_stale" {
		t.Fatalf("health = %+v", conditions["health"])
	}
	if conditions["coverage"].State != stStateTrue || conditions["coverage"].Reason != "backfill_complete" {
		t.Fatalf("coverage = %+v", conditions["coverage"])
	}
	if conditions["freshness"].State != stStateUnknown {
		t.Fatalf("freshness = %+v", conditions["freshness"])
	}
	if conditions["readiness"].State != stStateFalse || conditions["readiness"].Reason != "retrieval_may_be_incomplete" {
		t.Fatalf("readiness = %+v", conditions["readiness"])
	}
	if len(st.Alerts) != 1 || st.Alerts[0].Code != stAlertHeartbeatStale {
		t.Fatalf("alerts = %#v", st.Alerts)
	}
	if len(st.History) != 1 || st.History[0].TsUnixMs != fxSourceUpdatedMs ||
		st.History[0].FilesTotal != fxSourceCount {
		t.Fatalf("history = %#v", st.History)
	}
	if st.Rate != nil || st.Eta != nil {
		t.Fatalf("rate/eta = %v/%v", st.Rate, st.Eta)
	}
	if !strings.Contains(text, "Ingestion health=degraded (heartbeat_stale); coverage=ok (backfill_complete); freshness=unknown (heartbeat_stale); readiness=degraded (retrieval_may_be_incomplete).") ||
		!strings.Contains(text, "Historical startup snapshot: 4/4 files, 84870/84870 bytes (100.0%).") ||
		!strings.Contains(text, "Active alerts: 1.") {
		t.Fatalf("text:\n%s", text)
	}
}
