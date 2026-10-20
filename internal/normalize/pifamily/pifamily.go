// Package pifamily ports upstream crates/moraine-ingest-core/src/sources/pi_family.rs,
// the record normalizer shared by the pi-coding-agent and prime-agent
// harnesses.
package pifamily

import (
	"fmt"
	"sort"
	"strings"

	"github.com/candyflipline/mnemonic/internal/normalize/shared"
)

// Policy mirrors upstream PiFamilyPolicy: the harness spelling of the
// pi-family source and whether unknown record types stay raw only.
type Policy struct {
	Harness         string
	RawOnlyFallback bool
}

// PI mirrors upstream PiFamilyPolicy::PI.
var PI = Policy{Harness: "pi-coding-agent"}

// PRIME mirrors upstream PiFamilyPolicy::PRIME.
var PRIME = Policy{Harness: "prime-agent", RawOnlyFallback: true}

// SourceMetadata mirrors upstream pi_family_source_metadata: provider and
// model hints carried by model_change and message records.
func SourceMetadata(rec shared.JSON) shared.SourceMetadata {
	switch shared.ToStr(shared.Get(rec, "type")) {
	case "model_change":
		return shared.SourceMetadata{
			InferenceProvider: shared.ToStr(shared.Get(rec, "provider")),
			ModelHintFallback: shared.ToStr(shared.Get(rec, "modelId")),
		}
	case "message":
		message := shared.Get(rec, "message")
		return shared.SourceMetadata{
			InferenceProvider: shared.ToStr(shared.Get(message, "provider")),
			ModelHintFallback: shared.ToStr(shared.Get(message, "model")),
		}
	default:
		return shared.SourceMetadata{InferenceProvider: ""}
	}
}

// NormalizeRecord mirrors upstream normalize_pi_family_record: one record
// into event rows under the given policy. Link rows are not kept in this
// port.
func NormalizeRecord(rec shared.JSON, ctx *shared.Ctx, topType, baseUID, modelHint string, policy Policy) *shared.Partials {
	em := shared.NewEmitter(ctx)
	switch topType {
	case "session":
		handleSession(rec, baseUID, em)
	case "message":
		handleMessage(rec, baseUID, modelHint, policy.Harness, em)
	case "model_change":
		handleModelChange(rec, baseUID, policy.Harness, em)
	case "thinking_level_change":
		handleThinkingLevelChange(rec, baseUID, em)
	case "compaction":
		handleCompaction(rec, baseUID, em)
	case "branch_summary":
		handleBranchSummary(rec, baseUID, em)
	case "custom":
		if !policy.RawOnlyFallback {
			handleCustom(rec, baseUID, em)
		}
	case "custom_message":
		handleCustomMessage(rec, baseUID, em)
	case "label":
		handleLabel(rec, baseUID, em)
	case "title", "title_change":
		// The omp title shapes only exist for the omp source name; anything
		// else follows the harness's own fallback.
		if ctx.SourceName == "omp" {
			handleOMPTitle(rec, topType, baseUID, em)
		} else if !policy.RawOnlyFallback {
			handleUnknown(rec, topType, baseUID, em)
		}
	default:
		if !policy.RawOnlyFallback {
			handleUnknown(rec, topType, baseUID, em)
		}
	}
	return &em.Partials
}

func handleSession(rec shared.JSON, baseUID string, em *shared.Emitter) {
	row := em.EventForJSON(baseUID, "session_meta", "session_meta", "system",
		shared.ToStr(shared.Get(rec, "cwd")), rec)
	row.Event.ItemID = shared.ToStr(shared.Get(rec, "id"))
	em.PushEvent(row)
}

func handleOMPTitle(rec shared.JSON, topType, baseUID string, em *shared.Emitter) {
	row := em.EventForJSON(baseUID, "session_meta", "session_meta", "system",
		shared.ToStr(shared.Get(rec, "title")), rec)
	row.Event.ItemID = shared.ToStr(shared.Get(rec, "id"))
	row.Event.OpKind = topType
	em.PushEvent(row)
}

func handleMessage(rec shared.JSON, baseUID, modelHint, modelHarness string, em *shared.Emitter) {
	message := shared.Get(rec, "message")
	switch shared.ToStr(shared.Get(message, "role")) {
	case "user":
		handleUserMessage(rec, message, baseUID, modelHint, modelHarness, em)
	case "assistant":
		handleAssistantMessage(rec, message, baseUID, modelHint, modelHarness, em)
	case "toolResult":
		handleToolResultMessage(rec, message, baseUID, modelHint, modelHarness, em)
	case "bashExecution":
		handleBashExecutionMessage(rec, message, baseUID, modelHint, modelHarness, em)
	case "custom":
		handleCustomRoleMessage(rec, message, baseUID, modelHint, modelHarness, em)
	case "branchSummary":
		handleBranchSummaryMessage(rec, message, baseUID, modelHint, modelHarness, em)
	case "compactionSummary":
		handleCompactionSummaryMessage(rec, message, baseUID, modelHint, modelHarness, em)
	default:
		handleUnknownMessage(rec, message, baseUID, modelHint, modelHarness, em)
	}
}

func handleUserMessage(rec, message shared.JSON, baseUID, modelHint, modelHarness string, em *shared.Emitter) {
	content := shared.Get(message, "content")
	row := stampMessageCommon(
		em.EventForJSON(baseUID, "message", "user_message", "user",
			shared.ExtractMessageText(content), message).
			SetContentTypes(ContentTypes(content)),
		message, modelHint, modelHarness)
	row.Event.ItemID = shared.ToStr(shared.Get(rec, "id"))
	em.PushEvent(row)
}

func handleAssistantMessage(rec, message shared.JSON, baseUID, modelHint, modelHarness string, em *shared.Emitter) {
	content := shared.Get(message, "content")
	accounting := tokenAccounting(message)
	take := func() *shared.TokenAccounting {
		t := accounting
		accounting = nil
		return t
	}
	emitted := 0
	if blocks, ok := shared.AsArray(content); ok {
		for idx, block := range blocks {
			blockType := shared.ToStr(shared.Get(block, "type"))
			blockUID := em.Ctx.UIDForJSON(block, fmt.Sprintf("pi:assistant:block:%d", idx))
			var row *shared.EventRow
			switch blockType {
			case "thinking":
				row = assistantThinkingBlock(message, block, blockUID, modelHint, modelHarness, take(), em)
			case "toolCall":
				row = assistantToolCallBlock(message, block, blockUID, modelHint, modelHarness, take(), em)
			default:
				row = assistantTextBlock(message, block, blockUID, blockType, modelHint, modelHarness, take(), em)
			}
			row.Event.ItemID = shared.ToStr(shared.Get(rec, "id"))
			em.PushEvent(row)
			emitted++
		}
	}
	if emitted == 0 {
		row := stampMessageCommon(
			em.EventForJSON(baseUID, "message", "agent_message", "assistant",
				shared.ExtractMessageText(content), message).
				SetContentTypes(ContentTypes(content)),
			message, modelHint, modelHarness)
		row.Event.ItemID = shared.ToStr(shared.Get(rec, "id"))
		em.PushEvent(stampTokenAccounting(row, take()))
	}
}

func assistantTextBlock(message, block shared.JSON, blockUID, blockType, modelHint, modelHarness string, accounting *shared.TokenAccounting, em *shared.Emitter) *shared.EventRow {
	payloadType := blockType
	if payloadType == "" {
		payloadType = "text"
	}
	row := stampMessageCommon(
		em.EventForJSON(blockUID, "message", payloadType, "assistant",
			shared.ExtractMessageText(block), block).
			SetContentTypes(ContentTypes(block)),
		message, modelHint, modelHarness)
	return stampTokenAccounting(row, accounting)
}

func assistantThinkingBlock(message, block shared.JSON, blockUID, modelHint, modelHarness string, accounting *shared.TokenAccounting, em *shared.Emitter) *shared.EventRow {
	row := stampMessageCommon(
		em.EventForJSON(blockUID, "reasoning", "thinking", "assistant",
			shared.ExtractMessageText(block), block).MarkReasoning(),
		message, modelHint, modelHarness)
	return stampTokenAccounting(row, accounting)
}

func assistantToolCallBlock(message, block shared.JSON, blockUID, modelHint, modelHarness string, accounting *shared.TokenAccounting, em *shared.Emitter) *shared.EventRow {
	toolCallID := shared.ToStr(shared.Get(block, "id"))
	toolName := shared.ToStr(shared.Get(block, "name"))
	input := shared.Get(block, "arguments")
	inputJSON := shared.Compact(input)
	inputText := shared.ExtractMessageText(input)
	if inputText == "" {
		inputText = inputJSON
	}
	row := stampMessageCommon(
		em.EventForJSON(blockUID, "tool_call", "tool_use", "assistant", inputText, block).
			SetContentTypes([]string{"tool_use"}),
		message, modelHint, modelHarness)
	row.Event.ToolCallID = toolCallID
	row.Event.ToolName = toolName
	em.PushToolRequest(blockUID, toolCallID, "", toolName, inputJSON)
	return stampTokenAccounting(row, accounting)
}

func handleToolResultMessage(rec, message shared.JSON, baseUID, modelHint, modelHarness string, em *shared.Emitter) {
	content := shared.Get(message, "content")
	toolCallID := shared.ToStr(shared.Get(message, "toolCallId"))
	toolName := shared.ToStr(shared.Get(message, "toolName"))
	outputText := shared.ExtractMessageText(content)
	outputJSON := shared.Compact(message)
	toolError := shared.ToU8Bool(shared.Get(message, "isError"))
	row := stampMessageCommon(
		em.EventForJSON(baseUID, "tool_result", "tool_result", "tool", outputText, message).
			SetContentTypes(ContentTypes(content)),
		message, modelHint, modelHarness)
	row.Event.ItemID = shared.ToStr(shared.Get(rec, "id"))
	row.Event.ToolCallID = toolCallID
	row.Event.ToolName = toolName
	row.Event.ToolError = toolError
	em.PushEvent(row)
	em.PushToolResponse(baseUID, toolCallID, "", toolName, toolError, "", outputJSON, outputText)
}

func handleBashExecutionMessage(rec, message shared.JSON, baseUID, modelHint, modelHarness string, em *shared.Emitter) {
	command := shared.ToStr(shared.Get(message, "command"))
	output := shared.ToStr(shared.Get(message, "output"))
	toolCallID := shared.ToStr(shared.Get(rec, "id"))
	var toolError uint8
	if shared.ToU32(shared.Get(message, "exitCode")) != 0 || shared.ToU8Bool(shared.Get(message, "cancelled")) != 0 {
		toolError = 1
	}
	inputJSON := shared.Compact(map[string]any{"command": command})
	outputJSON := shared.Compact(message)
	row := stampMessageCommon(
		em.EventForJSON(baseUID, "tool_result", "tool_result", "tool", output, message).
			SetContentTypes([]string{"tool_result"}),
		message, modelHint, modelHarness)
	row.Event.ItemID = shared.ToStr(shared.Get(rec, "id"))
	row.Event.ToolCallID = toolCallID
	row.Event.ToolName = "bash"
	row.Event.ToolError = toolError
	em.PushEvent(row)
	em.PushToolRequest(baseUID, toolCallID, "", "bash", inputJSON)
	em.PushToolResponse(baseUID, toolCallID, "", "bash", toolError, inputJSON, outputJSON, output)
}

func handleCustomRoleMessage(rec, message shared.JSON, baseUID, modelHint, modelHarness string, em *shared.Emitter) {
	content := shared.Get(message, "content")
	row := stampMessageCommon(
		em.EventForJSON(baseUID, "message", "message", "system",
			shared.ExtractMessageText(content), message).
			SetContentTypes(ContentTypes(content)),
		message, modelHint, modelHarness)
	row.Event.ItemID = shared.ToStr(shared.Get(rec, "id"))
	row.Event.OpKind = shared.ToStr(shared.Get(message, "customType"))
	em.PushEvent(row)
}

func handleBranchSummaryMessage(rec, message shared.JSON, baseUID, modelHint, modelHarness string, em *shared.Emitter) {
	row := stampMessageCommon(
		em.EventForJSON(baseUID, "summary", "summary", "system",
			shared.ToStr(shared.Get(message, "summary")), message),
		message, modelHint, modelHarness)
	row.Event.ItemID = shared.ToStr(shared.Get(rec, "id"))
	row.Event.OriginEventID = shared.ToStr(shared.Get(message, "fromId"))
	em.PushEvent(row)
}

func handleCompactionSummaryMessage(rec, message shared.JSON, baseUID, modelHint, modelHarness string, em *shared.Emitter) {
	row := stampMessageCommon(
		em.EventForJSON(baseUID, "summary", "compacted", "system",
			shared.ToStr(shared.Get(message, "summary")), message),
		message, modelHint, modelHarness)
	row.Event.ItemID = shared.ToStr(shared.Get(rec, "id"))
	em.PushEvent(row)
}

func handleUnknownMessage(rec, message shared.JSON, baseUID, modelHint, modelHarness string, em *shared.Emitter) {
	row := stampMessageCommon(
		em.EventForJSON(baseUID, "unknown", "message", "system",
			shared.ExtractMessageText(message), message),
		message, modelHint, modelHarness)
	row.Event.ItemID = shared.ToStr(shared.Get(rec, "id"))
	em.PushEvent(row)
}

func handleModelChange(rec shared.JSON, baseUID, modelHarness string, em *shared.Emitter) {
	provider := shared.ToStr(shared.Get(rec, "provider"))
	model := shared.CanonicalizeModel(modelHarness, shared.ToStr(shared.Get(rec, "modelId")))
	text := model
	if provider != "" {
		text = provider + "/" + model
	}
	row := em.EventForJSON(baseUID, "system", "system", "system", text, rec)
	row.Event.ItemID = shared.ToStr(shared.Get(rec, "id"))
	row.Event.OpKind = "model_change"
	row.Event.Model = model
	em.PushEvent(row)
}

func handleThinkingLevelChange(rec shared.JSON, baseUID string, em *shared.Emitter) {
	row := em.EventForJSON(baseUID, "system", "thinking", "system",
		shared.ToStr(shared.Get(rec, "thinkingLevel")), rec)
	row.Event.ItemID = shared.ToStr(shared.Get(rec, "id"))
	row.Event.OpKind = "thinking_level_change"
	em.PushEvent(row)
}

func handleCompaction(rec shared.JSON, baseUID string, em *shared.Emitter) {
	row := em.EventForJSON(baseUID, "summary", "compacted", "system",
		shared.ToStr(shared.Get(rec, "summary")), rec)
	row.Event.ItemID = shared.ToStr(shared.Get(rec, "id"))
	row.Event.OriginEventID = shared.ToStr(shared.Get(rec, "firstKeptEntryId"))
	em.PushEvent(row)
}

func handleBranchSummary(rec shared.JSON, baseUID string, em *shared.Emitter) {
	row := em.EventForJSON(baseUID, "summary", "summary", "system",
		shared.ToStr(shared.Get(rec, "summary")), rec)
	row.Event.ItemID = shared.ToStr(shared.Get(rec, "id"))
	row.Event.OriginEventID = shared.ToStr(shared.Get(rec, "fromId"))
	em.PushEvent(row)
}

func handleCustom(rec shared.JSON, baseUID string, em *shared.Emitter) {
	row := em.EventForJSON(baseUID, "system", "system", "system",
		shared.ExtractMessageText(shared.Get(rec, "data")), rec)
	row.Event.ItemID = shared.ToStr(shared.Get(rec, "id"))
	row.Event.OpKind = shared.ToStr(shared.Get(rec, "customType"))
	em.PushEvent(row)
}

func handleCustomMessage(rec shared.JSON, baseUID string, em *shared.Emitter) {
	content := shared.Get(rec, "content")
	row := em.EventForJSON(baseUID, "message", "message", "system",
		shared.ExtractMessageText(content), rec)
	row.SetContentTypes(ContentTypes(content))
	row.Event.ItemID = shared.ToStr(shared.Get(rec, "id"))
	row.Event.OpKind = shared.ToStr(shared.Get(rec, "customType"))
	em.PushEvent(row)
}

func handleLabel(rec shared.JSON, baseUID string, em *shared.Emitter) {
	row := em.EventForJSON(baseUID, "system", "system", "system",
		shared.ToStr(shared.Get(rec, "label")), rec)
	row.Event.ItemID = shared.ToStr(shared.Get(rec, "id"))
	row.Event.OpKind = "label"
	row.Event.OriginEventID = shared.ToStr(shared.Get(rec, "targetId"))
	em.PushEvent(row)
}

func handleSessionInfo(rec shared.JSON, baseUID string, em *shared.Emitter) {
	row := em.EventForJSON(baseUID, "session_meta", "session_meta", "system",
		shared.ToStr(shared.Get(rec, "name")), rec)
	row.Event.ItemID = shared.ToStr(shared.Get(rec, "id"))
	row.Event.OpKind = "session_info"
	em.PushEvent(row)
}

func handleUnknown(rec shared.JSON, topType, baseUID string, em *shared.Emitter) {
	payloadType := topType
	if payloadType == "" {
		payloadType = "unknown"
	}
	row := em.EventForJSON(baseUID, "unknown", payloadType, "system",
		shared.ExtractMessageText(rec), rec)
	em.PushEvent(row)
}

// stampMessageCommon mirrors upstream pi_stamp_message_common: the message's
// model, falling back to the session model hint, and its stop reason as
// op_status.
func stampMessageCommon(row *shared.EventRow, message shared.JSON, modelHint, modelHarness string) *shared.EventRow {
	model := shared.CanonicalizeModel(modelHarness, shared.ToStr(shared.Get(message, "model")))
	if model == "" {
		model = shared.CanonicalizeModel(modelHarness, modelHint)
	}
	if model != "" {
		row.Event.Model = model
	}
	if stopReason := shared.ToStr(shared.Get(message, "stopReason")); stopReason != "" {
		row.Event.OpStatus = stopReason
	}
	return row
}

func stampTokenAccounting(row *shared.EventRow, accounting *shared.TokenAccounting) *shared.EventRow {
	if accounting != nil {
		row.SetTokenAccounting(*accounting)
	}
	return row
}

// tokenAccounting mirrors upstream pi_token_accounting: a generation
// accounting from the message's usage object, nil when it is absent or null.
func tokenAccounting(message shared.JSON) *shared.TokenAccounting {
	usage := shared.Get(message, "usage")
	if shared.IsNull(usage) {
		return nil
	}
	input := shared.ToU64(shared.Get(usage, "input"))
	output := shared.ToU64(shared.Get(usage, "output"))
	cacheRead := shared.ToU64(shared.Get(usage, "cacheRead"))
	cacheWrite := shared.ToU64(shared.Get(usage, "cacheWrite"))
	t := shared.NewTokenAccounting("generation").
		WithBuckets(
			shared.BucketValue{Key: "input_text", Value: input},
			shared.BucketValue{Key: "output_text", Value: output},
			shared.BucketValue{Key: "input_cache_read", Value: cacheRead},
			shared.BucketValue{Key: "input_cache_write", Value: cacheWrite},
		).
		WithLegacyScalars(satAdd(satAdd(input, cacheRead), cacheWrite), output, cacheRead, cacheWrite).
		WithRawUsage(usage)
	return &t
}

func satAdd(a, b uint64) uint64 {
	sum := a + b
	if sum < a {
		return ^uint64(0)
	}
	return sum
}

// ContentTypes mirrors upstream pi_content_types: the block types of a
// message content, with toolCall/thinking renamed, sorted and deduplicated.
func ContentTypes(content shared.JSON) []string {
	switch t := content.(type) {
	case string:
		if strings.TrimSpace(t) == "" {
			return []string{}
		}
		return []string{"text"}
	case []any:
		var out []string
		for _, item := range t {
			switch blockType := shared.ToStr(shared.Get(item, "type")); blockType {
			case "":
			case "toolCall":
				out = append(out, "tool_use")
			case "thinking":
				out = append(out, "reasoning")
			default:
				out = append(out, blockType)
			}
		}
		sort.Strings(out)
		out = compactSorted(out)
		if out == nil {
			out = []string{}
		}
		return out
	case map[string]any:
		switch blockType := shared.ToStr(shared.Get(content, "type")); blockType {
		case "":
			return []string{}
		case "toolCall":
			return []string{"tool_use"}
		case "thinking":
			return []string{"reasoning"}
		default:
			return []string{blockType}
		}
	default:
		return []string{}
	}
}

func compactSorted(sorted []string) []string {
	out := sorted[:0]
	for i, s := range sorted {
		if i == 0 || s != sorted[i-1] {
			out = append(out, s)
		}
	}
	return out
}
