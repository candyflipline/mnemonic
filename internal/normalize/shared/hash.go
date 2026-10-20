package shared

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/candyflipline/mnemonic/internal/schema"
)

// EventUID hashes the transport coordinates of one record plus a fingerprint
// into a provisional event id.
func EventUID(sourceFile string, sourceGeneration uint32, sourceLineNo, sourceOffset uint64, recordFingerprint, suffix string) string {
	material := fmt.Sprintf("%s|%d|%d|%d|%s|%s",
		sourceFile, sourceGeneration, sourceLineNo, sourceOffset, recordFingerprint, suffix)
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

// IOHash pins one tool call's input and output payload.
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
	SemanticPayload string
	ToolFields      []string
	Occurrence      uint32
}

// SemanticEventUID is the replay-stable canonical event id: a hash over the
// identity fields, the token maps and the sanitized semantic payload, with
// the folded tool identity when the event carries one.
func SemanticEventUID(event *schema.Event, folded *FoldedIdentity) string {
	var h hash.Hash = sha256.New()
	hashIdentityField(h, []byte(eventIdentityDomain))
	for _, field := range eventIdentityFields {
		hashIdentityField(h, identityFieldBytes(event, field))
	}
	hashNumericMapIdentity(h, bucketMapStrings(event.TokenUsageBuckets))
	hashNumericMapIdentity(h, nativeMapStrings(event.TokenUsageNativeUnits))
	if folded != nil {
		hashIdentityField(h, []byte(folded.SemanticPayload))
		for _, field := range folded.ToolFields {
			hashIdentityField(h, []byte(field))
		}
		return withOccurrence(h, folded.Occurrence)
	}
	payload, toolFields, occurrence := semanticPayloadParts(event.PayloadJSON)
	hashIdentityField(h, []byte(payload))
	for _, field := range toolFields {
		hashIdentityField(h, []byte(field))
	}
	return withOccurrence(h, occurrence)
}

func withOccurrence(h hash.Hash, occurrence uint32) string {
	base := h.Sum(nil)
	if occurrence == 0 {
		return hex.EncodeToString(base)
	}
	oh := sha256.New()
	hashIdentityField(oh, []byte(eventOccurrenceDomain))
	hashIdentityField(oh, []byte(hex.EncodeToString(base)))
	hashIdentityField(oh, []byte(strconv.FormatUint(uint64(occurrence), 10)))
	return hex.EncodeToString(oh.Sum(nil))
}

func identityFieldBytes(event *schema.Event, field string) []byte {
	switch field {
	case "author":
		return nil
	case "harness":
		return []byte(event.Harness)
	case "inference_provider":
		return []byte(event.InferenceProvider)
	case "session_id":
		return []byte(event.SessionID)
	case "event_kind":
		return []byte(event.EventKind)
	case "actor_kind":
		return []byte(event.ActorKind)
	case "payload_type":
		return []byte(event.PayloadType)
	case "op_kind":
		return []byte(event.OpKind)
	case "op_status":
		return []byte(event.OpStatus)
	case "request_id":
		return []byte(event.RequestID)
	case "trace_id":
		return []byte(event.TraceID)
	case "turn_index":
		return []byte(strconv.FormatUint(uint64(event.TurnIndex), 10))
	case "item_id":
		return []byte(event.ItemID)
	case "tool_call_id":
		return []byte(event.ToolCallID)
	case "parent_tool_call_id":
		return []byte(event.ParentToolCallID)
	case "origin_tool_call_id":
		return []byte(event.OriginToolCallID)
	case "tool_name":
		return []byte(event.ToolName)
	case "tool_phase":
		return []byte(event.ToolPhase)
	case "tool_error":
		return []byte(strconv.FormatUint(uint64(event.ToolError), 10))
	case "agent_run_id":
		return []byte(event.AgentRunID)
	case "agent_label":
		return []byte(event.AgentLabel)
	case "coord_group_id":
		return []byte(event.CoordGroupID)
	case "coord_group_label":
		return []byte(event.CoordGroupLabel)
	case "is_substream":
		return []byte(strconv.FormatUint(uint64(event.IsSubstream), 10))
	case "model":
		return []byte(event.Model)
	case "endpoint_kind":
		return []byte(event.EndpointKind)
	case "input_tokens":
		return []byte(strconv.FormatUint(uint64(event.InputTokens), 10))
	case "output_tokens":
		return []byte(strconv.FormatUint(uint64(event.OutputTokens), 10))
	case "cache_read_tokens":
		return []byte(strconv.FormatUint(uint64(event.CacheReadTokens), 10))
	case "cache_write_tokens":
		return []byte(strconv.FormatUint(uint64(event.CacheWriteTokens), 10))
	case "latency_ms":
		return []byte(strconv.FormatUint(uint64(event.LatencyMs), 10))
	case "retry_count":
		return []byte(strconv.FormatUint(uint64(event.RetryCount), 10))
	case "service_tier":
		return []byte(event.ServiceTier)
	case "content_types":
		return compactStrings(event.ContentTypes)
	case "has_reasoning":
		return []byte(strconv.FormatUint(uint64(event.HasReasoning), 10))
	case "text_content":
		return []byte(event.TextContent)
	default:
		return nil
	}
}

// compactStrings encodes a string array the way serde_json prints one.
func compactStrings(values []string) []byte {
	var b strings.Builder
	b.WriteByte('[')
	for i, v := range values {
		if i > 0 {
			b.WriteByte(',')
		}
		writeJSONString(&b, v)
	}
	return []byte(b.String())
}

// hashNumericMapIdentity hashes a sorted key/value component string built
// from pre-encoded map entries.
func hashNumericMapIdentity(h hash.Hash, encoded [][2]string) {
	var comp []byte
	for _, kv := range encoded {
		comp = appendIdentityComponent(comp, kv[0])
		comp = appendIdentityComponent(comp, kv[1])
	}
	hashIdentityField(h, comp)
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
	dst = append(dst, strconv.Itoa(len(value))...)
	dst = append(dst, ':')
	return append(dst, value...)
}

// hashIdentityField feeds "<decimal length>:<value>" into the hash.
func hashIdentityField(h hash.Hash, value []byte) {
	var digits [20]byte
	cursor := len(digits)
	length := len(value)
	for {
		cursor--
		digits[cursor] = byte('0' + length%10)
		length /= 10
		if length == 0 {
			break
		}
	}
	h.Write(digits[cursor:])
	h.Write([]byte{':'})
	h.Write(value)
}

// semanticPayloadParts strips derived payload fields and recovers the folded
// tool identity from a raw payload_json, like semantic_payload_parts.
func semanticPayloadParts(payloadJSON string) (string, []string, uint32) {
	emptyFields := make([]string, len(toolIdentityFields))
	parsed, err := DecodeJSON([]byte(payloadJSON))
	if err != nil {
		return payloadJSON, emptyFields, 0
	}
	obj, ok := AsObject(parsed)
	if !ok {
		return Compact(parsed), emptyFields, 0
	}
	var occurrence uint32
	if n, ok := AsNumber(obj[semanticOccurrenceField]); ok && n <= math.MaxUint32 {
		occurrence = uint32(n)
	}
	tool := obj["moraine_tool_io"]
	delete(obj, "moraine_tool_io")
	SanitizeSemanticPayload(obj)
	toolFields := make([]string, len(toolIdentityFields))
	if toolObj, isObj := AsObject(tool); isObj {
		for i, field := range toolIdentityFields {
			toolFields[i] = encodeIdentityValue(toolObj[field])
		}
	}
	return Compact(parsed), toolFields, occurrence
}

// SanitizeSemanticPayload drops the derived fields the canonical uid ignores.
func SanitizeSemanticPayload(obj map[string]any) {
	for _, field := range semanticPayloadExcludedFields {
		delete(obj, field)
	}
}

// AsNumber reads a JSON number as uint64.
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

// encodeIdentityValue mirrors encode_identity_value: strings raw, everything
// else compact JSON, absent empty.
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
