// Package export writes sessions out of the cache in the interchange formats
// the design doc names under HTTP export: ATIF trajectories validated against
// the ported Harbor rules, OTLP/JSON traces built from the spans relation, and
// JSONL datasets filtered by annotation (docs/design-open-source-at-scale.md
// 102-105).
package export

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/candyflipline/mnemonic/internal/normalize"
)

// SchemaVersion is the ATIF compatibility string the export writes
// (m-trajectory.py:15-28, default ATIF-v1.8).
const SchemaVersion = "ATIF-v1.8"

// Trajectory is the ATIF root object. Fields the models mark optional are
// omitted from the JSON when empty, as to_json_dict(exclude_none=True) writes
// them (m-trajectory.py:109-118).
type Trajectory struct {
	SchemaVersion string `json:"schema_version"`
	SessionID     string `json:"session_id,omitempty"`
	Agent         Agent  `json:"agent"`
	Steps         []Step `json:"steps"`
}

// Agent identifies the producing agent system (m-agent.py:11-18). Name is the
// harness, version the parser version that produced the events.
type Agent struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	ModelName string `json:"model_name,omitempty"`
}

// Step is one interaction turn (m-step.py:14-89, atif.md StepObject 130-144).
// Message is required and may be empty (atif.md:137).
type Step struct {
	StepID           int          `json:"step_id"`
	Timestamp        string       `json:"timestamp,omitempty"`
	Source           string       `json:"source"`
	ModelName        string       `json:"model_name,omitempty"`
	Message          string       `json:"message"`
	ReasoningContent string       `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall   `json:"tool_calls,omitempty"`
	Observation      *Observation `json:"observation,omitempty"`
	Metrics          *Metrics     `json:"metrics,omitempty"`
}

// ToolCall is one action inside an agent step (m-tool_call.py:8-31).
// Arguments is required and may be empty (atif.md:158).
type ToolCall struct {
	ToolCallID   string         `json:"tool_call_id"`
	FunctionName string         `json:"function_name"`
	Arguments    map[string]any `json:"arguments"`
}

// Observation holds the environment feedback of a step; results is required
// (atif.md:230).
type Observation struct {
	Results []ObservationResult `json:"results"`
}

// ObservationResult is one tool output, keyed to the step's tool_calls by
// SourceCallID (m-observation_result.py:11-31).
type ObservationResult struct {
	SourceCallID string `json:"source_call_id,omitempty"`
	Content      string `json:"content,omitempty"`
}

// Metrics is the per-step LLM usage (m-metrics.py:8-44). Cache writes have no
// top-level field; atif.md:209-216 puts provider-specific token counters in
// extra.
type Metrics struct {
	PromptTokens     uint64         `json:"prompt_tokens,omitempty"`
	CompletionTokens uint64         `json:"completion_tokens,omitempty"`
	CachedTokens     uint64         `json:"cached_tokens,omitempty"`
	Extra            map[string]any `json:"extra,omitempty"`
}

// eventRow is one published events row, the input of the mapping below.
type eventRow struct {
	EventTime       time.Time
	RequestID       string
	TraceID         string
	ItemID          string
	EventKind       string
	ActorKind       string
	ToolCallID      string
	ToolName        string
	TextContent     string
	ToolIOInputJSON string
	Model           string
	InputTokens     uint64
	OutputTokens    uint64
	CacheReadTokens uint64
	CacheWrite      uint64
}

const atifEventsSQL = `SELECT event_time, coalesce(request_id, ''), coalesce(trace_id, ''), coalesce(item_id, ''), event_kind, actor_kind, tool_call_id, tool_name,
  text_content, coalesce(tool_io_input_json, ''), model,
  input_tokens, output_tokens, cache_read_tokens, cache_write_tokens
FROM events WHERE session_id = ? AND event_order IS NOT NULL ORDER BY event_order`

// ErrSessionNotFound is what an export answers for a session the cache does
// not know and for one in hidden, so the two cannot be told apart.
var ErrSessionNotFound = errors.New("session not found")

func notFound(sessionID string) error {
	return fmt.Errorf("export: %w: %q", ErrSessionNotFound, sessionID)
}

// ATIF builds one session's trajectory from the cache. Only published rows
// take part (`event_order IS NOT NULL`, internal/query/query.go:21), in
// event_order. The harness of the first event names the agent. A session in
// hidden (deleted, per the server's session controls) is not found.
func ATIF(ctx context.Context, db *sql.DB, sessionID string, hidden map[string]bool) (*Trajectory, error) {
	if hidden[sessionID] {
		return nil, notFound(sessionID)
	}
	rows, err := db.QueryContext(ctx, atifEventsSQL, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var evs []eventRow
	harness := ""
	for rows.Next() {
		var r eventRow
		if err := rows.Scan(&r.EventTime, &r.RequestID, &r.TraceID, &r.ItemID, &r.EventKind, &r.ActorKind, &r.ToolCallID, &r.ToolName,
			&r.TextContent, &r.ToolIOInputJSON, &r.Model,
			&r.InputTokens, &r.OutputTokens, &r.CacheReadTokens, &r.CacheWrite); err != nil {
			return nil, err
		}
		evs = append(evs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := db.QueryRowContext(ctx,
		`SELECT harness FROM events WHERE session_id = $1 AND event_order IS NOT NULL ORDER BY event_order LIMIT 1`,
		sessionID).Scan(&harness); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, notFound(sessionID)
		}
		return nil, err
	}
	return buildTrajectory(sessionID, harness, evs), nil
}

// MarshalATIF builds the trajectory and returns its JSON, but only once the
// ported validator accepts it; a failed rule comes back as the error.
func MarshalATIF(ctx context.Context, db *sql.DB, sessionID string, hidden map[string]bool) ([]byte, error) {
	t, err := ATIF(ctx, db, sessionID, hidden)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(t)
	if err != nil {
		return nil, err
	}
	if msgs := ValidateJSON(b); len(msgs) > 0 {
		return nil, errors.New("export: ATIF validation failed: " + strings.Join(msgs, "; "))
	}
	return b, nil
}

// requestKey names the model request an assistant row belongs to, the way the
// spans derivations name a chat (internal/cache/index_spans_claudecode.go:
// request_id, else trace_id; the pi family: the record id in item_id). It is
// "" for harnesses whose rows carry no request boundary (codex, cursor), whose
// assistant runs stay one step.
func requestKey(harness string, ev eventRow) string {
	switch harness {
	case "claude-code":
		if ev.RequestID != "" {
			return ev.RequestID
		}
		return ev.TraceID
	case "pi-coding-agent", "prime-agent":
		return ev.ItemID
	}
	return ""
}

// buildTrajectory maps events to steps: one step per user message, per model
// request within a run of assistant rows (message, reasoning, tool_call), and
// per system/meta row; a tool result joins the step whose tool_calls carry its
// tool_call_id.
func buildTrajectory(sessionID, harness string, evs []eventRow) *Trajectory {
	version := 0
	if p, ok := normalize.For(harness); ok {
		version = p.Version()
	}
	t := &Trajectory{
		SchemaVersion: SchemaVersion,
		SessionID:     sessionID,
		Agent:         Agent{Name: harness, Version: strconv.Itoa(version)},
	}
	var cur *Step
	curReq := ""
	flush := func() {
		if cur == nil {
			return
		}
		if metricsEmpty(cur.Metrics) {
			cur.Metrics = nil
		}
		t.Steps = append(t.Steps, *cur)
		cur, curReq = nil, ""
	}
	for _, ev := range evs {
		switch {
		case ev.ActorKind == "assistant" && isAssistantKind(ev.EventKind):
			req := requestKey(harness, ev)
			if cur != nil && req != "" && curReq != "" && req != curReq {
				flush()
			}
			if cur == nil {
				cur = &Step{Source: "agent", Timestamp: isoMillis(ev.EventTime)}
			}
			if curReq == "" {
				curReq = req
			}
			absorbAssistant(cur, ev)
		case ev.ActorKind == "tool":
			// The result joins the step whose tool_calls carry its
			// tool_call_id, the open agent run, or the newest step, in that
			// order; only an exact match keeps the source_call_id.
			home := cur
			if home == nil || !stepHasCall(home, ev.ToolCallID) {
				home = nil
				for i := len(t.Steps) - 1; i >= 0 && home == nil; i-- {
					if stepHasCall(&t.Steps[i], ev.ToolCallID) {
						home = &t.Steps[i]
					}
				}
				if home == nil {
					home = cur
				}
				if home == nil && len(t.Steps) > 0 {
					home = &t.Steps[len(t.Steps)-1]
				}
			}
			res := ObservationResult{Content: ev.TextContent}
			if home != nil && ev.ToolCallID != "" && stepHasCall(home, ev.ToolCallID) {
				res.SourceCallID = ev.ToolCallID
			}
			if res.SourceCallID == "" && res.Content == "" {
				continue
			}
			if home == nil && len(t.Steps) > 0 {
				home = &t.Steps[len(t.Steps)-1]
			}
			if home == nil {
				continue
			}
			if home.Observation == nil {
				home.Observation = &Observation{}
			}
			home.Observation.Results = append(home.Observation.Results, res)
		default:
			flush()
			t.Steps = append(t.Steps, Step{
				Timestamp: isoMillis(ev.EventTime),
				Source:    sourceOf(ev),
				Message:   ev.TextContent,
			})
		}
	}
	flush()
	for i := range t.Steps {
		t.Steps[i].StepID = i + 1
	}
	return t
}

func isAssistantKind(kind string) bool {
	return kind == "message" || kind == "reasoning" || kind == "tool_call"
}

// absorbAssistant folds one assistant row into its step: reasoning rows feed
// reasoning_content, message rows the message, tool_call rows the tool_calls,
// and every row of one request carries that request's usage, so each token
// column keeps the max over the step's rows (the rule the spans derivation states,
// internal/cache/index_spans_claudecode.go:10-13).
func absorbAssistant(s *Step, ev eventRow) {
	switch ev.EventKind {
	case "reasoning":
		if s.ReasoningContent != "" {
			s.ReasoningContent += "\n"
		}
		s.ReasoningContent += ev.TextContent
	case "tool_call":
		if ev.ToolCallID == "" {
			break
		}
		s.ToolCalls = append(s.ToolCalls, ToolCall{
			ToolCallID:   ev.ToolCallID,
			FunctionName: ev.ToolName,
			Arguments:    toolArguments(ev),
		})
	case "message":
		if s.Message != "" {
			s.Message += "\n"
		}
		s.Message += ev.TextContent
	}
	absorbUsage(s, ev)
}

func metricsEmpty(m *Metrics) bool {
	return m == nil || m.PromptTokens == 0 && m.CompletionTokens == 0 &&
		m.CachedTokens == 0 && len(m.Extra) == 0
}

func absorbUsage(s *Step, ev eventRow) {
	if s.Metrics == nil {
		s.Metrics = &Metrics{}
	}
	m := s.Metrics
	m.PromptTokens = max(m.PromptTokens, ev.InputTokens)
	m.CompletionTokens = max(m.CompletionTokens, ev.OutputTokens)
	m.CachedTokens = max(m.CachedTokens, ev.CacheReadTokens)
	if ev.CacheWrite > 0 {
		if m.Extra == nil {
			m.Extra = map[string]any{}
		}
		if n, ok := m.Extra["cache_creation_input_tokens"].(uint64); !ok || ev.CacheWrite > n {
			m.Extra["cache_creation_input_tokens"] = ev.CacheWrite
		}
	}
}

// toolArguments reads the call's arguments object from the tool request's
// input JSON, falling back to the row's text and to {} when neither parses to
// an object (atif.md:158, arguments can be empty).
func toolArguments(ev eventRow) map[string]any {
	for _, src := range []string{ev.ToolIOInputJSON, ev.TextContent} {
		if src == "" {
			continue
		}
		var args map[string]any
		if json.Unmarshal([]byte(src), &args) == nil && args != nil {
			return args
		}
	}
	return map[string]any{}
}

func stepHasCall(s *Step, callID string) bool {
	if callID == "" {
		return false
	}
	for _, tc := range s.ToolCalls {
		if tc.ToolCallID == callID {
			return true
		}
	}
	return false
}

// sourceOf maps the event's actor to the ATIF source enum (m-step.py:26-29).
func sourceOf(ev eventRow) string {
	switch ev.ActorKind {
	case "user":
		return "user"
	case "assistant":
		return "agent"
	default:
		return "system"
	}
}

// isoMillis writes the ISO 8601 timestamp the models validate
// (m-step.py:93-102), with millisecond precision as DateTime64(3) stores.
func isoMillis(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}
