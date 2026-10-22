package export

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/candyflipline/mnemonic/internal/cache"
	"github.com/candyflipline/mnemonic/internal/cache/cachetest"
)

// goldenDB opens one golden as its own lake and converged cache, exactly as
// internal/query/fixture_test.go does; the goldens share session ids, so a
// per-harness assertion needs one golden per data dir.
func goldenDB(t *testing.T, golden string) *sql.DB {
	t.Helper()
	dataDir := t.TempDir()
	if err := cachetest.Lake(dataDir, golden); err != nil {
		t.Fatal(err)
	}
	ix, err := cache.Open(dataDir, cache.Options{MemoryLimit: "1GB", FlagsSQL: "0::USMALLINT"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ix.Close() })
	if _, err := ix.Converge(context.Background(), cache.RetryFailed); err != nil {
		t.Fatal(err)
	}
	return ix.DB()
}

// TestFixtureExports pins, per fixture harness, the exported ATIF's step
// count and first two steps, that the trajectory passes the ported validator,
// and that hand-broken trajectories fail with the rule's literal text; it
// also pins the OTLP export of the claude-code session.
func TestFixtureExports(t *testing.T) {
	type stepWant struct{ source, message, calls string }
	cases := []struct {
		golden   string
		session  string
		steps    int
		firstTwo [2]stepWant
	}{
		{
			golden: "codex", session: "019c5f6a-49bd-7920-ac67-1dd8e33b0e95", steps: 8,
			firstTwo: [2]stepWant{{"system", "", ""}, {"system", "", ""}},
		},
		{
			golden: "claude_code", session: "7c666c01-d38e-4658-8650-854ffb5b626e", steps: 3,
			firstTwo: [2]stepWant{
				{"user", "Please inspect the sample project.", ""},
				{"agent", "I will read the manifest.", "toolu_1"},
			},
		},
		{
			golden: "cursor", session: "11111111-2222-4333-8444-555555555555", steps: 2,
			firstTwo: [2]stepWant{
				{"user", "local fixture cursor user prompt cursor_fixture_keyword", ""},
				{"agent", "I will inspect the target file.\nlocal fixture cursor assistant reply cursor_fixture_keyword cursor_fixture_trace\n[]", "cursor-tool-fixture"},
			},
		},
		{
			golden: "pi_coding_agent", session: "11111111-2222-4333-8444-555555555555", steps: 11,
			firstTwo: [2]stepWant{
				{"system", "/work/pi-demo", ""},
				{"system", "openrouter/arcee-ai/trinity-large-thinking", ""},
			},
		},
		{
			golden: "prime_agent", session: "12345678-1234-4234-8234-123456789abc", steps: 10,
			firstTwo: [2]stepWant{
				{"system", "/work/prime-demo", ""},
				{"system", "openai-codex/gpt-5.6-sol", ""},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.golden, func(t *testing.T) {
			traj, err := ATIF(context.Background(), goldenDB(t, tc.golden), tc.session, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(traj.Steps) != tc.steps {
				t.Fatalf("steps = %d, want %d", len(traj.Steps), tc.steps)
			}
			for i, w := range tc.firstTwo {
				s := traj.Steps[i]
				calls := make([]string, len(s.ToolCalls))
				for j, call := range s.ToolCalls {
					calls[j] = call.ToolCallID
				}
				got := stepWant{s.Source, s.Message, strings.Join(calls, ",")}
				if got != w {
					t.Fatalf("step %d = %+v, want %+v", i+1, got, w)
				}
			}
			b, err := json.Marshal(traj)
			if err != nil {
				t.Fatal(err)
			}
			if msgs := ValidateJSON(b); len(msgs) != 0 {
				t.Fatalf("validator rejected the export:\n%s", strings.Join(msgs, "\n"))
			}
		})
	}

	t.Run("broken", func(t *testing.T) {
		head := `{"schema_version":"ATIF-v1.8","agent":{"name":"a","version":"1"},"steps":[`
		for name, tc := range map[string]struct {
			doc  string
			want []string
		}{
			"missing-step-id": {
				head + `{"source":"user","message":"hi"}]}`,
				[]string{"trajectory.steps.0.step_id: required field is missing"},
			},
			"bad-source": {
				head + `{"step_id":1,"source":"robot","message":"hi"}]}`,
				[]string{`trajectory.steps.0.source: Input should be "system", "user" or "agent", got 'robot'`},
			},
			"unknown-call-id": {
				head + `{"step_id":1,"source":"agent","message":"","tool_calls":[{"tool_call_id":"call_1","function_name":"f","arguments":{}}],"observation":{"results":[{"source_call_id":"call_X","content":"y"}]}}]}`,
				[]string{"trajectory.steps.0.observation.results.0.source_call_id: Observation result references source_call_id 'call_X' which is not found in step 1's tool_calls"},
			},
			"empty-call-id": {
				head + `{"step_id":1,"source":"agent","message":"","tool_calls":[{"tool_call_id":"call_1","function_name":"f","arguments":{}}],"observation":{"results":[{"source_call_id":"","content":"y"}]}}]}`,
				[]string{"trajectory.steps.0.observation.results.0.source_call_id: Observation result references source_call_id '' which is not found in step 1's tool_calls"},
			},
			"null-schema-version": {
				`{"schema_version":null,"agent":{"name":"a","version":"1"},"steps":[{"step_id":1,"source":"user","message":"hi"}]}`,
				[]string{`trajectory.schema_version: Input should be "ATIF-v1.0", "ATIF-v1.1", "ATIF-v1.2", "ATIF-v1.3", "ATIF-v1.4", "ATIF-v1.5", "ATIF-v1.6", "ATIF-v1.7" or "ATIF-v1.8", got 'None'`},
			},
			"subagent-without-agent": {
				head + `{"step_id":1,"source":"user","message":"hi"}],"subagent_trajectories":[{"steps":[{"step_id":1,"source":"user","message":"x"}]}]}`,
				[]string{
					`trajectory.subagent_trajectories.0.trajectory_id: trajectory_id is required for embedded subagents (agent.name="")`,
					"trajectory.agent: required field is missing",
				},
			},
			"empty-trajectory-id": {
				head + `{"step_id":1,"source":"user","message":"hi"}],"subagent_trajectories":[{"trajectory_id":"","agent":{"name":"s","version":"1"},"steps":[{"step_id":1,"source":"user","message":"x"}]}]}`,
				nil,
			},
		} {
			if got := ValidateJSON([]byte(tc.doc)); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("%s: errors = %#v, want %#v", name, got, tc.want)
			}
		}
	})

	t.Run("otlp-claude", func(t *testing.T) {
		doc, err := OTLP(context.Background(), goldenDB(t, "claude_code"), "7c666c01-d38e-4658-8650-854ffb5b626e", nil)
		if err != nil {
			t.Fatal(err)
		}
		spans := doc.ResourceSpans[0].ScopeSpans[0].Spans
		if len(spans) != 4 {
			t.Fatalf("spans = %d, want 4", len(spans))
		}
		var root otlpSpan
		for _, s := range spans {
			if s.ParentSpanID == "" {
				root = s
			}
		}
		if root.Name != "claude-code" || root.Kind != 3 {
			t.Fatalf("root span = %+v, want invoke_agent claude-code", root)
		}
		withUsage := map[string]bool{}
		for _, s := range spans {
			for _, a := range s.Attributes {
				if a.Key == "gen_ai.usage.input_tokens" {
					withUsage[s.Attributes[0].Value.StringValue] = true
				}
			}
		}
		if !reflect.DeepEqual(withUsage, map[string]bool{"chat": true}) {
			t.Fatalf("spans carrying gen_ai.usage by operation = %v, want only chat", withUsage)
		}
	})
}

// TestRequestsSplitAgentSteps pins that an agent loop becomes one agent step
// per model request, each with its own request's usage, and that the tool
// result lands on the step whose call it answers.
func TestRequestsSplitAgentSteps(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	evs := []eventRow{
		{EventTime: at, ActorKind: "user", EventKind: "message", TextContent: "fix it"},
		{EventTime: at, ActorKind: "assistant", EventKind: "tool_call", RequestID: "req-1", ToolCallID: "call_1", ToolName: "Read", ToolIOInputJSON: `{"path":"a"}`, InputTokens: 100, OutputTokens: 7},
		{EventTime: at, ActorKind: "tool", EventKind: "tool_result", ToolCallID: "call_1", TextContent: "file body"},
		{EventTime: at, ActorKind: "assistant", EventKind: "message", RequestID: "req-2", TextContent: "done", InputTokens: 200, OutputTokens: 9},
	}
	traj := buildTrajectory("s1", "claude-code", evs)
	type stepGot struct {
		source, message string
		calls, results  int
		prompt          uint64
	}
	var got []stepGot
	for _, s := range traj.Steps {
		g := stepGot{source: s.Source, message: s.Message, calls: len(s.ToolCalls)}
		if s.Observation != nil {
			g.results = len(s.Observation.Results)
		}
		if s.Metrics != nil {
			g.prompt = s.Metrics.PromptTokens
		}
		got = append(got, g)
	}
	want := []stepGot{{"user", "fix it", 0, 0, 0}, {"agent", "", 1, 1, 100}, {"agent", "done", 0, 0, 200}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("steps = %+v, want %+v", got, want)
	}
	if id := traj.Steps[1].Observation.Results[0].SourceCallID; id != "call_1" {
		t.Fatalf("result source_call_id = %q, want call_1", id)
	}
}

// TestDeletedSessionRefused pins that a session in the hidden set is not
// found by either single-session export, though the cache holds it.
func TestDeletedSessionRefused(t *testing.T) {
	const session = "7c666c01-d38e-4658-8650-854ffb5b626e"
	db := goldenDB(t, "claude_code")
	hidden := map[string]bool{session: true}
	if _, err := ATIF(context.Background(), db, session, hidden); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("ATIF of a deleted session: %v, want %v", err, ErrSessionNotFound)
	}
	if _, err := OTLP(context.Background(), db, session, hidden); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("OTLP of a deleted session: %v, want %v", err, ErrSessionNotFound)
	}
}
