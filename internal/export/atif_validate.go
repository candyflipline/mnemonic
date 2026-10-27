package export

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// This file ports Harbor's ATIF validator (validator.py) and the Pydantic
// trajectory models it drives to Go. Like validator.py:22-23 it collects every
// violation, and like validator.py:161-201 it reports each one as
// "trajectory.<loc>: <message>" with the field path joined by dots.
//
// Deviation from validator.py: errors raised by the models' trajectory-level
// validators (step-id sequencing, tool-call references) carry the location of
// the offending field, where Pydantic reports them at the root.
//
// Known gap, kept on purpose: Pydantic's lax mode coerces, and this port does
// not. An int field rejects 1.0, true and "1" here where Pydantic accepts
// them, and a bool field rejects 0 and 1. The export never emits those shapes.

func ValidateJSON(b []byte) []string {
	doc, err := decodeNumbers(b)
	if err != nil {
		return []string{"Invalid JSON: " + err.Error()}
	}
	return Validate(doc)
}

func Validate(doc any) []string {
	v := &validator{}
	if _, ok := doc.(map[string]any); !ok {
		v.errs = append(v.errs, "Trajectory must be a JSON object/dict")
		return v.errs
	}
	v.trajectory(doc)
	return v.errs
}

type validator struct{ errs []string }

func (v *validator) addf(loc, format string, args ...any) {
	v.errs = append(v.errs, "trajectory."+loc+": "+fmt.Sprintf(format, args...))
}

// missing reports a required field the models give default=... (m-trajectory.py:62,
// m-step.py:17, ...), in validator.py:168's wording.
func (v *validator) missing(loc string) { v.addf(loc, "required field is missing") }

// extraForbid reports a field outside a model's known set; every model sets
// extra="forbid" (m-trajectory.py:107, m-step.py:91, m-tool_call.py:31,
// m-agent.py:32, m-metrics.py:44, m-observation_result.py:42), rendered per
// validator.py:169-171.
func (v *validator) extraForbid(loc string, obj map[string]any, known []string) {
	for _, k := range sortedKeys(obj) {
		if !slices.Contains(known, k) {
			v.addf(join(loc, k), "unexpected field (not part of ATIF schema)")
		}
	}
}

func (v *validator) wantString(loc string, val any) bool {
	_, ok := val.(string)
	if !ok {
		v.addf(loc, "expected string, got %s", jsonKind(val))
	}
	return ok
}

func (v *validator) wantObject(loc string, val any) (map[string]any, bool) {
	m, ok := val.(map[string]any)
	if !ok {
		v.addf(loc, "expected object, got %s", jsonKind(val))
	}
	return m, ok
}

func (v *validator) wantArray(loc string, val any) ([]any, bool) {
	a, ok := val.([]any)
	if !ok {
		v.addf(loc, "expected array, got %s", jsonKind(val))
	}
	return a, ok
}

func (v *validator) wantInt(loc string, val any) (int64, bool) {
	n, ok := val.(json.Number)
	if !ok || strings.ContainsAny(n.String(), ".eE") {
		v.addf(loc, "expected integer, got %s", jsonKind(val))
		return 0, false
	}
	i, err := n.Int64()
	if err != nil {
		v.addf(loc, "expected integer, got %s", n.String())
		return 0, false
	}
	return i, true
}

func (v *validator) wantNumber(loc string, val any) bool {
	_, ok := val.(json.Number)
	if !ok {
		v.addf(loc, "expected number, got %s", jsonKind(val))
	}
	return ok
}

func (v *validator) wantBool(loc string, val any) bool {
	_, ok := val.(bool)
	if !ok {
		v.addf(loc, "expected boolean, got %s", jsonKind(val))
	}
	return ok
}

func jsonKind(val any) string {
	switch val.(type) {
	case string:
		return "string"
	case json.Number:
		return "number"
	case bool:
		return "boolean"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	case nil:
		return "null"
	}
	return "value"
}

// literal reports a failed Literal match, in validator.py:191-197's shape with
// pydantic's message for a Literal violation.
func (v *validator) literal(loc string, want []string, val any) {
	if val == nil {
		val = "None"
	}
	v.addf(loc, "Input should be %s, got '%v'", quoteJoin(want), val)
}

func join(loc, field string) string {
	if loc == "" {
		return field
	}
	return loc + "." + field
}

func sortedKeys(obj map[string]any) []string {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func quoteJoin(ss []string) string {
	quoted := make([]string, len(ss))
	for i, s := range ss {
		quoted[i] = strconv.Quote(s)
	}
	if len(quoted) > 1 {
		return strings.Join(quoted[:len(quoted)-1], ", ") + " or " + quoted[len(quoted)-1]
	}
	return strings.Join(quoted, ", ")
}

// schemaVersions is m-trajectory.py:15-28.
var schemaVersions = []string{
	"ATIF-v1.0", "ATIF-v1.1", "ATIF-v1.2", "ATIF-v1.3", "ATIF-v1.4",
	"ATIF-v1.5", "ATIF-v1.6", "ATIF-v1.7", "ATIF-v1.8",
}

// stepSources is m-step.py:26.
var stepSources = []string{"system", "user", "agent"}

var trajectoryKeys = []string{"schema_version", "session_id", "trajectory_id", "agent", "steps",
	"notes", "final_metrics", "continued_trajectory_ref", "extra", "subagent_trajectories"}

var agentKeys = []string{"name", "version", "model_name", "tool_definitions", "extra"}

var stepKeys = []string{"step_id", "timestamp", "source", "model_name", "reasoning_effort",
	"reasoning_content", "message", "tool_calls", "observation", "metrics",
	"is_copied_context", "llm_call_count", "extra"}

var toolCallKeys = []string{"tool_call_id", "function_name", "arguments", "extra"}

var observationKeys = []string{"results"}

var resultKeys = []string{"source_call_id", "content", "subagent_trajectory_ref", "extra"}

var metricsKeys = []string{"prompt_tokens", "completion_tokens", "cached_tokens", "cost_usd",
	"prompt_token_ids", "completion_token_ids", "logprobs", "extra"}

var finalMetricsKeys = []string{"total_prompt_tokens", "total_completion_tokens",
	"total_cached_tokens", "total_cost_usd", "total_steps", "extra"}

// trajectory validates the root object (m-trajectory.py:12-130, atif.md
// Root-Level Metadata 88-99).
func (v *validator) trajectory(doc any) {
	obj, ok := doc.(map[string]any)
	if !ok {
		return
	}
	v.trajectoryHead(obj)
	steps, ok := obj["steps"]
	if !ok || steps == nil {
		v.missing("steps") // m-trajectory.py:66-70
	} else if list, ok := v.wantArray("steps", steps); ok {
		var c stepsCheck
		for _, s := range list {
			c.add(s)
		}
		v.errs = append(v.errs, c.errs()...)
	}
	v.trajectoryTail(obj)
}

func (v *validator) trajectoryHead(obj map[string]any) {
	v.extraForbid("", obj, trajectoryKeys)

	// schema_version defaults to ATIF-v1.8 when absent, but a present null is
	// not a Literal member (m-trajectory.py:15-28).
	if sv, ok := obj["schema_version"]; ok {
		s, isStr := sv.(string)
		if !isStr || !slices.Contains(schemaVersions, s) {
			v.literal("schema_version", schemaVersions, sv)
		}
	}

	if s, ok := obj["session_id"]; ok && s != nil {
		v.wantString("session_id", s)
	}
	if s, ok := obj["trajectory_id"]; ok && s != nil {
		v.wantString("trajectory_id", s)
	}
	if s, ok := obj["notes"]; ok && s != nil {
		v.wantString("notes", s)
	}
	if s, ok := obj["continued_trajectory_ref"]; ok && s != nil {
		v.wantString("continued_trajectory_ref", s)
	}
	if e, ok := obj["extra"]; ok && e != nil {
		v.wantObject("extra", e)
	}

	agent, ok := obj["agent"]
	if !ok || agent == nil {
		v.missing("agent") // m-trajectory.py:62-65
	} else {
		v.agent(join("", "agent"), agent)
	}
}

func (v *validator) trajectoryTail(obj map[string]any) {
	if fm, ok := obj["final_metrics"]; ok && fm != nil {
		v.finalMetrics("final_metrics", fm)
	}

	if subs, ok := obj["subagent_trajectories"]; ok && subs != nil {
		v.embeddedSubagents("subagent_trajectories", subs)
	}
}

type stepsCheck struct {
	own, seq, refs validator
	n              int
}

func (c *stepsCheck) add(step any) {
	c.own.step(strconv.Itoa(c.n), step)
	c.seq.stepIDSequential(c.n, step)
	c.refs.toolCallReferences(c.n, step)
	c.n++
}

func (c *stepsCheck) errs() []string {
	if c.n == 0 {
		return []string{"trajectory.steps: List should have at least 1 item after validation, not 0"} // m-trajectory.py:68
	}
	return slices.Concat(c.own.errs, c.seq.errs, c.refs.errs)
}

// agent validates the agent object (m-agent.py:8-32, atif.md AgentSchema
// 101-111).
func (v *validator) agent(loc string, doc any) {
	obj, ok := v.wantObject(loc, doc)
	if !ok {
		return
	}
	v.extraForbid(loc, obj, agentKeys)
	for _, req := range []string{"name", "version"} {
		if val, ok := obj[req]; !ok || val == nil {
			v.missing(join(loc, req)) // m-agent.py:11-18
		} else {
			v.wantString(join(loc, req), val)
		}
	}
	if m, ok := obj["model_name"]; ok && m != nil {
		v.wantString(join(loc, "model_name"), m)
	}
	if td, ok := obj["tool_definitions"]; ok && td != nil {
		if list, ok := v.wantArray(join(loc, "tool_definitions"), td); ok {
			for i, d := range list {
				v.wantObject(join(loc, "tool_definitions."+strconv.Itoa(i)), d) // m-agent.py:23-26
			}
		}
	}
	if e, ok := obj["extra"]; ok && e != nil {
		v.wantObject(join(loc, "extra"), e)
	}
}

// step validates one step (m-step.py:14-139, atif.md StepObject 130-148).
func (v *validator) step(loc string, doc any) {
	obj, ok := v.wantObject(join("steps", loc), doc)
	if !ok {
		return
	}
	where := join("steps", loc)
	v.extraForbid(where, obj, stepKeys)

	sid, ok := obj["step_id"]
	if !ok || sid == nil {
		v.missing(where + ".step_id") // m-step.py:17-21
	} else if i, ok := v.wantInt(where+".step_id", sid); ok && i < 1 {
		v.addf(where+".step_id", "Input should be greater than or equal to 1, got %d", i)
	}

	if ts, ok := obj["timestamp"]; ok && ts != nil {
		v.isoTimestamp(where+".timestamp", ts) // m-step.py:93-102
	}

	var source string
	hasSource := false
	if val, present := obj["source"]; !present || val == nil {
		v.missing(where + ".source") // m-step.py:26-29
	} else if v.wantString(where+".source", val) {
		source, _ = val.(string)
		hasSource = slices.Contains(stepSources, source)
		if !hasSource {
			v.literal(where+".source", stepSources, val) // m-step.py:26
		}
	}

	if val, ok := obj["model_name"]; ok && val != nil {
		v.wantString(where+".model_name", val)
	}
	if val, ok := obj["reasoning_effort"]; ok && val != nil {
		switch val.(type) {
		case string, json.Number: // m-step.py:37-40
		default:
			v.addf(where+".reasoning_effort", "expected string or number, got %s", jsonKind(val))
		}
	}
	if val, ok := obj["reasoning_content"]; ok && val != nil {
		v.wantString(where+".reasoning_content", val)
	}

	// message is required, string or content-part array (m-step.py:41-47).
	if val, ok := obj["message"]; !ok || val == nil {
		v.missing(where + ".message")
	} else {
		v.contentValue(where+".message", val)
	}

	if tcs, ok := obj["tool_calls"]; ok && tcs != nil {
		if list, ok := v.wantArray(where+".tool_calls", tcs); ok {
			for i, tc := range list {
				v.toolCall(where+".tool_calls."+strconv.Itoa(i), tc) // m-tool_call.py:8-31
			}
		}
	}

	if obs, ok := obj["observation"]; ok && obs != nil {
		v.observation(where+".observation", obs)
	}

	if m, ok := obj["metrics"]; ok && m != nil {
		v.metrics(where+".metrics", m) // m-metrics.py:8-44
	}

	if b, ok := obj["is_copied_context"]; ok && b != nil {
		v.wantBool(where+".is_copied_context", b) // m-step.py:64-73
	}

	var llmCount int64
	haveLLMCount := false
	if n, ok := obj["llm_call_count"]; ok && n != nil {
		if i, ok := v.wantInt(where+".llm_call_count", n); ok {
			llmCount, haveLLMCount = i, true
			if i < 0 {
				v.addf(where+".llm_call_count", "Input should be greater than or equal to 0, got %d", i) // m-step.py:74-85
			}
		}
	}

	if e, ok := obj["extra"]; ok && e != nil {
		v.wantObject(where+".extra", e)
	}

	// Fields the models reserve for agent steps (m-step.py:104-121, atif.md 135-141).
	if hasSource && source != "agent" {
		for _, f := range []string{"model_name", "reasoning_effort", "reasoning_content", "tool_calls", "metrics"} {
			if val, ok := obj[f]; ok && val != nil {
				v.addf(where+"."+f, "Field '%s' is only applicable when source is 'agent', but source is '%s'", f, source)
			}
		}
	}
	// llm_call_count == 0 means a deterministic dispatch, whose LLM-specific
	// fields must be absent (m-step.py:123-139).
	if haveLLMCount && llmCount == 0 && hasSource && source == "agent" {
		for _, f := range []string{"metrics", "reasoning_content"} {
			if val, ok := obj[f]; ok && val != nil {
				v.addf(where+"."+f, "Field '%s' must be absent when llm_call_count is 0 (deterministic dispatch on a 'source: agent' step)", f)
			}
		}
	}
}

// stepIDSequential ports the Trajectory.validate_step_ids model validator
// for the step at index i: step_id counts from 1 without gaps
// (m-trajectory.py:120-130).
func (v *validator) stepIDSequential(i int, s any) {
	obj, ok := s.(map[string]any)
	if !ok {
		return
	}
	got, ok := obj["step_id"].(json.Number)
	if !ok {
		return
	}
	want := strconv.Itoa(i + 1)
	if got.String() != want {
		v.addf("steps."+strconv.Itoa(i)+".step_id",
			"expected %s (sequential from 1), got %s", want, got.String())
	}
}

// toolCallReferences ports Trajectory.validate_tool_call_references for the
// step at index i: every result's source_call_id must name a tool_call_id of
// the same step (m-trajectory.py:164-185).
func (v *validator) toolCallReferences(i int, s any) {
	obj, ok := s.(map[string]any)
	if !ok {
		return
	}
	where := "steps." + strconv.Itoa(i)
	obs, ok := obj["observation"].(map[string]any)
	if !ok {
		return
	}
	results, ok := obs["results"].([]any)
	if !ok {
		return
	}
	ids := map[string]bool{}
	if tcs, ok := obj["tool_calls"].([]any); ok {
		for _, tc := range tcs {
			if m, ok := tc.(map[string]any); ok {
				if id, ok := m["tool_call_id"].(string); ok {
					ids[id] = true
				}
			}
		}
	}
	for j, r := range results {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		if id, ok := m["source_call_id"].(string); ok && !ids[id] {
			stepNo := 0
			if n, ok := obj["step_id"].(json.Number); ok {
				stepNo, _ = strconv.Atoi(n.String())
			}
			v.addf(where+".observation.results."+strconv.Itoa(j)+".source_call_id",
				"Observation result references source_call_id '%s' which is not found in step %d's tool_calls",
				id, stepNo)
		}
	}
}

// toolCall validates one tool call (m-tool_call.py:8-31, atif.md
// ToolCallSchema 154-159).
func (v *validator) toolCall(loc string, doc any) {
	obj, ok := v.wantObject(loc, doc)
	if !ok {
		return
	}
	v.extraForbid(loc, obj, toolCallKeys)
	for _, req := range []string{"tool_call_id", "function_name"} {
		if val, ok := obj[req]; !ok || val == nil {
			v.missing(join(loc, req))
		} else {
			v.wantString(join(loc, req), val)
		}
	}
	if args, ok := obj["arguments"]; !ok || args == nil {
		v.missing(loc + ".arguments")
	} else {
		v.wantObject(loc+".arguments", args)
	}
	if e, ok := obj["extra"]; ok && e != nil {
		v.wantObject(loc+".extra", e)
	}
}

// observation validates the observation object; results is required
// (atif.md ObservationSchema 226-230).
func (v *validator) observation(loc string, doc any) {
	obj, ok := v.wantObject(loc, doc)
	if !ok {
		return
	}
	v.extraForbid(loc, obj, observationKeys)
	results, ok := obj["results"]
	if !ok || results == nil {
		v.missing(loc + ".results")
		return
	}
	list, ok := v.wantArray(loc+".results", results)
	if !ok {
		return
	}
	for i, r := range list {
		v.result(loc+".results."+strconv.Itoa(i), r) // m-observation_result.py:11-42
	}
}

// result validates one observation result (m-observation_result.py:11-42,
// atif.md ObservationResultSchema 232-238).
func (v *validator) result(loc string, doc any) {
	obj, ok := v.wantObject(loc, doc)
	if !ok {
		return
	}
	v.extraForbid(loc, obj, resultKeys)
	if id, ok := obj["source_call_id"]; ok && id != nil {
		v.wantString(loc+".source_call_id", id)
	}
	if c, ok := obj["content"]; ok && c != nil {
		v.contentValue(loc+".content", c)
	}
	if refs, ok := obj["subagent_trajectory_ref"]; ok && refs != nil {
		v.subagentRefs(loc+".subagent_trajectory_ref", refs)
	}
	if e, ok := obj["extra"]; ok && e != nil {
		v.wantObject(loc+".extra", e)
	}
}

// metrics validates the metrics object (m-metrics.py:8-44, atif.md
// MetricsSchema 165-174).
func (v *validator) metrics(loc string, doc any) {
	obj, ok := v.wantObject(loc, doc)
	if !ok {
		return
	}
	v.extraForbid(loc, obj, metricsKeys)
	for _, f := range []string{"prompt_tokens", "completion_tokens", "cached_tokens"} {
		if val, ok := obj[f]; ok && val != nil {
			v.wantInt(join(loc, f), val) // m-metrics.py:11-22
		}
	}
	if val, ok := obj["cost_usd"]; ok && val != nil {
		v.wantNumber(loc+".cost_usd", val) // m-metrics.py:23-26
	}
	for _, f := range []string{"prompt_token_ids", "completion_token_ids"} {
		if val, ok := obj[f]; ok && val != nil {
			if list, ok := v.wantArray(join(loc, f), val); ok { // m-metrics.py:27-34
				for i, n := range list {
					v.wantInt(join(loc, f)+"."+strconv.Itoa(i), n)
				}
			}
		}
	}
	if val, ok := obj["logprobs"]; ok && val != nil {
		if list, ok := v.wantArray(loc+".logprobs", val); ok { // m-metrics.py:35-38
			for i, n := range list {
				v.wantNumber(loc+".logprobs."+strconv.Itoa(i), n)
			}
		}
	}
	if e, ok := obj["extra"]; ok && e != nil {
		v.wantObject(loc+".extra", e)
	}
}

// finalMetrics validates the summary object (m-trajectory.py:8, atif.md
// FinalMetricsSchema 113-124).
func (v *validator) finalMetrics(loc string, doc any) {
	obj, ok := v.wantObject(loc, doc)
	if !ok {
		return
	}
	v.extraForbid(loc, obj, finalMetricsKeys)
	for _, f := range []string{"total_prompt_tokens", "total_completion_tokens", "total_cached_tokens", "total_steps"} {
		if val, ok := obj[f]; ok && val != nil {
			v.wantInt(join(loc, f), val) // atif.md:119-123
		}
	}
	if val, ok := obj["total_cost_usd"]; ok && val != nil {
		v.wantNumber(loc+".total_cost_usd", val) // atif.md:122
	}
	if e, ok := obj["extra"]; ok && e != nil {
		v.wantObject(loc+".extra", e)
	}
}

// embeddedSubagents validates the subagent_trajectories array: each element is
// a complete trajectory that must carry a trajectory_id, unique within the
// array (m-trajectory.py:132-162).
func (v *validator) embeddedSubagents(loc string, doc any) {
	list, ok := v.wantArray(loc, doc)
	if !ok {
		return
	}
	seen := map[string]bool{}
	for i, s := range list {
		where := loc + "." + strconv.Itoa(i)
		obj, ok := s.(map[string]any)
		if !ok {
			v.wantObject(where, s)
			continue
		}
		// Only a missing or null trajectory_id is "required"; an empty string
		// is present (m-trajectory.py:146-148).
		if tid, ok := obj["trajectory_id"].(string); ok {
			if seen[tid] {
				v.addf(where+".trajectory_id", "trajectory_id '%s' is not unique within subagent_trajectories", tid)
			}
			seen[tid] = true
		} else if obj["trajectory_id"] == nil {
			agent, _ := obj["agent"].(map[string]any)
			name, _ := agent["name"].(string)
			v.addf(where+".trajectory_id", "trajectory_id is required for embedded subagents (agent.name=%q)", name)
		}
		v.trajectory(s)
	}
}

// subagentRefs validates a result's SubagentTrajectoryRef array: each ref
// must be resolvable through trajectory_id or trajectory_path (atif.md
// SubagentTrajectoryRefSchema 355-370).
func (v *validator) subagentRefs(loc string, doc any) {
	list, ok := v.wantArray(loc, doc)
	if !ok {
		return
	}
	for i, r := range list {
		where := loc + "." + strconv.Itoa(i)
		obj, ok := v.wantObject(where, r)
		if !ok {
			continue
		}
		v.extraForbid(where, obj, []string{"trajectory_id", "trajectory_path", "session_id", "extra"})
		hasID := false
		if id, ok := obj["trajectory_id"]; ok && id != nil {
			hasID = v.wantString(where+".trajectory_id", id)
		}
		if p, ok := obj["trajectory_path"]; ok && p != nil {
			if v.wantString(where+".trajectory_path", p) {
				hasID = true
			}
		}
		if !hasID {
			v.addf(where, "A ref MUST set at least one of 'trajectory_id' or 'trajectory_path'")
		}
		if s, ok := obj["session_id"]; ok && s != nil {
			v.wantString(where+".session_id", s)
		}
		if e, ok := obj["extra"]; ok && e != nil {
			v.wantObject(where+".extra", e)
		}
	}
}

// contentValue validates a string-or-content-part field: message
// (m-step.py:41-47) and result content (m-observation_result.py:23-29).
func (v *validator) contentValue(loc string, val any) {
	switch c := val.(type) {
	case string:
		return
	case []any:
		for i, part := range c {
			v.contentPart(loc+"."+strconv.Itoa(i), part) // atif.md ContentPartSchema 271-277
		}
	default:
		v.addf(loc, "expected string or array, got %s", jsonKind(val))
	}
}

// contentPart validates one ContentPart: type is one of text, image, audio;
// text parts carry text and nothing else; media parts carry a source whose
// kind agrees with type (atif.md:271-277, 316-317).
func (v *validator) contentPart(loc string, doc any) {
	obj, ok := v.wantObject(loc, doc)
	if !ok {
		return
	}
	v.extraForbid(loc, obj, []string{"type", "text", "source"})
	typ, _ := obj["type"].(string)
	switch typ {
	case "text":
		if t, ok := obj["text"]; !ok || t == nil {
			v.missing(loc + ".text")
		} else {
			v.wantString(loc+".text", t)
		}
		if s, ok := obj["source"]; ok && s != nil {
			v.addf(loc+".source", "Field 'source' must be omitted when type is 'text'")
		}
	case "image", "audio":
		if _, ok := obj["text"]; ok {
			v.addf(loc+".text", "Field 'text' must be omitted when type is '%s'", typ)
		}
		src, ok := v.wantObject(loc+".source", obj["source"])
		if !ok {
			return
		}
		v.extraForbid(loc+".source", src, []string{"media_type", "path", "duration_sec"})
		mt, _ := src["media_type"].(string)
		if mt == "" {
			v.missing(loc + ".source.media_type") // atif.md:316
		} else if want := typ + "/"; !strings.HasPrefix(mt, want) {
			v.addf(loc+".source.media_type", "consumers MUST reject a source whose kind disagrees with type: %q is not %s*", mt, want)
		}
		if p, ok := src["path"]; !ok || p == nil || p == "" {
			v.missing(loc + ".source.path") // atif.md:317
		} else {
			v.wantString(loc+".source.path", p)
		}
		if d, ok := src["duration_sec"]; ok && d != nil && typ == "audio" {
			v.wantNumber(loc+".source.duration_sec", d)
		}
	default:
		v.literal(loc+".type", []string{"text", "image", "audio"}, obj["type"])
	}
}

// isoTimestamp ports Step.validate_timestamp: an ISO 8601 string
// (m-step.py:93-102).
func (v *validator) isoTimestamp(loc string, val any) {
	s, ok := val.(string)
	if !ok {
		v.addf(loc, "Invalid ISO 8601 timestamp: expected string, got %s", jsonKind(val))
		return
	}
	layouts := []string{
		"2006-01-02T15:04:05.999999999-07:00", "2006-01-02T15:04:05.999999999",
		"2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999",
		"2006-01-02", "2006-01-02T15:04Z07:00",
	}
	s = strings.Replace(s, "Z", "+00:00", 1)
	for _, layout := range layouts {
		if _, err := time.Parse(layout, s); err == nil {
			return
		}
	}
	v.addf(loc, "Invalid ISO 8601 timestamp: %s", val)
}
