package ql

import (
	"fmt"
	"slices"

	"github.com/candyflipline/mnemonic/internal/schema"
)

type Column struct {
	Name  string
	Type  Type
	Heavy bool
}

type Relation struct {
	Name    string
	Columns []Column
}

func (r *Relation) column(name string) (int, bool) {
	i := slices.IndexFunc(r.Columns, func(c Column) bool { return c.Name == name })
	return i, i >= 0
}

type Schema []Relation

func (s Schema) relation(name string) (*Relation, bool) {
	for i := range s {
		if s[i].Name == name {
			return &s[i], true
		}
	}
	return nil, false
}

func (s Schema) names() []string {
	out := make([]string, len(s))
	for i, r := range s {
		out[i] = r.Name
	}
	return out
}

var Relations = Schema{
	{"annotations", []Column{
		{"annotation_id", TString, false}, {"target_session_id", TString, false}, {"target_span_id", TString, false},
		{"name", TString, false}, {"value_number", TFloat, false}, {"value_label", TString, false},
		{"value_text", TString, false}, {"source", TString, false}, {"author", TString, false}, {"ts", TTime, false},
	}},
	{"events", append(eventColumns(), []Column{
		{"author", TString, false}, {"machine_id", TString, false},
		{"tool_io_input_preview", TString, false}, {"tool_io_output_preview", TString, false},
		{"tool_io_project_id", TString, false}, {"tool_io_repo_rel_path", TString, false},
		{"tool_io_worktree_root", TString, false},
		{"event_time", TTime, false}, {"event_order", TInt, false}, {"turn_seq", TInt, false}, {"event_ordinal", TInt, false},
	}...)},
	{"sessions", []Column{
		{"session_id", TString, false}, {"first_event_time", TTime, false}, {"last_event_time", TTime, false},
		{"total_turns", TInt, false}, {"total_events", TInt, false},
		{"completed", TInt, false},
	}},
	{"spans", []Column{
		{"session_id", TString, false}, {"span_id", TString, false}, {"parent_span_id", TString, false},
		{"kind", TString, false}, {"name", TString, false}, {"start_time", TTime, false}, {"end_time", TTime, false},
		{"status", TString, false}, {"model", TString, false}, {"input_tokens", TInt, false},
		{"output_tokens", TInt, false}, {"cache_read_tokens", TInt, false}, {"cache_write_tokens", TInt, false},
		{"cost", TFloat, false}, {"first_event_uid", TString, false}, {"last_event_uid", TString, false},
	}},
	{"turns", []Column{
		{"session_id", TString, false}, {"turn_seq", TInt, false}, {"event_count", TInt, false},
		{"completed", TInt, false}, {"terminal_uid", TString, false},
	}},
}

var clickHouseTypes = map[string]Type{
	"String": TString, "LowCardinality(String)": TString,
	"UInt8": TInt, "UInt16": TInt, "UInt32": TInt, "UInt64": TInt,
	"Date": TTime, "DateTime64(3)": TTime,
	"Array(String)":        TStrings,
	"Map(String, UInt64)":  TIntMap,
	"Map(String, Float64)": TFloatMap,
}

// heavyEventColumns are events columns the engine's light row does not hold:
// the binding reads them from the part's heavy record (payload_json,
// token_usage_json), last and only when a predicate or an output needs them.
var heavyEventColumns = []string{"payload_json", "token_usage_json", "model", "input_tokens",
	"output_tokens", "latency_ms", "request_id", "trace_id"}

// droppedEventColumns are ClickHouse contract columns neither the engine's
// light row nor its heavy record holds: the parsers filled them at parse time
// and the engine does not keep them, so the binding cannot answer them
// without faking. A query naming one fails like any unknown column. The
// engine coalesces op_status into tool_phase (Phase) and derives ingested_at
// as one fixed stamp, so both are dropped rather than served wrong.
var droppedEventColumns = map[string]bool{
	"session_date": true, "record_ts": true, "op_kind": true, "op_status": true,
	"parent_tool_call_id": true, "origin_event_id": true, "origin_tool_call_id": true,
	"tool_error": true, "agent_run_id": true, "agent_label": true,
	"coord_group_id": true, "coord_group_label": true, "is_substream": true,
	"cache_read_tokens": true, "cache_write_tokens": true, "retry_count": true,
	"service_tier": true, "has_reasoning": true, "ingested_at": true,
}

func eventColumns() []Column {
	var out []Column
	for _, c := range schema.EventColumns {
		if droppedEventColumns[c.Name] {
			continue
		}
		t, ok := clickHouseTypes[c.ClickHouse]
		if !ok {
			panic(fmt.Sprintf("ql: events column %s has ClickHouse type %s, which clickHouseTypes in internal/ql/schema.go does not map", c.Name, c.ClickHouse))
		}
		out = append(out, Column{c.Name, t, slices.Contains(heavyEventColumns, c.Name)})
	}
	return out
}
