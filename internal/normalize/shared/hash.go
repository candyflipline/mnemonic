package shared

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"

	"github.com/candyflipline/mnemonic/internal/schema"
)

func EventUID(sourceFile string, sourceLineNo, sourceOffset uint64, recordFingerprint, suffix string) string {
	material := fmt.Sprintf("%s|%d|%d|%d|%s|%s",
		sourceFile, wholeFile, sourceLineNo, sourceOffset, recordFingerprint, suffix)
	sum := sha256.Sum256([]byte(material))
	return hex.EncodeToString(sum[:])
}

// RawHash is the first 64 bits of the SHA-256 of s, big-endian.
func RawHash(s string) uint64 {
	sum := sha256.Sum256([]byte(s))
	return mustU64(hex.EncodeToString(sum[:8]))
}

func mustU64(hexPrefix string) uint64 {
	v, err := strconv.ParseUint(hexPrefix, 16, 64)
	if err != nil {
		return 0
	}
	return v
}

func IOHash(inputJSON, outputJSON string) uint64 {
	return RawHash(inputJSON + "\n" + outputJSON)
}

const (
	eventIdentityDomain     = "moraine:event:v2"
	eventOccurrenceDomain   = "moraine:event:occurrence:v1"
	semanticOccurrenceField = "moraine_semantic_occurrence"
)

var eventIdentityFields = []string{
	"author",
	"harness",
	"inference_provider",
	"session_id",
	"event_kind",
	"actor_kind",
	"payload_type",
	"op_kind",
	"op_status",
	"request_id",
	"trace_id",
	"turn_index",
	"item_id",
	"tool_call_id",
	"parent_tool_call_id",
	"origin_tool_call_id",
	"tool_name",
	"tool_phase",
	"tool_error",
	"agent_run_id",
	"agent_label",
	"coord_group_id",
	"coord_group_label",
	"is_substream",
	"model",
	"endpoint_kind",
	"input_tokens",
	"output_tokens",
	"cache_read_tokens",
	"cache_write_tokens",
	"latency_ms",
	"retry_count",
	"service_tier",
	"content_types",
	"has_reasoning",
	"text_content",
}

var toolIdentityFields = []string{
	"tool_call_id",
	"parent_tool_call_id",
	"tool_name",
	"tool_phase",
	"tool_error",
	"input_json",
	"output_json",
	"output_text",
}

var semanticPayloadExcludedFields = []string{
	"createdAt",
	"created_at",
	"cwd",
	"directory",
	"lastUpdatedAt",
	"last_updated",
	"moraine_tool_io",
	"moraine_emission_index",
	"moraine_semantic_occurrence",
	"project_id",
	"repo_rel_path",
	"request_event_uid",
	"session_start",
	"source_ref",
	"time_created",
	"timestamp",
	"updated_at",
	"workspacePath",
	"worktree_root",
}

// FoldedIdentity carries the pre-fold semantic payload of an event whose
// payload_json grew a moraine_tool_io object, plus the tool identity fields
// hashed alongside it.
type FoldedIdentity struct {
	SemanticPayload CanonicalJSON
	ToolFields      []string
	Occurrence      uint32
}

func SemanticEventUID(event *schema.Event, payload CanonicalJSON, folded *FoldedIdentity) string {
	semantic, toolFields, occurrence := payload, []string(nil), uint32(0)
	if folded != nil {
		semantic, toolFields, occurrence = folded.SemanticPayload, folded.ToolFields, folded.Occurrence
	} else {
		semantic, toolFields, occurrence = semanticPayloadParts(payload)
	}
	m := make([]byte, 0, 1024+len(semantic.s)+len(event.TextContent))
	m = appendIdentityComponent(m, eventIdentityDomain)
	for _, field := range eventIdentityFields {
		m = appendIdentityComponent(m, identityField(event, field))
	}
	m = appendNumericMapIdentity(m, bucketMapStrings(event.TokenUsageBuckets))
	m = appendNumericMapIdentity(m, nativeMapStrings(event.TokenUsageNativeUnits))
	m = appendIdentityComponent(m, semantic.s)
	for _, field := range toolFields {
		m = appendIdentityComponent(m, field)
	}
	return withOccurrence(sha256.Sum256(m), occurrence)
}

func withOccurrence(base [sha256.Size]byte, occurrence uint32) string {
	if occurrence == 0 {
		return hex.EncodeToString(base[:])
	}
	var m []byte
	m = appendIdentityComponent(m, eventOccurrenceDomain)
	m = appendIdentityComponent(m, hex.EncodeToString(base[:]))
	m = appendIdentityComponent(m, strconv.FormatUint(uint64(occurrence), 10))
	sum := sha256.Sum256(m)
	return hex.EncodeToString(sum[:])
}

func identityField(event *schema.Event, field string) string {
	switch field {
	case "author":
		return ""
	case "harness":
		return event.Harness
	case "inference_provider":
		return event.InferenceProvider
	case "session_id":
		return event.SessionID
	case "event_kind":
		return event.EventKind
	case "actor_kind":
		return event.ActorKind
	case "payload_type":
		return event.PayloadType
	case "op_kind":
		return event.OpKind
	case "op_status":
		return event.OpStatus
	case "request_id":
		return event.RequestID
	case "trace_id":
		return event.TraceID
	case "turn_index":
		return strconv.FormatUint(uint64(event.TurnIndex), 10)
	case "item_id":
		return event.ItemID
	case "tool_call_id":
		return event.ToolCallID
	case "parent_tool_call_id":
		return event.ParentToolCallID
	case "origin_tool_call_id":
		return event.OriginToolCallID
	case "tool_name":
		return event.ToolName
	case "tool_phase":
		return event.ToolPhase
	case "tool_error":
		return strconv.FormatUint(uint64(event.ToolError), 10)
	case "agent_run_id":
		return event.AgentRunID
	case "agent_label":
		return event.AgentLabel
	case "coord_group_id":
		return event.CoordGroupID
	case "coord_group_label":
		return event.CoordGroupLabel
	case "is_substream":
		return strconv.FormatUint(uint64(event.IsSubstream), 10)
	case "model":
		return event.Model
	case "endpoint_kind":
		return event.EndpointKind
	case "input_tokens":
		return strconv.FormatUint(uint64(event.InputTokens), 10)
	case "output_tokens":
		return strconv.FormatUint(uint64(event.OutputTokens), 10)
	case "cache_read_tokens":
		return strconv.FormatUint(uint64(event.CacheReadTokens), 10)
	case "cache_write_tokens":
		return strconv.FormatUint(uint64(event.CacheWriteTokens), 10)
	case "latency_ms":
		return strconv.FormatUint(uint64(event.LatencyMs), 10)
	case "retry_count":
		return strconv.FormatUint(uint64(event.RetryCount), 10)
	case "service_tier":
		return event.ServiceTier
	case "content_types":
		return compactStrings(event.ContentTypes)
	case "has_reasoning":
		return strconv.FormatUint(uint64(event.HasReasoning), 10)
	case "text_content":
		return event.TextContent
	default:
		return ""
	}
}

// compactStrings encodes a string array the way serde_json prints one.
func compactStrings(values []string) string {
	b := []byte{'['}
	for i, v := range values {
		if i > 0 {
			b = append(b, ',')
		}
		b = appendJSONString(b, v)
	}
	return string(append(b, ']'))
}

func appendNumericMapIdentity(dst []byte, encoded [][2]string) []byte {
	var comp []byte
	for _, kv := range encoded {
		comp = appendIdentityComponent(comp, kv[0])
		comp = appendIdentityComponent(comp, kv[1])
	}
	dst = strconv.AppendInt(dst, int64(len(comp)), 10)
	dst = append(dst, ':')
	return append(dst, comp...)
}

func bucketMapStrings(m map[string]uint64) [][2]string {
	keys := sortedKeysLen(len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([][2]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, [2]string{k, strconv.FormatUint(m[k], 10)})
	}
	return out
}

func nativeMapStrings(m map[string]float64) [][2]string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([][2]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, [2]string{k, RustFloatDisplay(m[k])})
	}
	return out
}

func sortedKeysLen(n int) []string { return make([]string, 0, n) }

func appendIdentityComponent(dst []byte, value string) []byte {
	dst = strconv.AppendInt(dst, int64(len(value)), 10)
	dst = append(dst, ':')
	return append(dst, value...)
}

// semanticPayloadParts strips derived payload fields and recovers the folded
// tool identity from canonical payload bytes, like semantic_payload_parts.
// The bytes are CanonicalJSON, so they are valid UTF-8, the keys are sorted,
// and a moraine_tool_io member re-decodes; the decode-and-recompact fallback
// the untyped version needed is gone.
func semanticPayloadParts(payload CanonicalJSON) (CanonicalJSON, []string, uint32) {
	emptyFields := make([]string, len(toolIdentityFields))
	s := payload.s
	members, ok := objectMembers(s)
	if !ok {
		// A non-object payload (Compact of a scalar or array) hashes as
		// itself, which is what the fallback's re-Compact produced.
		return payload, emptyFields, 0
	}
	toolFields := make([]string, len(toolIdentityFields))
	var occurrence uint32
	dropped := 0
	for _, m := range members {
		if !semanticPayloadDropped[m.key] {
			continue
		}
		dropped++
		switch m.key {
		case semanticOccurrenceField:
			if n, ok := AsNumber(json.Number(m.value)); ok && n <= math.MaxUint32 {
				occurrence = uint32(n)
			}
		case "moraine_tool_io":
			tool, _ := DecodeJSON([]byte(m.value))
			if toolObj, isObj := AsObject(tool); isObj {
				for i, field := range toolIdentityFields {
					toolFields[i] = encodeIdentityValue(toolObj[field])
				}
			}
		}
	}
	if dropped == 0 {
		return payload, toolFields, 0
	}
	return CanonicalJSON{s: joinMembers(s, members, semanticPayloadDropped, "", "")}, toolFields, occurrence
}

// SanitizeSemanticPayload drops the derived fields the canonical uid ignores.
func SanitizeSemanticPayload(obj map[string]any) {
	for _, field := range semanticPayloadExcludedFields {
		delete(obj, field)
	}
}

func AsNumber(v JSON) (uint64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	u, err := strconv.ParseUint(string(n), 10, 64)
	if err != nil {
		return 0, false
	}
	return u, true
}

// Strings raw, everything else compact JSON, absent empty.
func encodeIdentityValue(v JSON) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		return Compact(t)
	}
}

var semanticPayloadDropped = func() map[string]bool {
	m := map[string]bool{"moraine_tool_io": true}
	for _, f := range semanticPayloadExcludedFields {
		m[f] = true
	}
	return m
}()
