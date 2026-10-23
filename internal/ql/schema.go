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
		{"author", TString, false}, {"machine_id", TString, false}, {"ingested_at", TTime, false},
		{"tool_io_input_preview", TString, false}, {"tool_io_output_preview", TString, false},
		{"tool_io_project_id", TString, false}, {"tool_io_repo_rel_path", TString, false},
		{"tool_io_worktree_root", TString, false},
		{"event_time", TTime, false}, {"event_order", TInt, false}, {"turn_seq", TInt, false}, {"event_ordinal", TInt, false},
	}...)},
	{"sessions", []Column{
		{"session_id", TString, false}, {"first_event_time", TTime, false}, {"last_event_time", TTime, false},
		{"total_turns", TInt, false}, {"total_events", TInt, false}, {"tool_calls", TInt, false},
		{"tool_results", TInt, false}, {"user_messages", TInt, false}, {"assistant_messages", TInt, false},
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

var heavyEventColumns = []string{"payload_json", "token_usage_json"}

func eventColumns() []Column {
	out := make([]Column, len(schema.EventColumns))
	for i, c := range schema.EventColumns {
		t, ok := clickHouseTypes[c.ClickHouse]
		if !ok {
			panic(fmt.Sprintf("ql: events column %s has ClickHouse type %s, which clickHouseTypes in internal/ql/schema.go does not map", c.Name, c.ClickHouse))
		}
		out[i] = Column{c.Name, t, slices.Contains(heavyEventColumns, c.Name)}
	}
	return out
}
