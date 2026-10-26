// Package export writes sessions out of the index in the interchange formats
// the design doc names under HTTP export: ATIF trajectories validated against
// the ported Harbor rules, OTLP/JSON traces built from the spans relation, and
// JSONL datasets filtered by annotation (docs/design-open-source-at-scale.md
// 102-105).
package export

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"strconv"
	"strings"
	"time"

	"github.com/candyflipline/mnemonic/internal/index"
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

// ErrInvalidATIF is an ATIF export's error when the validator refuses the
// trajectory.
var ErrInvalidATIF = errors.New("ATIF validation failed")

// ErrSessionNotFound is what an export answers for a session the index does
// not know and for one in hidden, so the two cannot be told apart.
var ErrSessionNotFound = errors.New("session not found")

func notFound(sessionID string) error {
	return fmt.Errorf("export: %w: %q", ErrSessionNotFound, sessionID)
}

// session is one session's published rows in event order, as the exports
// read them; rows may be ranged more than once.
type session struct {
	id, harness string
	src         index.ExportSession
	// rowErr is the first rows-walk read error, set by rows and returned
	// after every range, so a frame read failure fails the export instead
	// of writing a short document.
	rowErr error
}

// rows walks the session's rows one at a time, remembering the first read
// error in rowErr for the caller to return after its range. errStopRows, the
// early break, is not an error.
func (s *session) rows() iter.Seq[eventRow] {
	return func(yield func(eventRow) bool) {
		err := s.src.EachRow(func(r *index.Row) error {
			if yield(eventRowOf(r)) {
				return nil
			}
			return errStopRows
		})
		if err != nil && err != errStopRows && s.rowErr == nil {
			s.rowErr = err
		}
	}
}

// errStopRows stops a rows walk early, the iterator's break.
var errStopRows = errors.New("export: rows stopped")

// openSession reads one session from the index. Only published rows take
// part (the events table's rows, in event order, which EachRow walks). A
// session in hidden (deleted, per the server's session controls) is not
// found.
func openSession(db *index.DB, sessionID string, hidden map[string]bool) (session, error) {
	if hidden[sessionID] {
		return session{}, notFound(sessionID)
	}
	es, ok, err := db.ExportSession(sessionID)
	if err != nil {
		return session{}, err
	}
	if !ok || es.NRows == 0 {
		return session{}, notFound(sessionID)
	}
	return session{id: sessionID, harness: es.Harness, src: es}, nil
}

func eventRowOf(r *index.Row) eventRow {
	return eventRow{
		EventTime:       r.EventTime(),
		RequestID:       r.RequestID,
		TraceID:         r.TraceID,
		ItemID:          r.ItemID,
		EventKind:       r.EventKind,
		ActorKind:       r.ActorKind,
		ToolCallID:      r.ToolCallID,
		ToolName:        r.ToolName,
		TextContent:     r.Text,
		ToolIOInputJSON: r.TioInput,
		Model:           r.Model,
		InputTokens:     r.InputTokens,
		OutputTokens:    r.OutputTokens,
		CacheReadTokens: r.CacheReadTokens,
		CacheWrite:      r.CacheWriteTokens,
	}
}

// trajectory is one session's ATIF document, built and written a step at a
// time: plan says which step each row lands in and last the row that
// completes each step, so a step leaves memory once it is written.
type trajectory struct {
	session
	agent Agent
	plan  []rowPlan
	last  []int32
}

// rowPlan is where one row lands: the index of its step, or -1 for a row no
// step takes; linked marks a tool result that keeps its source_call_id.
type rowPlan struct {
	step   int32
	linked bool
}

// newTrajectory opens the session and plans its steps. The harness of the
// first event names the agent.
func newTrajectory(db *index.DB, sessionID string, hidden map[string]bool) (*trajectory, error) {
	s, err := openSession(db, sessionID, hidden)
	if err != nil {
		return nil, err
	}
	version := 0
	if p, ok := normalize.For(s.harness); ok {
		version = p.Version()
	}
	t := &trajectory{session: s, agent: Agent{Name: s.harness, Version: strconv.Itoa(version)}}
	t.plan, t.last = planSteps(s.harness, s.rows())
	if err := s.rowErr; err != nil {
		return nil, err
	}
	return t, nil
}

// envelope is the root object with the given steps value.
func (t *trajectory) envelope(steps []Step) Trajectory {
	return Trajectory{SchemaVersion: SchemaVersion, SessionID: t.id, Agent: t.agent, Steps: steps}
}

// WriteATIF writes the session's trajectory to w once the ported validator
// accepts it, laid out as the CLI prints it: keys sorted at every level and
// indented by two spaces. A refused trajectory writes nothing.
func WriteATIF(w io.Writer, db *index.DB, sessionID string, hidden map[string]bool) error {
	t, err := newTrajectory(db, sessionID, hidden)
	if err != nil {
		return err
	}
	if err := t.validate(); err != nil {
		return err
	}
	env, err := json.Marshal(t.envelope([]Step{}))
	if err != nil {
		return err
	}
	if env, err = sortedIndent(env, ""); err != nil {
		return err
	}
	return writeArray(w, env, `"steps": []`, "  ", func(prefix string, put func([]byte) error) error {
		return t.eachStep(func(s *Step) error {
			b, err := json.Marshal(s)
			if err != nil {
				return err
			}
			if b, err = sortedIndent(b, prefix); err != nil {
				return err
			}
			return put(b)
		})
	})
}

// writeCompact writes the trajectory as json.Marshal lays it out, unchecked:
// the caller validated it.
func (t *trajectory) writeCompact(w io.Writer) error {
	env, err := json.Marshal(t.envelope([]Step{}))
	if err != nil {
		return err
	}
	return writeArray(w, env, `"steps":[]`, "", func(_ string, put func([]byte) error) error {
		return t.eachStep(func(s *Step) error {
			b, err := json.Marshal(s)
			if err != nil {
				return err
			}
			return put(b)
		})
	})
}

// validate runs the ported validator over the envelope and then each step,
// reporting what ValidateJSON reports for the whole document.
func (t *trajectory) validate() error {
	env, err := json.Marshal(t.envelope(nil))
	if err != nil {
		return err
	}
	root, err := decodeNumbers(env)
	if err != nil {
		return err
	}
	obj := root.(map[string]any)
	v := &validator{}
	v.trajectoryHead(obj)
	var steps stepsCheck
	err = t.eachStep(func(s *Step) error {
		b, err := json.Marshal(s)
		if err != nil {
			return err
		}
		doc, err := decodeNumbers(b)
		if err != nil {
			return err
		}
		steps.add(doc)
		return nil
	})
	if err != nil {
		return err
	}
	if steps.n == 0 {
		v.missing("steps")
	} else {
		v.errs = append(v.errs, steps.errs()...)
	}
	v.trajectoryTail(obj)
	if len(v.errs) > 0 {
		return fmt.Errorf("export: %w: %s", ErrInvalidATIF, strings.Join(v.errs, "; "))
	}
	return nil
}

// requestKey names the model request an assistant row belongs to, the way the
// 59796bc spans derivations name a chat (59796bc:internal/cache/
// index_spans_claudecode.go: request_id, else trace_id; the pi family: the
// record id in item_id). It is "" for harnesses whose rows carry no request
// boundary (codex, cursor), whose assistant runs stay one step.
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

// planSteps maps rows to steps: one step per user message, per model request
// within a run of assistant rows (message, reasoning, tool_call), and per
// system/meta row. A tool result joins the step whose tool_calls carry its
// tool_call_id (the open agent run first, then the newest closed step), else
// the open agent run, else the newest step, and keeps its source_call_id only
// on an exact match; a result with neither id match nor content is dropped.
// Steps are numbered in the order they open, which is the order they close.
func planSteps(harness string, rows iter.Seq[eventRow]) (plan []rowPlan, last []int32) {
	latest := map[string]int32{}
	curCalls := map[string]bool{}
	cur, curReq := int32(-1), ""
	open := func() int32 {
		last = append(last, 0)
		return int32(len(last) - 1)
	}
	closeCur := func() {
		for id := range curCalls {
			latest[id] = cur
		}
		clear(curCalls)
		cur, curReq = -1, ""
	}
	r := int32(0)
	for ev := range rows {
		step, linked := int32(-1), false
		switch {
		case ev.ActorKind == "assistant" && isAssistantKind(ev.EventKind):
			req := requestKey(harness, ev)
			if cur >= 0 && req != "" && curReq != "" && req != curReq {
				closeCur()
			}
			if cur < 0 {
				cur = open()
			}
			if curReq == "" {
				curReq = req
			}
			if ev.EventKind == "tool_call" && ev.ToolCallID != "" {
				curCalls[ev.ToolCallID] = true
			}
			step = cur
		case ev.ActorKind == "tool":
			home, found := int32(-1), false
			if ev.ToolCallID != "" {
				if cur >= 0 && curCalls[ev.ToolCallID] {
					home, found = cur, true
				} else if k, ok := latest[ev.ToolCallID]; ok {
					home, found = k, true
				}
			}
			if !found {
				home = cur
				if home < 0 {
					home = int32(len(last)) - 1
				}
			}
			if home >= 0 && (found || ev.TextContent != "") {
				step, linked = home, found
			}
		default:
			if cur >= 0 {
				closeCur()
			}
			step = open()
		}
		plan = append(plan, rowPlan{step, linked})
		if step >= 0 {
			last[step] = r
		}
		r++
	}
	return plan, last
}

// eachStep builds the planned steps from the rows and hands each to fn, in
// order, as soon as its last row is read. A rows read error comes back after
// the range, the writers' cue to fail rather than end a short document.
func (t *trajectory) eachStep(fn func(*Step) error) error {
	err := buildSteps(t.plan, t.last, t.rows(), fn)
	if err == nil {
		err = t.session.rowErr
	}
	return err
}

// buildSteps is the step builder over any rows iterator, plan and last from
// planSteps.
func buildSteps(plan []rowPlan, last []int32, rows iter.Seq[eventRow], fn func(*Step) error) error {
	building := map[int32]*Step{}
	next, r := int32(0), int32(0)
	for ev := range rows {
		p := plan[r]
		if p.step >= 0 {
			s := building[p.step]
			switch {
			case ev.ActorKind == "assistant" && isAssistantKind(ev.EventKind):
				if s == nil {
					s = &Step{Source: "agent", Timestamp: isoMillis(ev.EventTime)}
					building[p.step] = s
				}
				absorbAssistant(s, ev)
			case ev.ActorKind == "tool":
				res := ObservationResult{Content: ev.TextContent}
				if p.linked {
					res.SourceCallID = ev.ToolCallID
				}
				if s.Observation == nil {
					s.Observation = &Observation{}
				}
				s.Observation.Results = append(s.Observation.Results, res)
			default:
				building[p.step] = &Step{
					Timestamp: isoMillis(ev.EventTime),
					Source:    sourceOf(ev),
					Message:   ev.TextContent,
				}
			}
		}
		for ; next < int32(len(last)) && last[next] <= r; next++ {
			s := building[next]
			delete(building, next)
			if metricsEmpty(s.Metrics) {
				s.Metrics = nil
			}
			s.StepID = int(next) + 1
			if err := fn(s); err != nil {
				return err
			}
		}
		r++
	}
	return nil
}

func isAssistantKind(kind string) bool {
	return kind == "message" || kind == "reasoning" || kind == "tool_call"
}

// absorbAssistant folds one assistant row into its step: reasoning rows feed
// reasoning_content, message rows the message, tool_call rows the tool_calls,
// and every row of one request carries that request's usage, so each token
// column keeps the max over the step's rows (the rule the 59796bc spans
// derivation states, 59796bc:internal/cache/index_spans_claudecode.go:10-13).
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
