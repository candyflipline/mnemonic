package cursor

import (
	"fmt"
	"io"
	"strings"

	"github.com/candyflipline/mnemonic/internal/normalize"
	"github.com/candyflipline/mnemonic/internal/normalize/shared"
	"github.com/candyflipline/mnemonic/internal/schema"
)

type Parser struct{}

func (Parser) Harness() string { return "cursor" }

func (Parser) Parse(r io.Reader, src normalize.SourceMeta, from normalize.State) ([]schema.Event, normalize.State, error) {
	return shared.ParseJSONL(r, src, from, cursorSource{})
}

func init() { normalize.Register(Parser{}) }

type cursorSource struct{}

func (cursorSource) Harness() string                  { return "cursor" }
func (cursorSource) DefaultInferenceProvider() string { return "cursor" }

func (cursorSource) Preflight(rec shared.JSON) (shared.JSON, bool) { return rec, true }

func (cursorSource) SourceMetadata(rec shared.JSON) shared.SourceMetadata {
	return shared.SourceMetadata{
		InferenceProvider: "cursor",
		ModelHintFallback: cursorModelHint(rec),
	}
}

func (cursorSource) RecordTS(rec shared.JSON) string { return cursorRecordTS(rec) }

func (cursorSource) Cwd(rec shared.JSON) string {
	// The cursor_sqlite poller surfaces `workspaceIdentifier.uri.fsPath` as
	// `workspacePath` on synthesized composer records; bubbles and
	// file-backed transcripts carry no working directory of their own.
	return shared.ToStr(shared.Get(rec, "workspacePath"))
}

func (cursorSource) TopType(rec shared.JSON) string {
	if role := cursorRole(rec); role != "" {
		return role
	}
	if s, ok := shared.FirstString(rec, "type"); ok {
		return s
	}
	if s, ok := shared.FirstString(shared.Get(rec, "message"), "type", "kind"); ok {
		return s
	}
	return "message"
}

var cursorSessionIDKeys = []string{
	"conversationId", "conversation_id", "sessionId", "session_id",
	"threadId", "thread_id", "chatId", "chat_id",
}

func (cursorSource) SessionID(rec shared.JSON, sctx shared.SourceRecordContext) string {
	if id, ok := shared.FirstString(rec, cursorSessionIDKeys...); ok {
		return id
	}
	if id, ok := shared.FirstString(shared.Get(rec, "message"), cursorSessionIDKeys...); ok {
		return id
	}
	if sctx.SessionHint != "" {
		return sctx.SessionHint
	}
	if inferred := shared.InferSessionIDFromFile(sctx.SourceFile); inferred != "" {
		return inferred
	}
	return "cursor:" + sctx.BaseUID
}

func (cursorSource) Normalize(rec shared.JSON, ctx *shared.Ctx, topType, baseUID, modelHint string) *shared.Partials {
	switch topType {
	case "cursor_composer":
		return normalizeCursorComposer(rec, ctx)
	case "cursor_bubble":
		return normalizeCursorSqliteBubble(rec, ctx)
	}
	return normalizeCursorRecord(rec, ctx, topType, baseUID, modelHint)
}

func normalizeCursorComposer(rec shared.JSON, ctx *shared.Ctx) *shared.Partials {
	em := shared.NewEmitter(ctx)
	sessionID, _ := shared.FirstString(rec, "sessionId")
	uid := em.Ctx.UID(fmt.Sprintf("cursor_sqlite:composer:%s", sessionID), "session_meta")

	name, _ := shared.FirstString(rec, "name", "title")
	subtitle, _ := shared.FirstString(rec, "subtitle")
	var text string
	switch {
	case name != "" && subtitle != "":
		text = name + " — " + subtitle
	case name != "":
		text = name
	case subtitle != "":
		text = subtitle
	}

	em.PushEvent(em.EventForJSON(uid, "session_meta", "session_meta", "system", text, rec))
	return &em.Partials
}

// normalizeCursorSqliteBubble normalizes a `cursor_bubble` synthetic record
// (one per `bubbleId:` row). Bubbles mutate in place while Cursor streams
// (text grows, tool status flips pending → completed), so every event UID
// here is derived from the bubble's logical identity — never its payload —
// letting a re-emit replace the prior row.
func normalizeCursorSqliteBubble(rec shared.JSON, ctx *shared.Ctx) *shared.Partials {
	em := shared.NewEmitter(ctx)
	sessionID, _ := shared.FirstString(rec, "sessionId")
	bubbleID, _ := shared.FirstString(rec, "bubbleId")
	identity := fmt.Sprintf("cursor_sqlite:bubble:%s:%s", sessionID, bubbleID)
	bubbleType := shared.ToU64(shared.Get(rec, "bubbleType"))
	requestID, _ := shared.FirstString(rec, "requestId")

	stamp := func(row *shared.EventRow) *shared.EventRow {
		row.Event.RequestID = requestID
		row.Event.TraceID = requestID
		row.Event.ItemID = bubbleID
		return row
	}

	if text, ok := shared.AsString(shared.Pointer(rec, "/thinking/text")); ok && strings.TrimSpace(text) != "" {
		uid := em.Ctx.UID(identity, "reasoning")
		row := stamp(em.EventForJSON(uid, "reasoning", "reasoning", "assistant", text, shared.Get(rec, "thinking")))
		row.Event.HasReasoning = 1
		row.SetContentTypes([]string{"reasoning"})
		em.PushEvent(row)
	}

	if text, ok := shared.AsString(shared.Get(rec, "text")); ok && strings.TrimSpace(text) != "" {
		suffix, payloadType, actor := "agent_message", "agent_message", "assistant"
		if bubbleType == 1 {
			suffix, payloadType, actor = "user_message", "user_message", "user"
		}
		uid := em.Ctx.UID(identity, suffix)
		row := stamp(em.EventForJSON(uid, "message", payloadType, actor, text,
			map[string]any{"text": text}))
		row.SetContentTypes([]string{"text"})
		em.PushEvent(row)
	}

	if toolData := shared.Get(rec, "toolFormerData"); toolData != nil {
		if _, ok := shared.AsObject(toolData); ok {
			normalizeCursorToolFormer(identity, toolData, stamp, em)
		}
	}

	return &em.Partials
}

func normalizeCursorToolFormer(identity string, toolData shared.JSON, stamp func(*shared.EventRow) *shared.EventRow, em *shared.Emitter) {
	toolName := cursorFormerToolName(toolData)
	status := ""
	if s, ok := shared.FirstString(toolData, "status"); ok {
		status = strings.ToLower(s)
	}
	// Cursor joins two ids with a literal newline ("call_…\nfc_…"); the
	// first line is the stable call id.
	toolCallID := identity
	if raw, ok := shared.FirstString(toolData, "toolCallId"); ok {
		line := raw
		if i := strings.IndexByte(line, '\n'); i >= 0 {
			line = line[:i]
		}
		if line = strings.TrimSuffix(line, "\r"); line != "" {
			toolCallID = line
		}
	}

	input := shared.JSON(nil)
	if v, ok := shared.FirstPresent(toolData, "params"); ok && !shared.IsNull(v) {
		input = v
	} else if v, ok := shared.FirstPresent(toolData, "rawArgs"); ok && !shared.IsNull(v) {
		input = v
	}
	inputJSON := shared.Compact(input)
	inputText := cursorTextOrJSON(input)

	useUID := em.Ctx.UID(identity, "tool_use")
	row := stamp(em.EventForJSON(useUID, "tool_call", "tool_use", "assistant", inputText, toolData))
	row.SetContentTypes([]string{"tool_use"})
	row.Event.ToolCallID = toolCallID
	row.Event.ToolName = toolName
	row.Event.OpStatus = status
	em.PushEvent(row)
	em.PushToolRequest(useUID, toolCallID, "", toolName, inputJSON)

	// `result` can be legitimately absent on a completed call (ripgrep keeps
	// its summary in additionalData; `await` has neither params nor result),
	// so terminality is decided by status alone.
	terminal := false
	switch status {
	case "completed", "error", "errored", "cancelled", "canceled", "aborted", "rejected":
		terminal = true
	}
	if !terminal {
		return
	}

	output := shared.JSON(nil)
	if v, ok := shared.FirstPresent(toolData, "result"); ok && !shared.IsNull(v) {
		output = v
	} else if v, ok := shared.FirstPresent(toolData, "error"); ok && !shared.IsNull(v) {
		output = v
	} else if v, ok := shared.FirstPresent(toolData, "additionalData"); ok && !shared.IsNull(v) {
		output = v
	}
	outputJSON := shared.Compact(output)
	outputText := cursorTextOrJSON(output)
	rejected, _ := shared.AsBool(shared.Get(output, "rejected"))
	toolError := uint8(0)
	if status != "completed" || rejected {
		toolError = 1
	}

	resultUID := em.Ctx.UID(identity, "tool_result")
	row = stamp(em.EventForJSON(resultUID, "tool_result", "tool_result", "tool", outputText, output))
	row.SetContentTypes([]string{"tool_result"})
	row.Event.ToolCallID = toolCallID
	row.Event.ToolName = toolName
	row.Event.OpStatus = status
	row.Event.ToolError = toolError
	em.PushEvent(row)
	em.PushToolResponse(resultUID, toolCallID, "", toolName, toolError, inputJSON, outputJSON, outputText)
}

// cursorFormerToolName is the tool name for a `toolFormerData` payload. MCP
// calls degrade to "mcp--" on some errors; recover the real name from
// rawArgs/params when the recorded one is degenerate.
func cursorFormerToolName(toolData shared.JSON) string {
	name, _ := shared.FirstString(toolData, "name")
	if name != "" && name != "mcp--" && name != "mcp-" {
		return name
	}
	if tool, ok := shared.FirstString(shared.Get(toolData, "rawArgs"), "name"); ok {
		return "mcp:" + tool
	}
	if name == "" {
		return "unknown"
	}
	return name
}

func normalizeCursorRecord(rec shared.JSON, ctx *shared.Ctx, topType, baseUID, modelHint string) *shared.Partials {
	r := newCursorRecord(rec, topType, baseUID, modelHint)
	em := shared.NewEmitter(ctx)

	content := r.content()
	if blocks, ok := shared.AsArray(content); ok && len(blocks) > 0 {
		for idx, block := range blocks {
			normalizeCursorContentBlock(&r, idx, block, em)
		}
		return &em.Partials
	}
	if text, ok := shared.AsString(content); ok && strings.TrimSpace(text) != "" {
		emitCursorScalarMessage(&r, text, em)
		return &em.Partials
	}
	if !shared.IsNull(content) {
		emitCursorScalarMessage(&r, cursorTextOrJSON(content), em)
		return &em.Partials
	}
	emitCursorUnknown(&r, em)
	return &em.Partials
}

type cursorRecord struct {
	rec       shared.JSON
	message   shared.JSON
	topType   string
	baseUID   string
	role      string
	model     string
	requestID string
	itemID    string
}

func newCursorRecord(rec shared.JSON, topType, baseUID, modelHint string) cursorRecord {
	message := rec
	if v, ok := shared.FirstPresent(rec, "message"); ok {
		message = v
	}
	model := cursorModelHint(rec)
	if model == "" {
		model = shared.CanonicalizeModel("cursor", modelHint)
	}
	requestKeys := []string{"requestId", "request_id", "turnId", "turn_id"}
	requestID, _ := firstStringOf(rec, message, requestKeys)
	itemID, _ := firstStringOf(rec, message, []string{"id", "messageId", "message_id"})
	return cursorRecord{
		rec:       rec,
		message:   message,
		topType:   topType,
		baseUID:   baseUID,
		role:      cursorRole(rec),
		model:     model,
		requestID: requestID,
		itemID:    itemID,
	}
}

func (r *cursorRecord) content() shared.JSON {
	if v, ok := shared.FirstPresent(r.message, "content"); ok {
		return v
	}
	if v, ok := shared.FirstPresent(r.rec, "content"); ok {
		return v
	}
	if v, ok := shared.FirstPresent(r.message, "text"); ok {
		return v
	}
	if v, ok := shared.FirstPresent(r.rec, "text"); ok {
		return v
	}
	return nil
}

func (r *cursorRecord) actor() string {
	switch r.role {
	case "user":
		return "user"
	case "assistant":
		return "assistant"
	case "tool":
		return "tool"
	case "system", "runtime":
		return "system"
	}
	switch r.topType {
	case "user":
		return "user"
	case "tool":
		return "tool"
	case "system", "runtime":
		return "system"
	}
	return "assistant"
}

func (r *cursorRecord) messagePayloadType() string {
	switch r.actor() {
	case "user":
		return "user_message"
	case "assistant":
		return "agent_message"
	case "system":
		return "system"
	case "tool":
		return "tool_result"
	}
	return "message"
}

func (r *cursorRecord) stampCommon(row *shared.EventRow) *shared.EventRow {
	row.Event.Model = r.model
	row.Event.RequestID = r.requestID
	row.Event.TraceID = r.requestID
	row.Event.ItemID = r.itemID
	return row
}

func normalizeCursorContentBlock(r *cursorRecord, idx int, block shared.JSON, em *shared.Emitter) {
	blockType := cursorBlockType(block)
	switch blockType {
	case "tool_use", "tool_call", "function_call":
		emitCursorToolUse(r, idx, block, em)
	case "tool_result", "function_call_output", "tool_call_result":
		emitCursorToolResult(r, idx, block, em)
	case "thinking", "reasoning":
		emitCursorReasoning(r, idx, block, em)
	default:
		emitCursorTextBlock(r, idx, block, blockType, em)
	}
}

func emitCursorScalarMessage(r *cursorRecord, text string, em *shared.Emitter) {
	em.PushEvent(r.stampCommon(em.Event(r.baseUID, "message", r.messagePayloadType(),
		r.actor(), text, shared.Compact(r.message))))
}

func emitCursorTextBlock(r *cursorRecord, idx int, block shared.JSON, blockType string, em *shared.Emitter) {
	uid := cursorBlockUID(em, block, idx, "text")
	text := cursorTextOrJSON(block)
	contentType := blockType
	if contentType == "" {
		contentType = "text"
	}
	row := r.stampCommon(em.EventForJSON(uid, "message", r.messagePayloadType(),
		r.actor(), text, block))
	row.SetContentTypes([]string{contentType})
	em.PushEvent(row)
}

func emitCursorReasoning(r *cursorRecord, idx int, block shared.JSON, em *shared.Emitter) {
	uid := cursorBlockUID(em, block, idx, "reasoning")
	text := cursorTextOrJSON(block)
	row := r.stampCommon(em.EventForJSON(uid, "reasoning", "reasoning", "assistant", text, block))
	row.Event.HasReasoning = 1
	row.SetContentTypes([]string{"reasoning"})
	em.PushEvent(row)
}

func emitCursorToolUse(r *cursorRecord, idx int, block shared.JSON, em *shared.Emitter) {
	uid := cursorBlockUID(em, block, idx, "tool_use")
	toolName := cursorToolName(block)
	toolCallID, ok := cursorToolCallID(block)
	if !ok {
		toolCallID = uid
	}
	input := cursorToolInput(block)
	inputJSON := shared.Compact(input)
	inputText := cursorTextOrJSON(input)

	row := r.stampCommon(em.EventForJSON(uid, "tool_call", "tool_use", "assistant", inputText, block))
	row.SetContentTypes([]string{"tool_use"})
	row.Event.ToolCallID = toolCallID
	row.Event.ToolName = toolName
	row.Event.ItemID, _ = shared.FirstString(block, "id")
	em.PushEvent(row)
	em.PushToolRequest(uid, toolCallID, "", toolName, inputJSON)
}

func emitCursorToolResult(r *cursorRecord, idx int, block shared.JSON, em *shared.Emitter) {
	uid := cursorBlockUID(em, block, idx, "tool_result")
	toolCallID, _ := cursorToolCallID(block)
	toolName := cursorToolName(block)
	output := block
	if v, ok := shared.FirstPresent(block, "content", "output", "result"); ok {
		output = v
	}
	outputJSON := shared.Compact(output)
	outputText := cursorTextOrJSON(output)
	toolError := shared.ToU8Bool(shared.FirstOf(block, "is_error", "error"))

	row := r.stampCommon(em.EventForJSON(uid, "tool_result", "tool_result", "tool", outputText, block))
	row.SetContentTypes([]string{"tool_result"})
	row.Event.ToolCallID = toolCallID
	row.Event.ToolName = toolName
	row.Event.ToolError = toolError
	em.PushEvent(row)
	em.PushToolResponse(uid, toolCallID, "", toolName, toolError, "", outputJSON, outputText)
}

func emitCursorUnknown(r *cursorRecord, em *shared.Emitter) {
	em.PushEvent(r.stampCommon(em.EventForJSON(r.baseUID, "unknown", "unknown",
		r.actor(), cursorTextOrJSON(r.rec), r.rec)))
}

func cursorBlockUID(em *shared.Emitter, block shared.JSON, idx int, suffix string) string {
	return em.Ctx.UIDForJSON(block, fmt.Sprintf("cursor:block:%d:%s", idx, suffix))
}

func cursorBlockType(block shared.JSON) string {
	s, ok := shared.FirstString(block, "type", "kind")
	if !ok {
		return "text"
	}
	return strings.ToLower(s)
}

func cursorToolName(block shared.JSON) string {
	if s, ok := shared.FirstString(block, "name", "tool_name", "toolName"); ok {
		return s
	}
	if s, ok := shared.FirstString(shared.Get(block, "function"), "name"); ok {
		return s
	}
	return ""
}

func cursorToolCallID(block shared.JSON) (string, bool) {
	return shared.FirstString(block,
		"id", "tool_use_id", "toolUseId", "tool_call_id", "toolCallId", "call_id", "callId")
}

func cursorToolInput(block shared.JSON) shared.JSON {
	if v, ok := shared.FirstPresent(block, "input", "arguments"); ok {
		return v
	}
	if v, ok := shared.FirstPresent(shared.Get(block, "function"), "arguments"); ok {
		return v
	}
	return nil
}

func cursorTextOrJSON(v shared.JSON) string {
	text := shared.ExtractMessageText(v)
	if text == "" && !shared.IsNull(v) {
		return shared.Compact(v)
	}
	return text
}

var cursorTSKeys = []string{"timestamp", "createdAt", "created_at", "updatedAt", "updated_at", "time"}

func cursorRecordTS(rec shared.JSON) string {
	if s, ok := shared.FirstString(rec, cursorTSKeys...); ok {
		return s
	}
	if s, ok := shared.FirstString(shared.Get(rec, "message"), cursorTSKeys...); ok {
		return s
	}
	return ""
}

var cursorModelKeys = []string{"model", "modelName", "model_name", "modelId", "model_id", "composerModel"}

func cursorModelHint(rec shared.JSON) string {
	raw := ""
	if s, ok := shared.FirstString(rec, cursorModelKeys...); ok {
		raw = s
	} else if s, ok := shared.FirstString(shared.Get(rec, "message"), cursorModelKeys...); ok {
		raw = s
	}
	return shared.CanonicalizeModel("cursor", raw)
}

func cursorRole(rec shared.JSON) string {
	role := ""
	if s, ok := shared.FirstString(rec, "role"); ok {
		role = s
	} else if s, ok := shared.FirstString(shared.Get(rec, "message"), "role"); ok {
		role = s
	}
	return strings.ToLower(strings.TrimSpace(role))
}

func firstStringOf(v, fallback shared.JSON, keys []string) (string, bool) {
	if s, ok := shared.FirstString(v, keys...); ok {
		return s, true
	}
	return shared.FirstString(fallback, keys...)
}
