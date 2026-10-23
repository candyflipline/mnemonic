package codex

import (
	"fmt"
	"io"

	"github.com/candyflipline/mnemonic/internal/normalize"
	"github.com/candyflipline/mnemonic/internal/normalize/shared"
	"github.com/candyflipline/mnemonic/internal/schema"
)

type Parser struct{}

func (Parser) Harness() string { return "codex" }

func (Parser) Version() int { return 1 }

func (Parser) Parse(r io.Reader, src normalize.SourceMeta, from normalize.State, emit func(*schema.Event) error) (normalize.State, error) {
	return shared.ParseJSONL(r, src, from, codexSource{}, emit)
}

func init() { normalize.Register(Parser{}) }

type codexSource struct{}

func (codexSource) Harness() string                  { return "codex" }
func (codexSource) DefaultInferenceProvider() string { return "openai" }

func (codexSource) Preflight(rec shared.JSON) (shared.JSON, bool) { return rec, true }

func (codexSource) SourceMetadata(rec shared.JSON) shared.SourceMetadata {
	return shared.DefaultSourceMetadata(codexSource{}.DefaultInferenceProvider())
}

func (codexSource) RecordTS(rec shared.JSON) string {
	return shared.ToStr(shared.Get(rec, "timestamp"))
}

func (codexSource) TopType(rec shared.JSON) string { return shared.ToStr(shared.Get(rec, "type")) }

func (codexSource) Cwd(rec shared.JSON) string {
	// Codex carries the working directory in payload.cwd on session_meta
	// (turn_context in newer rollouts); other records inherit it through the
	// cwd hint.
	return shared.ToStr(shared.Pointer(rec, "/payload/cwd"))
}

func (codexSource) SessionID(rec shared.JSON, sctx shared.SourceRecordContext) string {
	if sctx.TopType == "session_meta" {
		if id := shared.ToStr(shared.Get(shared.Get(rec, "payload"), "id")); id != "" {
			return id
		}
	}
	if sctx.SessionHint == "" {
		return shared.InferSessionIDFromFile(sctx.SourceFile)
	}
	return sctx.SessionHint
}

func (codexSource) Normalize(rec shared.JSON, ctx *shared.Ctx, topType, baseUID, modelHint string) *shared.Partials {
	r := buildCodexRecord(rec, topType, baseUID, modelHint)
	em := shared.NewEmitter(ctx)
	routeCodexRecord(&r, em)
	stampCodexModelFallbacks(&r, em.Partials.Events)
	return &em.Partials
}

type codexRecord struct {
	rec         shared.JSON
	topType     string
	baseUID     string
	modelHint   string
	payload     shared.JSON
	payloadObj  map[string]any
	payloadJSON shared.CanonicalJSON
}

func buildCodexRecord(rec shared.JSON, topType, baseUID, modelHint string) codexRecord {
	payload := shared.Get(rec, "payload")
	payloadObj := shared.AsObjectOrEmpty(payload)
	return codexRecord{
		rec:         rec,
		topType:     topType,
		baseUID:     baseUID,
		modelHint:   modelHint,
		payload:     payload,
		payloadObj:  payloadObj,
		payloadJSON: shared.CompactCanonical(payloadObj),
	}
}

func (r *codexRecord) payloadField(key string) shared.JSON { return r.payloadObj[key] }

func (r *codexRecord) payloadType() string { return shared.ToStr(r.payloadField("type")) }

func routeCodexRecord(r *codexRecord, em *shared.Emitter) {
	switch r.topType {
	case "session_meta":
		normalizeCodexSessionMeta(r, em)
	case "turn_context":
		normalizeCodexTurnContext(r, em)
	case "response_item":
		normalizeCodexResponseItem(r, em)
	case "event_msg":
		normalizeCodexEventMsg(r, em)
	case "compacted":
		normalizeCodexCompacted(r, em)
	case "message", "function_call", "function_call_output", "reasoning":
		normalizeCodexLegacyTopLevel(r, em)
	default:
		normalizeCodexUnknownTopLevel(r, em)
	}
}

func normalizeCodexSessionMeta(r *codexRecord, em *shared.Emitter) {
	row := em.Event(r.baseUID, "session_meta", "session_meta", "system", "", r.payloadJSON)
	row.Event.ItemID = shared.ToStr(r.payloadField("id"))
	em.PushEvent(row)
}

func normalizeCodexTurnContext(r *codexRecord, em *shared.Emitter) {
	row := em.Event(r.baseUID, "turn_context", "turn_context", "system", "", r.payloadJSON)
	row.Event.TurnIndex = shared.ToU32(r.payloadField("turn_id"))
	if turnID := shared.ToStr(r.payloadField("turn_id")); turnID != "" {
		row.Event.RequestID = turnID
		row.Event.ItemID = turnID
	}
	if model := shared.CanonicalizeModel("codex", shared.ToStr(r.payloadField("model"))); model != "" {
		row.Event.Model = model
	}
	em.PushEvent(row)
}

func normalizeCodexResponseItem(r *codexRecord, em *shared.Emitter) {
	payloadType := r.payloadType()
	switch payloadType {
	case "message":
		emitCodexMessage(r.payload, r.baseUID, r.payloadJSON, true, em)
	case "function_call":
		handleCodexFunctionCall(r.payload, r.baseUID, r.payloadJSON, em)
	case "function_call_output":
		handleCodexFunctionCallOutput(r.payload, r.baseUID, r.payloadJSON, em)
	case "custom_tool_call":
		handleCodexCustomToolCall(r, em)
	case "custom_tool_call_output":
		handleCodexCustomToolCallOutput(r, em)
	case "web_search_call":
		handleCodexWebSearchCall(r, em)
	case "reasoning":
		handleCodexReasoning(r.payload, r.baseUID, r.payloadJSON, true, em)
	default:
		handleCodexUnknownResponseItem(r, payloadType, em)
	}
}

func emitCodexMessage(item shared.JSON, eventUID string, payloadJSON shared.CanonicalJSON, includeResponseFields bool, em *shared.Emitter) {
	role := shared.ToStr(shared.Get(item, "role"))
	if role == "" {
		role = "assistant"
	}
	content := shared.Get(item, "content")
	row := em.Event(eventUID, "message", "message", role,
		shared.ExtractMessageText(content), payloadJSON)
	row.SetContentTypes(shared.ExtractContentTypes(content))
	if includeResponseFields {
		row.Event.ItemID = shared.ToStr(shared.Get(item, "id"))
		row.Event.OpStatus = shared.ToStr(shared.Get(item, "phase"))
	}
	em.PushEvent(row)
}

func handleCodexFunctionCall(item shared.JSON, eventUID string, payloadJSON shared.CanonicalJSON, em *shared.Emitter) {
	args := shared.ToStr(shared.Get(item, "arguments"))
	callID := shared.ToStr(shared.Get(item, "call_id"))
	name := shared.ToStr(shared.Get(item, "name"))
	row := em.Event(eventUID, "tool_call", "function_call", "assistant", args, payloadJSON)
	row.Event.ToolCallID = callID
	row.Event.ToolName = name
	em.PushEvent(row)
	em.PushToolRequest(eventUID, callID, "", name, args)
}

func handleCodexFunctionCallOutput(item shared.JSON, eventUID string, payloadJSON shared.CanonicalJSON, em *shared.Emitter) {
	output := shared.ToStr(shared.Get(item, "output"))
	callID := shared.ToStr(shared.Get(item, "call_id"))
	outputJSON := shared.Compact(shared.Get(item, "output"))
	row := em.Event(eventUID, "tool_result", "function_call_output", "tool", output, payloadJSON)
	row.Event.ToolCallID = callID
	em.PushEvent(row)
	em.PushToolResponse(eventUID, callID, "", "", 0, "", outputJSON, output)
}

func handleCodexCustomToolCall(r *codexRecord, em *shared.Emitter) {
	input := shared.ToStr(r.payloadField("input"))
	callID := shared.ToStr(r.payloadField("call_id"))
	name := shared.ToStr(r.payloadField("name"))
	status := shared.ToStr(r.payloadField("status"))
	row := em.Event(r.baseUID, "tool_call", "custom_tool_call", "assistant", input, r.payloadJSON)
	row.Event.ToolCallID = callID
	row.Event.ToolName = name
	row.Event.OpStatus = status
	em.PushEvent(row)
	em.PushToolRequest(r.baseUID, callID, "", name, input)
}

func handleCodexCustomToolCallOutput(r *codexRecord, em *shared.Emitter) {
	output := shared.ToStr(r.payloadField("output"))
	callID := shared.ToStr(r.payloadField("call_id"))
	status := shared.ToStr(r.payloadField("status"))
	// The output string re-parses to its compact JSON; a non-JSON string
	// keeps the quoted-JSON form of the raw value.
	outputJSON := output
	if parsed, err := shared.DecodeJSON([]byte(output)); err == nil {
		outputJSON = shared.Compact(parsed)
	} else {
		outputJSON = shared.Compact(r.payloadField("output"))
	}
	row := em.Event(r.baseUID, "tool_result", "custom_tool_call_output", "tool", output, r.payloadJSON)
	row.Event.ToolCallID = callID
	row.Event.OpStatus = status
	em.PushEvent(row)
	em.PushToolResponse(r.baseUID, callID, "", "", 0, "", outputJSON, output)
}

func handleCodexWebSearchCall(r *codexRecord, em *shared.Emitter) {
	action := r.payloadField("action")
	actionType := shared.ToStr(shared.Get(action, "type"))
	status := shared.ToStr(r.payloadField("status"))
	row := em.Event(r.baseUID, "tool_call", "web_search_call", "assistant",
		shared.ExtractMessageText(action), r.payloadJSON)
	row.Event.ToolName = "web_search"
	row.Event.OpKind = actionType
	row.Event.OpStatus = status
	row.Event.ToolPhase = status
	em.PushEvent(row)
}

func handleCodexReasoning(item shared.JSON, eventUID string, payloadJSON shared.CanonicalJSON, includeItemID bool, em *shared.Emitter) {
	summary := shared.Get(item, "summary")
	row := em.Event(eventUID, "reasoning", "reasoning", "assistant",
		shared.ExtractMessageText(summary), payloadJSON).MarkReasoning()
	if includeItemID {
		row.Event.ItemID = shared.ToStr(shared.Get(item, "id"))
	}
	em.PushEvent(row)
}

func handleCodexUnknownResponseItem(r *codexRecord, payloadType string, em *shared.Emitter) {
	if payloadType == "" {
		payloadType = "response_item"
	}
	row := em.Event(r.baseUID, "unknown", payloadType, "system",
		shared.ExtractMessageText(r.payload), r.payloadJSON)
	em.PushEvent(row)
}

func normalizeCodexEventMsg(r *codexRecord, em *shared.Emitter) {
	payloadType := r.payloadType()
	actor := "system"
	switch payloadType {
	case "user_message":
		actor = "user"
	case "agent_message", "agent_reasoning":
		actor = "assistant"
	}
	payloadTypeForRow := payloadType
	if payloadTypeForRow == "" {
		payloadTypeForRow = "event_msg"
	}
	row := em.Event(r.baseUID, "event_msg", payloadTypeForRow, actor,
		shared.ExtractMessageText(r.payload), r.payloadJSON)
	if turnID := shared.ToStr(r.payloadField("turn_id")); turnID != "" {
		row.Event.RequestID = turnID
		row.Event.ItemID = turnID
	}
	if status := shared.ToStr(r.payloadField("status")); status != "" {
		row.Event.OpStatus = status
	}
	if payloadType == "token_count" {
		stampCodexTokenCount(r, row)
	} else if payloadType == "agent_reasoning" {
		row.MarkReasoning()
	}
	em.PushEvent(row)
}

func stampCodexTokenCount(r *codexRecord, row *shared.EventRow) {
	usage := shared.Get(r.payloadField("info"), "last_token_usage")
	inputTokens := shared.ToU32(shared.Get(usage, "input_tokens"))
	outputTokens := shared.ToU32(shared.Get(usage, "output_tokens"))
	cacheReadTokens := shared.ToU32(shared.FirstOf(usage, "cached_input_tokens", "cache_read_input_tokens"))
	cacheWriteTokens := shared.ToU32(shared.FirstOf(usage, "cache_creation_input_tokens", "cache_write_input_tokens"))

	accounting := shared.OpenAIGeneration(usage).
		WithLegacyScalars(uint64(inputTokens), uint64(outputTokens), uint64(cacheReadTokens), uint64(cacheWriteTokens)).
		WithRawUsageJSON(r.payloadJSON.String())
	row.SetTokenAccounting(accounting)
	row.Event.ServiceTier = shared.ToStr(shared.Get(r.payloadField("rate_limits"), "plan_type"))

	if model := resolveCodexTokenCountModel(r); model != "" {
		row.Event.Model = model
	}
}

func resolveCodexTokenCountModel(r *codexRecord) string {
	model := shared.ToStr(shared.Get(r.payloadField("rate_limits"), "limit_name"))
	fallbackModel := shared.ToStr(r.payloadField("model"))
	fallbackLimitID := shared.ToStr(shared.Get(r.payloadField("rate_limits"), "limit_id"))
	inheritedModel := shared.CanonicalizeModel("codex", r.modelHint)

	switch {
	case model != "":
		return shared.CanonicalizeModel("codex", model)
	case fallbackModel != "":
		return shared.CanonicalizeModel("codex", fallbackModel)
	case inheritedModel != "":
		return inheritedModel
	case fallbackLimitID != "":
		return shared.CanonicalizeModel("codex", fallbackLimitID)
	default:
		return ""
	}
}

func normalizeCodexCompacted(r *codexRecord, em *shared.Emitter) {
	row := em.Event(r.baseUID, "compacted_raw", "compacted", "system", "", r.payloadJSON)
	em.PushEvent(row)
	items, ok := shared.AsArray(r.payloadField("replacement_history"))
	if !ok {
		return
	}
	for idx, item := range items {
		normalizeCodexCompactedItem(r, item, idx, em)
	}
}

func normalizeCodexCompactedItem(r *codexRecord, item shared.JSON, idx int, em *shared.Emitter) {
	itemUID := em.Ctx.UIDForJSON(item, fmt.Sprintf("compacted:%d", idx))
	itemType := shared.ToStr(shared.Get(item, "type"))

	var kind, payloadType, actor, text string
	switch itemType {
	case "message":
		kind, payloadType, actor = "message", "message", shared.ToStr(shared.Get(item, "role"))
		text = shared.ExtractMessageText(shared.Get(item, "content"))
	case "function_call":
		kind, payloadType, actor = "tool_call", "function_call", "assistant"
		text = shared.ToStr(shared.Get(item, "arguments"))
	case "function_call_output":
		kind, payloadType, actor = "tool_result", "function_call_output", "tool"
		text = shared.ToStr(shared.Get(item, "output"))
	case "reasoning":
		kind, payloadType, actor = "reasoning", "reasoning", "assistant"
		text = shared.ExtractMessageText(shared.Get(item, "summary"))
	default:
		kind, payloadType, actor = "unknown", itemType, "system"
		if payloadType == "" {
			payloadType = "unknown"
		}
		text = shared.ExtractMessageText(item)
	}
	if actor == "" {
		actor = "assistant"
	}
	row := em.EventForJSON(itemUID, kind, payloadType, actor, text, item)
	row.Event.OriginEventID = r.baseUID
	if kind == "reasoning" {
		row.MarkReasoning()
	}
	em.PushEvent(row)
}

func normalizeCodexLegacyTopLevel(r *codexRecord, em *shared.Emitter) {
	payloadJSON := shared.CompactCanonical(r.rec)
	switch r.topType {
	case "message":
		emitCodexMessage(r.rec, r.baseUID, payloadJSON, false, em)
	case "function_call":
		handleCodexFunctionCall(r.rec, r.baseUID, payloadJSON, em)
	case "function_call_output":
		handleCodexFunctionCallOutput(r.rec, r.baseUID, payloadJSON, em)
	case "reasoning":
		handleCodexReasoning(r.rec, r.baseUID, payloadJSON, false, em)
	}
}

func normalizeCodexUnknownTopLevel(r *codexRecord, em *shared.Emitter) {
	payloadType := r.topType
	if payloadType == "" {
		payloadType = "unknown"
	}
	row := em.EventForJSON(r.baseUID, "unknown", payloadType, "system",
		shared.ExtractMessageText(r.rec), r.rec)
	em.PushEvent(row)
}

func stampCodexModelFallbacks(r *codexRecord, events []*shared.EventRow) {
	payloadModel := shared.CanonicalizeModel("codex", shared.ToStr(r.payloadField("model")))
	inheritedModel := shared.CanonicalizeModel("codex", r.modelHint)
	for _, row := range events {
		resolved := shared.CanonicalizeModel("codex", row.Event.Model)
		if resolved == "" {
			resolved = payloadModel
		}
		if resolved == "" {
			resolved = inheritedModel
		}
		if resolved != "" {
			row.Event.Model = resolved
		}
	}
}
