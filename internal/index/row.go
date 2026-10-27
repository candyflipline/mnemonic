package index

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"strings"

	"github.com/candyflipline/mnemonic/internal/schema"
)

// Row is the light part of one parsed event: every field a tool reads except
// payload_json and token_usage_json, which live in the heavy log.
type Row struct {
	UID, Session, SourceName, Harness, InfProv, Author      string
	SourceFile, SourceRef                                   string
	EventKind, ActorKind, PayloadType, ToolPhase, Phase     string
	ItemID, ToolCallID, ToolName, EndpointKind              string
	Text, TextPreview                                       string
	ProjectID, RepoRelPath, WorktreeRoot, Cwd               string
	Buckets, Native                                         string
	TioInput, TioInPrev, TioOutPrev, TioProj, TioRel, TioWT string
	RequestID, TraceID, ParentToolCallID                    string
	AgentRunID, AgentLabel, Model                           string
	OriginEventID, OpKind, OpStatus                         string
	TimeUs                                                  int64
	TsMs                                                    int64
	Gen                                                     uint64
	LineNo, Offset, Version                                 uint64
	Emission, TurnIndex                                     uint64
	IsSubstream, ToolError, LatencyMs                       uint64
	InputTokens, OutputTokens                               uint64
	CacheReadTokens, CacheWriteTokens                       uint64
	Bits                                                    uint64
	HeavyFrame                                              uint64
	HeavyIdx                                                uint64
}

const (
	bitTimeOK = 1 << iota
	bitTio
	bitMeta
)

func (r *Row) strs() []*string {
	return []*string{&r.UID, &r.Session, &r.SourceName, &r.Harness, &r.InfProv,
		&r.SourceFile, &r.SourceRef, &r.EventKind, &r.ActorKind, &r.PayloadType, &r.ToolPhase, &r.Phase,
		&r.ItemID, &r.ToolCallID, &r.ToolName, &r.EndpointKind, &r.Text, &r.TextPreview,
		&r.ProjectID, &r.RepoRelPath, &r.WorktreeRoot, &r.Cwd, &r.Buckets, &r.Native,
		&r.TioInput, &r.TioInPrev, &r.TioOutPrev, &r.TioProj, &r.TioRel, &r.TioWT,
		&r.RequestID, &r.TraceID, &r.ParentToolCallID, &r.AgentRunID, &r.AgentLabel, &r.Model,
		&r.OriginEventID, &r.OpKind, &r.OpStatus}
}

func (r *Row) ints() []*uint64 {
	return []*uint64{&r.Gen, &r.LineNo, &r.Offset, &r.Version, &r.Emission, &r.TurnIndex,
		&r.IsSubstream, &r.ToolError, &r.LatencyMs, &r.InputTokens, &r.OutputTokens,
		&r.CacheReadTokens, &r.CacheWriteTokens, &r.Bits, &r.HeavyFrame, &r.HeavyIdx}
}

func (r *Row) append(b []byte) []byte {
	for _, s := range r.strs() {
		b = binary.AppendUvarint(b, uint64(len(*s)))
		b = append(b, *s...)
	}
	b = binary.AppendVarint(b, r.TimeUs)
	b = binary.AppendVarint(b, r.TsMs)
	for _, n := range r.ints() {
		b = binary.AppendUvarint(b, *n)
	}
	return b
}

var errShort = errors.New("short row")

func (r *Row) decode(b []byte) ([]byte, error) {
	for _, s := range r.strs() {
		n, k := binary.Uvarint(b)
		if k <= 0 || uint64(len(b)-k) < n {
			return nil, errShort
		}
		*s = string(b[k : k+int(n)])
		b = b[k+int(n):]
	}
	v, k := binary.Varint(b)
	if k <= 0 {
		return nil, errShort
	}
	r.TimeUs, b = v, b[k:]
	if v, k = binary.Varint(b); k <= 0 {
		return nil, errShort
	}
	r.TsMs, b = v, b[k:]
	for _, n := range r.ints() {
		v, k := binary.Uvarint(b)
		if k <= 0 {
			return nil, errShort
		}
		*n, b = v, b[k:]
	}
	return b, nil
}

func rowFrom(ev *schema.Event) Row {
	r := Row{
		UID: ev.EventUID, Session: ev.SessionID, SourceName: ev.SourceName, Harness: ev.Harness,
		InfProv: ev.InferenceProvider, SourceFile: ev.SourceFile, SourceRef: ev.SourceRef,
		EventKind: ev.EventKind, ActorKind: ev.ActorKind, PayloadType: ev.PayloadType,
		ToolPhase: ev.ToolPhase, Phase: ev.ToolPhase, ItemID: ev.ItemID, ToolCallID: ev.ToolCallID,
		ToolName: ev.ToolName, EndpointKind: ev.EndpointKind, Text: ev.TextContent, TextPreview: ev.TextPreview,
		ProjectID: ev.ProjectID, RepoRelPath: ev.RepoRelPath, WorktreeRoot: ev.WorktreeRoot, Cwd: ev.Cwd,
		RequestID: ev.RequestID, TraceID: ev.TraceID, ParentToolCallID: ev.ParentToolCallID,
		AgentRunID: ev.AgentRunID, AgentLabel: ev.AgentLabel, Model: ev.Model,
		OriginEventID: ev.OriginEventID, OpKind: ev.OpKind, OpStatus: ev.OpStatus,
		Gen: uint64(ev.SourceGeneration), LineNo: ev.SourceLineNo, Offset: ev.SourceOffset,
		Version: ev.EventVersion, TurnIndex: uint64(ev.TurnIndex),
		IsSubstream: uint64(ev.IsSubstream), ToolError: uint64(ev.ToolError), LatencyMs: uint64(ev.LatencyMs),
		InputTokens: uint64(ev.InputTokens), OutputTokens: uint64(ev.OutputTokens),
		CacheReadTokens: uint64(ev.CacheReadTokens), CacheWriteTokens: uint64(ev.CacheWriteTokens),
	}
	if r.Phase == "" {
		r.Phase = ev.OpStatus
	}
	if ev.TokenUsageBuckets != nil {
		b, _ := json.Marshal(ev.TokenUsageBuckets)
		r.Buckets = string(b)
	}
	if ev.TokenUsageNativeUnits != nil {
		b, _ := json.Marshal(ev.TokenUsageNativeUnits)
		r.Native = string(b)
	}
	if us, ok := parseISOTimestampUs(ev.RecordTS); ok {
		r.TimeUs = us
		r.Bits |= bitTimeOK
	}
	if us, ok := parseISOTimestampUs(ev.EventTS); ok {
		r.TsMs = tsMsOf(us)
	}
	p := ev.PayloadJSON
	if strings.Contains(p, "moraine_emission_index") {
		r.Emission = emissionIndex(p)
	}
	if ev.ToolPhase == "request" && strings.Contains(p, "moraine_tool_io") {
		if tio, ok := toolIO(p); ok {
			r.Bits |= bitTio
			r.TioInput, r.TioInPrev, r.TioOutPrev = tio["input_json"], tio["input_preview"], tio["output_preview"]
			r.TioProj, r.TioRel, r.TioWT = tio["project_id"], tio["repo_rel_path"], tio["worktree_root"]
		}
	}
	if ev.EventKind == "session_meta" || (ev.SourceName == "omp" && strings.Contains(p, "title") &&
		(jsonTopString(p, "type") == "title" || jsonTopString(p, "type") == "title_change")) {
		r.Bits |= bitMeta
	}
	return r
}

func tsMsOf(eventUs int64) int64 { return floorDiv(eventUs, 1000) }

func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

func emissionIndex(p string) uint64 {
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(p), &m) != nil {
		return 0
	}
	v, ok := m["moraine_emission_index"]
	if !ok {
		return 0
	}
	s := strings.TrimSpace(jsonExtractString(v))
	n, ok := parseUintStrict(s)
	if !ok || n > 0xFFFFFFFF {
		return 0
	}
	return n
}

func parseUintStrict(s string) (uint64, bool) {
	s = strings.TrimPrefix(s, "+")
	if s == "" {
		return 0, false
	}
	var n uint64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + uint64(c-'0')
		if n > 1<<40 {
			return 0, false
		}
	}
	return n, true
}

func jsonExtractString(v json.RawMessage) string {
	t := strings.TrimSpace(string(v))
	if strings.HasPrefix(t, `"`) {
		var s string
		if json.Unmarshal(v, &s) == nil {
			return s
		}
	}
	return t
}

func jsonTopString(p, key string) string {
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(p), &m) != nil {
		return ""
	}
	v, ok := m[key]
	if !ok || string(v) == "null" {
		return ""
	}
	return jsonExtractString(v)
}

func toolIO(p string) (map[string]string, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(p), &m) != nil {
		return nil, false
	}
	v, ok := m["moraine_tool_io"]
	if !ok {
		return nil, false
	}
	out := map[string]string{}
	var obj map[string]json.RawMessage
	if json.Unmarshal(v, &obj) == nil {
		for _, k := range []string{"input_json", "input_preview", "output_preview", "project_id", "repo_rel_path", "worktree_root"} {
			if x, ok := obj[k]; ok && string(x) != "null" {
				out[k] = jsonExtractString(x)
			}
		}
	}
	return out, true
}
