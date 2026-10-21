package shared

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/candyflipline/mnemonic/internal/schema"
)

const wholeFile = 1

type Ctx struct {
	SourceName        string
	Harness           string
	InferenceProvider string
	SessionID         string
	SessionHint       string
	SessionDate       string
	Cwd               string
	SourceFile        string
	SourceLineNo      uint64
	SourceOffset      uint64
	RecordTS          string
	EventTS           string
}

// EventRow is one event under construction: the schema columns plus the
// transient folded identity used to compute the canonical uid.
type EventRow struct {
	Event  schema.Event
	Folded *FoldedIdentity
}

type Partials struct {
	Events []*EventRow
	Tools  []*ToolRow
}

type ToolRow struct {
	EventUID          string
	SessionID         string
	Harness           string
	InferenceProvider string
	SourceName        string
	ToolCallID        string
	ParentToolCallID  string
	ToolName          string
	ToolPhase         string
	ToolError         uint8
	InputJSON         string
	OutputJSON        string
	OutputText        string
	InputBytes        uint32
	OutputBytes       uint32
	InputPreview      string
	OutputPreview     string
	IOHash            uint64
	ProjectID         string
	RepoRelPath       string
	WorktreeRoot      string
	SourceRef         string
}

type Emitter struct {
	Ctx      *Ctx
	Partials Partials
}

func NewEmitter(ctx *Ctx) *Emitter { return &Emitter{Ctx: ctx} }

func (c *Ctx) UID(fingerprint, suffix string) string {
	return EventUID(c.SourceFile, c.SourceLineNo, c.SourceOffset, fingerprint, suffix)
}

func (c *Ctx) UIDForJSON(v JSON, suffix string) string {
	return c.UID(Compact(v), suffix)
}

func (c *Ctx) SourceRef() string {
	return fmt.Sprintf("%s:%d:%d", c.SourceFile, wholeFile, c.SourceLineNo)
}

func canonicalEventKind(value string) string {
	if eventKindInDomain(value) {
		return value
	}
	return "unknown"
}

func canonicalPayloadType(value string) string {
	if payloadTypeInDomain(value) {
		return value
	}
	return "unknown"
}

var eventKindDomain = map[string]bool{
	"session_meta": true, "turn_context": true, "message": true, "tool_call": true,
	"tool_result": true, "reasoning": true, "event_msg": true, "compacted_raw": true,
	"progress": true, "system": true, "summary": true, "queue_operation": true,
	"file_history_snapshot": true, "unknown": true,
}

var payloadTypeDomain = map[string]bool{
	"session_meta": true, "turn_context": true, "message": true, "function_call": true,
	"function_call_output": true, "custom_tool_call": true, "custom_tool_call_output": true,
	"web_search_call": true, "reasoning": true, "response_item": true, "event_msg": true,
	"user_message": true, "agent_message": true, "agent_reasoning": true, "token_count": true,
	"task_started": true, "task_complete": true, "turn_aborted": true, "item_completed": true,
	"search_results_received": true, "compacted": true, "thinking": true, "tool_use": true,
	"tool_result": true, "text": true, "progress": true, "system": true, "summary": true,
	"queue-operation": true, "file-history-snapshot": true, "unknown": true,
}

func eventKindInDomain(value string) bool   { return eventKindDomain[value] }
func payloadTypeInDomain(value string) bool { return payloadTypeDomain[value] }

func (e *Emitter) Event(eventUID, eventKind, payloadType, actorKind, textContent, payloadJSON string) *EventRow {
	ctx := e.Ctx
	text := TruncateChars(textContent, textLimit)
	attention := EventFileAttentionFields(ctx.Cwd)
	row := &EventRow{Event: schema.Event{
		EventUID:              eventUID,
		SessionID:             ctx.SessionID,
		SessionDate:           ctx.SessionDate,
		SourceName:            ctx.SourceName,
		Harness:               ctx.Harness,
		InferenceProvider:     ctx.InferenceProvider,
		SourceFile:            ctx.SourceFile,
		SourceInode:           wholeFile,
		SourceGeneration:      wholeFile,
		SourceLineNo:          ctx.SourceLineNo,
		SourceOffset:          ctx.SourceOffset,
		SourceRef:             ctx.SourceRef(),
		RecordTS:              ctx.RecordTS,
		EventTS:               ctx.EventTS,
		EventKind:             canonicalEventKind(eventKind),
		ActorKind:             actorKind,
		PayloadType:           canonicalPayloadType(payloadType),
		EndpointKind:          "generation",
		ContentTypes:          []string{},
		TextContent:           text,
		TextPreview:           TruncateChars(text, previewLimit),
		PayloadJSON:           payloadJSON,
		TokenUsageBuckets:     zeroBuckets(),
		TokenUsageNativeUnits: zeroNativeUnits(),
		ProjectID:             attention.ProjectID,
		RepoRelPath:           attention.RepoRelPath,
		WorktreeRoot:          attention.WorktreeRoot,
		EventVersion:          EventVersion(),
		Cwd:                   ctx.Cwd,
	}}
	return row
}

func (e *Emitter) EventForJSON(eventUID, eventKind, payloadType, actorKind, textContent string, payload JSON) *EventRow {
	return e.Event(eventUID, eventKind, payloadType, actorKind, textContent, Compact(payload))
}

func (e *Emitter) PushEvent(row *EventRow) { e.Partials.Events = append(e.Partials.Events, row) }

func (e *Emitter) PushToolRequest(eventUID, toolCallID, parentToolCallID, toolName, inputJSON string) {
	e.Partials.Tools = append(e.Partials.Tools,
		e.toolRow(eventUID, toolCallID, parentToolCallID, toolName, "request", 0, inputJSON, "", ""))
}

func (e *Emitter) PushToolResponse(eventUID, toolCallID, parentToolCallID, toolName string, toolError uint8, inputJSON, outputJSON, outputText string) {
	e.Partials.Tools = append(e.Partials.Tools,
		e.toolRow(eventUID, toolCallID, parentToolCallID, toolName, "response", toolError, inputJSON, outputJSON, outputText))
}

func (e *Emitter) toolRow(eventUID, toolCallID, parentToolCallID, toolName, toolPhase string, toolError uint8, inputJSON, outputJSON, outputText string) *ToolRow {
	attention := ToolFileAttentionFields(e.Ctx.Cwd, toolPhase, inputJSON)
	inputJSON = TruncateChars(inputJSON, textLimit)
	outputJSON = TruncateChars(outputJSON, textLimit)
	outputText = TruncateChars(outputText, textLimit)
	return &ToolRow{
		EventUID:          eventUID,
		SessionID:         e.Ctx.SessionID,
		Harness:           e.Ctx.Harness,
		InferenceProvider: e.Ctx.InferenceProvider,
		SourceName:        e.Ctx.SourceName,
		ToolCallID:        toolCallID,
		ParentToolCallID:  parentToolCallID,
		ToolName:          toolName,
		ToolPhase:         toolPhase,
		ToolError:         toolError,
		InputJSON:         inputJSON,
		OutputJSON:        outputJSON,
		OutputText:        outputText,
		InputBytes:        uint32(len(inputJSON)),
		OutputBytes:       uint32(len(outputJSON)),
		InputPreview:      TruncateChars(inputJSON, previewLimit),
		OutputPreview:     TruncateChars(outputText, previewLimit),
		IOHash:            IOHash(inputJSON, outputJSON),
		ProjectID:         attention.ProjectID,
		RepoRelPath:       attention.RepoRelPath,
		WorktreeRoot:      attention.WorktreeRoot,
		SourceRef:         e.Ctx.SourceRef(),
	}
}

func (r *EventRow) SetText(text string) *EventRow {
	text = TruncateChars(text, textLimit)
	r.Event.TextContent = text
	r.Event.TextPreview = TruncateChars(text, previewLimit)
	return r
}

func (r *EventRow) SetContentTypes(values []string) *EventRow {
	if values == nil {
		values = []string{}
	}
	r.Event.ContentTypes = values
	return r
}

func (r *EventRow) SetTokenAccounting(t TokenAccounting) *EventRow {
	r.Event.EndpointKind = t.EndpointKind
	r.Event.InputTokens = t.InputTokens
	r.Event.OutputTokens = t.OutputTokens
	r.Event.CacheReadTokens = t.CacheReadTokens
	r.Event.CacheWriteTokens = t.CacheWriteTokens
	r.Event.TokenUsageJSON = t.TokenUsageJSON
	r.Event.TokenUsageBuckets = t.Buckets()
	r.Event.TokenUsageNativeUnits = t.NativeUnits()
	return r
}

func (r *EventRow) MarkReasoning() *EventRow {
	r.Event.HasReasoning = 1
	return r.SetContentTypes([]string{"reasoning"})
}

// MapOf renders the tool row as the JSON object the fold embeds under
// moraine_tool_io.
func (t *ToolRow) MapOf() map[string]any {
	return map[string]any{
		"event_uid":           t.EventUID,
		"session_id":          t.SessionID,
		"harness":             t.Harness,
		"inference_provider":  t.InferenceProvider,
		"source_name":         t.SourceName,
		"tool_call_id":        t.ToolCallID,
		"parent_tool_call_id": t.ParentToolCallID,
		"tool_name":           t.ToolName,
		"tool_phase":          t.ToolPhase,
		"tool_error":          jsonNumberUint(uint64(t.ToolError)),
		"input_json":          t.InputJSON,
		"output_json":         t.OutputJSON,
		"output_text":         t.OutputText,
		"input_bytes":         jsonNumberUint(uint64(t.InputBytes)),
		"output_bytes":        jsonNumberUint(uint64(t.OutputBytes)),
		"input_preview":       t.InputPreview,
		"output_preview":      t.OutputPreview,
		"io_hash":             jsonNumberUint(t.IOHash),
		"project_id":          t.ProjectID,
		"repo_rel_path":       t.RepoRelPath,
		"worktree_root":       t.WorktreeRoot,
		"source_ref":          t.SourceRef,
	}
}

func ResolveModelHint(events []*EventRow, harness, fallback string) string {
	for i := len(events) - 1; i >= 0; i-- {
		if normalized := CanonicalizeModel(harness, events[i].Event.Model); normalized != "" {
			return normalized
		}
	}
	return CanonicalizeModel(harness, fallback)
}

// ToolIdentityFields encodes the tool identity fields the canonical uid
// hashes, in TOOL_IDENTITY_FIELDS order.
func ToolIdentityFields(t *ToolRow) []string {
	return []string{
		t.ToolCallID,
		t.ParentToolCallID,
		t.ToolName,
		t.ToolPhase,
		Compact(jsonNumberUint(uint64(t.ToolError))),
		t.InputJSON,
		t.OutputJSON,
		t.OutputText,
	}
}

func jsonNumberUint(v uint64) json.Number {
	return json.Number(strconv.FormatUint(v, 10))
}
