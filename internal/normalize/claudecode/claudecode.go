package claudecode

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/candyflipline/mnemonic/internal/normalize"
	"github.com/candyflipline/mnemonic/internal/normalize/shared"
	"github.com/candyflipline/mnemonic/internal/schema"
)

type Parser struct{}

func (Parser) Harness() string { return "claude-code" }

func (Parser) Parse(r io.Reader, src normalize.SourceMeta) ([]schema.Event, error) {
	return shared.ParseJSONL(r, src, claudeSource{})
}

func init() { normalize.Register(Parser{}) }

type claudeSource struct{}

func (claudeSource) Harness() string                  { return "claude-code" }
func (claudeSource) DefaultInferenceProvider() string { return "anthropic" }

func (claudeSource) Preflight(rec shared.JSON) (shared.JSON, bool) { return rec, true }

func (claudeSource) SourceMetadata(rec shared.JSON) shared.SourceMetadata {
	return shared.DefaultSourceMetadata(claudeSource{}.DefaultInferenceProvider())
}

func (claudeSource) RecordTS(rec shared.JSON) string {
	return shared.ToStr(shared.Get(rec, "timestamp"))
}

func (claudeSource) TopType(rec shared.JSON) string { return shared.ToStr(shared.Get(rec, "type")) }

func (claudeSource) Cwd(rec shared.JSON) string {
	// Claude Code stamps the working directory on every JSONL record.
	return shared.ToStr(shared.Get(rec, "cwd"))
}

func (claudeSource) SessionID(rec shared.JSON, sctx shared.SourceRecordContext) string {
	if sctx.SourceName == "claude-cowork" {
		if sctx.SessionHint != "" {
			return sctx.SessionHint
		}
		if sessionID, ok := coworkSessionPath(sctx.SourceFile); ok {
			return sessionID
		}
	}
	if sessionID := shared.ToStr(shared.Get(rec, "sessionId")); sessionID != "" {
		return sessionID
	}
	if sctx.SessionHint == "" {
		return shared.InferSessionIDFromFile(sctx.SourceFile)
	}
	return sctx.SessionHint
}

func (claudeSource) Normalize(rec shared.JSON, ctx *shared.Ctx, topType, baseUID, modelHint string) *shared.Partials {
	r := newClaudeRecord(rec, topType, baseUID)
	em := shared.NewEmitter(ctx)
	switch topType {
	case "cowork-session-meta":
		normalizeCoworkSessionMeta(&r, em)
		return &em.Partials
	case "attachment", "last-prompt", "ai-title":
		return &em.Partials
	}
	if r.isMessageRecord() {
		normalizeClaudeMessageRecord(&r, em)
	} else {
		normalizeClaudeOperationalRecord(&r, em)
	}
	return &em.Partials
}

func coworkSessionPath(sourceFile string) (string, bool) {
	if filepath.Ext(sourceFile) != ".jsonl" || filepath.Base(sourceFile) == "audit.jsonl" {
		return "", false
	}
	for dir := sourceFile; ; {
		sessionID, ok := strings.CutPrefix(filepath.Base(dir), "local_")
		if ok && sessionID != "" && hasAncestorName(dir, "local-agent-mode-sessions") {
			rel := strings.TrimPrefix(strings.TrimPrefix(sourceFile, dir), "/")
			parts := strings.Split(rel, "/")
			if len(parts) >= 4 && parts[0] == ".claude" && parts[1] == "projects" && allNormalComponents(parts) {
				return filepath.Base(dir), true
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

func hasAncestorName(dir, name string) bool {
	for {
		if filepath.Base(dir) == name {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}

func allNormalComponents(parts []string) bool {
	for _, part := range parts {
		if part == "" {
			return false
		}
	}
	return true
}

type claudeRecord struct {
	rec              shared.JSON
	message          shared.JSON
	topType          string
	baseUID          string
	parentUUID       string
	requestID        string
	traceID          string
	agentRunID       string
	agentLabel       string
	coordGroupLabel  string
	isSubstream      uint8
	model            string
	accounting       shared.TokenAccounting
	serviceTier      string
	itemID           string
	parentToolCallID string
	originEventID    string
	originToolCallID string
	toolUseID        string
	toolPhase        string
}

func newClaudeRecord(rec shared.JSON, topType, baseUID string) claudeRecord {
	message := shared.Get(rec, "message")
	usage := shared.Get(message, "usage")
	inputTokens := shared.ToU32(shared.Get(usage, "input_tokens"))
	outputTokens := shared.ToU32(shared.Get(usage, "output_tokens"))
	cacheReadTokens := shared.ToU32(shared.Get(usage, "cache_read_input_tokens"))
	cacheWriteTokens := shared.ToU32(shared.Get(usage, "cache_creation_input_tokens"))
	reasoningTokens := shared.ToU64(usageReasoningTokens(usage))
	serverToolUseTokens := shared.SumNumericObject(shared.Get(usage, "server_tool_use"))
	outputText := shared.SatSub(uint64(outputTokens), reasoningTokens+serverToolUseTokens)

	return claudeRecord{
		rec:             rec,
		message:         message,
		topType:         topType,
		baseUID:         baseUID,
		parentUUID:      shared.ToStr(shared.Get(rec, "parentUuid")),
		requestID:       shared.ToStr(shared.Get(rec, "requestId")),
		traceID:         shared.ToStr(shared.Get(rec, "requestId")),
		agentRunID:      shared.ToStr(shared.Get(rec, "agentId")),
		agentLabel:      shared.ToStr(shared.Get(rec, "agentName")),
		coordGroupLabel: shared.ToStr(shared.Get(rec, "teamName")),
		isSubstream:     shared.ToU8Bool(shared.Get(rec, "isSidechain")),
		model:           shared.CanonicalizeModel("claude-code", shared.ToStr(shared.Get(message, "model"))),
		accounting: shared.NewTokenAccounting("generation").
			WithBuckets(
				shared.BucketValue{Key: "input_text", Value: uint64(inputTokens)},
				shared.BucketValue{Key: "output_text", Value: outputText},
				shared.BucketValue{Key: "input_cache_read", Value: uint64(cacheReadTokens)},
				shared.BucketValue{Key: "input_cache_write", Value: uint64(cacheWriteTokens)},
				shared.BucketValue{Key: "reasoning", Value: reasoningTokens},
				shared.BucketValue{Key: "server_tool_use", Value: serverToolUseTokens},
			).
			WithLegacyScalars(uint64(inputTokens), uint64(outputTokens), uint64(cacheReadTokens), uint64(cacheWriteTokens)),
		serviceTier:      shared.ToStr(shared.Get(usage, "service_tier")),
		itemID:           shared.ToStr(shared.Get(rec, "uuid")),
		parentToolCallID: shared.ToStr(shared.Get(rec, "parentToolUseID")),
		originEventID:    shared.ToStr(shared.Get(rec, "sourceToolAssistantUUID")),
		originToolCallID: shared.ToStr(shared.Get(rec, "sourceToolUseID")),
		toolUseID:        shared.ToStr(shared.Get(rec, "toolUseID")),
		toolPhase:        shared.ToStr(shared.Get(rec, "stop_reason")),
	}
}

func usageReasoningTokens(usage shared.JSON) shared.JSON {
	if v := shared.FirstOf(usage, "reasoning_tokens", "thinking_tokens"); v != nil {
		return v
	}
	return shared.Get(shared.Get(usage, "output_tokens_details"), "reasoning_tokens")
}

func (r *claudeRecord) isMessageRecord() bool { return r.topType == "assistant" || r.topType == "user" }

func (r *claudeRecord) stampCommon(row *shared.EventRow) *shared.EventRow {
	ev := &row.Event
	ev.RequestID = r.requestID
	ev.TraceID = r.traceID
	ev.AgentRunID = r.agentRunID
	ev.AgentLabel = r.agentLabel
	ev.CoordGroupLabel = r.coordGroupLabel
	if r.isSubstream != 0 {
		ev.IsSubstream = 1
	}
	ev.Model = r.model
	row.SetTokenAccounting(r.accounting)
	ev.ServiceTier = r.serviceTier
	ev.ItemID = r.itemID
	ev.OriginEventID = r.originEventID
	ev.OriginToolCallID = r.originToolCallID
	return row
}

func (r *claudeRecord) stampContentBlock(row *shared.EventRow) *shared.EventRow {
	row = r.stampCommon(row)
	row.Event.ParentToolCallID = r.parentToolCallID
	row.Event.OriginToolCallID = r.originToolCallID
	row.Event.ToolPhase = r.toolPhase
	return row
}

func normalizeClaudeMessageRecord(r *claudeRecord, em *shared.Emitter) {
	actor := claudeActorForMessage(r.topType, r.message)
	content := shared.Get(r.message, "content")
	if items, ok := shared.AsArray(content); ok && len(items) > 0 {
		for idx, item := range items {
			em.PushEvent(normalizeClaudeContentBlock(r, em, actor, idx, item))
		}
		return
	}
	normalizeClaudeScalarMessage(r, em, actor)
}

func claudeActorForMessage(topType string, message shared.JSON) string {
	if topType == "assistant" || shared.ToStr(shared.Get(message, "role")) == "assistant" {
		return "assistant"
	}
	return "user"
}

func normalizeClaudeContentBlock(r *claudeRecord, em *shared.Emitter, actor string, idx int, item shared.JSON) *shared.EventRow {
	blockType := shared.ToStr(shared.Get(item, "type"))
	blockUID := em.Ctx.UIDForJSON(item, fmt.Sprintf("claude:block:%d", idx))
	var row *shared.EventRow
	switch blockType {
	case "thinking":
		row = handleClaudeThinkingBlock(r, em, blockUID, item)
	case "tool_use":
		row = handleClaudeToolUseBlock(r, em, blockUID, item)
	case "tool_result":
		row = handleClaudeToolResultBlock(r, em, blockUID, item)
	default:
		row = handleClaudeTextBlock(r, em, blockUID, item, actor, blockType)
	}
	return row
}

func handleClaudeThinkingBlock(r *claudeRecord, em *shared.Emitter, blockUID string, item shared.JSON) *shared.EventRow {
	return r.stampContentBlock(
		em.EventForJSON(blockUID, "reasoning", "reasoning", "assistant",
			shared.ExtractMessageText(item), item).MarkReasoning())
}

func handleClaudeToolUseBlock(r *claudeRecord, em *shared.Emitter, blockUID string, item shared.JSON) *shared.EventRow {
	toolCallID := shared.ToStr(shared.Get(item, "id"))
	toolName := shared.ToStr(shared.Get(item, "name"))
	input := shared.Get(item, "input")
	inputJSON := shared.Compact(input)
	row := r.stampContentBlock(
		em.EventForJSON(blockUID, "tool_call", "tool_use", "assistant",
			shared.ExtractMessageText(input), item).
			SetContentTypes([]string{"tool_use"}))
	row.Event.ToolCallID = toolCallID
	row.Event.ToolName = toolName
	em.PushToolRequest(blockUID, toolCallID, r.parentToolCallID, toolName, inputJSON)
	return row
}

func handleClaudeToolResultBlock(r *claudeRecord, em *shared.Emitter, blockUID string, item shared.JSON) *shared.EventRow {
	toolCallID := shared.ToStr(shared.Get(item, "tool_use_id"))
	output := shared.Get(item, "content")
	outputJSON := shared.Compact(output)
	outputText := shared.ExtractMessageText(output)
	toolError := shared.ToU8Bool(shared.Get(item, "is_error"))
	row := r.stampContentBlock(
		em.EventForJSON(blockUID, "tool_result", "tool_result", "tool",
			outputText, item).
			SetContentTypes([]string{"tool_result"}))
	row.Event.ToolCallID = toolCallID
	row.Event.ToolError = toolError
	em.PushToolResponse(blockUID, toolCallID, r.parentToolCallID, "", toolError, "", outputJSON, outputText)
	return row
}

func handleClaudeTextBlock(r *claudeRecord, em *shared.Emitter, blockUID string, item shared.JSON, actor, blockType string) *shared.EventRow {
	payloadType := blockType
	if payloadType == "" {
		payloadType = "text"
	}
	row := em.EventForJSON(blockUID, "message", payloadType, actor,
		shared.ExtractMessageText(item), item)
	if blockType != "" {
		row.SetContentTypes([]string{blockType})
	}
	return r.stampContentBlock(row)
}

func normalizeClaudeScalarMessage(r *claudeRecord, em *shared.Emitter, actor string) {
	content := shared.Get(r.message, "content")
	row := r.stampCommon(em.Event(r.baseUID, "message", "message", actor,
		shared.ExtractMessageText(r.message), shared.Compact(r.rec)))
	row.SetContentTypes(shared.ExtractContentTypes(content))
	em.PushEvent(row)
}

func normalizeCoworkSessionMeta(r *claudeRecord, em *shared.Emitter) {
	title := shared.ToStr(shared.Get(r.rec, "title"))
	row := em.Event(r.baseUID, "session_meta", "session_meta", "system",
		title, shared.Compact(r.rec))
	row.Event.Model = shared.CanonicalizeModel("claude-code", shared.ToStr(shared.Get(r.rec, "model")))
	em.PushEvent(row)
}

func normalizeClaudeOperationalRecord(r *claudeRecord, em *shared.Emitter) {
	eventKind := claudeEventKindForTopType(r.topType)
	payloadType := claudePayloadType(r)
	eventPayloadType := payloadType
	if eventPayloadType == "" {
		eventPayloadType = r.topType
	}
	row := r.stampCommon(em.Event(r.baseUID, eventKind, eventPayloadType, "system",
		shared.ExtractMessageText(r.rec), shared.Compact(r.rec)))
	row.Event.OpKind = payloadType
	row.Event.OpStatus = shared.ToStr(shared.Get(r.rec, "status"))
	row.Event.LatencyMs = shared.ToU32(shared.Get(r.rec, "durationMs"))
	row.Event.RetryCount = shared.ToU16(shared.Get(r.rec, "retryAttempt"))
	em.PushEvent(row)
}

func claudeEventKindForTopType(topType string) string {
	switch topType {
	case "progress":
		return "progress"
	case "system":
		return "system"
	case "summary":
		return "summary"
	case "queue-operation":
		return "queue_operation"
	case "file-history-snapshot":
		return "file_history_snapshot"
	default:
		return "unknown"
	}
}

func claudePayloadType(r *claudeRecord) string {
	if r.topType == "progress" {
		return shared.ToStr(shared.Get(shared.Get(r.rec, "data"), "type"))
	}
	if r.topType == "system" {
		return shared.ToStr(shared.Get(r.rec, "subtype"))
	}
	return r.topType
}
